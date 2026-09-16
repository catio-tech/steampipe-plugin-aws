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
	assert.Zero(t, catchAll.MaxConcurrency,
		"the catch-all is a call-rate ceiling only - see TestCatchAllHasNoConcurrencyCap")
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

// TestCatchAllHasNoConcurrencyCap is the Bug #875 regression pin: the catch-all
// is a CALL-RATE ceiling and nothing else.
//
// MaxConcurrency is a different mechanism from FillRate/BucketSize, not a
// tighter setting of it. The SDK acquires the semaphore slot BEFORE the
// rate-limit wait and holds it through the call (plugin/hydrate_call.go:87,
// :96-124), the semaphore instance is shared process-wide per scope-value tuple
// (rate_limiter/hydrate_limiter.go:21-38), and a row that cannot get a slot
// spins in startAllHydrateCalls' uncancellable 10ms loop
// (plugin/row_data.go:77-122, no ctx.Done() check). Together those make a
// concurrency cap here a CROSS-QUERY head-of-line block on
// (connection, region, service): one query holding every slot starves every
// other query on that service+region to zero rows until its own deadline.
//
// That is Bug #875 - catiopipe query_idle_timeout hits 183 -> 429, 91% of them
// at rows_streamed=0, i.e. killed before their first row. The token bucket has
// no such mode: it delays calls, it does not hold a resource another query
// needs. So the bucket (the Bug #863 fix) stays at 200/200 and the semaphore
// stays gone.
func TestCatchAllHasNoConcurrencyCap(t *testing.T) {
	defs := pluginRateLimiters(t)
	catchAll := defs[len(defs)-1]
	require.Equal(t, catchAllLimiterName, catchAll.Name)

	assert.Zerof(t, catchAll.MaxConcurrency,
		"%q must NOT set MaxConcurrency. A concurrency semaphore on the catch-all applies to "+
			"~488 regional tables that have none of their own, is acquired before the rate-limit "+
			"wait and held through it, and is waited on in an uncancellable loop - that is Bug #875 "+
			"(idle timeouts 183 -> 429, 91%% before the first row). If you are adding one back, that "+
			"is a design change; do not just delete this assertion.", catchAllLimiterName)

	// The other half of the contract: removing the semaphore must not have
	// weakened the Bug #863 rate ceiling.
	assert.Equal(t, float64(200), float64(catchAll.FillRate),
		"the 200 calls/s token bucket IS the Bug #863 fix and must stay")
	assert.Equal(t, int64(200), catchAll.BucketSize,
		"the 200 calls/s token bucket IS the Bug #863 fix and must stay")
}

// TestCatchAllAppliesToListCalls pins that this limiter gates FETCH (List)
// calls, not only per-row hydrates.
//
// This is a correction of record. Bug #875's first analysis exonerated the
// catch-all on the grounds that "a limiter cannot delay a first LIST call",
// which is false: QueryData.resolveListRateLimiters builds the List limiter
// from d.Table.List.Tags (plugin/query_data_rate_limiters.go:131-140) and
// WaitForListRateLimit waits on it before the list call (:12-14). The scope
// values for that call are the List tags (service, action) plus the matrix
// item: setMatrixItem writes "region" into d.Quals as an equals qual
// (plugin/query_data.go:469-476) and populateRateLimitScopeValues lifts matrix
// quals into the scope-value map (query_data_rate_limiters.go:58-82), so every
// table with a GetMatrixItemFunc - all 488 regional ones - supplies "region"
// for its List call too.
//
// So a List call presents exactly the keys this Definition scopes on, and an
// empty Where matches it. The next reader should not repeat the misreading.
func TestCatchAllAppliesToListCalls(t *testing.T) {
	defs := pluginRateLimiters(t)
	catchAll := defs[len(defs)-1]
	require.Equal(t, catchAllLimiterName, catchAll.Name)
	require.NoError(t, catchAll.Initialise())

	// Shaped like the scope-value map the SDK builds for a List call: the
	// Table.List.Tags pair plus the region lifted from the matrix qual.
	listScopes := []map[string]string{
		{"connection": "aws", "region": "us-east-1", "service": "appsync", "action": "ListGraphqlApis"},
		{"connection": "aws", "region": "us-east-1", "service": "kafka", "action": "ListClusters"},
		{"connection": "aws", "region": "eu-west-1", "service": "ecr", "action": "DescribeRepositories"},
	}
	for _, scope := range listScopes {
		// Every Scope key must have a value, or the SDK drops the Definition
		// for this call - that is the half the original analysis got wrong.
		for _, key := range catchAll.Scope {
			require.Containsf(t, scope, key,
				"a List call must supply %q for %q to apply: service/action come from "+
					"Table.List.Tags and region from the matrix-item qual", key, catchAllLimiterName)
		}
		assert.Truef(t, catchAll.SatisfiesFilters(scope),
			"%q must gate List calls too, not only hydrates: scope %v", catchAllLimiterName, scope)
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

	// Concurrency is the other axis, and it is the one Bug #875 was about.
	// Dropping MaxConcurrency from the catch-all (TestCatchAllHasNoConcurrencyCap)
	// must leave the inherited semaphores exactly as upstream set them: those
	// are per-service caps chosen against documented AWS control-plane quotas,
	// and unlike a catch-all semaphore they are narrow enough that a stalled
	// holder cannot starve unrelated services.
	//
	// The map is the COMPLETE set of inherited limiters that set MaxConcurrency;
	// every other limiter must have none. Enumerated fail-closed so that both a
	// changed value and a newly added semaphore fail here rather than ship.
	expectedConcurrency := map[string]int64{
		"aws_cloudfront": 10,
		"aws_kinesis_list_streams_list_stream_consumers_list_tags_for_stream": 5,
		"aws_kinesis_describe_stream":                                         10,
		"aws_kinesis_describe_stream_summary":                                 20,
		"aws_route53":                                                         10,
		"aws_waf":                                                             10,
		"aws_wafv2":                                                           10,
	}
	seen := map[string]bool{}
	for _, def := range defs {
		if def.Name == catchAllLimiterName {
			continue
		}
		want, ok := expectedConcurrency[def.Name]
		if !ok {
			assert.Zerof(t, def.MaxConcurrency,
				"limiter %q has gained a MaxConcurrency of %d. A concurrency semaphore is not a "+
					"tighter rate limit - it is held across the rate-limit wait and waited on in the "+
					"SDK's uncancellable retry loop (Bug #875). Add it deliberately, and to this map.",
				def.Name, def.MaxConcurrency)
			continue
		}
		seen[def.Name] = true
		assert.Equalf(t, want, def.MaxConcurrency,
			"limiter %q must keep its own MaxConcurrency of %d - the Bug #875 removal applies to "+
				"%q only", def.Name, want, catchAllLimiterName)
	}
	for name := range expectedConcurrency {
		assert.Truef(t, seen[name], "expected limiter %q to still be defined", name)
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
//     get NO ceiling from it: the SDK skips a Definition unless every Scope key
//     has a value for the call, and those tables issue calls with no "region"
//     value. See the aws_default_hydrate_ceiling comment block in plugin.go for
//     the full per-service breakdown of what those 98 tables are and what does
//     still bound them - kept in one place so the numbers cannot drift apart.
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
