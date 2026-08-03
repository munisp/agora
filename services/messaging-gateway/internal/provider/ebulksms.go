package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// EBulkSMS sends SMS via the eBulksSMS JSON API (SPEC-W12 Agent A).
//
// ASSUMPTION (no live keys in this wave): the request/response shape below
// follows the eBulksSMS developer docs as
//
//	POST {base}/sendsms
//	{"username","apikey","sender","messagetext","flash":0,"recipients"}
//
// with recipients as a single MSISDN string (their API also accepts a
// comma-separated list — the gateway sends exactly one recipient per call,
// like the other providers). Success/error is classified by HTTP status via
// the shared Client machinery; any provider-side JSON error envelope is
// surfaced verbatim in the response body. Verify against a live account
// before enabling in the failover chain.
type EBulkSMS struct {
	Client   *Client
	BaseURL  string // https://api.ebulksms.com
	APIKey   string // EBULK_API_KEY
	Username string // EBULK_USERNAME
	Sender   string // EBULK_SENDER default sender id (optional)
}

// Configured reports whether the provider has the credentials it needs.
func (e *EBulkSMS) Configured() bool { return e.APIKey != "" && e.Username != "" }

// SendSMS delivers one non-flash SMS. sender overrides the default sender id
// when given.
func (e *EBulkSMS) SendSMS(ctx context.Context, to, message, sender string) (int, []byte, error) {
	from := sender
	if from == "" {
		from = e.Sender
	}
	return e.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		payload, err := json.Marshal(map[string]any{
			"username":    e.Username,
			"apikey":      e.APIKey,
			"sender":      from,
			"messagetext": message,
			"flash":       0,
			"recipients":  to,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal ebulksms payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/sendsms", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
}
