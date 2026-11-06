package aws

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	transfertypes "github.com/aws/aws-sdk-go-v2/service/transfer/types"
	"github.com/hashicorp/go-hclog"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin/context_key"
)

// isolatedContext keeps the test from reaching AWS: no shared config or
// credentials, no instance metadata, and a deadline. A call that the code
// under test should have skipped then fails fast with an error, never with a
// real request or a hang.
func isolatedContext(t *testing.T) context.Context {
	t.Helper()
	none := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", none)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", none)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Setenv(k, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return context.WithValue(ctx, context_key.Logger, hclog.NewNullLogger())
}

// A deleted stack's detail calls are skipped: no client, no call, no row
// data. The calls of a stack that is not deleted are not skipped.
func TestCloudFormationDeletedStackSkipsDetailCalls(t *testing.T) {
	ctx := isolatedContext(t)
	deleted := &plugin.HydrateData{Item: cfntypes.StackSummary{StackName: aws.String("gone"), StackStatus: cfntypes.StackStatusDeleteComplete}}
	// No connection: a call would need a client, which this QueryData cannot
	// give, so a nil, nil answer means that the call was skipped.
	d := &plugin.QueryData{}
	for name, call := range map[string]plugin.HydrateFunc{
		"getCloudFormationStack": getCloudFormationStack,
		"getStackTemplate":       getStackTemplate,
		"describeStackResources": describeStackResources,
	} {
		got, err := call(ctx, d, deleted)
		if err != nil || got != nil {
			t.Errorf("%s on a deleted stack: %v, %v; want nil, nil", name, got, err)
		}
	}
	for _, c := range []struct {
		item any
		want bool
	}{
		{cfntypes.StackSummary{StackStatus: cfntypes.StackStatusDeleteComplete}, true},
		{cfntypes.StackSummary{StackStatus: cfntypes.StackStatusDeleteInProgress}, false},
		{cfntypes.StackSummary{StackStatus: cfntypes.StackStatusCreateComplete}, false},
		{cfntypes.Stack{StackStatus: cfntypes.StackStatusDeleteComplete}, false},
		{nil, false},
	} {
		if got := deletedStackSummary(c.item); got != c.want {
			t.Errorf("deletedStackSummary(%#v) = %v, want %v", c.item, got, c.want)
		}
	}
}

// Users are listed only on a server whose identity provider is SERVICE_MANAGED
// (or not given).
func TestTransferUsersOnlyOnServiceManagedServers(t *testing.T) {
	ctx := isolatedContext(t)
	for _, c := range []struct {
		idp  transfertypes.IdentityProviderType
		want bool
	}{
		{transfertypes.IdentityProviderTypeServiceManaged, true},
		{"", true},
		{transfertypes.IdentityProviderTypeAwsDirectoryService, false},
		{transfertypes.IdentityProviderTypeApiGateway, false},
		{transfertypes.IdentityProviderTypeAwsLambda, false},
	} {
		server := transfertypes.ListedServer{ServerId: aws.String("s-1"), IdentityProviderType: c.idp}
		if got := serverHasUsers(server); got != c.want {
			t.Errorf("serverHasUsers(%q) = %v, want %v", c.idp, got, c.want)
		}
		if c.want {
			continue
		}
		// No connection: a ListUsers call would need a client.
		got, err := listTransferUsers(ctx, &plugin.QueryData{}, &plugin.HydrateData{Item: server})
		if err != nil || got != nil {
			t.Errorf("listTransferUsers on a %s server: %v, %v; want nil, nil", c.idp, got, err)
		}
	}
}
