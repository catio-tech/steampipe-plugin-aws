package aws

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
)

// recordingHook records every report.
type recordingHook struct {
	mu     sync.Mutex
	calls  []AWSCall
	events []AttemptEvent
	// The value of ctxMark on the contexts of the reports.
	marks []string
	panic bool
	// onAttempt, when set, is called with each attempt event and the number
	// of events so far of that kind.
	onAttempt func(e AttemptEvent, n int)
	seen      map[AttemptEvent]int
}

type ctxMark struct{}

func (h *recordingHook) Attempt(ctx context.Context, e AttemptEvent) {
	h.mu.Lock()
	h.events = append(h.events, e)
	mark, _ := ctx.Value(ctxMark{}).(string)
	h.marks = append(h.marks, mark)
	if h.seen == nil {
		h.seen = map[AttemptEvent]int{}
	}
	h.seen[e]++
	n, on := h.seen[e], h.onAttempt
	h.mu.Unlock()
	if on != nil {
		on(e, n)
	}
	if h.panic {
		panic("made-up hook panic")
	}
}

func (h *recordingHook) CallEnded(ctx context.Context, c AWSCall) {
	h.mu.Lock()
	h.calls = append(h.calls, c)
	mark, _ := ctx.Value(ctxMark{}).(string)
	h.marks = append(h.marks, mark)
	h.mu.Unlock()
	if h.panic {
		panic("made-up hook panic")
	}
}

func (h *recordingHook) got() ([]AWSCall, []AttemptEvent, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls), slices.Clone(h.events), slices.Clone(h.marks)
}

// setCallHook sets a hook and the limiter for one test.
func setCallHook(t *testing.T, h AWSCallHook, limiter bool) {
	t.Helper()
	SetAWSCallHook(h)
	SetAdaptiveLimiter(limiter)
	t.Cleanup(func() {
		SetAWSCallHook(nil)
		SetAdaptiveLimiter(false)
	})
}

// scriptServer answers STS GetCallerIdentity with the script's answers in
// order, and then with its last one: "ok", "throttle" (Throttling) or "deny"
// (AccessDenied).
type scriptServer struct {
	mu     sync.Mutex
	script []string
	n      int
}

func (s *scriptServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	answer := s.script[min(s.n, len(s.script)-1)]
	s.n++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	switch answer {
	case "throttle", "deny":
		code := map[string]string{"throttle": "Throttling", "deny": "AccessDenied"}[answer]
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>%s</Code><Message>made-up message</Message></Error><RequestId>1</RequestId></ErrorResponse>`, code)
	default:
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDATEST</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>2</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	}
}

func (s *scriptServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// keyConnection is a connection with made-up keys in its config.
func keyConnection(name string) *plugin.Connection {
	ak, sk := "AKIACONFIG", "secret"
	conn := &plugin.Connection{Name: name}
	conn.SetConfig(awsConfig{AccessKey: &ak, SecretKey: &sk})
	return conn
}

// stsCallIn makes one GetCallerIdentity call through the plugin's client setup
// for the connection and region, against url.
func stsCallIn(ctx context.Context, t *testing.T, d *plugin.QueryData, region, url string) error {
	t.Helper()
	cfg, err := getClientWithMaxRetries(ctx, d, region, 4, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	client := sts.NewFromConfig(*cfg, func(o *sts.Options) { o.BaseEndpoint = aws.String(url) })
	_, err = client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	return err
}

func TestAWSCallHookCountsAttemptsAndThrottles(t *testing.T) {
	hookTestEnv(t)
	hook := &recordingHook{}
	setCallHook(t, hook, false)
	srv := httptest.NewServer(&scriptServer{script: []string{"throttle", "throttle", "ok"}})
	defer srv.Close()

	ctx, d := queryData(t, keyConnection("counted"))
	ctx = context.WithValue(ctx, ctxMark{}, "scan-1")
	if err := stsCallIn(ctx, t, d, "us-east-1", srv.URL); err != nil {
		t.Fatalf("GetCallerIdentity: %v", err)
	}
	calls, events, marks := hook.got()
	want := AWSCall{Connection: "counted", Service: "STS", Operation: "GetCallerIdentity", Region: "us-east-1", Outcome: AWSCallOK, Attempts: 3, Throttles: 2}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("calls = %+v, want [%+v]", calls, want)
	}
	wantEvents := []AttemptEvent{AttemptSent, AttemptEnded, AttemptSent, AttemptEnded, AttemptSent, AttemptEnded}
	if !slices.Equal(events, wantEvents) {
		t.Errorf("events = %v, want %v (no limiter events with the limiter off)", events, wantEvents)
	}
	for _, m := range marks {
		if m != "scan-1" {
			t.Fatalf("a report did not get the call's context: marks %v", marks)
		}
	}
}

func TestAWSCallHookOutcomes(t *testing.T) {
	hookTestEnv(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for _, tc := range []struct {
		name     string
		script   []string
		url      string
		cancel   bool
		want     AWSCall
		requests int
	}{
		{name: "denied", script: []string{"deny"}, requests: 1,
			want: AWSCall{Outcome: AWSCallError, Code: "AccessDenied", Attempts: 1}},
		{name: "throttled to the attempt limit", script: []string{"throttle"}, requests: 4,
			want: AWSCall{Outcome: AWSCallThrottled, Code: "Throttling", Attempts: 4, Throttles: 4}},
		{name: "context ended", script: []string{"ok"}, cancel: true,
			want: AWSCall{Outcome: AWSCallCanceled}},
		{name: "no answer", url: closedURL,
			want: AWSCall{Outcome: AWSCallNoResponse, Attempts: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := &recordingHook{}
			setCallHook(t, hook, false)
			url := tc.url
			var s *scriptServer
			if url == "" {
				s = &scriptServer{script: tc.script}
				srv := httptest.NewServer(s)
				defer srv.Close()
				url = srv.URL
			}
			ctx, d := queryData(t, keyConnection("outcomes"))
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := stsCallIn(ctx, t, d, "us-east-1", url); err == nil {
				t.Fatal("the call succeeded")
			}
			calls, _, _ := hook.got()
			want := tc.want
			want.Connection, want.Service, want.Operation, want.Region = "outcomes", "STS", "GetCallerIdentity", "us-east-1"
			if len(calls) != 1 || calls[0] != want {
				t.Errorf("calls = %+v, want [%+v]", calls, want)
			}
			if s != nil && s.requests() != tc.requests {
				t.Errorf("requests = %d, want %d", s.requests(), tc.requests)
			}
		})
	}
}

// After a throttle, the next attempt of the same connection, service and region
// waits for the limiter. Another region has its own limiter.
func TestAdaptiveLimiterWaitsAfterAThrottle(t *testing.T) {
	hookTestEnv(t)
	hook := &recordingHook{}
	setCallHook(t, hook, true)
	srv := httptest.NewServer(&scriptServer{script: []string{"throttle", "ok"}})
	defer srv.Close()

	ctx, d := queryData(t, keyConnection("limited"))
	if err := stsCallIn(ctx, t, d, "us-east-1", srv.URL); err != nil {
		t.Fatalf("GetCallerIdentity: %v", err)
	}
	if err := stsCallIn(ctx, t, d, "eu-west-1", srv.URL); err != nil {
		t.Fatalf("GetCallerIdentity in eu-west-1: %v", err)
	}
	calls, events, _ := hook.got()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want 2", calls)
	}
	if c := calls[0]; c.Attempts != 2 || c.Throttles != 1 || c.LimiterWait < 500*time.Millisecond {
		t.Errorf("us-east-1: %+v, want 2 attempts, 1 throttle and a limiter wait of at least 500ms", c)
	}
	if c := calls[1]; c.Region != "eu-west-1" || c.Attempts != 1 || c.LimiterWait > 250*time.Millisecond {
		t.Errorf("eu-west-1: %+v, want 1 attempt with no wait (its own limiter)", c)
	}
	wantEvents := []AttemptEvent{
		LimiterWaitStarted, LimiterWaitEnded, AttemptSent, AttemptEnded,
		LimiterWaitStarted, LimiterWaitEnded, AttemptSent, AttemptEnded,
		LimiterWaitStarted, LimiterWaitEnded, AttemptSent, AttemptEnded,
	}
	if !slices.Equal(events, wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
}

// A call that waits for the limiter stops when its context ends, and is not
// repeated.
func TestAdaptiveLimiterWaitEndsWithTheContext(t *testing.T) {
	hookTestEnv(t)
	ctx, d := queryData(t, keyConnection("limited"))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The first attempt is throttled; the repeat must wait for the limiter
	// (its bucket is empty after the throttle). The context ends as that wait
	// starts.
	hook := &recordingHook{onAttempt: func(e AttemptEvent, n int) {
		if e == LimiterWaitStarted && n == 2 {
			cancel()
		}
	}}
	setCallHook(t, hook, true)
	s := &scriptServer{script: []string{"throttle", "ok"}}
	srv := httptest.NewServer(s)
	defer srv.Close()

	err := stsCallIn(ctx, t, d, "us-east-1", srv.URL)
	var canceled *aws.RequestCanceledError
	if !errors.As(err, &canceled) {
		t.Fatalf("GetCallerIdentity = %v, want a canceled request", err)
	}
	calls, events, _ := hook.got()
	if len(calls) != 1 || calls[0].Outcome != AWSCallCanceled || calls[0].Attempts != 1 {
		t.Errorf("calls = %+v, want one canceled call with 1 attempt", calls)
	}
	if n := s.requests(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	want := []AttemptEvent{LimiterWaitStarted, LimiterWaitEnded, AttemptSent, AttemptEnded, LimiterWaitStarted, LimiterWaitEnded}
	if !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
}

// Without the hook and the limiter (the Steampipe path), the client options
// and the middleware stacks are as before.
func TestNoHookNoLimiterLeavesTheClients(t *testing.T) {
	hookTestEnv(t)
	SetAWSCallHook(nil)
	SetAdaptiveLimiter(false)
	opts := []func(*middleware.Stack) error{func(*middleware.Stack) error { return nil }}
	if got := withAWSCallMiddleware(opts, "plain", "us-east-1"); len(got) != 1 || &got[0] != &opts[0] {
		t.Fatalf("options = %d, want the same slice", len(got))
	}

	plain := stackLists(t, false)
	hooked := stackLists(t, true)
	for name, list := range plain {
		if slices.ContainsFunc(list, func(id string) bool { return strings.HasPrefix(id, "Catio") }) {
			t.Errorf("%s without the hook: %v", name, list)
		}
		// The hook adds its two middlewares and changes nothing else.
		without := slices.DeleteFunc(slices.Clone(hooked[name]), func(id string) bool { return strings.HasPrefix(id, "Catio") })
		if !slices.Equal(without, list) {
			t.Errorf("%s: the hook changed more than its middlewares:\n%v\n%v", name, list, hooked[name])
		}
	}
	for _, name := range []string{"sts finalize", "costexplorer finalize"} {
		list := hooked[name]
		r := slices.Index(list, "Retry")
		if r < 0 || r+1 >= len(list) || list[r+1] != "CatioAWSAttempt" {
			t.Errorf("%s with the hook: CatioAWSAttempt does not follow Retry: %v", name, list)
		}
	}
	for _, name := range []string{"sts initialize", "costexplorer initialize"} {
		if list := hooked[name]; list[len(list)-1] != "CatioAWSCall" {
			t.Errorf("%s with the hook: CatioAWSCall is not last: %v", name, list)
		}
	}
}

// stackLists gives the Initialize and Finalize steps of an STS and a Cost
// Explorer call, with or without the call hook.
func stackLists(t *testing.T, hooked bool) map[string][]string {
	t.Helper()
	if hooked {
		setCallHook(t, &recordingHook{}, true)
	} else {
		SetAWSCallHook(nil)
		SetAdaptiveLimiter(false)
	}
	srv := httptest.NewServer(&scriptServer{script: []string{"ok"}})
	defer srv.Close()
	ctx, d := queryData(t, keyConnection("lists"))
	cfg, err := getClientWithMaxRetries(ctx, d, "us-east-1", 4, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	capture := func(prefix string) func(*middleware.Stack) error {
		return func(s *middleware.Stack) error {
			out[prefix+" initialize"] = s.Initialize.List()
			out[prefix+" finalize"] = s.Finalize.List()
			return nil
		}
	}
	c := sts.NewFromConfig(*cfg, func(o *sts.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.APIOptions = append(o.APIOptions, capture("sts"))
	})
	_, _ = c.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	ce := costexplorer.NewFromConfig(*cfg, func(o *costexplorer.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.APIOptions = append(o.APIOptions, capture("costexplorer"))
	})
	_, _ = ce.GetCostAndUsage(context.Background(), &costexplorer.GetCostAndUsageInput{
		Granularity: cetypes.GranularityDaily, Metrics: []string{"UnblendedCost"},
		TimePeriod: &cetypes.DateInterval{Start: aws.String("2026-10-01"), End: aws.String("2026-10-02")},
	})
	return out
}

// With the credentials hook too (it moves Retry in older clients), each attempt
// is counted, and the attempt middleware still follows Retry.
func TestAWSCallHookWithTheCredentialsHook(t *testing.T) {
	hookTestEnv(t)
	SetCredentialsHook(&fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE", "AKIAHOOKTWO"}, repeat: true})
	t.Cleanup(func() { SetCredentialsHook(nil) })
	hook := &recordingHook{}
	setCallHook(t, hook, true)
	h := &ceServer{stsServer{expired: 1}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{})
	ctx, d := queryData(t, conn)
	cfg, err := getClientWithMaxRetries(ctx, d, "us-east-1", 4, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var finalize []string
	client := costexplorer.NewFromConfig(*cfg, func(o *costexplorer.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.APIOptions = append(o.APIOptions, func(s *middleware.Stack) error { finalize = s.Finalize.List(); return nil })
	})
	_, err = client.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
		Granularity: cetypes.GranularityDaily, Metrics: []string{"UnblendedCost"},
		TimePeriod: &cetypes.DateInterval{Start: aws.String("2026-10-01"), End: aws.String("2026-10-02")},
	})
	if err != nil {
		t.Fatalf("GetCostAndUsage: %v", err)
	}
	if keys := h.seen(); len(keys) != 2 || keys[1] != "AKIAHOOKTWO" {
		t.Errorf("signed with %v, want the hook's second set on the repeat", keys)
	}
	calls, _, _ := hook.got()
	want := AWSCall{Connection: "hooked", Service: "Cost Explorer", Operation: "GetCostAndUsage", Region: "us-east-1", Outcome: AWSCallOK, Attempts: 2}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want 1", calls)
	}
	got := calls[0]
	got.LimiterWait = 0
	if got != want {
		t.Errorf("call = %+v, want %+v", got, want)
	}
	r, a := slices.Index(finalize, "Retry"), slices.Index(finalize, "ResolveAuthScheme")
	if r < 0 || r > a || finalize[r+1] != "CatioAWSAttempt" {
		t.Errorf("finalize = %v, want Retry, then CatioAWSAttempt, before ResolveAuthScheme", finalize)
	}
}

// The endpoint_url branch builds the config again; the hook still applies.
func TestAWSCallHookWithEndpointURL(t *testing.T) {
	hookTestEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENVKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	hook := &recordingHook{}
	setCallHook(t, hook, false)
	srv := httptest.NewServer(&scriptServer{script: []string{"ok"}})
	defer srv.Close()
	conn := &plugin.Connection{Name: "endpoint"}
	conn.SetConfig(awsConfig{EndpointUrl: aws.String(srv.URL)})
	ctx, d := queryData(t, conn)
	if err := callSTSWith(ctx, d, nil); err != nil {
		t.Fatalf("GetCallerIdentity: %v", err)
	}
	if calls, _, _ := hook.got(); len(calls) != 1 || calls[0].Outcome != AWSCallOK || calls[0].Attempts != 1 {
		t.Errorf("calls = %+v, want one ok call", calls)
	}
}

func TestAWSCallHookPanicIsDropped(t *testing.T) {
	hookTestEnv(t)
	hook := &recordingHook{panic: true}
	setCallHook(t, hook, true)
	srv := httptest.NewServer(&scriptServer{script: []string{"ok"}})
	defer srv.Close()
	ctx, d := queryData(t, keyConnection("panics"))
	if err := stsCallIn(ctx, t, d, "us-east-1", srv.URL); err != nil {
		t.Fatalf("GetCallerIdentity: %v (a hook panic must not fail the call)", err)
	}
	if calls, events, _ := hook.got(); len(calls) != 1 || len(events) != 4 {
		t.Errorf("calls %d, events %d; want 1 and 4", len(calls), len(events))
	}
}

// Many calls at once, with the hook: some throttled with the limiter off, and
// none throttled with the limiter on (a throttle would slow the test down by
// seconds). Run with -race.
func TestAWSCallHookConcurrentCalls(t *testing.T) {
	for _, tc := range []struct {
		name          string
		limiter       bool
		throttleEvery int
	}{
		{"throttles, no limiter", false, 7},
		{"limiter", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookTestEnv(t)
			hook := &recordingHook{}
			setCallHook(t, hook, tc.limiter)
			var mu sync.Mutex
			n, throttled := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				n++
				s := &scriptServer{script: []string{"ok"}}
				if tc.throttleEvery > 0 && n%tc.throttleEvery == 0 {
					throttled++
					s.script = []string{"throttle"}
				}
				mu.Unlock()
				s.ServeHTTP(w, r)
			}))
			defer srv.Close()
			ctx, d := queryData(t, keyConnection("busy"))
			regions := []string{"us-east-1", "us-west-2", "eu-west-1"}
			var wg sync.WaitGroup
			for i := range 30 {
				wg.Go(func() {
					for range 5 {
						if err := stsCallIn(ctx, t, d, regions[i%len(regions)], srv.URL); err != nil {
							t.Errorf("GetCallerIdentity: %v", err)
						}
					}
				})
			}
			wg.Wait()
			calls, events, _ := hook.got()
			if len(calls) != 150 {
				t.Fatalf("calls = %d, want 150", len(calls))
			}
			attempts, throttles := 0, 0
			for _, c := range calls {
				attempts += c.Attempts
				throttles += c.Throttles
			}
			mu.Lock()
			defer mu.Unlock()
			if attempts != n || throttles != throttled || attempts-throttles != 150 {
				t.Errorf("attempts %d, throttles %d; requests %d, throttled %d", attempts, throttles, n, throttled)
			}
			count := map[AttemptEvent]int{}
			for _, e := range events {
				count[e]++
			}
			waits := 0
			if tc.limiter {
				waits = n
			}
			if count[AttemptSent] != n || count[AttemptEnded] != n || count[LimiterWaitStarted] != waits || count[LimiterWaitEnded] != waits {
				t.Errorf("events %v, requests %d", count, n)
			}
		})
	}
}

func TestCallOutcome(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	opErr := func(err error) error {
		return &smithy.OperationError{ServiceID: "EC2", OperationName: "DescribeVpcs", Err: err}
	}
	for _, tc := range []struct {
		name          string
		ctx           context.Context
		err           error
		outcome, code string
	}{
		{"ok", live, nil, AWSCallOK, ""},
		{"throttle code", live, opErr(&smithy.GenericAPIError{Code: "RequestLimitExceeded", Message: "made-up"}), AWSCallThrottled, "RequestLimitExceeded"},
		{"other code", live, opErr(&smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "made-up"}), AWSCallError, "UnauthorizedOperation"},
		{"canceled request", live, opErr(&aws.RequestCanceledError{Err: errors.New("made-up")}), AWSCallCanceled, ""},
		{"context ended", done, opErr(errors.New("made-up")), AWSCallCanceled, ""},
		{"network", live, opErr(&net.OpError{Op: "dial", Err: errors.New("made-up refused")}), AWSCallNoResponse, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, code := callOutcome(tc.ctx, tc.err)
			if outcome != tc.outcome || code != tc.code {
				t.Errorf("callOutcome = %q, %q; want %q, %q", outcome, code, tc.outcome, tc.code)
			}
		})
	}
}

// SetAdaptiveLimiter drops the limiters' state, and off means none.
func TestSetAdaptiveLimiter(t *testing.T) {
	t.Cleanup(func() { SetAdaptiveLimiter(false) })
	SetAdaptiveLimiter(true)
	a := adaptiveLimiter("c", "EC2", "us-east-1")
	if a == nil || adaptiveLimiter("c", "EC2", "us-east-1") != a {
		t.Fatal("the limiter of a key is not kept")
	}
	if adaptiveLimiter("c", "EC2", "eu-west-1") == a || adaptiveLimiter("c", "S3", "us-east-1") == a || adaptiveLimiter("d", "EC2", "us-east-1") == a {
		t.Error("two keys share a limiter")
	}
	SetAdaptiveLimiter(true)
	if adaptiveLimiter("c", "EC2", "us-east-1") == a {
		t.Error("SetAdaptiveLimiter kept the old state")
	}
	SetAdaptiveLimiter(false)
	if adaptiveLimiter("c", "EC2", "us-east-1") != nil {
		t.Error("a limiter with the limiter off")
	}
}

// panicMiddleware panics in Finalize, as a broken transport could.
type panicMiddleware struct{}

func (panicMiddleware) ID() string { return "TestPanic" }

func (panicMiddleware) HandleFinalize(context.Context, middleware.FinalizeInput, middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
	panic("made-up transport panic")
}

// A panic below the middlewares still ends the attempt and the call for the
// hook, and goes on to the caller unchanged.
func TestAWSCallHookPanicBelow(t *testing.T) {
	hookTestEnv(t)
	hook := &recordingHook{}
	setCallHook(t, hook, true)
	ctx, d := queryData(t, keyConnection("panics"))
	cfg, err := getClientWithMaxRetries(ctx, d, "us-east-1", 4, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	client := sts.NewFromConfig(*cfg, func(o *sts.Options) {
		o.BaseEndpoint = aws.String("http://127.0.0.1:1")
		o.APIOptions = append(o.APIOptions, func(s *middleware.Stack) error {
			return s.Finalize.Add(panicMiddleware{}, middleware.After)
		})
	})
	got := func() (p any) {
		defer func() { p = recover() }()
		_, _ = client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		return nil
	}()
	if got != "made-up transport panic" {
		t.Fatalf("recovered %v, want the transport's panic", got)
	}
	calls, events, _ := hook.got()
	want := []AttemptEvent{LimiterWaitStarted, LimiterWaitEnded, AttemptSent, AttemptEnded}
	if !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	if len(calls) != 1 || calls[0].Outcome != AWSCallNoResponse || calls[0].Attempts != 1 {
		t.Errorf("calls = %+v, want one no_response call with 1 attempt", calls)
	}
}
