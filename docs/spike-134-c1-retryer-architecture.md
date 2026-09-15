# Spec #134 Track C1, Spike A: AWS Plugin Retryer Architecture

## Answer

**Per-connection retryer. Adaptive mode is viable through the plugin.**

Each Steampipe connection gets its own AWS SDK retryer, built from per-connection
config. There is no shared or static retryer reused across connections.

## Code Evidence

### 1. Per-connection config drives retry parameters

`aws/connection_config.go:17-18`:
```go
MaxErrorRetryAttempts *int `hcl:"max_error_retry_attempts"`
MinErrorRetryDelay    *int `hcl:"min_error_retry_delay"`
```

Each Steampipe connection (aws.spc) has its own `awsConfig` instance with these
fields. `GetConfig(d.Connection)` returns the config for the current connection.

### 2. Retryer is built per connection-region, from per-connection config

`aws/service.go:1951-1998` (`getClientUncached`):
- Reads `awsSpcConfig := GetConfig(d.Connection)` (line 1959)
- Extracts `maxRetries` from `awsSpcConfig.MaxErrorRetryAttempts` or `AWS_MAX_ATTEMPTS` env var, default 9
- Extracts `minRetryDelay` from `awsSpcConfig.MinErrorRetryDelay`, default 25ms
- Calls `getClientWithMaxRetries(ctx, d, region, maxRetries, minRetryDelay)`

`aws/service.go:2000-2040` (`getClientWithMaxRetries`):
```go
// line 2010: get per-connection base config
baseCfg, err := getBaseClientForAccount(ctx, d)
cfg := baseCfg.Copy()

// line 2028: build a FRESH retryer with per-connection params
retryer := retry.NewStandard(func(o *retry.StandardOptions) {
    o.MaxAttempts = maxRetries
    o.MaxBackoff = 5 * time.Minute
    o.RateLimiter = NoOpRateLimit{}
    o.Backoff = NewExponentialJitterBackoff(minRetryDelay, maxRetries)
})
cfg.Retryer = func() aws.Retryer {
    return retry.AddWithErrorCodes(retryer, "UnknownError")
}
```

The retryer captures `maxRetries` and `minRetryDelay` from the closure. These
are per-connection values.

### 3. Base config is per-connection but does NOT include a retryer

`aws/service.go:2243`:
```go
var getBaseClientForAccountCached = plugin.HydrateFunc(getBaseClientForAccountUncached).Memoize(memoize.WithTtl(time.Hour * 24 * 30))
```

`aws/service.go:2248-2356` (`getBaseClientForAccountUncached`):
- Creates `aws.Config` via `config.LoadDefaultConfig(ctx, configOptions...)`
- Sets credentials, HTTP client, profile, logging
- Does NOT set `cfg.Retryer` (uses SDK default)
- Cached per-connection with 30-day TTL

The retryer is added later in `getClientWithMaxRetries`, after copying the base
config. So the base config's SDK-default retryer is always overwritten.

### 4. Client config is cached per connection-region

`aws/service.go:1936`:
```go
var getClientCached = plugin.HydrateFunc(getClientUncached).Memoize(memoize.WithCacheKeyFunction(getClientCacheKey))
```

Cache key: `getClient-<region>` (line 1944). The SDK's `Memoize()` is per-connection,
so the `*aws.Config` (with its retryer) is built once per connection-region pair
and cached for the connection's lifetime.

### 5. Special cases: hardcoded retry, still per-connection

Two callers bypass the connection config with hardcoded values:
- `aws/service.go:388` (CloudControl): `getClientWithMaxRetries(ctx, d, region, 4, 25*time.Millisecond)`
- `aws/service.go:700` (EC2LowRetry): `getClientWithMaxRetries(ctx, d, region, 4, 25*time.Millisecond)`

These still build a fresh retryer per connection-region. They just ignore the
connection config's retry settings.

### 6. HTTP client IS shared, retryer IS NOT

`aws/service.go:2321`:
```go
configOptions = append(configOptions, config.WithHTTPClient(sharedHTTPClient))
```

A single `sharedHTTPClient` (initialized via `initializeHTTPClient()`, line 2116)
is shared across ALL connections for DNS caching and connection pooling. But
the retryer is set independently on each `*aws.Config` and is not shared.

### 7. History: custom plugin-level retryer was removed

`CHANGELOG.md:1002`:
> Removed custom plugin level retryer which was unnecessary as the plugin already
> uses the AWS SDK retryer. (#1932)

This confirms the plugin uses the AWS SDK v2 retryer (`retry.NewStandard`),
not a custom wrapper.

## Retryer Architecture Summary

```
Connection (aws.spc)          per-connection awsConfig
    |                              max_error_retry_attempts
    |                              min_error_retry_delay
    v
getBaseClientForAccountCached   per-connection, 30-day TTL
    |  config.LoadDefaultConfig    NO retryer set (SDK default)
    |  sharedHTTPClient            shared across ALL connections
    v
getClientWithMaxRetries         per connection-region
    |  baseCfg.Copy()              copy base config
    |  retry.NewStandard(...)      FRESH retryer from per-connection params
    |  cfg.Retryer = func()...     set on copied config
    v
getClientCached (Memoize)       cached per connection-region
    |  key: "getClient-<region>"
    v
*aws.Config with retryer        one per connection-region, cached
```

## Implication for C3/C4: Governor Placement

**Adaptive mode is viable through the plugin. The governor can live in the plugin.**

Because each connection builds its own retryer from per-connection config,
per-account adaptive behavior can be expressed by tuning the retryer parameters
per connection.

### Static per-connection tuning (set at connection config time)

Directly viable. The `max_error_retry_attempts` and `min_error_retry_delay`
connection config fields already support per-connection static values. A
governor that sets these at connection provisioning time needs no code change
to the retryer path.

### Dynamic per-connection tuning (adjust at runtime based on throttle feedback)

Viable but requires cache invalidation. The `*aws.Config` with its retryer is
built once and cached via `Memoize` per connection-region. To adjust retry
parameters at runtime, the governor would need to:

1. Update the connection config (already supported: the plugin's
   `connectionConfigCredentialsProvider` at `aws/credentials_provider.go`
   re-reads connection config on every credential Retrieve, proving the
   SDK mutates `Connection.Config` in-place via `UpdateConnectionConfigs`).
2. Invalidate the `getClientCached` Memoize cache for that connection, so the
   next call rebuilds the `*aws.Config` with the new retryer.

Alternatively, a mutable retryer wrapper could be used: instead of baking
`maxRetries`/`minRetryDelay` into the closure at construction time, the
retryer could read from a mutable per-connection state object. This avoids
cache invalidation but requires modifying `ExponentialJitterBackoff` and the
`retry.NewStandard` options to read from a shared mutable source.

### Recommendation

The governor should live in the **plugin** (Track C3/C4), not in PoolManager.
The retryer is per-connection, so per-account adaptive behavior is expressible.
For dynamic adjustment, prefer the mutable-wrapper approach over cache
invalidation, because invalidating the Memoize cache would also force
re-creation of the entire `*aws.Config` (including credentials resolution,
which has side effects like IMDS/STS calls).

## Turn-budget estimate

Spike only (research, no code change). ~12 tool calls to trace the full path.
---

## Bug #863 addendum

Added while investigating Bug #863 (`query_absolute_ceiling` hydrate read-speed
collapse). Design doc:
`~/Workspace/design-docs/bug863-hydrate-collapse-design.md` (item 15).
This section is **documentation only** — the retryer is not changed.

### The retry-token bucket is disabled

`aws/service.go:2028-2040` builds the per-connection retryer:

```go
retryer := retry.NewStandard(func(o *retry.StandardOptions) {
    rand.New(rand.NewSource(time.Now().UnixNano()))
    o.MaxAttempts = maxRetries
    o.MaxBackoff = 5 * time.Minute
    o.RateLimiter = NoOpRateLimit{} // With no rate limiter
    o.Backoff = NewExponentialJitterBackoff(minRetryDelay, maxRetries)
})
```

`o.RateLimiter = NoOpRateLimit{}` (line 2033) replaces the AWS SDK's retry-token
bucket with a no-op. That bucket is the SDK mechanism that exists specifically to
cap in-flight retry demand: it charges a token for each retry attempt and refuses
the retry when the budget is exhausted, so a client that is being throttled
stops amplifying its own load. With the no-op in place there is no quota check at
all — each of the `MaxAttempts` attempts (9 in the deployed configuration) can be
issued unconditionally, and the `ExponentialJitterBackoff` ladder
(`minRetryDelay` 25 ms × 3^n, capped by `MaxBackoff`) is the **only** thing
spacing them out. Throttle absorption is therefore unbounded in demand and
bounded only in time.

### Consequence: throttles are invisible upstream

A call that is throttled and then succeeds on a later attempt returns **success**
to the caller. Nothing in the return value, the error path, or the plugin's
telemetry distinguishes it from a call that was never throttled — it is simply
slower by the accumulated backoff. Upstream sees a slow call, never an error.

This is why catiopipe's `throttle_events` counter stayed at 0 for the whole of
the Bug #863 collapse: read throughput fell to 2-4 rows/s while every single
hydrate call reported success. The absorption is what converted a hard,
countable failure mode into an uncountable slow one, and it is why the bug ran
all the way into the 1800 s absolute ceiling before anything fired.

### Why the retryer is deliberately not changed here

Restoring the SDK retry-token bucket, or lowering `MaxAttempts`, would convert
silent slowness into visible errors across every tenant and every table at once.
That is design risk **R4**, rated high-impact-if-attempted: the change is too
blunt to ship as part of a throughput fix, and it would surface errors for
callers who are currently fine.

The chosen fix bounds the *demand* instead of the *absorption*. The catch-all
`aws_default_hydrate_ceiling` limiter in `aws/plugin.go` (design item 14) gives
every table that has no specific limiter a ceiling of 200 calls/s per
connection-region-service, instead of the unbounded fan-out that
`MultiLimiter.Wait()` returns when no `Definition` matches. Once that limiter
holds, the retryer stops being exercised in the first place, and revisiting
`NoOpRateLimit` becomes a cheap, separately measurable question rather than a
fleet-wide behavioural change.

### References

- Bug #863 — `query_absolute_ceiling` hydrate read-speed collapse (sqs, athena).
- Bug #875 — `query_idle_timeout` on relationship tables; same mechanism,
  triggered earlier in the scan.
- Design doc `bug863-hydrate-collapse-design.md` §2 items 14/15, §6.2, §10.3
  (blast radius), §14 risks R3/R4.
