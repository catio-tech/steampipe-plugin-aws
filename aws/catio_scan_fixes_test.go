package aws

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/smithy-go"
	"github.com/hashicorp/go-hclog"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin/context_key"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin/transform"
)

// A deregistered (INACTIVE) task definition has DeregisteredAt, a *time.Time.
// Its column must take the time as it is: UnixToTimestamp wants a number, and
// its error failed the whole region's scan of the table.
func TestEcsTaskDefinitionDeregisteredAt(t *testing.T) {
	ctx := context.Background()
	col := tableColumn(t, tableAwsEcsTaskDefinition(ctx), "deregistered_at")
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   *time.Time
		want any
	}{
		{"deregistered", &at, at},
		{"active", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &ecstypes.TaskDefinition{DeregisteredAt: tc.in}}
			got, err := col.Transform.Execute(ctx, &transform.TransformData{HydrateItem: item, ColumnName: col.Name})
			if err != nil {
				t.Fatalf("transform failed: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			gotTime, ok := got.(*time.Time)
			if !ok || !gotTime.Equal(at) {
				t.Fatalf("got %#v, want %v", got, at)
			}
			// The SDK must take the value for a TIMESTAMP column.
			if _, err := plugin.NewQueryColumn(col, "").ToColumnValue(got); err != nil {
				t.Fatalf("the column value does not convert: %v", err)
			}
		})
	}
}

// Each detail call of aws_ec2_instance that names the instance ignores
// InvalidInstanceID.NotFound (an instance that ended during the scan), and
// otherwise only what the connection's ignore_error_codes and
// ignore_error_messages ignore.
func TestEc2InstanceDetailCallsIgnoreGoneInstance(t *testing.T) {
	// The plugin's default ignore check logs: the SDK puts a logger on each
	// call's context.
	ctx := context.WithValue(context.Background(), context_key.Logger, hclog.NewNullLogger())
	table := tableAwsEc2Instance(ctx)
	plain := &plugin.QueryData{Connection: &plugin.Connection{Name: "conn_1"}}
	configured := &plugin.Connection{Name: "conn_2"}
	configured.SetConfig(awsConfig{IgnoreErrorCodes: []string{"AccessDenied"}, IgnoreErrorMessages: []string{"made up$"}})
	withConfig := &plugin.QueryData{Connection: configured}
	h := &plugin.HydrateData{}
	calls := map[string]int{"DescribeInstanceAttribute": 0, "GetLaunchTemplateData": 0, "DescribeInstanceStatus": 0}
	for _, c := range table.HydrateConfig {
		action := c.Tags["action"]
		if _, ok := calls[action]; !ok {
			continue
		}
		calls[action]++
		name := shortFuncName(helpers.GetFunctionName(c.Func))
		if c.IgnoreConfig == nil || c.IgnoreConfig.ShouldIgnoreErrorFunc == nil {
			t.Fatalf("%s: no ignore config", name)
		}
		ignore := c.IgnoreConfig.ShouldIgnoreErrorFunc
		if !ignore(ctx, plain, h, apiErr("InvalidInstanceID.NotFound")) {
			t.Errorf("%s: InvalidInstanceID.NotFound is not ignored", name)
		}
		for _, code := range []string{"InvalidParameterValue", "RequestLimitExceeded", "InternalError", "AccessDenied"} {
			if ignore(ctx, plain, h, apiErr(code)) {
				t.Errorf("%s: %s is ignored with no connection config", name, code)
			}
		}
		// The connection's codes and messages (apiErr's message is "made up").
		if !ignore(ctx, withConfig, h, apiErr("AccessDenied")) {
			t.Errorf("%s: the connection's ignore_error_codes are not applied", name)
		}
		if !ignore(ctx, withConfig, h, &smithy.GenericAPIError{Code: "InternalError", Message: "made up"}) {
			t.Errorf("%s: the connection's ignore_error_messages are not applied", name)
		}
	}
	if want := map[string]int{"DescribeInstanceAttribute": 6, "GetLaunchTemplateData": 1, "DescribeInstanceStatus": 1}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("detail calls %v, want %v", calls, want)
	}
}

func tableColumn(t *testing.T, table *plugin.Table, name string) *plugin.Column {
	t.Helper()
	for _, c := range table.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("table %s has no column %s", table.Name, name)
	return nil
}
