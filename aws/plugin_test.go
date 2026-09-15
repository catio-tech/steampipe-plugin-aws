package aws

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/turbot/steampipe-plugin-sdk/v6/rate_limiter"
)

// catchAllLimiterName is the Bug #863 prevention limiter: the last entry in
// RateLimiters, with an empty Where so that it matches every call that no
// earlier, more specific definition already covers.
const catchAllLimiterName = "aws_default_hydrate_ceiling"

// expectedRateLimiterCount pins the size of the RateLimiters slice so a silent
// drop of a definition fails the build: 20 per-service limiters inherited from
// upstream plus the Bug #863 catch-all.
const expectedRateLimiterCount = 21

var (
	rateLimitersOnce sync.Once
	rateLimiters     []*rate_limiter.Definition
)

// pluginRateLimiters builds the plugin once (table construction needs no AWS
// credentials) and returns its rate limiter definitions.
func pluginRateLimiters(t *testing.T) []*rate_limiter.Definition {
	t.Helper()
	rateLimitersOnce.Do(func() {
		rateLimiters = Plugin(context.Background()).RateLimiters
	})
	require.NotEmpty(t, rateLimiters, "plugin must declare rate limiters")
	return rateLimiters
}

// TestRateLimiterDefinitionsAreValid checks every definition parses and
// validates. Initialise() compiles a non-empty Where into a scope filter and
// returns an error if it is unparseable; it is a no-op for an empty Where.
func TestRateLimiterDefinitionsAreValid(t *testing.T) {
	for _, def := range pluginRateLimiters(t) {
		t.Run(def.Name, func(t *testing.T) {
			assert.NotEmpty(t, def.Name, "rate limiter must have a name")
			assert.Greater(t, float64(def.FillRate), float64(0), "rate limiter %q must have a positive fill rate", def.Name)
			require.NoError(t, def.Initialise(), "rate limiter %q has an unparseable Where: %q", def.Name, def.Where)
			assert.Empty(t, def.Validate(), "rate limiter %q failed validation", def.Name)
		})
	}
}

// TestRateLimiterCount pins the total definition count.
func TestRateLimiterCount(t *testing.T) {
	assert.Len(t, pluginRateLimiters(t), expectedRateLimiterCount,
		"rate limiter count changed - update expectedRateLimiterCount deliberately, never to make this pass")
}

// TestCatchAllRateLimiterIsLast asserts the Bug #863 catch-all exists exactly
// once, sits last, and carries the sizing from the design doc (item 14).
func TestCatchAllRateLimiterIsLast(t *testing.T) {
	defs := pluginRateLimiters(t)

	var matches []int
	for i, def := range defs {
		if def.Name == catchAllLimiterName {
			matches = append(matches, i)
		}
	}
	require.Len(t, matches, 1, "expected exactly one %q definition", catchAllLimiterName)
	assert.Equal(t, len(defs)-1, matches[0], "%q must be the LAST definition so every specific limiter takes precedence", catchAllLimiterName)

	catchAll := defs[matches[0]]
	assert.Equal(t, "", catchAll.Where, "catch-all must have an empty Where so it matches every call")
	assert.Equal(t, float64(200), float64(catchAll.FillRate))
	assert.Equal(t, int64(200), catchAll.BucketSize)
	assert.Equal(t, int64(200), catchAll.MaxConcurrency)
	assert.Equal(t, []string{"connection", "region", "service"}, catchAll.Scope)
}

// TestCatchAllRateLimiterMatchesEverything asserts the SDK mechanism the fix
// relies on: an empty Where leaves parsedFilter nil, and SatisfiesFilters then
// returns true for any scope values.
func TestCatchAllRateLimiterMatchesEverything(t *testing.T) {
	defs := pluginRateLimiters(t)
	catchAll := defs[len(defs)-1]
	require.Equal(t, catchAllLimiterName, catchAll.Name)
	require.NoError(t, catchAll.Initialise())

	scopes := []map[string]string{
		{"connection": "aws", "region": "us-east-1", "service": "sqs", "action": "GetQueueAttributes"},
		{"connection": "aws", "region": "us-west-2", "service": "athena", "action": "GetQueryExecution"},
		{"connection": "other", "region": "eu-west-1", "service": "ecs", "action": "DescribeServices"},
		{},
	}
	for _, scope := range scopes {
		assert.True(t, catchAll.SatisfiesFilters(scope), "catch-all must match scope values %v", scope)
	}
}

// TestExistingRateLimitersUnchanged pins a sample of the inherited per-service
// limiters, which the Bug #863 change must leave untouched.
func TestExistingRateLimitersUnchanged(t *testing.T) {
	byName := map[string]*rate_limiter.Definition{}
	for _, def := range pluginRateLimiters(t) {
		byName[def.Name] = def
	}

	tests := []struct {
		name     string
		fillRate float64
		where    string
	}{
		{
			name:     "aws_lambda_get_function",
			fillRate: 100,
			where:    "service = 'lambda' and action = 'GetFunction'",
		},
		{
			name:     "aws_iam_policy_get_policy",
			fillRate: 35,
			where:    "service = 'iam' and action = 'GetPolicy'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := byName[tc.name]
			require.True(t, ok, "expected rate limiter %q to still be defined", tc.name)
			assert.Equal(t, tc.fillRate, float64(def.FillRate))
			assert.Equal(t, tc.where, def.Where)
		})
	}
}

// TestOnlyCatchAllMatchesEverything guards against a second match-everything
// limiter being added later, which would fight the catch-all for the same calls.
func TestOnlyCatchAllMatchesEverything(t *testing.T) {
	for _, def := range pluginRateLimiters(t) {
		if def.Name == catchAllLimiterName {
			continue
		}
		assert.NotEmpty(t, def.Where, "rate limiter %q has an empty Where - only %q may match every call", def.Name, catchAllLimiterName)
	}
}
