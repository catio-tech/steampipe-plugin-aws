package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin/context_key"
)

func apiErrMsg(code, msg string) error { return &smithy.GenericAPIError{Code: code, Message: msg} }

func TestPermissionErrorReadsAsAccessDenied(t *testing.T) {
	orig := apiErr("ForbiddenException")
	err := deniedAs(orig, "x:Y")
	var ae smithy.APIError
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, "AccessDeniedException", ae.ErrorCode(), "the connection's permission codes decide")
	assert.ErrorIs(t, err, orig, "the AWS error stays reachable")
}

func TestIgnoredErrorTakesThePermissionErrorsAction(t *testing.T) {
	d := &plugin.QueryData{}
	e := ignoredError(d, "call", nil, "tag:Action", deniedAs(apiErr("ValidationException"), "kms:Decrypt"))
	assert.Equal(t, "AccessDeniedException", e.Code)
	assert.Equal(t, "kms:Decrypt", e.Action)
	e = ignoredError(d, "call", nil, "tag:Action", deniedAs(apiErr("ForbiddenException"), ""))
	assert.Equal(t, "tag:Action", e.Action, "no action of its own: the call's tags")
	e = ignoredError(d, "call", nil, "tag:Action", apiErr("AccessDenied"))
	assert.Equal(t, "tag:Action", e.Action)
}

func TestServerlessrepoPolicyError(t *testing.T) {
	forbidden := apiErr("ForbiddenException")
	out, err, ok := serverlessrepoPolicyError(forbidden, false)
	assert.True(t, ok)
	assert.Nil(t, out)
	assert.NoError(t, err, "without the hook: no policy, as before")

	_, err, ok = serverlessrepoPolicyError(forbidden, true)
	assert.True(t, ok)
	var pe *permissionError
	assert.True(t, errors.As(err, &pe), "with the hook: a denial for the hook to report")

	_, _, ok = serverlessrepoPolicyError(apiErr("ThrottlingException"), true)
	assert.False(t, ok, "other errors are the caller's")
}

func TestScheduleGetError(t *testing.T) {
	kms := apiErrMsg("ValidationException", "The KMS key is not usable by the caller")
	other := apiErrMsg("ValidationException", "Name is too long")
	assert.Same(t, kms, scheduleGetError(kms, false), "without the hook: as before")
	assert.Same(t, other, scheduleGetError(other, true))
	var pe *permissionError
	require.True(t, errors.As(scheduleGetError(kms, true), &pe))
	assert.Equal(t, "kms:Decrypt", pe.action)
	assert.True(t, isScheduleKMSDenial(kms))
	assert.False(t, isScheduleKMSDenial(other))
}

// Through the real plugin built with the hook: the per-call check of each table
// ignores the mapped error (the connection ignores AccessDeniedException, as the
// worker's does) and reports the denied columns with the right action.
func TestMappedDenialsAreReported(t *testing.T) {
	rec := &recordHook{}
	setHookForTest(t, rec.hook)
	p := Plugin(context.Background())
	conn := &plugin.Connection{Name: "conn_1"}
	conn.SetConfig(awsConfig{Regions: []string{"us-east-1"}, IgnoreErrorCodes: []string{"NoSuchTagSet", "AccessDenied", "AccessDeniedException", "UnauthorizedOperation"}})
	ctx := context.WithValue(context.Background(), context_key.Logger, hclog.NewNullLogger())

	check := func(table string, hydrate plugin.HydrateFunc) *plugin.IgnoreConfig {
		t.Helper()
		tb := p.TableMap[table]
		require.NotNil(t, tb, table)
		if tb.Get != nil && helpers.GetFunctionName(tb.Get.Hydrate) == helpers.GetFunctionName(hydrate) {
			return tb.Get.IgnoreConfig
		}
		c := configFor(tb, hydrate)
		require.NotNil(t, c, "%s has a config for the call", table)
		return c
	}

	repo := p.TableMap["aws_serverlessapplicationrepository_application"]
	c := check(repo.Name, getServerlessApplicationRepositoryApplicationPolicy)
	assert.True(t, c.ShouldIgnoreErrorFunc(ctx, &plugin.QueryData{Table: repo, Connection: conn}, &plugin.HydrateData{Item: "app"},
		deniedAs(apiErr("ForbiddenException"), "")))

	sched := p.TableMap["aws_scheduler_schedule"]
	c = check(sched.Name, getAwsSchedulerSchedule)
	assert.True(t, c.ShouldIgnoreErrorFunc(ctx, &plugin.QueryData{Table: sched, Connection: conn}, &plugin.HydrateData{Item: "schedule"},
		scheduleGetError(apiErrMsg("ValidationException", "The KMS key is not usable"), true)))

	got := rec.got()
	require.Len(t, got, 2)
	assert.Equal(t, "getServerlessApplicationRepositoryApplicationPolicy", got[0].Call)
	assert.Equal(t, "AccessDeniedException", got[0].Code)
	assert.Equal(t, "serverlessrepo:GetApplicationPolicy", got[0].Action)
	assert.Contains(t, got[0].Columns, "statements")
	assert.Equal(t, "get", got[1].Call)
	assert.Equal(t, "AccessDeniedException", got[1].Code)
	assert.Equal(t, "kms:Decrypt", got[1].Action)
	assert.NotEmpty(t, got[1].Columns, "the get hydrates a listed schedule: its columns are denied")
}
