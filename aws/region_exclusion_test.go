package aws

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
)

// The region list of a service is memoized and shared by every table of the
// connection, so an exclusion must not change it.
func TestRegionsWithoutKeepsTheInput(t *testing.T) {
	cached := []string{"us-east-1", "me-central-1", "eu-west-1", "us-west-2"}
	before := slices.Clone(cached)

	got := regionsWithout(cached, "me-central-1")

	if want := []string{"us-east-1", "eu-west-1", "us-west-2"}; !slices.Equal(got, want) {
		t.Errorf("regionsWithout = %v, want %v", got, want)
	}
	if !slices.Equal(cached, before) {
		t.Errorf("the cached list changed: %v, want %v", cached, before)
	}
	// A second table with no exclusion still sees every region.
	if got := regionsWithout(cached); !slices.Equal(got, before) {
		t.Errorf("no exclusion = %v, want %v", got, before)
	}
}

func TestIgnoreScheduleDetailErrors(t *testing.T) {
	ignore := ignoreScheduleDetailErrors()
	d := &plugin.QueryData{Connection: &plugin.Connection{Name: "test"}}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"KMS key", &smithy.GenericAPIError{Code: "ValidationException", Message: "Unable to access the KMS key"}, true},
		{"other validation error", &smithy.GenericAPIError{Code: "ValidationException", Message: "1 validation error detected"}, false},
		{"not found", &smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: "Schedule not found"}, true},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"}, false},
		{"not an API error", errors.New("dial tcp: i/o timeout"), false},
	} {
		if got := ignore(context.Background(), d, nil, tc.err); got != tc.want {
			t.Errorf("%s: ignored = %v, want %v", tc.name, got, tc.want)
		}
	}
}
