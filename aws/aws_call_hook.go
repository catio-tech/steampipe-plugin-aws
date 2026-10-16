package aws

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// AWSCall is one AWS API call of the plugin, after its last attempt. A host
// that runs the plugin in its own process (the catiopipe-worker) gets one per
// call through SetAWSCallHook. It holds no error message: AWS error messages
// name ARNs, users and emails.
type AWSCall struct {
	// Connection is the connection name.
	Connection string
	// Service is the SDK's service id, for example "EC2" or "Elastic Load
	// Balancing v2".
	Service string
	// Operation is the API operation, for example "DescribeInstances".
	Operation string
	// Region is the region of the endpoint that the call used.
	Region string
	// Outcome is one of AWSCallOK, AWSCallThrottled, AWSCallError,
	// AWSCallCanceled and AWSCallNoResponse.
	Outcome string
	// Code is the AWS error code of the last attempt, or "" when AWS gave no
	// error code.
	Code string
	// Attempts is the number of attempts that the retryer made (1 when the
	// first attempt was the last). It is 0 when the call failed before its
	// first attempt.
	Attempts int
	// Throttles is the number of attempts that AWS throttled (the SDK's
	// throttle error codes).
	Throttles int
	// LimiterWait is the time that the attempts waited for the adaptive
	// limiter (SetAdaptiveLimiter). It is 0 when the limiter is off.
	LimiterWait time.Duration
}

// The outcomes of an AWS call (AWSCall.Outcome).
const (
	// AWSCallOK: the call succeeded.
	AWSCallOK = "ok"
	// AWSCallThrottled: AWS throttled the last attempt (the retryer stopped).
	AWSCallThrottled = "throttled"
	// AWSCallError: AWS answered with another error. Code has its code.
	AWSCallError = "error"
	// AWSCallCanceled: the caller's context ended.
	AWSCallCanceled = "canceled"
	// AWSCallNoResponse: no answer from AWS: a network or DNS error, a
	// timeout, a request that could not be signed or sent, or a panic below
	// the call middleware.
	AWSCallNoResponse = "no_response"
)

// AttemptEvent is a step of one attempt of an AWS call (AWSCallHook.Attempt).
type AttemptEvent int

const (
	// LimiterWaitStarted: the attempt starts to wait for the adaptive limiter.
	// Only when the limiter is on. LimiterWaitEnded always follows.
	LimiterWaitStarted AttemptEvent = iota + 1
	// LimiterWaitEnded: the wait has ended, with a token or with the end of
	// the call's context.
	LimiterWaitEnded
	// AttemptSent: the attempt goes on to be signed and sent. AttemptEnded
	// always follows, also after a panic.
	AttemptSent
	// AttemptEnded: the attempt has its answer or its error.
	AttemptEnded
)

// AWSCallHook receives the AWS calls of the plugin. The plugin calls it from
// many table scans at once, and waits for it: it must be fast and safe for
// concurrent use. ctx is the call's context, so a host can find its own values
// there (the plugin passes a scan's context down to its AWS calls when the
// query cache is off). A panic in the hook is recovered and dropped, so it
// cannot fail the call.
type AWSCallHook interface {
	// Attempt is called at each step of each attempt. A host can use it to see
	// that a scan still makes progress: an attempt that waits for the adaptive
	// limiter is waiting, not stuck.
	Attempt(ctx context.Context, e AttemptEvent)
	// CallEnded is called once per call, after its last attempt (also after
	// a panic below the call middleware, as AWSCallNoResponse).
	CallEnded(ctx context.Context, c AWSCall)
}

var (
	awsCallHook atomic.Pointer[AWSCallHook]
	// adaptiveLimiterOn: SetAdaptiveLimiter(true).
	adaptiveLimiterOn atomic.Bool
	// adaptiveLimiters holds one limiter per connection, service and region:
	// limiterKey → *retry.AdaptiveMode.
	adaptiveLimiters sync.Map
)

// SetAWSCallHook sets the hook, process-wide. Set it before the plugin serves
// its first query: the plugin builds the client config of a connection and
// region once, and adds the call middleware only when the hook or the adaptive
// limiter is on at that time. With neither (the Steampipe path), the clients
// are exactly as before. nil removes the hook (calls then report nothing).
func SetAWSCallHook(h AWSCallHook) {
	if h == nil {
		awsCallHook.Store(nil)
		return
	}
	awsCallHook.Store(&h)
}

func getAWSCallHook() AWSCallHook {
	if h := awsCallHook.Load(); h != nil {
		return *h
	}
	return nil
}

// SetAdaptiveLimiter turns the adaptive limiter on or off, process-wide. Turn
// it on before the plugin serves its first query (see SetAWSCallHook).
//
// The limiter is the AWS SDK's adaptive retry rate limiter (retry.AdaptiveMode,
// as in the SDKs' "adaptive" retry mode), one per connection, service and
// region. It does not limit until AWS throttles an attempt. Then it lowers the
// rate of attempts of that service and region (to 70% of the measured rate, at
// least 0.5 per second), and raises it again, on a cubic curve, while
// attempts succeed. It works on top of the plugin's rate limiters
// (SetRateLimiters), which limit the hydrate calls before they make AWS calls,
// and on top of the retryer, which waits before it repeats an attempt.
//
// Each call changes the setting and drops the state of every limiter.
func SetAdaptiveLimiter(on bool) {
	adaptiveLimiterOn.Store(on)
	adaptiveLimiters.Clear()
}

type limiterKey struct{ connection, service, region string }

// adaptiveLimiter returns the limiter of a connection, service and region, or
// nil when the limiter is off.
func adaptiveLimiter(connection, service, region string) *retry.AdaptiveMode {
	if !adaptiveLimiterOn.Load() {
		return nil
	}
	k := limiterKey{connection, service, region}
	if l, ok := adaptiveLimiters.Load(k); ok {
		return l.(*retry.AdaptiveMode)
	}
	l, _ := adaptiveLimiters.LoadOrStore(k, retry.NewAdaptiveMode())
	return l.(*retry.AdaptiveMode)
}

// awsThrottles is the SDK's throttle check, as the adaptive limiter uses it.
var awsThrottles = retry.IsErrorThrottles(retry.DefaultThrottles)

// withAWSCallMiddleware adds the call and attempt middleware to the API options
// of a connection's client in a region, when the hook or the adaptive limiter is
// on. Otherwise it returns opts unchanged.
func withAWSCallMiddleware(opts []func(*middleware.Stack) error, connection, region string) []func(*middleware.Stack) error {
	if getAWSCallHook() == nil && !adaptiveLimiterOn.Load() {
		return opts
	}
	add := func(stack *middleware.Stack) error {
		// Last in Initialize: after the service metadata (service, operation
		// and region) and the input validation, so that a call that fails
		// validation, and makes no request, is not counted.
		if err := stack.Initialize.Add(&awsCallMiddleware{connection: connection, region: region}, middleware.After); err != nil {
			return err
		}
		// Right after Retry: once per attempt. The credentials hook's option
		// (retryBeforeAuth) runs before this one, so Retry is already where it
		// stays.
		m := &awsAttemptMiddleware{connection: connection, region: region}
		if _, ok := stack.Finalize.Get("Retry"); ok {
			return stack.Finalize.Insert(m, "Retry", middleware.After)
		}
		return stack.Finalize.Add(m, middleware.Before)
	}
	return append(slices.Clone(opts), add)
}

// callState collects the attempts of one call. The attempts of a call run one
// after the other, on the call's goroutine.
type callState struct {
	attempts, throttles int
	waited              time.Duration
}

type callStateKey struct{}

// awsCallMiddleware reports each call when it ends.
type awsCallMiddleware struct{ connection, region string }

func (*awsCallMiddleware) ID() string { return "CatioAWSCall" }

func (m *awsCallMiddleware) HandleInitialize(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
	out middleware.InitializeOutput, metadata middleware.Metadata, err error,
) {
	st := &callState{}
	// A panic below (a middleware, the transport) still reports the call, as
	// no_response. The deferred function does not recover, so the panic goes
	// on with its own stack.
	ended := false
	defer func() {
		if !ended {
			m.report(ctx, st, AWSCallNoResponse, "")
		}
	}()
	out, metadata, err = next.HandleInitialize(context.WithValue(ctx, callStateKey{}, st), in)
	ended = true
	outcome, code := callOutcome(ctx, err)
	m.report(ctx, st, outcome, code)
	return out, metadata, err
}

func (m *awsCallMiddleware) report(ctx context.Context, st *callState, outcome, code string) {
	h := getAWSCallHook()
	if h == nil {
		return
	}
	call := AWSCall{
		Connection:  m.connection,
		Service:     awsmiddleware.GetServiceID(ctx),
		Operation:   awsmiddleware.GetOperationName(ctx),
		Region:      callRegion(ctx, m.region),
		Outcome:     outcome,
		Code:        code,
		Attempts:    st.attempts,
		Throttles:   st.throttles,
		LimiterWait: st.waited,
	}
	safeHook(func() { h.CallEnded(ctx, call) })
}

// awsAttemptMiddleware runs once per attempt: it waits for the adaptive
// limiter, counts the attempt and its throttle, and reports its steps.
type awsAttemptMiddleware struct{ connection, region string }

func (*awsAttemptMiddleware) ID() string { return "CatioAWSAttempt" }

func (m *awsAttemptMiddleware) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
	out middleware.FinalizeOutput, metadata middleware.Metadata, err error,
) {
	st, _ := ctx.Value(callStateKey{}).(*callState)
	if st == nil {
		// The call middleware is always there; this keeps a stack that lost it
		// working.
		st = &callState{}
	}
	h := getAWSCallHook()
	var release func(error) error
	if l := adaptiveLimiter(m.connection, awsmiddleware.GetServiceID(ctx), callRegion(ctx, m.region)); l != nil {
		r, werr := m.wait(ctx, h, st, l)
		if werr != nil {
			// Only the end of the call's context stops the wait. A canceled
			// request is not retried.
			return out, metadata, &aws.RequestCanceledError{Err: werr}
		}
		release = r
	}
	st.attempts++
	attemptEvent(h, ctx, AttemptSent)
	// Deferred, so that AttemptEnded also follows a panic below (with no
	// recover: the panic goes on with its own stack).
	defer attemptEvent(h, ctx, AttemptEnded)
	out, metadata, err = next.HandleFinalize(ctx, in)
	if release != nil {
		_ = release(err)
	}
	if err != nil && awsThrottles.IsErrorThrottle(err).Bool() {
		st.throttles++
	}
	return out, metadata, err
}

// wait waits for the adaptive limiter's attempt token. LimiterWaitEnded always
// follows LimiterWaitStarted.
func (m *awsAttemptMiddleware) wait(ctx context.Context, h AWSCallHook, st *callState, l *retry.AdaptiveMode) (func(error) error, error) {
	attemptEvent(h, ctx, LimiterWaitStarted)
	defer attemptEvent(h, ctx, LimiterWaitEnded)
	start := time.Now()
	defer func() { st.waited += time.Since(start) }()
	return l.GetAttemptToken(ctx)
}

// callRegion is the region in the call's metadata (an operation option can
// change it), or else the client's.
func callRegion(ctx context.Context, client string) string {
	if r := awsmiddleware.GetRegion(ctx); r != "" {
		return r
	}
	return client
}

// callOutcome classifies the error of a call. It keeps only the AWS error
// code, never the message.
func callOutcome(ctx context.Context, err error) (outcome, code string) {
	if err == nil {
		return AWSCallOK, ""
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if awsThrottles.IsErrorThrottle(err).Bool() {
			return AWSCallThrottled, apiErr.ErrorCode()
		}
		return AWSCallError, apiErr.ErrorCode()
	}
	var canceled *aws.RequestCanceledError
	if ctx.Err() != nil || errors.As(err, &canceled) || errors.Is(err, context.Canceled) {
		return AWSCallCanceled, ""
	}
	return AWSCallNoResponse, ""
}

func attemptEvent(h AWSCallHook, ctx context.Context, e AttemptEvent) {
	if h != nil {
		safeHook(func() { h.Attempt(ctx, e) })
	}
}

// safeHook runs a hook call, and drops a panic.
func safeHook(f func()) {
	defer func() { _ = recover() }()
	f()
}
