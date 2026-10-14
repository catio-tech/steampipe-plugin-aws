package aws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/hashicorp/go-hclog"
	"github.com/turbot/steampipe-plugin-sdk/v6/connection"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin"
	"github.com/turbot/steampipe-plugin-sdk/v6/plugin/context_key"
)

// fakeHook gives one set of credentials at a time; ExpiredToken moves to the
// next set.
type fakeHook struct {
	mu        sync.Mutex
	conn      string
	keys      []string
	cur       int
	repeat    bool
	expired   int
	retrieves int
	providers int
}

func (f *fakeHook) Provider(connection string) aws.CredentialsProvider {
	f.mu.Lock()
	f.providers++
	f.mu.Unlock()
	if connection != f.conn {
		return nil
	}
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.retrieves++
		return aws.Credentials{
			AccessKeyID: f.keys[f.cur], SecretAccessKey: "secret", SessionToken: "token",
			CanExpire: true, Expires: time.Now().Add(time.Hour),
		}, nil
	})
}

func (f *fakeHook) ExpiredToken(context.Context, string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired++
	if f.cur < len(f.keys)-1 {
		f.cur++
	}
	return f.repeat
}

// stsServer answers GetCallerIdentity: with ExpiredToken for the first
// `expired` requests, then with success. It records the access key of each
// request.
type stsServer struct {
	mu      sync.Mutex
	expired int
	keys    []string
}

var credentialKey = regexp.MustCompile(`Credential=([A-Z0-9]+)/`)

func (s *stsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	m := credentialKey.FindStringSubmatch(r.Header.Get("Authorization"))
	key := ""
	if m != nil {
		key = m[1]
	}
	s.keys = append(s.keys, key)
	n := len(s.keys)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	if n <= s.expired {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>ExpiredToken</Code><Message>The security token included in the request is expired</Message></Error><RequestId>1</RequestId></ErrorResponse>`))
		return
	}
	_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDATEST</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>2</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
}

func (s *stsServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

// hookTestEnv keeps the SDK away from the machine's AWS files and variables.
func hookTestEnv(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")
}

func queryData(t *testing.T, conn *plugin.Connection) (context.Context, *plugin.QueryData) {
	t.Helper()
	cache, err := connection.NewConnectionCache(conn.Name, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), context_key.Logger, hclog.NewNullLogger())
	return ctx, &plugin.QueryData{Connection: conn, ConnectionCache: cache}
}

// callSTS makes one GetCallerIdentity call through the plugin's own client
// setup for the connection (getClientWithMaxRetries), against srv.
func callSTS(t *testing.T, conn *plugin.Connection, srv *httptest.Server) error {
	t.Helper()
	ctx, d := queryData(t, conn)
	return callSTSWith(ctx, d, srv)
}

func callSTSWith(ctx context.Context, d *plugin.QueryData, srv *httptest.Server) error {
	cfg, err := getClientWithMaxRetries(ctx, d, "us-east-1", 4, 5*time.Millisecond)
	if err != nil {
		return err
	}
	client := sts.NewFromConfig(*cfg, func(o *sts.Options) {
		if srv != nil {
			o.BaseEndpoint = aws.String(srv.URL)
		}
	})
	_, err = client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	return err
}

func TestCredentialsHookRepeatsWithTheNewSet(t *testing.T) {
	hookTestEnv(t)
	hook := &fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE", "AKIAHOOKTWO"}, repeat: true}
	SetCredentialsHook(hook)
	t.Cleanup(func() { SetCredentialsHook(nil) })
	srv := httptest.NewServer(&stsServer{expired: 1})
	defer srv.Close()

	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{})
	if err := callSTS(t, conn, srv); err != nil {
		t.Fatalf("GetCallerIdentity: %v", err)
	}
	keys := srv.Config.Handler.(*stsServer).seen()
	if len(keys) != 2 || keys[0] != "AKIAHOOKONE" || keys[1] != "AKIAHOOKTWO" {
		t.Errorf("signed with %v, want [AKIAHOOKONE AKIAHOOKTWO]", keys)
	}
	if hook.expired != 1 || hook.retrieves != 2 {
		t.Errorf("ExpiredToken calls %d, Retrieve calls %d; want 1 and 2", hook.expired, hook.retrieves)
	}
}

func TestCredentialsHookStopsTheCall(t *testing.T) {
	hookTestEnv(t)
	hook := &fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE"}, repeat: false}
	SetCredentialsHook(hook)
	t.Cleanup(func() { SetCredentialsHook(nil) })
	srv := httptest.NewServer(&stsServer{expired: 10})
	defer srv.Close()

	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{})
	err := callSTS(t, conn, srv)
	if !isExpiredToken(err) {
		t.Fatalf("GetCallerIdentity = %v, want the ExpiredToken error", err)
	}
	if n := len(srv.Config.Handler.(*stsServer).seen()); n != 1 || hook.expired != 1 {
		t.Errorf("requests %d, ExpiredToken calls %d; want 1 and 1", n, hook.expired)
	}
}

// The retryer's attempt limit (4 here) bounds the repeats.
func TestCredentialsHookAttemptLimit(t *testing.T) {
	hookTestEnv(t)
	hook := &fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE"}, repeat: true}
	SetCredentialsHook(hook)
	t.Cleanup(func() { SetCredentialsHook(nil) })
	srv := httptest.NewServer(&stsServer{expired: 100})
	defer srv.Close()

	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{})
	if err := callSTS(t, conn, srv); !isExpiredToken(err) {
		t.Fatalf("GetCallerIdentity = %v, want the ExpiredToken error", err)
	}
	if n := len(srv.Config.Handler.(*stsServer).seen()); n != 4 {
		t.Errorf("requests = %d, want 4", n)
	}
}

// With no hook, or a hook with no provider for the connection, the keys of the
// connection config sign the call, and ExpiredToken is not repeated: the
// Steampipe path does not change.
func TestNoCredentialsHookKeepsTheConnectionConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook CredentialsHook
	}{
		{"no hook", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookTestEnv(t)
			SetCredentialsHook(tc.hook)
			t.Cleanup(func() { SetCredentialsHook(nil) })
			srv := httptest.NewServer(&stsServer{expired: 1})
			defer srv.Close()

			ak, sk, st := "AKIACONFIG", "secret", "token"
			conn := &plugin.Connection{Name: "plain"}
			conn.SetConfig(awsConfig{AccessKey: &ak, SecretKey: &sk, SessionToken: &st})
			if err := callSTS(t, conn, srv); !isExpiredToken(err) {
				t.Fatalf("GetCallerIdentity = %v, want the ExpiredToken error, not repeated", err)
			}
			if keys := srv.Config.Handler.(*stsServer).seen(); len(keys) != 1 || keys[0] != "AKIACONFIG" {
				t.Errorf("signed with %v, want [AKIACONFIG]", keys)
			}
		})
	}
}

func TestIsExpiredToken(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&smithy.GenericAPIError{Code: "ExpiredToken"}, true},
		{&smithy.GenericAPIError{Code: "ExpiredTokenException"}, true},
		{&smithy.GenericAPIError{Code: "AccessDeniedException"}, false},
		{&smithy.GenericAPIError{Code: "RequestExpired"}, true},
		{&smithy.GenericAPIError{Code: "SignatureDoesNotMatch"}, false},
		{errors.New("dial tcp: i/o timeout"), false},
		{nil, false},
	} {
		if got := isExpiredToken(tc.err); got != tc.want {
			t.Errorf("isExpiredToken(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A hook with no provider for the connection fails closed: the call must not
// fall back to the process's own credentials.
func TestCredentialsHookFailsClosed(t *testing.T) {
	hookTestEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAPODROLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	SetCredentialsHook(&fakeHook{conn: "other", keys: []string{"AKIAHOOKONE"}, repeat: true})
	t.Cleanup(func() { SetCredentialsHook(nil) })
	srv := httptest.NewServer(&stsServer{})
	defer srv.Close()

	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{})
	if err := callSTS(t, conn, srv); !errors.Is(err, errNoHookCredentials) {
		t.Fatalf("GetCallerIdentity = %v, want errNoHookCredentials", err)
	}
	if n := len(srv.Config.Handler.(*stsServer).seen()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

// A base client built before the hook was set does not use it: its queries
// fail, and do not go out with the old credentials.
func TestCredentialsHookSetAfterTheBaseClient(t *testing.T) {
	hookTestEnv(t)
	SetCredentialsHook(nil)
	t.Cleanup(func() { SetCredentialsHook(nil) })
	srv := httptest.NewServer(&stsServer{})
	defer srv.Close()

	ak, sk := "AKIACONFIG", "secret"
	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{AccessKey: &ak, SecretKey: &sk})
	ctx, d := queryData(t, conn)
	if err := callSTSWith(ctx, d, srv); err != nil {
		t.Fatalf("before the hook: %v", err)
	}
	SetCredentialsHook(&fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE"}, repeat: true})
	err := callSTSWith(ctx, d, srv)
	if err == nil || !strings.Contains(err.Error(), "built before the credentials hook") {
		t.Fatalf("after the hook: %v, want the built-before error", err)
	}
	if keys := srv.Config.Handler.(*stsServer).seen(); len(keys) != 1 {
		t.Errorf("requests = %v, want only the one before the hook", keys)
	}
}

// The plugin asks the hook for a connection's provider once, however many
// clients it builds.
func TestCredentialsHookProviderOnce(t *testing.T) {
	hookTestEnv(t)
	hook := &fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE"}, repeat: true}
	SetCredentialsHook(hook)
	t.Cleanup(func() { SetCredentialsHook(nil) })
	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{})
	ctx, d := queryData(t, conn)
	for range 50 {
		if _, err := getClientWithMaxRetries(ctx, d, "us-east-1", 4, 5*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if hook.providers != 1 {
		t.Errorf("Provider calls = %d, want 1", hook.providers)
	}
}

// When the hook does not repeat, the cached set is still dropped: the next call
// gets the set that the hook gives then.
func TestCredentialsHookNoRepeatDropsTheSet(t *testing.T) {
	hookTestEnv(t)
	hook := &fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE", "AKIAHOOKTWO"}, repeat: false}
	SetCredentialsHook(hook)
	t.Cleanup(func() { SetCredentialsHook(nil) })
	srv := httptest.NewServer(&stsServer{expired: 1})
	defer srv.Close()
	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{})
	ctx, d := queryData(t, conn)
	if err := callSTSWith(ctx, d, srv); !isExpiredToken(err) {
		t.Fatalf("first call = %v, want ExpiredToken", err)
	}
	if err := callSTSWith(ctx, d, srv); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if keys := srv.Config.Handler.(*stsServer).seen(); len(keys) != 2 || keys[1] != "AKIAHOOKTWO" {
		t.Errorf("signed with %v, want the second call with AKIAHOOKTWO", keys)
	}
}

// endpoint_url builds the config again: it keeps the hook's credentials.
func TestCredentialsHookWithEndpointURL(t *testing.T) {
	hookTestEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAPODROLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	hook := &fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE", "AKIAHOOKTWO"}, repeat: true}
	SetCredentialsHook(hook)
	t.Cleanup(func() { SetCredentialsHook(nil) })
	srv := httptest.NewServer(&stsServer{expired: 1})
	defer srv.Close()
	conn := &plugin.Connection{Name: "hooked"}
	conn.SetConfig(awsConfig{EndpointUrl: aws.String(srv.URL)})
	ctx, d := queryData(t, conn)
	if err := callSTSWith(ctx, d, nil); err != nil {
		t.Fatalf("GetCallerIdentity: %v", err)
	}
	if keys := srv.Config.Handler.(*stsServer).seen(); len(keys) != 2 || keys[0] != "AKIAHOOKONE" || keys[1] != "AKIAHOOKTWO" {
		t.Errorf("signed with %v, want [AKIAHOOKONE AKIAHOOKTWO]", keys)
	}
}

// ceServer answers Cost Explorer GetCostAndUsage (JSON protocol): with
// ExpiredTokenException for the first `expired` requests, then with success.
type ceServer struct{ stsServer }

func (s *ceServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	m := credentialKey.FindStringSubmatch(r.Header.Get("Authorization"))
	key := ""
	if m != nil {
		key = m[1]
	}
	s.keys = append(s.keys, key)
	n := len(s.keys)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if n <= s.expired {
		w.Header().Set("X-Amzn-Errortype", "ExpiredTokenException")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"ExpiredTokenException","message":"The security token included in the request is expired"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ResultsByTime":[]}`))
}

// Cost Explorer's client (an older one) puts its retry middleware after
// GetIdentity. The repeat must still use the hook's new set.
func TestCredentialsHookOlderClientRepeatsWithTheNewSet(t *testing.T) {
	hookTestEnv(t)
	hook := &fakeHook{conn: "hooked", keys: []string{"AKIAHOOKONE", "AKIAHOOKTWO"}, repeat: true}
	SetCredentialsHook(hook)
	t.Cleanup(func() { SetCredentialsHook(nil) })
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
	client := costexplorer.NewFromConfig(*cfg, func(o *costexplorer.Options) { o.BaseEndpoint = aws.String(srv.URL) })
	_, err = client.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
		Granularity: cetypes.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
		TimePeriod:  &cetypes.DateInterval{Start: aws.String("2026-10-01"), End: aws.String("2026-10-02")},
	})
	if err != nil {
		t.Fatalf("GetCostAndUsage: %v", err)
	}
	if keys := h.seen(); len(keys) != 2 || keys[0] != "AKIAHOOKONE" || keys[1] != "AKIAHOOKTWO" {
		t.Errorf("signed with %v, want [AKIAHOOKONE AKIAHOOKTWO]", keys)
	}
}
