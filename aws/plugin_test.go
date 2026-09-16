package aws

import (
	"context"
	"regexp"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/turbot/steampipe-plugin-sdk/v6/rate_limiter"
)

// catchAllLimiterName is the Bug #863 prevention limiter: the last entry in
// RateLimiters, with an empty Where so that it matches EVERY call - including
// calls that a more specific definition already covers. There is no precedence
// between definitions: matching is order-independent at runtime (the SDK
// iterates a map, plugin/plugin_rate_limiter.go:62) and limiters are additive
// (a call reserves on all matching definitions and waits for the longest
// delay). Its position in the slice is a convention, not a behaviour - see
// TestCatchAllRateLimiterIsLast.
const catchAllLimiterName = "aws_default_hydrate_ceiling"

// expectedRateLimiterCount pins the size of the RateLimiters slice so a silent
// drop of a definition fails the build: 20 per-service limiters inherited from
// upstream plus the Bug #863 catch-all.
const expectedRateLimiterCount = 21

var (
	rateLimitersOnce sync.Once
	rateLimiters     []*rate_limiter.Definition
)

// serviceWhereRe extracts the service name from a limiter Where clause, which
// are all of the form "service = '<name>' and ...". Hoisted out of the loop in
// TestCatchAllDoesNotBindExistingServices so it compiles once.
var serviceWhereRe = regexp.MustCompile(`service = '([^']+)'`)

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
	assert.Equal(t, len(defs)-1, matches[0],
		"%q must be the LAST definition - a readability/convention pin from design item 14; matching is "+
			"order-independent at runtime (the SDK iterates a map), so this guards the config's legibility, "+
			"not its behaviour", catchAllLimiterName)

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

// TestCatchAllDoesNotBindExistingServices pins the property that makes the
// Bug #863 catch-all safe to add.
//
// Matching limiters are NOT mutually exclusive: the SDK collects every
// Definition whose Scope values are present and whose Where is satisfied, and
// MultiLimiter.Wait() reserves on all of them and waits the longest delay. The
// catch-all therefore applies in addition to each specific limiter, and it
// buckets per connection-region-service - i.e. across ALL actions of a service.
//
// So for the specific limiters to stay the binding constraint (and their
// behaviour to stay unchanged), the catch-all's fill rate must exceed the sum
// of the per-action fill rates of every service that is already limited. If
// someone later adds per-action limiters whose total passes 200/s, the
// catch-all would silently become that service's bottleneck - this test fails
// first.
func TestCatchAllDoesNotBindExistingServices(t *testing.T) {
	defs := pluginRateLimiters(t)

	catchAll := defs[len(defs)-1]
	require.Equal(t, catchAllLimiterName, catchAll.Name)

	// service name -> summed fill rate of its specific limiters
	perService := map[string]float64{}
	for _, def := range defs {
		if def.Name == catchAllLimiterName {
			continue
		}
		m := serviceWhereRe.FindStringSubmatch(def.Where)
		require.Len(t, m, 2, "limiter %q has a Where this test cannot attribute to a service: %q", def.Name, def.Where)
		perService[m[1]] += float64(def.FillRate)
	}
	require.NotEmpty(t, perService, "expected to attribute some limiters to services")

	for service, total := range perService {
		assert.LessOrEqualf(t, total, float64(catchAll.FillRate),
			"service %q has specific limiters totalling %v calls/s, which exceeds the %v calls/s catch-all "+
				"(%q buckets per connection-region-service, across all actions). The catch-all would become "+
				"this service's bottleneck - raise it or re-scope it deliberately.",
			service, total, float64(catchAll.FillRate), catchAllLimiterName)
	}
}

// TestCatchAllScopeIncludesRegion pins "region" in the catch-all's Scope.
//
// This is the load-bearing half of the ceiling's shape, and it cuts both ways:
//
//   - Keeping "region" is what makes the ceiling 200 calls/s PER REGION. Dropping
//     it would leave a global-per-service 200 calls/s ceiling shared by all 488
//     regional tables across every region a connection scans - a real throughput
//     regression, not a tightening of the same bound.
//   - Keeping "region" is also why the 98 tables with no GetMatrixItemFunc
//     (every aws_iam_* table, plus cost_*/ce_*, cloudfront_*, globalaccelerator_*,
//     health_*, route53_*, s3_*, shield_* and waf_*, among others) get NO ceiling
//     from it: the SDK skips a Definition unless every Scope key has a value for
//     the call, and those tables issue calls with no "region" value. They stay
//     bounded only by whichever of their own limiters also omit "region" from
//     their Scope, and then only for the actions those limiters name - the 18 iam
//     tables by 170 calls/s across 7 of the 39 iam actions they call, 6
//     cloudfront_* and 7 of the 8 route53_* tables by 5 calls/s each - and for
//     the remaining 67 there is no limit at all.
//
// Widening coverage to those tables is a deliberate design decision, so any
// future edit to this Scope has to confront this test rather than slip past it.
func TestCatchAllScopeIncludesRegion(t *testing.T) {
	defs := pluginRateLimiters(t)
	catchAll := defs[len(defs)-1]
	require.Equal(t, catchAllLimiterName, catchAll.Name)

	assert.Containsf(t, catchAll.Scope, "region",
		"%q must keep \"region\" in its Scope: without it the ceiling becomes a single global "+
			"200 calls/s bucket per connection-service shared by all 488 regional tables. If you are "+
			"removing it to cover the 98 tables that have no region scope value, that is a design "+
			"change - make it deliberately, do not just delete this assertion.", catchAllLimiterName)

	// The SDK drops the whole Definition when a scope key has no value for the
	// call, so a call with no "region" (a no-GetMatrixItemFunc table) is not
	// rate limited by the catch-all at all. Pin that this is still the case.
	require.NoError(t, catchAll.Initialise())
	noRegion := map[string]string{"connection": "aws", "service": "iam", "action": "GetRole"}
	var missing []string
	for _, key := range catchAll.Scope {
		if _, ok := noRegion[key]; !ok {
			missing = append(missing, key)
		}
	}
	assert.Equalf(t, []string{"region"}, missing,
		"expected %q to be skipped for a call with no region scope value (it is the only missing "+
			"scope key); if this changed, the coverage gap documented on the Definition in plugin.go "+
			"is now wrong", catchAllLimiterName)
}
