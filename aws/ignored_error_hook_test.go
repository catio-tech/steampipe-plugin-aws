//nolint:staticcheck // SA1019: the SDK still runs the deprecated ShouldIgnoreError predicates, so matching its decisions needs them.
package aws

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe-plugin-sdk/v6/grpc/proto"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin/context_key"
)

// recordHook collects what the hook gets.
type recordHook struct {
	mu   sync.Mutex
	errs []IgnoredError
}

func (r *recordHook) hook(_ context.Context, e IgnoredError) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, e)
}

func (r *recordHook) got() []IgnoredError {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]IgnoredError(nil), r.errs...)
}

func setHookForTest(t *testing.T, h IgnoredErrorHook) {
	t.Helper()
	SetIgnoredErrorHook(h)
	t.Cleanup(func() { SetIgnoredErrorHook(nil) })
}

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: "made up"} }

func codeIs(codes ...string) plugin.ErrorPredicateWithContext {
	return func(_ context.Context, _ *plugin.QueryData, _ *plugin.HydrateData, err error) bool {
		var ae smithy.APIError
		if !errors.As(err, &ae) {
			return false
		}
		for _, c := range codes {
			if ae.ErrorCode() == c {
				return true
			}
		}
		return false
	}
}

func legacyCodeIs(code string) plugin.ErrorPredicate {
	return func(err error) bool {
		var ae smithy.APIError
		return errors.As(err, &ae) && ae.ErrorCode() == code
	}
}

func decide(c *plugin.IgnoreConfig, err error) bool {
	ctx := context.WithValue(context.Background(), context_key.Logger, hclog.NewNullLogger())
	return (c.ShouldIgnoreError != nil && c.ShouldIgnoreError(err)) ||
		(c.ShouldIgnoreErrorFunc != nil && c.ShouldIgnoreErrorFunc(ctx, &plugin.QueryData{}, &plugin.HydrateData{}, err))
}

func TestEffectiveIgnoreFollowsDefaultTo(t *testing.T) {
	pluginDefault := &plugin.IgnoreConfig{ShouldIgnoreErrorFunc: codeIs("P")}
	tableDefault := &plugin.IgnoreConfig{ShouldIgnoreErrorFunc: codeIs("T")}
	for _, tc := range []struct {
		name     string
		own      *plugin.IgnoreConfig
		legacy   plugin.ErrorPredicate
		defaults []*plugin.IgnoreConfig
		ignores  []string
		keeps    []string
	}{
		{"own func wins", &plugin.IgnoreConfig{ShouldIgnoreErrorFunc: codeIs("O")}, nil, []*plugin.IgnoreConfig{tableDefault}, []string{"O"}, []string{"T", "P"}},
		{"own legacy in config wins", &plugin.IgnoreConfig{ShouldIgnoreError: legacyCodeIs("L")}, nil, []*plugin.IgnoreConfig{tableDefault}, []string{"L"}, []string{"T"}},
		{"legacy field wins over defaults", nil, legacyCodeIs("L"), []*plugin.IgnoreConfig{tableDefault}, []string{"L"}, []string{"T"}},
		{"empty own takes the first default", &plugin.IgnoreConfig{}, nil, []*plugin.IgnoreConfig{tableDefault, pluginDefault}, []string{"T"}, []string{"P"}},
		{"nil default is skipped", nil, nil, []*plugin.IgnoreConfig{nil, pluginDefault}, []string{"P"}, []string{"T"}},
		{"empty default is taken (DefaultTo copies nils)", nil, nil, []*plugin.IgnoreConfig{{}, pluginDefault}, []string{"P"}, nil},
		{"nothing ignores", nil, nil, nil, nil, []string{"T", "P"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := effectiveIgnore(tc.own, tc.legacy, tc.defaults...)
			for _, code := range tc.ignores {
				assert.True(t, decide(c, apiErr(code)), "ignores %s", code)
			}
			for _, code := range tc.keeps {
				assert.False(t, decide(c, apiErr(code)), "keeps %s", code)
			}
		})
	}
}

// Fake hydrate functions: each has its own name, as the SDK names hydrates.
func hydrateA(context.Context, *plugin.QueryData, *plugin.HydrateData) (any, error) { return nil, nil }
func hydrateB(context.Context, *plugin.QueryData, *plugin.HydrateData) (any, error) { return nil, nil }
func listFake(context.Context, *plugin.QueryData, *plugin.HydrateData) (any, error) { return nil, nil }
func getFake(context.Context, *plugin.QueryData, *plugin.HydrateData) (any, error)  { return nil, nil }

func fakePlugin() (*plugin.Plugin, *plugin.Table) {
	t := &plugin.Table{
		Name:                "fake_table",
		DefaultIgnoreConfig: &plugin.IgnoreConfig{ShouldIgnoreErrorFunc: codeIs("AccessDenied")},
		List:                &plugin.ListConfig{Hydrate: listFake, Tags: map[string]string{"service": "fake", "action": "ListThings"}},
		Get: &plugin.GetConfig{
			Hydrate:      getFake,
			KeyColumns:   plugin.SingleColumn("id"),
			IgnoreConfig: &plugin.IgnoreConfig{ShouldIgnoreErrorFunc: codeIs("NotFoundException")},
		},
		HydrateConfig: []plugin.HydrateConfig{{
			Func:         hydrateA,
			Tags:         map[string]string{"service": "fake", "action": "DescribeThing"},
			IgnoreConfig: &plugin.IgnoreConfig{ShouldIgnoreErrorFunc: codeIs("AccessDeniedException")},
		}},
		Columns: []*plugin.Column{
			{Name: "id", Type: proto.ColumnType_STRING},
			{Name: "detail", Type: proto.ColumnType_STRING, Hydrate: hydrateA},
			{Name: "detail_2", Type: proto.ColumnType_STRING, Hydrate: hydrateA},
			{Name: "tags", Type: proto.ColumnType_JSON, Hydrate: hydrateB},
			{Name: "get_only", Type: proto.ColumnType_STRING, Hydrate: getFake},
		},
	}
	p := &plugin.Plugin{
		Name:                "fake",
		DefaultIgnoreConfig: &plugin.IgnoreConfig{ShouldIgnoreErrorFunc: codeIs("UnauthorizedOperation")},
		TableMap:            map[string]*plugin.Table{t.Name: t},
	}
	return p, t
}

func configFor(t *plugin.Table, f plugin.HydrateFunc) *plugin.IgnoreConfig {
	name := helpers.GetFunctionName(f)
	for i := range t.HydrateConfig {
		if helpers.GetFunctionName(t.HydrateConfig[i].Func) == name {
			return t.HydrateConfig[i].IgnoreConfig
		}
	}
	return nil
}

func callPredicate(c *plugin.IgnoreConfig, d *plugin.QueryData, err error) bool {
	ctx := context.WithValue(context.Background(), context_key.Logger, hclog.NewNullLogger())
	return c.ShouldIgnoreErrorFunc(ctx, d, &plugin.HydrateData{}, err)
}

func TestReportIgnoredErrorsNamesTheCall(t *testing.T) {
	rec := &recordHook{}
	setHookForTest(t, rec.hook)
	p, tbl := fakePlugin()
	reportIgnoredErrors(p)

	d := &plugin.QueryData{
		Table:       tbl,
		Connection:  &plugin.Connection{Name: "conn_1"},
		EqualsQuals: map[string]*proto.QualValue{"region": {Value: &proto.QualValue_StringValue{StringValue: "us-east-1"}}},
	}

	// hydrateA has its own config: it ignores only AccessDeniedException.
	a := configFor(tbl, hydrateA)
	require.NotNil(t, a)
	assert.True(t, callPredicate(a, d, apiErr("AccessDeniedException")))
	assert.False(t, callPredicate(a, d, apiErr("AccessDenied")), "the table default does not apply to a config that sets a check")
	// hydrateB had no config: it got one with the table's default.
	b := configFor(tbl, hydrateB)
	require.NotNil(t, b, "a column hydrate with no config gets one")
	assert.True(t, callPredicate(b, d, apiErr("AccessDenied")))
	assert.False(t, callPredicate(b, d, apiErr("UnauthorizedOperation")), "the table default set a check, so the plugin default does not apply")
	assert.Nil(t, configFor(tbl, getFake), "the get call keeps its own config, not a second one")
	assert.Nil(t, configFor(tbl, listFake))
	// list and get.
	assert.True(t, callPredicate(tbl.List.IgnoreConfig, d, apiErr("AccessDenied")))
	assert.True(t, callPredicate(tbl.Get.IgnoreConfig, d, apiErr("NotFoundException")))
	assert.False(t, callPredicate(tbl.Get.IgnoreConfig, d, apiErr("AccessDenied")))

	assert.Equal(t, []IgnoredError{
		{Connection: "conn_1", Table: "fake_table", Region: "us-east-1", Call: "hydrateA", Columns: []string{"detail", "detail_2"}, Code: "AccessDeniedException", Action: "fake:DescribeThing"},
		{Connection: "conn_1", Table: "fake_table", Region: "us-east-1", Call: "hydrateB", Columns: []string{"tags"}, Code: "AccessDenied"},
		{Connection: "conn_1", Table: "fake_table", Region: "us-east-1", Call: "list", Code: "AccessDenied", Action: "fake:ListThings"},
		{Connection: "conn_1", Table: "fake_table", Region: "us-east-1", Call: "get", Columns: []string{"get_only"}, Code: "NotFoundException"},
	}, rec.got(), "one report per ignored error, none for an error that is not ignored")
}

func TestReportIgnoredErrorsClearsLegacyChecks(t *testing.T) {
	rec := &recordHook{}
	setHookForTest(t, rec.hook)
	p, tbl := fakePlugin()
	tbl.HydrateConfig[0].IgnoreConfig = nil
	tbl.HydrateConfig[0].ShouldIgnoreError = legacyCodeIs("Legacy")
	reportIgnoredErrors(p)

	c := configFor(tbl, hydrateA)
	assert.Nil(t, tbl.HydrateConfig[0].ShouldIgnoreError, "a legacy check would run first and skip the report")
	assert.Nil(t, c.ShouldIgnoreError)
	assert.True(t, callPredicate(c, &plugin.QueryData{Table: tbl}, apiErr("Legacy")))
	require.Len(t, rec.got(), 1)
	assert.Equal(t, "hydrateA", rec.got()[0].Call)
}

func TestNoReportWithoutHookAtCallTime(t *testing.T) {
	rec := &recordHook{}
	setHookForTest(t, rec.hook)
	p, tbl := fakePlugin()
	reportIgnoredErrors(p)
	SetIgnoredErrorHook(nil)
	assert.True(t, callPredicate(tbl.List.IgnoreConfig, &plugin.QueryData{Table: tbl}, apiErr("AccessDenied")), "the decision does not depend on the hook")
	assert.Empty(t, rec.got())
}

// Without a hook, Plugin builds the tables exactly as before: the Steampipe
// path (the old extraction path) uses this fork too.
func TestPluginWithoutHookIsUnchanged(t *testing.T) {
	SetIgnoredErrorHook(nil)
	kms := Plugin(context.Background()).TableMap["aws_kms_key"]
	assert.Len(t, kms.HydrateConfig, 5)
	assert.Nil(t, kms.List.IgnoreConfig)
	assert.NotNil(t, kms.Get.IgnoreConfig.ShouldIgnoreErrorFunc)
}

// sampleErrors are the codes that the aws plugin's checks name, and one error
// with no code.
var sampleErrors = []error{
	apiErr("AccessDenied"), apiErr("AccessDeniedException"), apiErr("UnauthorizedOperation"),
	apiErr("NoSuchTagSet"), apiErr("NotFoundException"), apiErr("ResourceNotFoundException"),
	apiErr("NoSuchEntity"), apiErr("InvalidParameterValue"), apiErr("ValidationException"),
	apiErr("ThrottlingException"), apiErr("NoSuchBucketPolicy"), errors.New("no code"),
}

// decision is one call's decision for one error, with a panic recorded as such.
func decision(fn func() bool) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "panic"
		}
	}()
	return fmt.Sprint(fn())
}

// serve initialises a plugin as the worker's host does, so the SDK resolves
// (and validates) every table's configs.
func serve(t *testing.T, p *plugin.Plugin, config string) {
	t.Helper()
	srv := plugin.Server(&plugin.ServeOpts{PluginFunc: func(context.Context) *plugin.Plugin { return p }})
	res, err := srv.SetAllConnectionConfigs(&proto.SetAllConnectionConfigsRequest{
		Configs: []*proto.ConnectionConfig{{
			Connection: "conn_1", Plugin: "hub.steampipe.io/plugins/turbot/aws@latest",
			PluginShortName: "aws", PluginInstance: "aws", Config: config,
		}},
		MaxCacheSizeMb: -1,
	})
	require.NoError(t, err)
	require.Empty(t, res.FailedConnections, "the SDK accepts the tables")
}

// Across every table of the plugin, each call's new check makes the same
// decision as the check that the SDK resolves for that call without a hook,
// for every sample error, with and without the permission codes in the
// connection's ignore_error_codes.
func TestEveryCallDecidesAsBefore(t *testing.T) {
	for _, codes := range [][]string{nil, {"NoSuchTagSet", "AccessDenied", "AccessDeniedException", "UnauthorizedOperation"}} {
		t.Run(fmt.Sprint(codes), func(t *testing.T) {
			hcl := "regions = [\"us-east-1\"]\n"
			if codes != nil {
				hcl = fmt.Sprintf("regions = [\"us-east-1\"]\nignore_error_codes = [%s]\n", quoteList(codes))
			}
			conn := &plugin.Connection{Name: "conn_1"}
			conn.SetConfig(awsConfig{Regions: []string{"us-east-1"}, IgnoreErrorCodes: codes})

			SetIgnoredErrorHook(nil)
			before := Plugin(context.Background())
			serve(t, before, hcl)

			rec := &recordHook{}
			setHookForTest(t, rec.hook)
			after := Plugin(context.Background())
			serve(t, after, hcl)

			ctx := context.WithValue(context.Background(), context_key.Logger, hclog.NewNullLogger())
			checked := 0
			for name, tb := range before.TableMap {
				ta := after.TableMap[name]
				require.NotNil(t, ta, name)
				d := &plugin.QueryData{Table: ta, Connection: conn}
				run := func(c *plugin.IgnoreConfig, err error) string {
					return decision(func() bool {
						return (c.ShouldIgnoreError != nil && c.ShouldIgnoreError(err)) ||
							(c.ShouldIgnoreErrorFunc != nil && c.ShouldIgnoreErrorFunc(ctx, d, &plugin.HydrateData{}, err))
					})
				}
				compare := func(call string, b, a *plugin.IgnoreConfig) {
					require.NotNil(t, a, "%s %s has a config", name, call)
					assert.Nil(t, a.ShouldIgnoreError, "%s %s: no legacy check", name, call)
					for _, err := range sampleErrors {
						assert.Equal(t, run(b, err), run(a, err), "%s %s: %v", name, call, err)
						checked++
					}
				}
				if tb.List != nil {
					compare("list", tb.List.IgnoreConfig, ta.List.IgnoreConfig)
				}
				if tb.Get != nil {
					compare("get", tb.Get.IgnoreConfig, ta.Get.IgnoreConfig)
				}
				// Each column hydrate: the SDK's resolved config before (its own, or
				// the table default for one with no config), the new config after.
				for _, col := range tb.Columns {
					if col.Hydrate == nil {
						continue
					}
					hn := helpers.GetFunctionName(col.Hydrate)
					if tb.Get != nil && hn == helpers.GetFunctionName(tb.Get.Hydrate) {
						continue
					}
					b := configFor(tb, col.Hydrate)
					if b == nil {
						b = tb.DefaultIgnoreConfig
					}
					compare(hn, b, configFor(ta, col.Hydrate))
				}
			}
			assert.Greater(t, checked, 10000, "the test covers the plugin")
		})
	}
}

func quoteList(codes []string) string {
	out := ""
	for i, c := range codes {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", c)
	}
	return out
}
