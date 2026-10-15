//nolint:staticcheck // SA1019: the SDK still runs the deprecated ShouldIgnoreError predicates, so matching its decisions needs them.
package aws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/aws/smithy-go"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
)

// IgnoredError is one error that the plugin ignored: the call failed, and the
// table scan went on without its data. A host that runs the plugin in its own
// process (the catiopipe-worker) gets one per ignored error through
// SetIgnoredErrorHook, so it can tell "denied" from "empty" and list the columns
// that a denied call did not fill.
type IgnoredError struct {
	// Connection is the connection name.
	Connection string
	// Table is the table name, for example "aws_kms_key".
	Table string
	// Region is the scan's region, or "" for a table that has no region matrix.
	Region string
	// Call is "list" or "get" for the table's list and get calls (for a table with
	// a ParentHydrate, "list" is the parent list call: the SDK checks only that
	// one against the list's ignore config; a denied child list fails the scan),
	// and the hydrate function's name (for example "getAwsKmsKeyRotationStatus")
	// for a column's detail call.
	Call string
	// Columns are the columns that the call fills: for a detail call, the
	// columns whose Hydrate is the call; for a get call that hydrates a listed
	// row, the columns whose Hydrate is the get. Empty for the list, and for a get
	// that fetches the row itself: their failure leaves no row. A new slice on
	// every report.
	Columns []string
	// Code is the AWS error code (smithy.APIError), or "" when the error has none.
	Code string
	// Action is the IAM action of the call from its tags, for example
	// "kms:GetKeyRotationStatus", or "" when the call has no service and action
	// tags.
	Action string
}

// IgnoredErrorHook receives each ignored error. The plugin calls it from many
// table scans at once, once per ignored error (so once per row for a detail
// call), and waits for it: it must be fast and safe for concurrent use. The
// hook gets every error that the connection ignores, also the ones that mean
// "no data" (NotFound, NoSuchTagSet): the host classifies them by Code. It
// gets no error text: AWS error messages name ARNs, users and emails. A panic
// in the hook is recovered and dropped, so it cannot fail the scan.
type IgnoredErrorHook func(ctx context.Context, e IgnoredError)

var (
	ignoredErrorHook atomic.Pointer[IgnoredErrorHook]
	// builtWithHook: Plugin gave every call its own ignore check (a hook was set
	// when it built the tables). Code that turns a hidden denial into an error
	// for the check to report (aws_kms_key) reads this, not the hook: without
	// the per-call checks, nothing would report it.
	builtWithHook atomic.Bool
)

// SetIgnoredErrorHook sets the hook. Set it before the host builds the plugin
// (Plugin): only a plugin built while a hook is set gives each call its own
// ignore check, which is what lets the plugin name the call. With no hook,
// Plugin builds the tables exactly as before, and nothing is reported. nil
// removes the hook (calls then report nothing).
func SetIgnoredErrorHook(h IgnoredErrorHook) {
	if h == nil {
		ignoredErrorHook.Store(nil)
		return
	}
	ignoredErrorHook.Store(&h)
}

func getIgnoredErrorHook() IgnoredErrorHook {
	if h := ignoredErrorHook.Load(); h != nil {
		return *h
	}
	return nil
}

// reportIgnoredErrors gives every call of every table its own ignore check, so
// the check knows which call failed. The SDK passes the check no call name
// (WrapHydrate gives ShouldIgnoreErrorFunc only ctx, the query data, the
// hydrate data and the error), and many calls share the table's and the
// plugin's default checks.
//
// Each new check decides exactly as the SDK would have decided for that call:
// it resolves the call's own config, then the plugin's DefaultGetConfig (get
// only), then the table's default, then the plugin's default, in the SDK's
// order (IgnoreConfig.DefaultTo: the first config that sets either predicate
// wins, legacy ShouldIgnoreError first). It then reports each error that it
// ignores. The call's legacy ShouldIgnoreError fields are cleared, because the
// SDK runs a legacy predicate before ShouldIgnoreErrorFunc and returns at once
// when it is true: the report would be skipped.
//
// It must run before the SDK initialises the tables (the SDK copies defaults
// into each config then, and a column hydrate with no HydrateConfig gets a
// private config that this code cannot reach). So each column hydrate without
// a HydrateConfig gets one, with only its Func and the new check: the SDK
// fills the rest from the table's defaults as before.
func reportIgnoredErrors(p *plugin.Plugin) {
	builtWithHook.Store(true)
	pluginLevel := map[string]bool{}
	for i := range p.HydrateConfig {
		c := &p.HydrateConfig[i]
		name := helpers.GetFunctionName(c.Func)
		pluginLevel[name] = true
		// The SDK initialises a plugin-level config with no table, so with no
		// defaults.
		own := effectiveIgnore(c.IgnoreConfig, c.ShouldIgnoreError)
		c.IgnoreConfig = reportingIgnoreConfig(own, shortFuncName(name), nil, tagAction(c.Tags), false)
		c.ShouldIgnoreError = nil
	}
	for _, t := range p.TableMap {
		reportTableIgnoredErrors(p, t, pluginLevel)
	}
}

func reportTableIgnoredErrors(p *plugin.Plugin, t *plugin.Table, pluginLevel map[string]bool) {
	tableDefault := tableDefaultIgnore(p, t)

	// The columns that each detail call fills.
	columns := map[string][]string{}
	for _, col := range t.Columns {
		if col == nil || col.Hydrate == nil {
			continue
		}
		name := helpers.GetFunctionName(col.Hydrate)
		columns[name] = append(columns[name], col.Name)
	}

	if t.List != nil {
		own := effectiveIgnore(t.List.IgnoreConfig, t.List.ShouldIgnoreError, tableDefault)
		// With a ParentHydrate, the SDK checks the parent call against the list's
		// config: its action is the parent's (ParentTags), not the child's.
		action := tagAction(t.List.Tags)
		if t.List.ParentHydrate != nil {
			action = tagAction(t.List.ParentTags)
		}
		t.List.IgnoreConfig = reportingIgnoreConfig(own, "list", nil, action, false)
		t.List.ShouldIgnoreError = nil
	}
	listName := ""
	if t.List != nil && t.List.Hydrate != nil {
		listName = helpers.GetFunctionName(t.List.Hydrate)
	}
	getName := ""
	if t.Get != nil && t.Get.Hydrate != nil {
		getName = helpers.GetFunctionName(t.Get.Hydrate)
		var defaults []*plugin.IgnoreConfig
		if p.DefaultGetConfig != nil {
			defaults = append(defaults, effectiveIgnore(p.DefaultGetConfig.IgnoreConfig, p.DefaultGetConfig.ShouldIgnoreError))
		}
		defaults = append(defaults, tableDefault)
		own := effectiveIgnore(t.Get.IgnoreConfig, t.Get.ShouldIgnoreError, defaults...)
		// The get call also hydrates columns when the list does not fill them.
		t.Get.IgnoreConfig = reportingIgnoreConfig(own, "get", columns[getName], tagAction(t.Get.Tags), true)
		t.Get.ShouldIgnoreError = nil
	}

	configured := map[string]bool{}
	for i := range t.HydrateConfig {
		c := &t.HydrateConfig[i]
		if c.Func == nil {
			continue
		}
		name := helpers.GetFunctionName(c.Func)
		configured[name] = true
		own := effectiveIgnore(c.IgnoreConfig, c.ShouldIgnoreError, tableDefault)
		c.IgnoreConfig = reportingIgnoreConfig(own, shortFuncName(name), columns[name], tagAction(c.Tags), false)
		c.ShouldIgnoreError = nil
	}
	// Column hydrates with no HydrateConfig (not the get call, which has its
	// own config, and not one that the plugin configures for every table). A
	// table that uses the legacy HydrateDependencies may not also have
	// HydrateConfig (SDK validation), so its unconfigured hydrates stay as they
	// are and report nothing.
	if len(t.HydrateDependencies) > 0 {
		return
	}
	added := map[string]bool{}
	for _, col := range t.Columns {
		if col == nil || col.Hydrate == nil {
			continue
		}
		name := helpers.GetFunctionName(col.Hydrate)
		if configured[name] || added[name] || name == getName || name == listName || pluginLevel[name] {
			continue
		}
		added[name] = true
		t.HydrateConfig = append(t.HydrateConfig, plugin.HydrateConfig{
			Func:         col.Hydrate,
			IgnoreConfig: reportingIgnoreConfig(tableDefault, shortFuncName(name), columns[name], "", false),
		})
	}
}

// tableDefaultIgnore is the table's default ignore config as the SDK resolves
// it (Table.initialise): DefaultIgnoreConfig, its legacy DefaultShouldIgnoreError,
// then the plugin's DefaultIgnoreConfig.
func tableDefaultIgnore(p *plugin.Plugin, t *plugin.Table) *plugin.IgnoreConfig {
	return effectiveIgnore(t.DefaultIgnoreConfig, t.DefaultShouldIgnoreError, p.DefaultIgnoreConfig)
}

// effectiveIgnore resolves one call's ignore config without changing the
// inputs: own (with the legacy predicate copied in when own has none), then
// each default in order, by the rule of IgnoreConfig.DefaultTo.
func effectiveIgnore(own *plugin.IgnoreConfig, legacy plugin.ErrorPredicate, defaults ...*plugin.IgnoreConfig) *plugin.IgnoreConfig {
	c := &plugin.IgnoreConfig{}
	if own != nil {
		c.ShouldIgnoreError = own.ShouldIgnoreError
		c.ShouldIgnoreErrorFunc = own.ShouldIgnoreErrorFunc
	}
	if c.ShouldIgnoreError == nil {
		c.ShouldIgnoreError = legacy
	}
	for _, d := range defaults {
		if c.ShouldIgnoreError != nil || c.ShouldIgnoreErrorFunc != nil {
			break
		}
		if d == nil {
			continue
		}
		c.ShouldIgnoreError = d.ShouldIgnoreError
		c.ShouldIgnoreErrorFunc = d.ShouldIgnoreErrorFunc
	}
	return c
}

// reportingIgnoreConfig returns a config with one ShouldIgnoreErrorFunc that
// decides as eff does (legacy predicate first, as WrapHydrate does) and reports
// what it ignores. isGet: the get call, which either fetches a row (no item
// yet: its failure leaves no row, so no columns) or hydrates a listed row.
func reportingIgnoreConfig(eff *plugin.IgnoreConfig, call string, columns []string, action string, isGet bool) *plugin.IgnoreConfig {
	legacy, fn := eff.ShouldIgnoreError, eff.ShouldIgnoreErrorFunc
	return &plugin.IgnoreConfig{
		ShouldIgnoreErrorFunc: func(ctx context.Context, d *plugin.QueryData, h *plugin.HydrateData, err error) bool {
			ignore := (legacy != nil && legacy(err)) || (fn != nil && fn(ctx, d, h, err))
			if ignore {
				cols := columns
				if isGet && (h == nil || h.Item == nil) {
					cols = nil
				}
				report(ctx, ignoredError(d, call, cols, action, err))
			}
			return ignore
		},
	}
}

// report calls the hook, if any. A panic in the hook is dropped: inside the
// SDK's WrapHydrate it would turn the ignored error into a failed scan.
func report(ctx context.Context, e IgnoredError) {
	hook := getIgnoredErrorHook()
	if hook == nil {
		return
	}
	defer func() { _ = recover() }()
	hook(ctx, e)
}

func ignoredError(d *plugin.QueryData, call string, columns []string, action string, err error) IgnoredError {
	e := IgnoredError{Call: call, Columns: slices.Clone(columns), Action: action}
	if d != nil {
		if d.Connection != nil {
			e.Connection = d.Connection.Name
		}
		if d.Table != nil {
			e.Table = d.Table.Name
		}
		e.Region = d.EqualsQualString(matrixKeyRegion)
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		e.Code = ae.ErrorCode()
	}
	return e
}

// tagAction is the IAM action that a call's rate-limiter tags name, for example
// {"service": "kms", "action": "DescribeKey"} → "kms:DescribeKey".
func tagAction(tags map[string]string) string {
	if tags["service"] == "" || tags["action"] == "" {
		return ""
	}
	return tags["service"] + ":" + tags["action"]
}

// shortFuncName drops the package path: ".../aws.getAwsKmsKeyData" →
// "getAwsKmsKeyData".
func shortFuncName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}
