package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// SPEC-W21 Agent A: WhatsApp Cloud API provider — WHATSAPP_MOCK=1
// deterministic mock default (FCM_MOCK posture) + honest live posture.

func TestWhatsAppMockDefaultFromEnv(t *testing.T) {
	// No WHATSAPP_* env set: the zero-config posture is the mock.
	for _, k := range []string{"WHATSAPP_MOCK", "WHATSAPP_CLOUD_API_TOKEN", "WHATSAPP_PHONE_NUMBER_ID", "WHATSAPP_BUSINESS_ACCOUNT_ID", "WHATSAPP_CLOUD_API_BASE_URL"} {
		t.Setenv(k, "")
	}
	w := NewWhatsAppFromEnv(nil)
	require.True(t, w.Mock, "WHATSAPP_MOCK must default to the mock posture")
	require.True(t, w.Configured(), "mock mode counts as configured")
	require.Equal(t, DefaultWhatsAppBaseURL, w.BaseURL)
}

func TestWhatsAppMockSendsDeterministicWamid(t *testing.T) {
	w := NewWhatsApp(WhatsAppConfig{Mock: true}, nil)
	status, body, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{
		To: "+2348012345678", Template: "vote_reminder", Params: []string{"Ada", "Ward 3"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	id := WhatsAppMessageID(body)
	require.Contains(t, id, "wamid.mock-", "mock must return a fake wamid")

	// Deterministic: same input, same id; language default participates.
	_, body2, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{
		To: "+2348012345678", Template: "vote_reminder", Language: "en",
	})
	require.NoError(t, err)
	require.Equal(t, id, WhatsAppMessageID(body2), "empty language defaults to en (same deterministic id)")
}

func TestWhatsAppMockFailHook(t *testing.T) {
	w := NewWhatsApp(WhatsAppConfig{Mock: true}, nil)
	_, _, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{
		To: "mock-fail", Template: "t",
	})
	require.Error(t, err)
	pe, ok := err.(*Error)
	require.True(t, ok)
	require.Equal(t, http.StatusInternalServerError, pe.StatusCode)
}

func TestWhatsAppValidation(t *testing.T) {
	w := NewWhatsApp(WhatsAppConfig{Mock: true}, nil)
	_, _, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{Template: "t"})
	require.ErrorContains(t, err, "phone is required")
	_, _, err = w.SendTemplate(context.Background(), WhatsAppTemplateMessage{To: "+234"})
	require.ErrorContains(t, err, "template name is required")
	params := make([]string, MaxWhatsAppTemplateParams+1)
	_, _, err = w.SendTemplate(context.Background(), WhatsAppTemplateMessage{To: "+234", Template: "t", Params: params})
	require.ErrorContains(t, err, "at most 10")
}

// Live mode without credentials fails honestly — never a fake success.
func TestWhatsAppLiveUnconfiguredHonestFailure(t *testing.T) {
	w := NewWhatsApp(WhatsAppConfig{Mock: false}, nil)
	require.False(t, w.Configured())
	_, _, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{To: "+234", Template: "t"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "WHATSAPP_CLOUD_API_TOKEN")
	pe, ok := err.(*Error)
	require.True(t, ok)
	require.Equal(t, 0, pe.StatusCode, "local failure, not a provider response")
}

// The live payload matches the Meta Cloud API template shape (bearer auth,
// {base}/{phoneNumberID}/messages, language code, positional body params).
func TestWhatsAppLivePayloadShape(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp","messages":[{"id":"wamid.HBgM"}]}`))
	}))
	defer srv.Close()

	w := NewWhatsApp(WhatsAppConfig{
		Mock: false, Token: "tok-1", PhoneNumberID: "123456", BaseURL: srv.URL,
	}, nil)
	w.Client.sleep = func(context.Context, int) {} // no backoff in tests

	status, body, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{
		To: "+2348012345678", Template: "vote_reminder", Language: "en_US",
		Params: []string{"Ada", "Ward 3"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "wamid.HBgM", WhatsAppMessageID(body))

	require.Equal(t, "Bearer tok-1", gotAuth)
	require.Equal(t, "/123456/messages", gotPath)
	require.Equal(t, "whatsapp", gotBody["messaging_product"])
	require.Equal(t, "+2348012345678", gotBody["to"])
	require.Equal(t, "template", gotBody["type"])
	tpl, ok := gotBody["template"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "vote_reminder", tpl["name"])
	lang, ok := tpl["language"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "en_US", lang["code"])
	comps, ok := tpl["components"].([]any)
	require.True(t, ok, "params must render the body components block")
	require.Len(t, comps, 1)
	comp, ok := comps[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "body", comp["type"])
	params, ok := comp["parameters"].([]any)
	require.True(t, ok)
	require.Len(t, params, 2)
	p0, ok := params[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "text", p0["type"])
	require.Equal(t, "Ada", p0["text"])
}

// No params → the components block is omitted entirely (Meta rejects empty
// components arrays on some template types).
func TestWhatsAppLiveOmitsComponentsWithoutParams(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.X"}]}`))
	}))
	defer srv.Close()

	w := NewWhatsApp(WhatsAppConfig{Mock: false, Token: "t", PhoneNumberID: "1", BaseURL: srv.URL}, nil)
	_, _, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{To: "+234", Template: "plain"})
	require.NoError(t, err)
	tpl, ok := gotBody["template"].(map[string]any)
	require.True(t, ok)
	_, hasComps := tpl["components"]
	require.False(t, hasComps)
	require.Equal(t, "en", tpl["language"].(map[string]any)["code"], "language default en")
}

// A provider 4xx is a caller fault: no retry, provider body surfaced.
func TestWhatsAppLiveClientErrorNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"template name does not exist"}}`))
	}))
	defer srv.Close()

	w := NewWhatsApp(WhatsAppConfig{Mock: false, Token: "t", PhoneNumberID: "1", BaseURL: srv.URL}, nil)
	_, _, err := w.SendTemplate(context.Background(), WhatsAppTemplateMessage{To: "+234", Template: "nope"})
	require.Error(t, err)
	require.True(t, ClientError(err))
	require.Contains(t, err.Error(), "template name does not exist")
	require.Equal(t, 1, calls, "4xx must not be retried")
}
