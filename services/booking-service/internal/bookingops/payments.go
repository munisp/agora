package bookingops

// Payments rail client (SPEC-W45 K11/K12): the booking money loop calls
// payments-service for deposit capture (booking completion) and refunds
// (booking refund). Fail-CLOSED posture: when PAYMENTS_URL is not wired the
// client is nil at the Deps level and money-moving endpoints answer 503
// with an explicit config signal — never a silent skip.
//
// Auth: payments-service accepts a valid X-Internal-Token as full service
// authentication (K2; PAYMENTS_INTERNAL_TOKEN on both sides). The caller's
// gateway identity (X-User-Id / X-User-Roles) is ALSO propagated so the K6
// money-role gate and the K7 deposit-provenance record see the real
// operator when no internal token is configured.
//
// Contract (CODER-K owns payments-service):
//   - POST {base}/v1/deposits/{id}/capture  body {tenant_id, amount_cents?}
//   - POST {base}/v1/refunds                body {tenant_id, deposit_id?,
//     amount_cents, reason?, idempotency_key} (idempotency_key REQUIRED)
//   - POST {base}/v1/transfers              (lending disbursement bridge —
//     consumed by internal/consumer's HTTPRail; wired in cmd/server/main.go)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ErrPaymentsNotConfigured marks money-loop calls attempted without a wired
// payments rail (PAYMENTS_URL unset). httpapi maps it to 503 with the
// config signal in the body.
var ErrPaymentsNotConfigured = errors.New("payments rail not configured: set PAYMENTS_URL (and PAYMENTS_INTERNAL_TOKEN) on booking-service")

// CaptureResult mirrors the payments capture response (fields booking
// surfaces; duplicated per service-boundary rules).
type CaptureResult struct {
	DepositID string `json:"deposit_id"`
	Captured  bool   `json:"captured"`
}

// RefundResult mirrors the payments refund response (SPEC-W45 K12
// RefundResponse): the ledger refund id/amount PLUS the honest rail outcome.
// Status is payments' truth — "refunded" (provider accepted, or a voided
// pending hold) | "queued_manual" (the ledger refund committed but the
// provider refund must be executed manually; RailDetail carries the reason).
// Booking NEVER invents a status: an absent field stays empty rather than
// being masked as "posted".
type RefundResult struct {
	RefundID    string `json:"refund_id"`
	AmountCents int64  `json:"amount_cents"`
	Status      string `json:"status,omitempty"`
	// Rail is the provider rail attempted ("flutterwave") or "none".
	Rail string `json:"rail,omitempty"`
	// RailDetail explains the rail outcome (e.g. why a refund queued for
	// manual execution); empty when payments omitted it.
	RailDetail string `json:"rail_detail,omitempty"`
}

// PaymentsClient is the booking-side client of the payments money routes.
type PaymentsClient struct {
	baseURL string
	token   string
	hc      *http.Client
	log     *zap.Logger
}

// NewPaymentsClient builds the client for baseURL (PAYMENTS_URL, trailing
// slash trimmed); token is PAYMENTS_INTERNAL_TOKEN (may be empty — the
// caller-identity headers then carry the money-role proof).
func NewPaymentsClient(baseURL, token string, log *zap.Logger) *PaymentsClient {
	if log == nil {
		log = zap.NewNop()
	}
	return &PaymentsClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      &http.Client{Timeout: 15 * time.Second},
		log:     log,
	}
}

// CallerIdentity carries the gateway-resolved operator identity forwarded
// to payments for the K6 money-role gate + K7 provenance.
type CallerIdentity struct {
	UserID string   // JWT sub (X-User-Id)
	Roles  []string // realm roles (X-User-Roles csv)
}

// do issues one JSON POST against the payments rail with the auth headers.
func (c *PaymentsClient) do(ctx context.Context, path string, payload any, caller CallerIdentity, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Internal-Token", c.token)
	}
	if caller.UserID != "" {
		req.Header.Set("X-User-Id", caller.UserID)
	}
	if len(caller.Roles) > 0 {
		req.Header.Set("X-User-Roles", strings.Join(caller.Roles, ","))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("payments %s: %w", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("payments %s: unreadable response: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		return &PaymentsError{Status: resp.StatusCode, Path: path, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("payments %s: undecodable response: %w", path, err)
		}
	}
	return nil
}

// PaymentsError is a non-2xx answer from the payments rail.
type PaymentsError struct {
	Status int
	Path   string
	Body   string
}

func (e *PaymentsError) Error() string {
	return fmt.Sprintf("payments %s answered %d: %s", e.Path, e.Status, e.Body)
}

// CaptureDeposit invokes POST /v1/deposits/{id}/capture (K11). Payments
// derives the capture transfer id deterministically from the deposit id, so
// retries are idempotent by construction; the idempotencyKey argument
// ("complete-{booking_id}") is carried for provenance/logging.
func (c *PaymentsClient) CaptureDeposit(ctx context.Context, tenantID string, depositID uuid.UUID, idempotencyKey string, caller CallerIdentity) (CaptureResult, error) {
	var raw struct {
		DepositID string `json:"deposit_id"`
		Result    struct {
			Captured *string `json:"captured"`
		} `json:"result"`
	}
	err := c.do(ctx, "/v1/deposits/"+depositID.String()+"/capture",
		map[string]any{"tenant_id": tenantID}, caller, &raw)
	if err != nil {
		return CaptureResult{}, err
	}
	c.log.Info("deposit captured via payments rail",
		zap.String("deposit_id", raw.DepositID), zap.String("idempotency_key", idempotencyKey))
	return CaptureResult{DepositID: raw.DepositID, Captured: true}, nil
}

// Refund invokes POST /v1/refunds (K12). idempotency_key is REQUIRED by
// payments (400 when absent) — callers pass "refund-{booking_id}" or their
// own key; the rail execution (Flutterwave attempt vs queued_manual honest
// fallback) is payments-side (CODER-K).
func (c *PaymentsClient) Refund(ctx context.Context, tenantID string, depositID *uuid.UUID, amountCents int64, reason, idempotencyKey string, caller CallerIdentity) (RefundResult, error) {
	payload := map[string]any{
		"tenant_id":       tenantID,
		"amount_cents":    amountCents,
		"idempotency_key": idempotencyKey,
	}
	if depositID != nil {
		payload["deposit_id"] = depositID.String()
	}
	if strings.TrimSpace(reason) != "" {
		payload["reason"] = strings.TrimSpace(reason)
	}
	// Payments' RefundResponse carries id/refund_id, amount/amount_cents
	// and (K12) the honest rail outcome status/rail/rail_detail — decode
	// them all so a queued_manual refund is NEVER reported as posted.
	var raw struct {
		ID          string `json:"id"`
		RefundID    string `json:"refund_id"`
		Amount      int64  `json:"amount"`
		AmountCents int64  `json:"amount_cents"`
		Status      string `json:"status"`
		Rail        string `json:"rail"`
		RailDetail  string `json:"rail_detail"`
	}
	if err := c.do(ctx, "/v1/refunds", payload, caller, &raw); err != nil {
		return RefundResult{}, err
	}
	res := RefundResult{
		RefundID:    raw.ID,
		AmountCents: raw.Amount,
		Status:      raw.Status,
		Rail:        raw.Rail,
		RailDetail:  raw.RailDetail,
	}
	if res.RefundID == "" {
		res.RefundID = raw.RefundID
	}
	if res.AmountCents == 0 {
		res.AmountCents = raw.AmountCents
	}
	c.log.Info("refund posted via payments rail",
		zap.String("refund_id", res.RefundID), zap.Int64("amount_cents", res.AmountCents),
		zap.String("status", res.Status), zap.String("rail", res.Rail),
		zap.String("idempotency_key", idempotencyKey))
	return res, nil
}
