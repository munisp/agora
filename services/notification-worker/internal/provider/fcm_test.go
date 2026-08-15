package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newTestFCM builds an FCM provider with no retry backoff.
func newTestFCM(t *testing.T, cfg FCMConfig) *FCM {
	t.Helper()
	f, err := NewFCM(cfg, zap.NewNop())
	require.NoError(t, err)
	f.Client.sleep = func(context.Context, int) {}
	return f
}

// ---------------------------------------------------------------------------
// FCM_MOCK=1 opt-in (default OFF, SIM-010): deterministic, no network
// ---------------------------------------------------------------------------

func TestFCMMockDefaultDeterministic(t *testing.T) {
	// BaseURL points nowhere: any network use would fail the test.
	f := newTestFCM(t, FCMConfig{Mock: true, BaseURL: "http://127.0.0.1:1"})
	require.True(t, f.Configured(), "mock mode needs no credentials")

	status, body, err := f.SendPush(context.Background(), PushMessage{Token: "device-abc", Title: "Hi", Body: "there"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"name":"projects/mock-project/messages/mock-068c3bfd45941f8f"}`, string(body),
		"mock message name must be deterministic (sha256 of the token)")

	// Same input → same name (determinism), different token → different name.
	_, body2, err := f.SendPush(context.Background(), PushMessage{Token: "device-abc", Title: "other"})
	require.NoError(t, err)
	require.Equal(t, string(body), string(body2))
	_, body3, err := f.SendPush(context.Background(), PushMessage{Token: "device-xyz"})
	require.NoError(t, err)
	require.NotEqual(t, string(body), string(body3))
}

func TestFCMMockHooks(t *testing.T) {
	f := newTestFCM(t, FCMConfig{Mock: true, ProjectID: "p-9"})

	// mock-fail → provider 500 error.
	_, _, err := f.SendPush(context.Background(), PushMessage{Token: "mock-fail"})
	require.Error(t, err)
	require.Equal(t, http.StatusInternalServerError, err.(*Error).StatusCode)

	// mock-unregistered → 404 classified as unregistered (prune the token).
	_, _, err = f.SendPush(context.Background(), PushMessage{Token: "mock-unregistered"})
	require.Error(t, err)
	pe := err.(*Error)
	require.Equal(t, http.StatusNotFound, pe.StatusCode)
	require.True(t, Unregistered(pe.StatusCode, []byte(pe.Body)))

	// Project id flows into the deterministic message name.
	status, body, err := f.SendPush(context.Background(), PushMessage{Token: "tok"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(body), "projects/p-9/messages/mock-")
}

func TestFCMUnconfiguredLive(t *testing.T) {
	f := newTestFCM(t, FCMConfig{Mock: false})
	require.False(t, f.Configured())
	_, _, err := f.SendPush(context.Background(), PushMessage{Token: "tok"})
	require.ErrorContains(t, err, "fcm not configured")
}

// ---------------------------------------------------------------------------
// Legacy server-key API (httptest fake)
// ---------------------------------------------------------------------------

type capturedRequest struct {
	path string
	auth string
	body []byte
}

// fakeFCMServer records requests and serves scripted status codes per call.
type fakeFCMServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []capturedRequest
	// statuses are served in order; the last one repeats.
	statuses []int
}

func newFakeFCMServer(t *testing.T, statuses ...int) *fakeFCMServer {
	t.Helper()
	f := &fakeFCMServer{statuses: statuses}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		f.reqs = append(f.reqs, capturedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		n := len(f.reqs)
		status := f.statuses[len(f.statuses)-1]
		if n <= len(f.statuses) {
			status = f.statuses[n-1]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if strings.HasSuffix(r.URL.Path, "messages:send") {
			fmt.Fprint(w, `{"name":"projects/proj-1/messages/123"}`)
		} else {
			fmt.Fprintf(w, `{"success":%d}`, map[bool]int{true: 1, false: 0}[status < 400])
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestFCMLegacyServerKey(t *testing.T) {
	fake := newFakeFCMServer(t, http.StatusOK)
	f := newTestFCM(t, FCMConfig{Mock: false, ServerKey: "legacy-key", BaseURL: fake.srv.URL})
	require.True(t, f.Configured())

	status, _, err := f.SendPush(context.Background(), PushMessage{
		Token: "tok-1", Title: "T", Body: "B", Data: map[string]string{"booking_id": "b-1"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	fake.mu.Lock()
	require.Len(t, fake.reqs, 1)
	req := fake.reqs[0]
	fake.mu.Unlock()
	require.Equal(t, "/fcm/send", req.path)
	require.Equal(t, "key=legacy-key", req.auth)
	require.JSONEq(t, `{
		"to": "tok-1",
		"notification": {"title": "T", "body": "B"},
		"data": {"booking_id": "b-1"},
		"priority": "high"
	}`, string(req.body))
}

// Retries: a 500 then a 200 succeeds on the second attempt; a 4xx fails
// immediately without a retry.
func TestFCMRetryPolicy(t *testing.T) {
	fake := newFakeFCMServer(t, http.StatusInternalServerError, http.StatusOK)
	f := newTestFCM(t, FCMConfig{Mock: false, ServerKey: "k", BaseURL: fake.srv.URL})
	status, _, err := f.SendPush(context.Background(), PushMessage{Token: "tok"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	fake.mu.Lock()
	require.Len(t, fake.reqs, 2, "5xx must be retried")
	fake.mu.Unlock()

	fake4xx := newFakeFCMServer(t, http.StatusBadRequest)
	f4 := newTestFCM(t, FCMConfig{Mock: false, ServerKey: "k", BaseURL: fake4xx.srv.URL})
	_, _, err = f4.SendPush(context.Background(), PushMessage{Token: "tok"})
	require.Error(t, err)
	require.True(t, ClientError(err))
	fake4xx.mu.Lock()
	require.Len(t, fake4xx.reqs, 1, "4xx must NOT be retried")
	fake4xx.mu.Unlock()
}

// ---------------------------------------------------------------------------
// HTTP v1 (service account) — httptest token endpoint + FCM fake
// ---------------------------------------------------------------------------

// testServiceAccountJSON mints a throwaway RSA key and wraps it in a
// service-account JSON document whose token_uri points at the fake.
func testServiceAccountJSON(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	sa := map[string]string{
		"project_id":   "proj-1",
		"client_email": "fcm@proj-1.iam.gserviceaccount.com",
		"private_key":  pemKey,
		"token_uri":    tokenURI,
	}
	raw, err := json.Marshal(sa)
	require.NoError(t, err)
	return string(raw)
}

func TestFCMHTTPv1ServiceAccount(t *testing.T) {
	var mu sync.Mutex
	var tokenCalls int
	var v1Body []byte
	var v1Auth, v1Path string

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokenCalls++
		mu.Unlock()
		require.NoError(t, r.ParseForm())
		require.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", r.Form.Get("grant_type"))
		assertion := r.Form.Get("assertion")
		require.NotEmpty(t, assertion)
		parts := strings.Split(assertion, ".")
		require.Len(t, parts, 3, "JWT-bearer assertion must be a signed JWT")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"test-access-token","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/v1/projects/proj-1/messages:send", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		v1Auth = r.Header.Get("Authorization")
		v1Path = r.URL.Path
		v1Body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"projects/proj-1/messages/456"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := newTestFCM(t, FCMConfig{
		Mock:            false,
		CredentialsJSON: testServiceAccountJSON(t, srv.URL+"/token"),
		BaseURL:         srv.URL,
	})
	require.True(t, f.Configured())
	require.Equal(t, "proj-1", f.ProjectID, "creds project_id must win over empty FCM_PROJECT_ID")

	status, body, err := f.SendPush(context.Background(), PushMessage{
		Token: "tok-ios-ish", Title: "Booking", Body: "Confirmed", Data: map[string]string{"kind": "confirmation"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"name":"projects/proj-1/messages/456"}`, string(body))

	mu.Lock()
	require.Equal(t, 1, tokenCalls)
	require.Equal(t, "/v1/projects/proj-1/messages:send", v1Path)
	require.Equal(t, "Bearer test-access-token", v1Auth)
	require.JSONEq(t, `{"message":{
		"token": "tok-ios-ish",
		"notification": {"title": "Booking", "body": "Confirmed"},
		"data": {"kind": "confirmation"}
	}}`, string(v1Body))
	mu.Unlock()

	// Second send reuses the cached access token (no second /token call).
	_, _, err = f.SendPush(context.Background(), PushMessage{Token: "tok-2", Title: "x"})
	require.NoError(t, err)
	mu.Lock()
	require.Equal(t, 1, tokenCalls, "access token must be cached until expiry")
	mu.Unlock()
}

// FCM_PROJECT_ID fills in when the credentials JSON has no project_id, and
// an explicit FCM_PROJECT_ID loses to the credentials project_id.
func TestFCMProjectIDResolution(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	f := newTestFCM(t, FCMConfig{Mock: true, ProjectID: "env-proj"})
	require.Equal(t, "env-proj", f.ProjectID)

	creds := testServiceAccountJSON(t, srv.URL+"/token")
	f2 := newTestFCM(t, FCMConfig{Mock: false, CredentialsJSON: creds, ProjectID: "env-proj"})
	require.Equal(t, "proj-1", f2.ProjectID, "credentials project_id wins")
}

func TestFCMBadCredentialsJSON(t *testing.T) {
	_, err := NewFCM(FCMConfig{CredentialsJSON: `{"project_id":"p"}`}, zap.NewNop())
	require.ErrorContains(t, err, "FCM_CREDENTIALS_JSON")

	_, err = NewFCM(FCMConfig{CredentialsJSON: `not json`}, zap.NewNop())
	require.ErrorContains(t, err, "FCM_CREDENTIALS_JSON")
}

func TestUnregisteredHelper(t *testing.T) {
	require.True(t, Unregistered(http.StatusNotFound, nil))
	require.True(t, Unregistered(http.StatusBadRequest, []byte(`{"error":{"details":[{"errorCode":"UNREGISTERED"}]}}`)))
	require.True(t, Unregistered(http.StatusOK, []byte(`{"results":[{"error":"NotRegistered"}]}`)))
	require.False(t, Unregistered(http.StatusInternalServerError, []byte(`boom`)))
}

// The token endpoint path in parseServiceAccount must survive a URL with
// query components (regression guard for the url.Values form encoding).
func TestTokenEndpointFormEncoding(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"x","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	f := newTestFCM(t, FCMConfig{Mock: false, CredentialsJSON: testServiceAccountJSON(t, srv.URL)})
	tok, err := f.tok.Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "x", tok)
	require.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", gotForm.Get("grant_type"))
	require.NotEmpty(t, gotForm.Get("assertion"))
}
