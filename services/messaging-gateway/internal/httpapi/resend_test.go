package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/provider"
)

const testWhsec = "whsec_dGVzdC1zZWNyZXQtMTIzNDU2Nzg5MA=="

func svixHeaders(t *testing.T, body []byte) http.Header {
	t.Helper()
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(testWhsec, "whsec_"))
	id, ts := "msg_1", strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	h := http.Header{}
	h.Set("svix-id", id)
	h.Set("svix-timestamp", ts)
	h.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return h
}

func resendServer(t *testing.T) (*Server, *captureBridge, *httptest.Server) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer re_test" || r.URL.Path != "/emails/receiving/em_1" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"from": "Ada Obi <ada@example.com>", "subject": "RSA balance",
			"text": "How do I check it?\n\nOn Mon, someone wrote:\n> old", "message_id": "<m9@example.com>"})
	}))
	cb := &captureBridge{done: make(chan struct{}, 1)}
	s := &Server{Log: zap.NewNop(), Bridge: cb, ResendWebhookSecret: testWhsec, ResendAPIKey: "re_test", ResendBaseURL: api.URL,
		Sites: map[string]channel.Site{"email:leadway-pensure@mail-agora.upi.dev": {SiteSlug: "leadway-pensure", TenantID: "t"}}}
	return s, cb, api
}

func postResend(h http.Handler, body []byte, hdr http.Header) int {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/resend", strings.NewReader(string(body)))
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestResendWebhookTakesOnlyAgoraMail(t *testing.T) {
	s, cb, api := resendServer(t)
	defer api.Close()
	h := s.Router()

	idre := []byte(`{"type":"email.received","data":{"email_id":"em_0","to":["someone@mail-dev.upi.dev"]}}`)
	if code := postResend(h, idre, svixHeaders(t, idre)); code != http.StatusOK {
		t.Fatalf("idre mail must be acked and ignored: %d", code)
	}
	forged := []byte(`{"type":"email.received","data":{"email_id":"em_1","to":["leadway-pensure@mail-agora.upi.dev"]}}`)
	bad := svixHeaders(t, []byte("other"))
	if code := postResend(h, forged, bad); code != http.StatusUnauthorized {
		t.Fatalf("forged signature: %d", code)
	}
	ok := []byte(`{"type":"email.received","data":{"email_id":"em_1","to":["Leadway <Leadway-Pensure@mail-agora.upi.dev>"]}}`)
	if code := postResend(h, ok, svixHeaders(t, ok)); code != http.StatusAccepted {
		t.Fatalf("agora mail: %d", code)
	}
	select {
	case <-cb.done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge not called")
	}
	m := cb.msgs[0]
	if m.From != "ada@example.com" || m.FromName != "Ada Obi" || m.To != "leadway-pensure@mail-agora.upi.dev" ||
		m.Text != "Subject: RSA balance\n\nHow do I check it?" || m.MessageID != "<m9@example.com>" {
		t.Fatalf("message %+v", m)
	}
}

func TestResendSendReply(t *testing.T) {
	var got map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emails" || r.Header.Get("Authorization") != "Bearer re_test" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"id":"em_out"}`))
	}))
	defer api.Close()
	e := &provider.Email{ResendAPIKey: "re_test", ResendBaseURL: api.URL}
	id, err := e.SendReply(context.Background(), "leadway-pensure@mail-agora.upi.dev", "Leadway Pensure · Agora demo",
		"ada@example.com", "RSA balance", "Hello Ada", "<m9@example.com>")
	if err != nil || id != "em_out" {
		t.Fatalf("send: %v %q", err, id)
	}
	if got["from"] != "Leadway Pensure · Agora demo <leadway-pensure@mail-agora.upi.dev>" || got["subject"] != "Re: RSA balance" {
		t.Fatalf("payload %+v", got)
	}
	if hdr, _ := got["headers"].(map[string]any); hdr["In-Reply-To"] != "<m9@example.com>" {
		t.Fatalf("threading %+v", got["headers"])
	}
}
