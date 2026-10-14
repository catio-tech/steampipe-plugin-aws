package aws

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
)

// CredentialsHook lets a host process that runs this plugin in-process (the
// catiopipe-worker) give the plugin the AWS credentials of a connection. With a
// hook, no keys go into the connection config, and the host renews them in
// memory. The Steampipe path sets no hook, so nothing changes there.
type CredentialsHook interface {
	// Provider returns the credentials provider of a connection, or nil to use
	// the connection config as before. The plugin wraps it in one
	// aws.CredentialsCache per connection, so Retrieve runs again only when the
	// Expires of the last set has passed, or after ExpiredToken.
	Provider(connection string) aws.CredentialsProvider
	// ExpiredToken is called when AWS refused a call of the connection because
	// its security token expired. It returns true when the call may be
	// repeated, after any wait that the host needs (for a set that is only
	// seconds old, for example), and false when it must not be. ctx is the
	// call's context. Before the repeat, the plugin drops its cached set, so the
	// repeat uses the set that Provider returns then. The retryer's attempt
	// limit bounds the repeats; the host decides how many of them it allows.
	ExpiredToken(ctx context.Context, connection string) bool
}

var credentialsHook atomic.Pointer[CredentialsHook]

// SetCredentialsHook sets the hook. Call it before the plugin serves any query:
// the plugin builds the base client of each connection once. nil removes it.
func SetCredentialsHook(h CredentialsHook) {
	if h == nil {
		credentialsHook.Store(nil)
		return
	}
	credentialsHook.Store(&h)
}

func getCredentialsHook() CredentialsHook {
	if h := credentialsHook.Load(); h != nil {
		return *h
	}
	return nil
}

// hookCredentials returns the cached credentials of a connection from the hook,
// or nil when there is no hook or it has no provider for the connection.
func hookCredentials(connection string) *aws.CredentialsCache {
	h := getCredentialsHook()
	if h == nil {
		return nil
	}
	p := h.Provider(connection)
	if p == nil {
		return nil
	}
	if c, ok := p.(*aws.CredentialsCache); ok {
		return c
	}
	return aws.NewCredentialsCache(p)
}

// expiredTokenCodes are the error codes of a call that AWS refused because its
// security token expired. Some services, for example Cost Explorer, also give
// them for a token that is only seconds old.
var expiredTokenCodes = map[string]struct{}{
	"ExpiredToken":          {},
	"ExpiredTokenException": {},
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
	if !r.hook.ExpiredToken(ctx, r.connection) {
		return nil, errors.New("the credentials hook does not repeat the call")
	}
	r.cache.Invalidate()
	return func(error) error { return nil }, nil
}

// RetryDelay is 0 for an expired-token error: the hook has already waited.
func (r *expiredTokenRetryer) RetryDelay(attempt int, err error) (time.Duration, error) {
	if isExpiredToken(err) {
		return 0, nil
	}
	return r.RetryerV2.RetryDelay(attempt, err)
}

// withExpiredTokenRetry wraps a retryer when the connection gets its
// credentials from the hook.
func withExpiredTokenRetry(retryer aws.Retryer, connection string, creds aws.CredentialsProvider) aws.Retryer {
	h := getCredentialsHook()
	cache, ok := creds.(*aws.CredentialsCache)
	if h == nil || !ok || h.Provider(connection) == nil {
		return retryer
	}
	v2, ok := retryer.(aws.RetryerV2)
	if !ok {
		v2 = &retryerV1Adapter{retryer}
	}
	return &expiredTokenRetryer{RetryerV2: v2, hook: h, connection: connection, cache: cache}
}

// retryerV1Adapter gives a plain aws.Retryer the RetryerV2 method.
type retryerV1Adapter struct{ aws.Retryer }

func (a *retryerV1Adapter) GetAttemptToken(context.Context) (func(error) error, error) {
	return a.GetInitialToken(), nil
}
