package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
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

// Each DescribeInstanceAttribute detail call of aws_ec2_instance ignores
// InvalidInstanceID.NotFound (an instance that ended during the scan), and only
// that and the connection's ignore codes.
func TestEc2InstanceAttributeCallsIgnoreGoneInstance(t *testing.T) {
	ctx := context.Background()
	table := tableAwsEc2Instance(ctx)
	d := &plugin.QueryData{Connection: &plugin.Connection{Name: "conn_1"}}
	h := &plugin.HydrateData{}
	found := 0
	for _, c := range table.HydrateConfig {
		if c.Tags["action"] != "DescribeInstanceAttribute" {
			continue
		}
		found++
		name := shortFuncName(helpers.GetFunctionName(c.Func))
		if c.IgnoreConfig == nil || c.IgnoreConfig.ShouldIgnoreErrorFunc == nil {
			t.Fatalf("%s: no ignore config", name)
		}
		ignore := c.IgnoreConfig.ShouldIgnoreErrorFunc
		if !ignore(ctx, d, h, apiErr("InvalidInstanceID.NotFound")) {
			t.Errorf("%s: InvalidInstanceID.NotFound is not ignored", name)
		}
		for _, code := range []string{"InvalidParameterValue", "RequestLimitExceeded", "InternalError"} {
			if ignore(ctx, d, h, apiErr(code)) {
				t.Errorf("%s: %s is ignored", name, code)
			}
		}
	}
	if found != 6 {
		t.Fatalf("found %d DescribeInstanceAttribute detail calls, want 6", found)
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
