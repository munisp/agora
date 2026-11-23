// Package inbox bridges Agora's conversations and Chatwoot, the human agents' inbox (2026-11-23).
//
//   - Every chat message (web chat, WhatsApp, Telegram) is mirrored into a Chatwoot conversation: the customer's lines
//     as incoming messages, the receptionist's as outgoing messages from the "Agora receptionist" agent bot. While the
//     bot handles a conversation Chatwoot keeps it "pending".
//   - EscalationRequested (the customer asked for a person, on a chat or a call) opens the conversation for staff.
//   - A staff reply (Chatwoot webhook) is stored as a "staff" turn on the Agora conversation, sent to WhatsApp
//     customers through messaging-gateway (web chat customers fetch it through the widget), and marks the conversation
//     handed off: the receptionist stays quiet. Resolving it in Chatwoot hands it back to the receptionist.
//
// Calls (voice/phone conversations) are not mirrored line by line; an escalated call gets a conversation with a note.
package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
)

// Sync-map kinds owned by this package.
const (
	KindConversation = "inbox_conversation" // opendesk_id = stored conversation id, twenty_id = "a<account>:c<conv>"
	KindContact      = "inbox_contact"      // opendesk_id = phone or "conv:<id>", twenty_id = "a<account>:p<contact>"

	TypeConversationTurn = "com.opendesk.conversation.ConversationTurn"
	TypeEscalation       = "com.opendesk.conversation.EscalationRequested"
)

// Route sends one business's conversations to its Chatwoot account (INBOX_ROUTES).
type Route struct {
	Name      string   `json:"name"`
	Tenants   []string `json:"tenants"` // tenant uuid and/or slug
	URL       string   `json:"url"`
	AccountID int      `json:"account_id"`
	InboxID   int      `json:"inbox_id"`
	TokenEnv  string   `json:"token_env"`     // admin user's api_access_token
	BotEnv    string   `json:"bot_token_env"` // agent bot's access token (receptionist's lines)
	Token     string   `json:"-"`
	BotToken  string   `json:"-"`
}

// Bridge is the Agora <-> Chatwoot connector.
type Bridge struct {
	Routes          []Route
	Map             *syncmap.Store
	ConversationURL string // conversation-service base
	InternalToken   string // conversation-service X-Internal-Token (call-details endpoint)
	GatewayURL      string // messaging-gateway base (WhatsApp/Telegram sends)
	HC              *http.Client
	Log             *zap.Logger
}

func (b *Bridge) http() *http.Client {
	if b.HC == nil {
		b.HC = &http.Client{Timeout: 15 * time.Second}
	}
	return b.HC
}

func (b *Bridge) route(tenant string) *Route {
	for i := range b.Routes {
		for _, t := range b.Routes[i].Tenants {
			if strings.EqualFold(t, tenant) {
				return &b.Routes[i]
			}
		}
	}
	return nil
}

func (b *Bridge) routeForAccount(account int) *Route {
	for i := range b.Routes {
		if b.Routes[i].AccountID == account {
			return &b.Routes[i]
		}
	}
	return nil
}

func tenantPtr(t string) *uuid.UUID {
	if id, err := uuid.Parse(t); err == nil {
		return &id
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------------------
// Agora -> Chatwoot
// ---------------------------------------------------------------------------------------------------------------

// HandleTurn mirrors one ConversationTurn (opendesk.conversation.transcripts).
func (b *Bridge) HandleTurn(ctx context.Context, evt events.CloudEvent) error {
	if evt.Type != TypeConversationTurn {
		return nil
	}
	convID, _ := evt.Data["conversationId"].(string)
	tenant := firstNonEmpty(str(evt.Data["tenantId"]), evt.TenantID)
	role, _ := evt.Data["role"].(string)
	text := strings.TrimSpace(str(evt.Data["text"]))
	if convID == "" || text == "" {
		return nil
	}
	rt := b.route(tenant)
	if rt == nil {
		return nil
	}
	// The receptionist's request_human tool line (chats and calls both record it) opens the conversation for
	// staff. It always belongs to the right stored conversation, unlike the EscalationRequested event, which has
	// no stored id when the customer asks for a person in their very first message.
	if role == "tool" && strings.HasPrefix(text, "request_human(") {
		return b.openForStaff(ctx, rt, tenant, convID, escalationReason(text))
	}
	if role != "user" && role != "agent" {
		return nil // other tool/system lines stay in Agora; staff lines came FROM Chatwoot
	}
	cw, err := b.ensureConversation(ctx, rt, tenant, convID, false)
	if err != nil || cw == 0 {
		return err
	}
	if role == "user" {
		return b.post(ctx, rt, rt.Token, cw, text, "incoming", false)
	}
	return b.post(ctx, rt, rt.BotToken, cw, text, "outgoing", false)
}

// HandleEscalation opens the conversation for staff when the customer asked for a person.
func (b *Bridge) HandleEscalation(ctx context.Context, evt events.CloudEvent) error {
	tenant := firstNonEmpty(str(evt.Data["tenant_id"]), evt.TenantID)
	stored := str(evt.Data["stored_conversation_id"])
	rt := b.route(tenant)
	if rt == nil {
		return nil
	}
	if stored == "" {
		b.Log.Info("escalation without stored conversation id; not opened in the inbox",
			zap.String("conversation_id", str(evt.Data["conversation_id"])))
		return nil
	}
	return b.openForStaff(ctx, rt, tenant, stored, strings.ReplaceAll(firstNonEmpty(str(evt.Data["reason"]), "caller requested"), "_", " "))
}

// openForStaff opens the stored conversation's Chatwoot conversation with a private note for the staff member.
func (b *Bridge) openForStaff(ctx context.Context, rt *Route, tenant, stored, reason string) error {
	cw, err := b.ensureConversation(ctx, rt, tenant, stored, true)
	if err != nil || cw == 0 {
		return err
	}
	note := "The customer asked to speak to a person"
	if reason != "" {
		note += " (" + reason + ")"
	}
	note += ". Your reply goes straight to the customer, and the receptionist stays quiet from then on. " +
		"Resolve the conversation to hand it back to the receptionist."
	if err := b.post(ctx, rt, rt.BotToken, cw, note, "outgoing", true); err != nil {
		return err
	}
	b.Log.Info("inbox conversation opened for staff", zap.String("conversation_id", stored), zap.Int("inbox_conversation", cw))
	return b.status(ctx, rt, cw, "open")
}

// escalationReason pulls the reason out of a "request_human(reason: ...)" tool line.
func escalationReason(text string) string {
	r := strings.TrimSuffix(strings.TrimPrefix(text, "request_human("), ")")
	r = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r), "reason:"))
	return strings.TrimSuffix(r, ".")
}

type storedConversation struct {
	Channel      string `json:"channel"`
	ContactPhone string `json:"contact_phone"`
	ContactName  string `json:"contact_name"`
	CallDetails  struct {
		ContactName string `json:"contact_name"`
	} `json:"call_details"`
	Turns []struct {
		Role string `json:"role"`
		Text string `json:"text"`
	} `json:"turns"`
}

func isCall(channel string) bool { return channel == "voice" || channel == "phone" }

// ensureConversation returns the Chatwoot conversation mirroring the stored conversation, creating contact and
// conversation on first sight. Calls are only created when escalated (forCall); otherwise 0.
func (b *Bridge) ensureConversation(ctx context.Context, rt *Route, tenant, stored string, escalated bool) (int, error) {
	tp := tenantPtr(tenant)
	if m, err := b.Map.Get(ctx, KindConversation, stored, tp); err == nil && m.TwentyID != "" {
		_, conv := parseRef(m.TwentyID)
		return conv, nil
	} else if err != nil && !errors.Is(err, syncmap.ErrNotFound) {
		return 0, err
	}
	var sc storedConversation
	if err := b.conversationJSON(ctx, http.MethodGet, "/v1/conversations/"+url.PathEscape(stored), tenant, nil, &sc); err != nil {
		if isNotFound(err) {
			return 0, nil // deleted since (erasure, cleanup): nothing to mirror
		}
		return 0, fmt.Errorf("read stored conversation: %w", err)
	}
	if isCall(sc.Channel) && !escalated {
		return 0, nil
	}
	name := firstNonEmpty(sc.CallDetails.ContactName, sc.ContactName)
	contact, err := b.ensureContact(ctx, rt, tenant, stored, name, sc.ContactPhone)
	if err != nil {
		return 0, err
	}
	var ci struct {
		SourceID string `json:"source_id"`
	}
	if err := b.cw(ctx, rt, rt.Token, http.MethodPost, fmt.Sprintf("/contacts/%d/contact_inboxes", contact),
		map[string]any{"inbox_id": rt.InboxID, "source_id": stored}, &ci); err != nil {
		return 0, fmt.Errorf("contact inbox: %w", err)
	}
	status := "pending"
	if escalated {
		status = "open"
	}
	var conv struct {
		ID int `json:"id"`
	}
	body := map[string]any{
		"source_id": firstNonEmpty(ci.SourceID, stored), "inbox_id": rt.InboxID, "contact_id": contact, "status": status,
		"custom_attributes": map[string]any{"agora_conversation_id": stored, "agora_channel": sc.Channel},
	}
	if err := b.cw(ctx, rt, rt.Token, http.MethodPost, "/conversations", body, &conv); err != nil {
		return 0, fmt.Errorf("create conversation: %w", err)
	}
	if err := b.Map.Put(ctx, KindConversation, stored, ref(rt.AccountID, "c", conv.ID), tp); err != nil {
		return 0, err
	}
	if isCall(sc.Channel) {
		// An escalated call: staff need the gist, not a live chat. Its words so far, as one private note.
		var lines []string
		for _, t := range sc.Turns {
			who := map[string]string{"user": "Caller", "agent": "Receptionist"}[t.Role]
			if who != "" && strings.TrimSpace(t.Text) != "" {
				lines = append(lines, who+": "+strings.TrimSpace(t.Text))
			}
		}
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		note := "Phone call — the caller asked for a person. Call them back on " + firstNonEmpty(sc.ContactPhone, "the number in Agora") + "."
		if len(lines) > 0 {
			note += "\n\nThe call so far:\n" + strings.Join(lines, "\n")
		}
		_ = b.post(ctx, rt, rt.BotToken, conv.ID, note, "outgoing", true)
	}
	b.Log.Info("inbox conversation created", zap.String("conversation_id", stored), zap.Int("inbox_conversation", conv.ID))
	return conv.ID, nil
}

func (b *Bridge) ensureContact(ctx context.Context, rt *Route, tenant, stored, name, phone string) (int, error) {
	tp := tenantPtr(tenant)
	e164 := E164(phone)
	key := "conv:" + stored
	if e164 != "" {
		key = e164
	}
	if m, err := b.Map.Get(ctx, KindContact, key, tp); err == nil && m.TwentyID != "" {
		_, id := parseRef(m.TwentyID)
		if strings.TrimSpace(name) != "" {
			// A known number gave a name this time (web form): name the contact (best effort).
			_ = b.cw(ctx, rt, rt.Token, http.MethodPut, fmt.Sprintf("/contacts/%d", id), map[string]any{"name": name}, nil)
		}
		return id, nil
	}
	if name == "" {
		name = firstNonEmpty(e164, "Website visitor")
	}
	body := map[string]any{"name": name, "identifier": "agora:" + key}
	if e164 != "" {
		body["phone_number"] = e164
	}
	var created struct {
		Payload struct {
			Contact struct {
				ID int `json:"id"`
			} `json:"contact"`
		} `json:"payload"`
	}
	id := 0
	if err := b.cw(ctx, rt, rt.Token, http.MethodPost, "/contacts", body, &created); err == nil {
		id = created.Payload.Contact.ID
	} else {
		// Already there (phone/identifier taken): find it.
		var found struct {
			Payload []struct {
				ID int `json:"id"`
			} `json:"payload"`
		}
		q := firstNonEmpty(e164, "agora:"+key)
		if err2 := b.cw(ctx, rt, rt.Token, http.MethodGet, "/contacts/search?q="+url.QueryEscape(q), nil, &found); err2 != nil || len(found.Payload) == 0 {
			return 0, fmt.Errorf("create contact: %w", err)
		}
		id = found.Payload[0].ID
	}
	if err := b.Map.Put(ctx, KindContact, key, ref(rt.AccountID, "p", id), tp); err != nil {
		return 0, err
	}
	return id, nil
}

func (b *Bridge) post(ctx context.Context, rt *Route, token string, conv int, text, kind string, private bool) error {
	body := map[string]any{"content": text, "message_type": kind, "private": private}
	if err := b.cw(ctx, rt, token, http.MethodPost, fmt.Sprintf("/conversations/%d/messages", conv), body, nil); err != nil {
		return fmt.Errorf("post %s message: %w", kind, err)
	}
	return nil
}

func (b *Bridge) status(ctx context.Context, rt *Route, conv int, status string) error {
	return b.cw(ctx, rt, rt.Token, http.MethodPost, fmt.Sprintf("/conversations/%d/toggle_status", conv),
		map[string]any{"status": status}, nil)
}

// ---------------------------------------------------------------------------------------------------------------
// Chatwoot -> Agora (account webhook)
// ---------------------------------------------------------------------------------------------------------------

type webhook struct {
	Event       string `json:"event"`
	ID          int    `json:"id"`
	Content     string `json:"content"`
	MessageType any    `json:"message_type"` // "outgoing" (webhooks) or 1
	Private     bool   `json:"private"`
	Status      string `json:"status"`
	AccountID   int    `json:"account_id"`
	Account     struct {
		ID int `json:"id"`
	} `json:"account"`
	Sender struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"sender"`
	Conversation struct {
		ID int `json:"id"`
	} `json:"conversation"`
}

// ServeWebhook handles Chatwoot's account webhook (message_created, conversation_status_changed). Mounted on a path
// carrying the shared secret; Chatwoot calls it from inside the cluster.
func (b *Bridge) ServeWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	var h webhook
	if err := json.Unmarshal(raw, &h); err != nil {
		http.Error(w, "json", http.StatusBadRequest)
		return
	}
	if err := b.handleWebhook(r.Context(), h); err != nil {
		b.Log.Warn("inbox webhook failed", zap.String("event", h.Event), zap.Error(err))
		http.Error(w, "failed", http.StatusBadGateway) // Chatwoot does not retry; logged for follow-up
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (b *Bridge) handleWebhook(ctx context.Context, h webhook) error {
	account := h.AccountID
	if account == 0 {
		account = h.Account.ID
	}
	rt := b.routeForAccount(account)
	if rt == nil {
		return nil
	}
	switch h.Event {
	case "message_created":
		outgoing := fmt.Sprint(h.MessageType) == "outgoing" || fmt.Sprint(h.MessageType) == "1"
		if !outgoing || h.Private || h.Sender.Type != "user" || strings.TrimSpace(h.Content) == "" {
			return nil // customer lines, private notes and the bot's own mirrored lines
		}
		if fresh, err := b.Map.MarkWebhookSeen(ctx, fmt.Sprintf("chatwoot:%d:msg:%d", account, h.ID)); err != nil || !fresh {
			return err // already delivered (Chatwoot sent it twice)
		}
		return b.staffReply(ctx, rt, h.Conversation.ID, strings.TrimSpace(h.Content), firstNonEmpty(h.Sender.Name, "Staff"))
	case "conversation_status_changed":
		conv := h.ID
		if h.Status == "resolved" {
			return b.setHandoff(ctx, rt, conv, map[string]any{"active": false})
		}
	}
	return nil
}

func (b *Bridge) stored(ctx context.Context, rt *Route, conv int) (syncmap.Mapping, error) {
	return b.Map.GetByTwentyID(ctx, KindConversation, ref(rt.AccountID, "c", conv))
}

func (b *Bridge) staffReply(ctx context.Context, rt *Route, conv int, text, agent string) error {
	m, err := b.stored(ctx, rt, conv)
	if err != nil {
		if errors.Is(err, syncmap.ErrNotFound) {
			return nil // a conversation Agora does not know (created by hand in Chatwoot)
		}
		return err
	}
	tenant := ""
	if m.TenantID != nil {
		tenant = m.TenantID.String()
	}
	stored := m.OpenDeskID
	// 1) the reply joins the Agora transcript (the dashboard shows it; the web widget fetches it from there)
	if err := b.conversationJSON(ctx, http.MethodPost, "/v1/conversations/"+url.PathEscape(stored)+"/turns", tenant,
		map[string]any{"role": "staff", "text": text}, nil); err != nil {
		return fmt.Errorf("store staff turn: %w", err)
	}
	// 2) the receptionist steps back
	if err := b.setHandoff(ctx, rt, conv, map[string]any{"active": true, "agent": agent, "inbox_conversation_id": conv}); err != nil {
		return err
	}
	// 3) channels that can be pushed to: WhatsApp / Telegram through the messaging gateway
	var sc storedConversation
	if err := b.conversationJSON(ctx, http.MethodGet, "/v1/conversations/"+url.PathEscape(stored), tenant, nil, &sc); err != nil {
		return fmt.Errorf("read stored conversation: %w", err)
	}
	switch sc.Channel {
	case "whatsapp", "telegram":
		if b.GatewayURL == "" || sc.ContactPhone == "" {
			return fmt.Errorf("no gateway or contact for %s reply", sc.Channel)
		}
		body, _ := json.Marshal(map[string]string{"to": sc.ContactPhone, "message": text})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.GatewayURL, "/")+"/v1/"+sc.Channel+"/send", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := b.http().Do(req)
		if err != nil {
			return fmt.Errorf("send %s reply: %w", sc.Channel, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			return fmt.Errorf("send %s reply: %d %s", sc.Channel, resp.StatusCode, msg)
		}
	}
	b.Log.Info("staff reply delivered", zap.String("conversation_id", stored), zap.String("channel", sc.Channel))
	return nil
}

func (b *Bridge) setHandoff(ctx context.Context, rt *Route, conv int, handoff map[string]any) error {
	m, err := b.stored(ctx, rt, conv)
	if err != nil {
		if errors.Is(err, syncmap.ErrNotFound) {
			return nil
		}
		return err
	}
	if m.TenantID == nil {
		return nil
	}
	body := map[string]any{"tenant_id": m.TenantID.String(), "details": map[string]any{"handoff": handoff}}
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.ConversationURL, "/")+"/internal/conversations/"+url.PathEscape(m.OpenDeskID)+"/call-details", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", b.InternalToken)
	resp, err := b.http().Do(req)
	if err != nil {
		return fmt.Errorf("set handoff: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("set handoff: %d %s", resp.StatusCode, msg)
	}
	b.Log.Info("handoff updated", zap.String("conversation_id", m.OpenDeskID), zap.Any("handoff", handoff))
	return nil
}

// ---------------------------------------------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------------------------------------------

func (b *Bridge) cw(ctx context.Context, rt *Route, token, method, path string, body, out any) error {
	u := strings.TrimRight(rt.URL, "/") + fmt.Sprintf("/api/v1/accounts/%d", rt.AccountID) + path
	return b.doJSON(ctx, method, u, map[string]string{"api_access_token": token}, body, out)
}

func (b *Bridge) conversationJSON(ctx context.Context, method, path, tenant string, body, out any) error {
	return b.doJSON(ctx, method, strings.TrimRight(b.ConversationURL, "/")+path,
		map[string]string{"X-Tenant-ID": tenant, "X-Tenant-Slugs": tenant}, body, out)
}

func (b *Bridge) doJSON(ctx context.Context, method, u string, hdr map[string]string, body, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := b.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, u, resp.StatusCode, truncate(string(data), 300))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// E164 normalises Nigerian numbers to +234…; other "+" numbers pass through; anything else is "".
func E164(phone string) string {
	var d strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			d.WriteRune(r)
		}
	}
	s := d.String()
	switch {
	case strings.HasPrefix(s, "234") && len(s) == 13:
		return "+" + s
	case strings.HasPrefix(s, "0") && len(s) == 11:
		return "+234" + s[1:]
	case len(s) == 10 && (s[0] == '7' || s[0] == '8' || s[0] == '9'):
		return "+234" + s
	case strings.HasPrefix(strings.TrimSpace(phone), "+") && len(s) >= 8 && len(s) <= 15:
		return "+" + s
	}
	return ""
}

func isNotFound(err error) bool { return err != nil && strings.Contains(err.Error(), ": 404 ") }

func ref(account int, kind string, id int) string { return fmt.Sprintf("a%d:%s%d", account, kind, id) }

func parseRef(s string) (int, int) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 || len(parts[0]) < 2 || len(parts[1]) < 2 {
		return 0, 0
	}
	a, _ := strconv.Atoi(parts[0][1:])
	id, _ := strconv.Atoi(parts[1][1:])
	return a, id
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
