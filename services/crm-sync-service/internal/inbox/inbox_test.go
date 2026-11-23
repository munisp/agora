package inbox

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

func TestE164(t *testing.T) {
	cases := map[string]string{
		"08025554417":    "+2348025554417",
		"+2348025554417": "+2348025554417",
		"2348025554417":  "+2348025554417",
		"8025554417":     "+2348025554417",
		"+447700900123":  "+447700900123",
		"":               "",
		"12345":          "",
	}
	for in, want := range cases {
		if got := E164(in); got != want {
			t.Errorf("E164(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefRoundTrip(t *testing.T) {
	a, id := parseRef(ref(3, "c", 42))
	if a != 3 || id != 42 {
		t.Fatalf("parseRef(ref(3,c,42)) = %d,%d", a, id)
	}
	if a, id := parseRef("garbage"); a != 0 || id != 0 {
		t.Fatalf("parseRef(garbage) = %d,%d", a, id)
	}
}

// Only a staff member's public reply may leave Chatwoot: the bot's own mirrored lines, private notes and customer
// messages must be ignored before any lookup happens (Map is nil here: touching it would panic).
func TestWebhookIgnoresNonStaffMessages(t *testing.T) {
	b := &Bridge{Routes: []Route{{AccountID: 1}}, Log: zap.NewNop()}
	ignored := []webhook{
		{Event: "message_created", AccountID: 1, MessageType: "incoming", Content: "hi", Sender: sender("contact")},
		{Event: "message_created", AccountID: 1, MessageType: "outgoing", Content: "hello", Sender: sender("agent_bot")},
		{Event: "message_created", AccountID: 1, MessageType: "outgoing", Content: "note", Private: true, Sender: sender("user")},
		{Event: "message_created", AccountID: 1, MessageType: "outgoing", Content: "  ", Sender: sender("user")},
		{Event: "message_created", AccountID: 9, MessageType: "outgoing", Content: "other account", Sender: sender("user")},
		{Event: "conversation_status_changed", AccountID: 9, Status: "resolved"},
		{Event: "conversation_created", AccountID: 1},
	}
	for i, h := range ignored {
		if err := b.handleWebhook(context.Background(), h); err != nil {
			t.Errorf("case %d: unexpected error %v", i, err)
		}
	}
}

func sender(kind string) struct {
	Name string `json:"name"`
	Type string `json:"type"`
} {
	return struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}{Name: "x", Type: kind}
}

func TestRouteMatching(t *testing.T) {
	b := &Bridge{Routes: []Route{{Name: "lw", Tenants: []string{"bb69e5fd-aa86-4d4b-9d86-4cfe5c33ae52", "leadway-pensure"}, AccountID: 1}}}
	if b.route("LEADWAY-PENSURE") == nil || b.route("bb69e5fd-aa86-4d4b-9d86-4cfe5c33ae52") == nil {
		t.Fatal("route not matched")
	}
	if b.route("someone-else") != nil || b.routeForAccount(2) != nil {
		t.Fatal("unexpected route")
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(errors.New(`GET http://x/v1/conversations/1: 404 {"detail":"not found"}`)) || isNotFound(errors.New("GET x: 500 boom")) || isNotFound(nil) {
		t.Fatal("isNotFound misclassifies")
	}
}

func TestEscalationReason(t *testing.T) {
	if got := escalationReason("request_human(reason: Caller has a complicated RSA transfer question.)"); got != "Caller has a complicated RSA transfer question" {
		t.Fatalf("got %q", got)
	}
	if got := escalationReason("request_human()"); got != "" {
		t.Fatalf("got %q", got)
	}
}
