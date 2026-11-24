// Package callsummary writes the plain-language part of a call's CRM note: it
// reads the call's transcript from conversation-service and asks an
// OpenAI-compatible model (the platform's local LLM) for two or three
// sentences a business owner can read at a glance — what the caller wanted,
// what the receptionist did, and any booking made.
//
// Best-effort by design: any failure returns "" and the note falls back to the
// structured facts only (length, phone, booked, handed over).
package callsummary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Summarizer is safe for concurrent use. An empty ConversationURL or LLMURL
// disables it (Summarize returns "").
type Summarizer struct {
	ConversationURL string // e.g. http://conversation:7007
	LLMURL          string // OpenAI-compatible base, e.g. http://ollama:11434/v1
	Model           string
	HC              *http.Client
	Settle          time.Duration // wait before reading the transcript (default 3s; tests set 1ns)
}

func (s *Summarizer) settle() time.Duration {
	if s.Settle > 0 {
		return s.Settle
	}
	return 3 * time.Second
}

// New builds a Summarizer with the given per-request timeout.
func New(conversationURL, llmURL, model string, timeout time.Duration) *Summarizer {
	return &Summarizer{ConversationURL: strings.TrimRight(conversationURL, "/"), LLMURL: strings.TrimRight(llmURL, "/"),
		Model: model, HC: &http.Client{Timeout: timeout}}
}

const systemPrompt = "You write the call summary a business owner reads in their CRM after the AI receptionist " +
	"handled a call. Write two or three short sentences of plain English, in the third person (\"The caller ...\"). " +
	"Say what the caller wanted, what the receptionist did, and any appointment booked — the service, the day, the " +
	"date and the time exactly as said in the call. If the call was not in English, say which language it was in " +
	"and summarise in English. Never repeat a BVN, NIN, PIN, OTP or card number. No headings, no bullet points, " +
	"no preamble."

// 2026-11-24: web chat / WhatsApp / Telegram conversations reach the CRM too; their summary speaks of a chat.
const chatSystemPrompt = "You write the chat summary a business owner reads in their CRM after the AI receptionist " +
	"handled a text conversation (web chat or WhatsApp). Write two or three short sentences of plain English, in the " +
	"third person (\"The customer ...\"). Say what the customer wanted, what the receptionist did, and any " +
	"appointment booked — the service, the day, the date and the time exactly as written in the chat. If the chat " +
	"was not in English, say which language it was in and summarise in English. Never repeat a BVN, NIN, PIN, OTP " +
	"or card number. No headings, no bullet points, no preamble."

type textChatKey struct{}

// WithTextChat marks the summary as one of a text chat rather than a call.
func WithTextChat(ctx context.Context) context.Context {
	return context.WithValue(ctx, textChatKey{}, true)
}

func isTextChat(ctx context.Context) bool { v, _ := ctx.Value(textChatKey{}).(bool); return v }

type turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// SaveSummary stores the summary on the conversation (conversation-service internal route, 2026-11-23) so the
// dashboard shows the same summary as the CRM note. Token: CONVERSATION_INTERNAL_TOKEN.
func (s *Summarizer) SaveSummary(ctx context.Context, conversationID, tenantID, summary string) error {
	if s == nil || s.ConversationURL == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"tenant_id": tenantID, "details": map[string]string{"summary": summary}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.ConversationURL+"/internal/conversations/"+url.PathEscape(conversationID)+"/call-details", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", os.Getenv("CONVERSATION_INTERNAL_TOKEN"))
	resp, err := s.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("save summary: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Summarize returns the summary sentences for a conversation, or "".
func (s *Summarizer) Summarize(ctx context.Context, conversationID, tenantID string) string {
	if s == nil || s.ConversationURL == "" || s.LLMURL == "" || conversationID == "" || tenantID == "" {
		return ""
	}
	// The call's last lines are stored a moment after it ends: give them time to land.
	select {
	case <-time.After(s.settle()):
	case <-ctx.Done():
		return ""
	}
	turns, err := s.turns(ctx, conversationID, tenantID)
	if err != nil || len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range turns {
		who := "Caller"
		if isTextChat(ctx) {
			who = "Customer"
		}
		if t.Role == "agent" || t.Role == "assistant" {
			who = "Receptionist"
		} else if t.Role != "user" {
			continue
		}
		if txt := strings.TrimSpace(t.Text); txt != "" {
			fmt.Fprintf(&b, "%s: %s\n", who, txt)
		}
	}
	transcript := b.String()
	if transcript == "" {
		return ""
	}
	if len(transcript) > 12000 {
		transcript = transcript[len(transcript)-12000:]
	}
	prompt := systemPrompt
	if isTextChat(ctx) {
		prompt = chatSystemPrompt
	}
	out, err := s.complete(ctx, prompt, transcript)
	if err != nil {
		return ""
	}
	return Clean(out)
}

func (s *Summarizer) turns(ctx context.Context, conversationID, tenantID string) ([]turn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.ConversationURL+"/v1/conversations/"+url.PathEscape(conversationID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Slugs", tenantID)
	resp, err := s.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("conversation %s: status %d", conversationID, resp.StatusCode)
	}
	var body struct {
		Turns []turn `json:"turns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Turns, nil
}

func (s *Summarizer) complete(ctx context.Context, prompt, transcript string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"model": s.Model,
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
			{"role": "user", "content": transcript},
		},
		"temperature":      0.2,
		"reasoning_effort": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.LLMURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ollama")
	resp, err := s.HC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: no choices")
	}
	return out.Choices[0].Message.Content, nil
}

var (
	longDigits = regexp.MustCompile(`\b\d{10,11}\b`) // a BVN/NIN-length number the model repeated anyway
	spaces     = regexp.MustCompile(`\s+`)
)

// Clean trims the model's reply to plain sentences: no markdown emphasis or
// headings, one paragraph, at most 600 characters, and no 10-11 digit numbers
// except a phone number starting with 0 (BVN/NIN are 11 digits too, so only
// numbers that look like Nigerian mobiles survive).
func Clean(s string) string {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("**", "", "__", "", "#", "").Replace(s)
	s = longDigits.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) == 11 && strings.HasPrefix(m, "0") && (m[1] == '7' || m[1] == '8' || m[1] == '9') {
			return m
		}
		return "[number removed]"
	})
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if len(s) > 600 {
		cut := strings.LastIndex(s[:600], ". ")
		if cut < 200 {
			cut = 597
		}
		s = strings.TrimSpace(s[:cut+1])
	}
	return s
}
