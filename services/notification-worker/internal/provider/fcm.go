package provider

// Firebase Cloud Messaging provider (SPEC-W16 contract §1).
//
// Configuration (env, wired by cmd/worker via internal/config):
//
//	FCM_MOCK             default "1": deterministic mock, no network (below)
//	FCM_SERVER_KEY       legacy FCM server key → legacy HTTP API
//	FCM_CREDENTIALS_JSON service-account JSON → FCM HTTP v1 + OAuth2
//	FCM_PROJECT_ID       GCP project id (HTTP v1 path; the service-account
//	                     project_id wins when both are set)
//	FCM_BASE_URL         default https://fcm.googleapis.com (tests/override)
//
// Auth selection when FCM_MOCK=0: FCM_CREDENTIALS_JSON takes precedence
// (HTTP v1); FCM_SERVER_KEY alone selects the legacy API.
//
// ASSUMPTION: the legacy FCM HTTP API (POST /fcm/send, Authorization:
// key=...) was deprecated by Google with shutdown announced for 2024; it is
// retained ONLY because the wave contract mandates FCM_SERVER_KEY support.
// New deployments should use FCM_CREDENTIALS_JSON (HTTP v1).
//
// ASSUMPTION: the HTTP v1 request envelope below follows the public Google
// docs shape {"message":{token, notification:{title,body}, data}}; the
// platform-specific blocks (android.priority, apns.headers/aps) are
// omitted — FCM applies defaults — because the exact per-platform field
// shapes are not pinned by this repo's contract. Add them here when a real
// deployment needs them.
//
// ASSUMPTION: OAuth2 access tokens are obtained with the service-account
// JWT-bearer grant (RFC 7523: grant_type
// urn:ietf:params:oauth:grant-type:jwt-bearer, scope
// https://www.googleapis.com/auth/firebase.messaging, assertion = RS256
// JWT signed with the service-account private_key against token_uri).
// Implemented with the stdlib only (no google client dependency).

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// DefaultFCMBaseURL is the production FCM endpoint base.
const DefaultFCMBaseURL = "https://fcm.googleapis.com"

// fcmScope is the OAuth2 scope required by FCM HTTP v1.
const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// FCMConfig carries the FCM_* environment configuration (see file header).
type FCMConfig struct {
	Mock            bool   // FCM_MOCK (default true): deterministic, no network
	ServerKey       string // FCM_SERVER_KEY (legacy API)
	CredentialsJSON string // FCM_CREDENTIALS_JSON (service account, HTTP v1)
	ProjectID       string // FCM_PROJECT_ID (HTTP v1; creds project_id wins)
	BaseURL         string // FCM_BASE_URL override (default DefaultFCMBaseURL)
}

// FCM sends push notifications via Firebase Cloud Messaging.
type FCM struct {
	Client    *Client
	BaseURL   string
	ProjectID string
	ServerKey string
	Mock      bool

	creds *serviceAccount // parsed FCM_CREDENTIALS_JSON (nil in legacy/mock mode)
	tok   *tokenSource    // OAuth2 cache (HTTP v1 only)
}

// NewFCM builds the provider from the environment-derived config. A
// malformed FCM_CREDENTIALS_JSON is a boot-time error (fail fast, mirroring
// the quiet-hours config validation in cmd/worker).
func NewFCM(cfg FCMConfig, log *zap.Logger) (*FCM, error) {
	f := &FCM{
		Client:    NewClient("fcm", log),
		BaseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		ProjectID: cfg.ProjectID,
		ServerKey: cfg.ServerKey,
		Mock:      cfg.Mock,
	}
	if f.BaseURL == "" {
		f.BaseURL = DefaultFCMBaseURL
	}
	if cfg.CredentialsJSON != "" {
		sa, err := parseServiceAccount([]byte(cfg.CredentialsJSON))
		if err != nil {
			return nil, fmt.Errorf("FCM_CREDENTIALS_JSON: %w", err)
		}
		f.creds = sa
		if sa.ProjectID != "" {
			f.ProjectID = sa.ProjectID
		}
		f.tok = newTokenSource(f.Client.HC, sa)
	}
	return f, nil
}

// Name implements PushProvider.
func (f *FCM) Name() string { return "fcm" }

// Configured implements PushProvider. Mock mode counts as configured: it
// needs no credentials (this is the default developer experience).
func (f *FCM) Configured() bool {
	if f.Mock {
		return true
	}
	if f.creds != nil {
		return f.ProjectID != ""
	}
	return f.ServerKey != ""
}

// SendPush implements PushProvider: mock → deterministic local result;
// otherwise legacy server-key API or HTTP v1 depending on configuration.
func (f *FCM) SendPush(ctx context.Context, msg PushMessage) (int, []byte, error) {
	if msg.Token == "" {
		return 0, nil, &Error{StatusCode: 0, Body: "fcm: empty device token"}
	}
	if f.Mock {
		return f.sendMock(msg)
	}
	if !f.Configured() {
		return 0, nil, &Error{StatusCode: 0, Body: "fcm not configured: set FCM_CREDENTIALS_JSON or FCM_SERVER_KEY (or FCM_MOCK=1)"}
	}
	if f.creds != nil {
		return f.sendV1(ctx, msg)
	}
	return f.sendLegacy(ctx, msg)
}

// ---------------------------------------------------------------------------
// Deterministic mock (FCM_MOCK=1 default)
// ---------------------------------------------------------------------------

// sendMock mirrors the deterministic mock idiom of booking-service's payout
// MockProvider: no network, deterministic results, documented test hooks:
//   - token "mock-fail"          → provider 500 error
//   - token "mock-unregistered"  → 404 (Unregistered → prune the token)
//   - anything else              → 200 with a deterministic message name
//     {"name":"projects/<project>/messages/mock-<sha256(token)[:16]>"}
func (f *FCM) sendMock(msg PushMessage) (int, []byte, error) {
	switch msg.Token {
	case "mock-fail":
		return 0, nil, &Error{StatusCode: http.StatusInternalServerError, Body: "fcm mock: send failed (token mock-fail)"}
	case "mock-unregistered":
		return 0, nil, &Error{StatusCode: http.StatusNotFound, Body: `{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`}
	}
	project := f.ProjectID
	if project == "" {
		project = "mock-project"
	}
	sum := sha256.Sum256([]byte(msg.Token))
	body, _ := json.Marshal(map[string]string{
		"name": fmt.Sprintf("projects/%s/messages/mock-%s", project, hex.EncodeToString(sum[:])[:16]),
	})
	return http.StatusOK, body, nil
}

// ---------------------------------------------------------------------------
// Legacy server-key API (FCM_SERVER_KEY)
// ---------------------------------------------------------------------------

// ASSUMPTION (see file header): legacy API shape per the pre-deprecation
// Google docs — POST {base}/fcm/send, Authorization: key=<server key>,
// {"to": token, "notification": {...}, "data": {...}, "priority": "high"}.
func (f *FCM) sendLegacy(ctx context.Context, msg PushMessage) (int, []byte, error) {
	return f.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		payload, err := json.Marshal(map[string]any{
			"to":           msg.Token,
			"notification": map[string]string{"title": msg.Title, "body": msg.Body},
			"data":         msg.Data,
			"priority":     "high",
		})
		if err != nil {
			return nil, fmt.Errorf("marshal fcm legacy payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.BaseURL+"/fcm/send", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "key="+f.ServerKey)
		return req, nil
	})
}

// ---------------------------------------------------------------------------
// HTTP v1 API (FCM_CREDENTIALS_JSON)
// ---------------------------------------------------------------------------

// ASSUMPTION (see file header): HTTP v1 shape per the Google docs —
// POST {base}/v1/projects/{project}/messages:send, Authorization: Bearer
// <oauth2 access token>, envelope {"message":{...}}.
func (f *FCM) sendV1(ctx context.Context, msg PushMessage) (int, []byte, error) {
	return f.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		token, err := f.tok.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("fcm oauth2 token: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"message": map[string]any{
				"token":        msg.Token,
				"notification": map[string]string{"title": msg.Title, "body": msg.Body},
				"data":         msg.Data,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal fcm v1 payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("%s/v1/projects/%s/messages:send", f.BaseURL, url.PathEscape(f.ProjectID)),
			bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		return req, nil
	})
}

// Unregistered reports whether an FCM failure means the device token is
// gone (app uninstalled / token expired) and should be pruned from
// booking-service's device_tokens: HTTP v1 signals 404 with errorCode
// UNREGISTERED; the legacy API signals 404 or a NotRegistered result error.
func Unregistered(status int, body []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	s := string(body)
	return strings.Contains(s, "UNREGISTERED") || strings.Contains(s, "NotRegistered")
}

// ---------------------------------------------------------------------------
// Service-account credentials + OAuth2 JWT-bearer token source
// ---------------------------------------------------------------------------

// serviceAccount is the subset of the Google service-account JSON key file
// the provider needs (FCM_CREDENTIALS_JSON).
type serviceAccount struct {
	ProjectID  string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey string `json:"private_key"` // PEM PKCS#8
	TokenURI   string `json:"token_uri"`   // default https://oauth2.googleapis.com/token

	key *rsa.PrivateKey
}

// defaultTokenURI is used when the credentials JSON omits token_uri
// (Google's key files always carry it; the default is a safety net).
const defaultTokenURI = "https://oauth2.googleapis.com/token"

func parseServiceAccount(raw []byte) (*serviceAccount, error) {
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if sa.ClientEmail == "" {
		return nil, fmt.Errorf("missing client_email")
	}
	if sa.PrivateKey == "" {
		return nil, fmt.Errorf("missing private_key")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("private_key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private_key: %w", err)
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private_key is %T, want *rsa.PrivateKey", k)
	}
	sa.key = rsaKey
	if sa.TokenURI == "" {
		sa.TokenURI = defaultTokenURI
	}
	return &sa, nil
}

// tokenSource mints and caches OAuth2 access tokens via the JWT-bearer
// grant (ASSUMPTION shape, see file header).
type tokenSource struct {
	hc *http.Client
	sa *serviceAccount

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newTokenSource(hc *http.Client, sa *serviceAccount) *tokenSource {
	return &tokenSource{hc: hc, sa: sa}
}

// Token returns a cached access token, refreshing 60s before expiry.
func (ts *tokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.token != "" && time.Now().Add(60*time.Second).Before(ts.expires) {
		return ts.token, nil
	}
	assertion, err := ts.sa.jwtBearer(time.Now())
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := ts.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("token endpoint: decode: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: status %d: %s %s", resp.StatusCode, out.Error, out.ErrorDesc)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint: empty access_token")
	}
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	ts.token = out.AccessToken
	ts.expires = time.Now().Add(time.Duration(expiresIn) * time.Second)
	return ts.token, nil
}

// jwtBearer builds the RS256-signed JWT assertion: iss=client_email,
// scope=fcm, aud=token_uri, iat=now, exp=now+1h.
func (sa *serviceAccount) jwtBearer(now time.Time) (string, error) {
	enc := func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
	header, err := enc(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := enc(map[string]any{
		"iss":   sa.ClientEmail,
		"scope": fcmScope,
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	unsigned := header + "." + claims
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(nil, sa.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
