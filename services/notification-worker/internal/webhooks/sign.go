// Package webhooks implements the outbound webhook dispatcher (Wave 5 #10):
// it consumes opendesk.booking.events + opendesk.conversation.events,
// matches per-tenant subscriptions and starts one durable
// WebhookDeliveryWorkflow per delivery (HMAC-signed POST with exponential
// backoff retries).
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Header names on every outbound delivery.
const (
	HeaderSignature = "X-OpenDesk-Signature" // sha256=<hex HMAC-SHA256(secret, body)> (plain hex for payload type "incident")
	HeaderEvent     = "X-OpenDesk-Event"     // CloudEvents type, e.g. com.opendesk.booking.BookingCreated
	HeaderTimestamp = "X-OpenDesk-Timestamp" // unix seconds of the attempt
	HeaderDelivery  = "X-OpenDesk-Delivery"  // delivery id (dedup key for receivers)
	// HeaderIncident carries the incident id on payload type "incident"
	// deliveries (SPEC-W11 Part B §4).
	HeaderIncident = "X-OpenDesk-Incident"
)

// SignatureHeader computes the X-OpenDesk-Signature value for a body.
// An empty secret yields an empty signature (unsigned delivery — allowed
// only when WEBHOOK_SIGNING_REQUIRED=false; see httpapi create handler).
func SignatureHeader(secret string, body []byte) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// SignatureHex computes the X-OpenDesk-Signature value for payload type
// "incident" deliveries (SPEC-W11 Part B §4): lowercase hex
// HMAC-SHA256(secret, body) WITHOUT the "sha256=" prefix. An empty secret
// yields an empty signature (unsigned delivery).
func SignatureHex(secret string, body []byte) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// EventMatches reports whether a subscription's event filter covers an
// event type. Supported filters: exact type ("com.opendesk.booking.BookingCreated"),
// prefix wildcard ("com.opendesk.booking.*") and the global wildcard "*".
func EventMatches(filter []string, eventType string) bool {
	for _, f := range filter {
		f = strings.TrimSpace(f)
		switch {
		case f == "*":
			return true
		case f == eventType:
			return true
		case strings.HasSuffix(f, ".*"):
			if strings.HasPrefix(eventType, strings.TrimSuffix(f, "*")) {
				return true
			}
		}
	}
	return false
}
