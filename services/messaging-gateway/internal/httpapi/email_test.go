package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/provider"
)

type captureBridge struct {
	mu    sync.Mutex
	msgs  []channel.InboundMessage
	route []string
	done  chan struct{}
}

func (c *captureBridge) Handle(_ context.Context, m channel.InboundMessage, routeID string) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.route = append(c.route, routeID)
	c.mu.Unlock()
	c.done <- struct{}{}
	return nil
}

func (c *captureBridge) BindTelegramContact(string, string) {}

func TestLatestEmailTextDropsTheQuotedThread(t *testing.T) {
	in := "Thanks, Saturday works.\n\nOn Mon, 24 Nov 2026 at 10:00, Leadway <x@y.com> wrote:\n> Shall we book you?\n> Yes"
	if got := LatestEmailText(in); got != "Thanks, Saturday works." {
		t.Fatalf("got %q", got)
	}
	if got := LatestEmailText("> only quoted\nHello there"); got != "Hello there" {
		t.Fatalf("got %q", got)
	}
}

func TestReplyIsThreadedAndHeaderSafe(t *testing.T) {
	msg, id := provider.BuildReply("desk@biz.example", "Leadway Pensure", "ada@example.com",
		"RSA balance\r\nBcc: evil@x.com", "Line one\nLine two", "<abc@mail.example.com>", time.Unix(0, 0))
	s := string(msg)
	for _, want := range []string{"To: ada@example.com\r\n", "In-Reply-To: <abc@mail.example.com>\r\n",
		"References: <abc@mail.example.com>\r\n", "Line one\r\nLine two\r\n"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "\r\nBcc:") {
		t.Fatal("subject injected a header")
	}
	if !strings.HasSuffix(id, "@biz.example>") {
		t.Fatalf("message id %q", id)
	}
	if provider.ReplySubject("Re: hi") != "Re: hi" || provider.ReplySubject("") != "Re: your message" {
		t.Fatal("reply subject")
	}
}

func TestEmailWebhookAuthAndAccept(t *testing.T) {
	cb := &captureBridge{done: make(chan struct{}, 1)}
	srv := &Server{Log: zap.NewNop(), Bridge: cb}
	h := srv.Router()

	post := func(path, body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec.Code
	}
	if code := post("/webhooks/email/x", `{}`); code != http.StatusServiceUnavailable {
		t.Fatalf("unset secret: %d", code)
	}
	srv.EmailInboundSecret = "s3cret"
	h = srv.Router()
	if code := post("/webhooks/email/wrong", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", code)
	}
	if code := post("/webhooks/email/s3cret", `{"from":"not-an-address","to":"desk@biz.example","text":"hi"}`); code != http.StatusBadRequest {
		t.Fatalf("bad from: %d", code)
	}
	body := `{"from":"Ada Obi <Ada@Example.com>","to":"Desk@Biz.example","subject":"RSA balance","text":"How do I check it?","message_id":"<m1@example.com>"}`
	if code := post("/webhooks/email/s3cret", body); code != http.StatusAccepted {
		t.Fatalf("accept: %d", code)
	}
	select {
	case <-cb.done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge not called")
	}
	m := cb.msgs[0]
	if m.Channel != "email" || m.From != "ada@example.com" || m.To != "desk@biz.example" || cb.route[0] != "desk@biz.example" {
		t.Fatalf("message %+v route %v", m, cb.route)
	}
	if !strings.HasPrefix(m.Text, "Subject: RSA balance\n\nHow do I check it?") || m.MessageID != "<m1@example.com>" {
		t.Fatalf("text/id %q %q", m.Text, m.MessageID)
	}
}
