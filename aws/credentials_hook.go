package aws

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// CredentialsHook lets a host process that runs this plugin in-process (the
// catiopipe-worker) give the plugin the AWS credentials of a connection. With a
// hook, no keys go into the connection config, and the host renews them in
// memory. The Steampipe path sets no hook, so nothing changes there.
//
// Once a hook is set, a connection that the hook gives no provider fails: it
// never falls back to the process's own credentials (in the worker, the pod's
// role), which would read Catio's account in place of the customer's.
//
// The plugin calls the methods concurrently, from many table scans.
type CredentialsHook interface {
	// Provider returns the credentials provider of a connection, or nil when the
	// hook has none. The plugin asks when it first builds a client of the
	// connection (two first builds at the same time may both ask), keeps one provider for
	// the life of the process, and wraps it in one aws.CredentialsCache. The
	// cache calls Retrieve again only when the Expires of the last set has
	// passed (it has no expiry window), so Retrieve must return CanExpire=true,
	// and an Expires equal to the time when the host wants to give a different
	// set (for example, when the next set is old enough to use), not the STS
	// expiration.
	Provider(connection string) aws.CredentialsProvider
	// ExpiredToken is called when AWS refused a call of the connection because
	// its security token expired. It returns true when the call may be
	// repeated, after any wait that the host needs (for a set that is only
	// seconds old, for example), and false when it must not be. ctx is the
	// call's context. The plugin drops its cached set in both cases, so the next
	// attempt, and every later call, gets the set that Provider returns then.
	//
	// The call carries no attempt number and no sign of the set that it used:
	// the host's rule is per set, not per call. The retryer's attempt limit
	// (max_error_retry_attempts, 9 by default) bounds the repeats of one call,
	// and the plugin adds no backoff to them. Many calls can fail at once, so
	// the host must make one renewal out of many ExpiredToken calls.
	ExpiredToken(ctx context.Context, connection string) bool
}

var (
	credentialsHook atomic.Pointer[CredentialsHook]
	// hookCaches holds the credentials cache of each connection that the hook
	// gave a provider: connection name → *aws.CredentialsCache.
	hookCaches sync.Map
)

// SetCredentialsHook sets the hook, once per process, before the plugin serves
// any query of a connection that the hook covers: the plugin builds the base
// client of each connection once, and a base client built before the hook does
// not use it (its queries then fail). Setting the same hook again changes
// nothing. A host that changes the credentials of a connection over time (the
// fetchers, for example) gives one long-lived provider per connection name,
// whose Retrieve finds the current set. nil removes the hook.
func SetCredentialsHook(h CredentialsHook) {
	if cur := getCredentialsHook(); cur != nil && h != nil && sameHook(cur, h) {
		return
	}
	hookCaches.Clear()
	if h == nil {
		credentialsHook.Store(nil)
		return
	}
	credentialsHook.Store(&h)
}

// sameHook tells whether two hooks are the same pointer. Other hooks never
// count as the same: comparing them could panic (a struct with an interface
// field that holds a slice, for example).
func sameHook(a, b CredentialsHook) bool {
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	return ta == tb && ta.Kind() == reflect.Pointer && a == b
}

func getCredentialsHook() CredentialsHook {
	if h := credentialsHook.Load(); h != nil {
		return *h
	}
	return nil
}

// errNoHookCredentials: a hook is set, but it gives the connection no
// credentials.
var errNoHookCredentials = errors.New("the credentials hook gives this connection no credentials")

// hookCredentials returns the cached credentials of a connection from the hook.
// It returns nil and no error when no hook is set, and an error when a hook is
// set but has no provider for the connection.
func hookCredentials(connection string) (*aws.CredentialsCache, error) {
	h := getCredentialsHook()
	if h == nil {
		return nil, nil
	}
	if c, ok := hookCaches.Load(connection); ok {
		return c.(*aws.CredentialsCache), nil
	}
	p := h.Provider(connection)
	if p == nil {
		return nil, fmt.Errorf("connection %q: %w", connection, errNoHookCredentials)
	}
	cache, ok := p.(*aws.CredentialsCache)
	if !ok {
		cache = aws.NewCredentialsCache(p)
	}
	c, _ := hookCaches.LoadOrStore(connection, cache)
	return c.(*aws.CredentialsCache), nil
}

// expiredTokenCodes are the error codes of a call that AWS refused because its
// security token expired. Some services, for example Cost Explorer, also give
// them for a token that is only seconds old. EC2 gives RequestExpired for an
// expired token (aws-sdk-go v1 counts it as an expired-credentials code for
// EC2); other services give it for clock skew, which a repeat after the
// SDK's clock-skew correction may also fix.
var expiredTokenCodes = map[string]struct{}{
	"ExpiredToken":          {},
	"ExpiredTokenException": {},
	"RequestExpired":        {},
}

func isExpiredToken(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	_, ok := expiredTokenCodes[apiErr.ErrorCode()]
	return ok
}

// expiredTokenRetryer repeats a call that got an expired-token error when the
// hook allows it, with the set that the hook's provider gives then. The SDK's
// retry middleware calls GetRetryToken (with the call's context) and then
// RetryDelay for each failed attempt that IsErrorRetryable accepts. Every other
// error goes to the wrapped retryer.
type expiredTokenRetryer struct {
	aws.RetryerV2
	hook       CredentialsHook
	connection string
	cache      *aws.CredentialsCache
}

func (r *expiredTokenRetryer) IsErrorRetryable(err error) bool {
	if isExpiredToken(err) {
		return true
	}
	return r.RetryerV2.IsErrorRetryable(err)
}

// GetRetryToken asks the host whether an expired-token call may be repeated.
// The SDK stops the call when it returns an error.
func (r *expiredTokenRetryer) GetRetryToken(ctx context.Context, err error) (func(error) error, error) {
	if !isExpiredToken(err) {
		return r.RetryerV2.GetRetryToken(ctx, err)
	}
	repeat := r.hook.ExpiredToken(ctx, r.connection)
	r.cache.Invalidate()
	if !repeat {
		return nil, errors.New("the credentials hook does not repeat the call")
	}
	return func(error) error { return nil }, nil
}

// RetryDelay is 0 for an expired-token error: the hook has already waited.
func (r *expiredTokenRetryer) RetryDelay(attempt int, err error) (time.Duration, error) {
	if isExpiredToken(err) {
		return 0, nil
	}
	return r.RetryerV2.RetryDelay(attempt, err)
}

// hookClientOptions are the client options of a connection whose credentials
// come from the hook: the expired-token retryer, and the retry middleware
// before the auth scheme, so that each attempt gets the credentials again.
func hookClientOptions(retryer func() aws.Retryer, connection string, cache *aws.CredentialsCache) (func() aws.Retryer, func(*middleware.Stack) error) {
	h := getCredentialsHook()
	wrapped := func() aws.Retryer {
		r := retryer()
		v2, ok := r.(aws.RetryerV2)
		if !ok || h == nil {
			return r
		}
		return &expiredTokenRetryer{RetryerV2: v2, hook: h, connection: connection, cache: cache}
	}
	return wrapped, retryBeforeAuth
}

// retryBeforeAuth moves the retry middleware before ResolveAuthScheme, as the
// newer service clients have it. Older clients insert it before Signing, after
// GetIdentity, so every attempt of a call would be signed with the set that the
// first attempt got, and a repeat after ExpiredToken could not use a new set.
// It changes only a client whose Retry comes after ResolveAuthScheme: in the
// newer clients, InterceptAttempt and RetryMetricsHeader follow Retry inside the
// attempt loop, and must stay there.
func retryBeforeAuth(stack *middleware.Stack) error {
	retryAt, authAt := -1, -1
	for i, id := range stack.Finalize.List() {
		switch id {
		case "Retry":
			retryAt = i
		case "ResolveAuthScheme":
			authAt = i
		}
	}
	if retryAt < 0 || authAt < 0 || retryAt < authAt {
		return nil
	}
	m, err := stack.Finalize.Remove("Retry")
	if err != nil {
		return nil
	}
	return stack.Finalize.Insert(m, "ResolveAuthScheme", middleware.Before)
}
