package recon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// Service runs the receipt ↔ payment reconciliation sweep (SPEC-W56 Scope A
// item 4): per tenant over the trailing LookbackDays window it (a) flags
// receipt-bearing captures with no payment candidate
// (receipt_without_payment), (b) flags paid attempts with no linked receipt
// (payment_without_receipt — ONLY for tenants with ≥1 receipt-bearing
// capture in the window, pinned decision 4: the sweep enumerates exactly
// those tenants, so the gate is data-driven, not a flag), and (c) flags
// matched pairs whose amounts diverge (amount_mismatch, paper-vs-rail).
// Every NEW finding emits one ReceiptReconFinding CloudEvent through the
// outbox in the SAME transaction (pinned decision 7).
type Service struct {
	Store *Store
	Log   *zap.Logger
	// EventsTopic is the outbox topic (BOOKING_EVENTS_TOPIC, default
	// opendesk.booking.events). Empty suppresses emission (findings still
	// persist) — mirrors the house "empty topic disables emission" idiom.
	EventsTopic string
	// LookbackDays is the trailing sweep window (RECON_LOOKBACK_DAYS, 30).
	LookbackDays int
	// MatchWindowDays is the ± contact-chain appointment window
	// (RECON_MATCH_WINDOW_DAYS, 7).
	MatchWindowDays int
	// AmountWindowDays is the ± amount+date rung window
	// (RECON_AMOUNT_WINDOW_DAYS, 3).
	AmountWindowDays int
	// ResolveSlug resolves the tenant slug for the CloudEvent subject
	// (notification-worker ChannelRouter routing key, V-INT-50 posture).
	// Nil-safe: unknown slug → empty subject, never a dropped finding.
	ResolveSlug func(ctx context.Context, tenantID uuid.UUID) string
}

func (s *Service) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

func (s *Service) lookback() time.Duration {
	if s.LookbackDays <= 0 {
		return 30 * 24 * time.Hour
	}
	return time.Duration(s.LookbackDays) * 24 * time.Hour
}

func (s *Service) matchWindowDays() int {
	if s.MatchWindowDays <= 0 {
		return 7
	}
	return s.MatchWindowDays
}

func (s *Service) amountWindowDays() int {
	if s.AmountWindowDays <= 0 {
		return 3
	}
	return s.AmountWindowDays
}

// Summary is the SweepOnce outcome, returned by the manual trigger
// endpoint and logged by the ticker.
type Summary struct {
	RunID          uuid.UUID `json:"run_id"`
	TenantsScanned int       `json:"tenants_scanned"`
	TenantsFailed  int       `json:"tenants_failed"`
	FindingsNew    int       `json:"findings_new"`
	FindingsDuped  int       `json:"findings_deduped"`
}

// RunTicker is the ticker driver (pinned decision 6): a sweep per period
// until ctx is cancelled. RECON_SWEEP_MINUTES=0 disables the ticker at the
// wiring site (main.go logs the disabled posture) — Run never starts.
func (s *Service) RunTicker(ctx context.Context, period time.Duration) {
	if period <= 0 {
		return
	}
	t := time.NewTicker(period)
	defer t.Stop()
	s.log().Info("receipt recon sweep ticker started", zap.Duration("period", period))
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sum, err := s.SweepOnce(ctx)
			if err != nil {
				s.log().Error("receipt recon sweep failed", zap.Error(err))
				continue
			}
			s.log().Info("receipt recon sweep complete",
				zap.String("run_id", sum.RunID.String()),
				zap.Int("tenants_scanned", sum.TenantsScanned),
				zap.Int("tenants_failed", sum.TenantsFailed),
				zap.Int("findings_new", sum.FindingsNew),
				zap.Int("findings_deduped", sum.FindingsDuped))
		}
	}
}

// SweepOnce runs one full sweep: enumerate the tenants with receipt-bearing
// captures in the window (pinned 4 gate), then sweep each tenant. A failed
// tenant is logged + counted and never aborts the run.
func (s *Service) SweepOnce(ctx context.Context) (Summary, error) {
	sum := Summary{RunID: uuid.New()}
	since := time.Now().UTC().Add(-s.lookback())
	tenants, err := s.Store.TenantsWithReceiptCaptures(ctx, since)
	if err != nil {
		return sum, err
	}
	for _, tenantID := range tenants {
		n, dup, err := s.sweepTenant(ctx, sum.RunID, tenantID, since)
		if err != nil {
			sum.TenantsFailed++
			s.log().Error("receipt recon tenant sweep failed",
				zap.String("tenant_id", tenantID.String()), zap.Error(err))
			continue
		}
		sum.TenantsScanned++
		sum.FindingsNew += n
		sum.FindingsDuped += dup
	}
	return sum, nil
}

// SweepOnceSince (SPEC-W66 WS1-B) is SweepOnce with an operator-overridden
// window and an optional tenant scope — the manual trigger's backfill
// surface. A zero since falls back to the configured trailing LookbackDays
// window; a non-nil tenantID restricts the sweep to that one tenant. The
// pinned-4 gate applies unchanged in every case: tenant enumeration is still
// "tenants with ≥1 receipt-bearing capture in the window", so a tenant_id
// naming a tenant with no captures in the window sweeps nothing
// (tenants_scanned=0 — payment_without_receipt stays data-gated, never
// operator-forced).
//
// Timeout behavior (pinned): when ctx expires (the manual trigger's
// triggerTimeout on a deep window) the loop stops BETWEEN tenants and
// returns the PARTIAL Summary — counts cover fully-swept tenants plus any
// findings the interrupted tenant committed before expiry (findings persist
// in per-finding transactions) — together with the context error; the
// trigger maps that to 202 + partial:true instead of a silent 504. A crash
// or timeout mid-tenant is resumable by simply re-calling: the dedupe index
// (uq_receipt_recon_findings_dedupe + ON CONFLICT DO NOTHING) is the
// checkpoint, and a resolved finding can never be reopened (the dedupe key
// ignores resolved_at and ResolveFinding guards on resolved_at IS NULL).
func (s *Service) SweepOnceSince(ctx context.Context, since time.Time, tenantID *uuid.UUID) (Summary, error) {
	sum := Summary{RunID: uuid.New()}
	if since.IsZero() {
		since = time.Now().UTC().Add(-s.lookback())
	}
	tenants, err := s.Store.TenantsWithReceiptCaptures(ctx, since)
	if err != nil {
		return sum, err
	}
	for _, id := range tenants {
		if tenantID != nil && id != *tenantID {
			continue
		}
		if err := ctx.Err(); err != nil {
			// Expired between tenants: stop here with partial progress.
			return sum, err
		}
		n, dup, err := s.sweepTenant(ctx, sum.RunID, id, since)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// Expired mid-tenant: the findings it committed are counted
				// (they persist), the tenant is NOT counted as swept.
				sum.FindingsNew += n
				sum.FindingsDuped += dup
				return sum, ctxErr
			}
			sum.TenantsFailed++
			s.log().Error("receipt recon tenant sweep failed",
				zap.String("tenant_id", id.String()), zap.Error(err))
			continue
		}
		sum.TenantsScanned++
		sum.FindingsNew += n
		sum.FindingsDuped += dup
	}
	return sum, nil
}

// sweepTenant runs all three finding passes for one tenant. Each finding is
// persisted (+ outbox) in its own transaction so one poison row never rolls
// back the tenant's other findings.
func (s *Service) sweepTenant(ctx context.Context, runID, tenantID uuid.UUID, since time.Time) (newCount, dupCount int, err error) {
	captures, err := s.loadCaptures(ctx, tenantID, since)
	if err != nil {
		return 0, 0, err
	}
	matchedBookings := map[uuid.UUID]bool{}
	for _, cap := range captures {
		outcome, rv, lerr := s.matchCapture(ctx, tenantID, cap)
		if lerr != nil {
			return newCount, dupCount, fmt.Errorf("match capture %s: %w", cap.ClientID, lerr)
		}
		f := s.findingForCapture(runID, tenantID, cap, outcome, rv)
		if f == nil {
			// Clean matched pair: record the booking as receipt-linked for
			// the payment_without_receipt pass.
			if outcome.Candidate != nil {
				matchedBookings[outcome.Candidate.BookingID] = true
			}
			continue
		}
		if outcome.Candidate != nil {
			matchedBookings[outcome.Candidate.BookingID] = true
		}
		// Ambiguous winning rung: the receipt plausibly belongs to one of
		// the candidates, so the bookings are receipt-linked (no
		// payment_without_receipt double-flag); the capture-side
		// receipt_without_payment finding already carries the candidate
		// list for ops.
		for _, c := range outcome.Ambiguous {
			matchedBookings[c.BookingID] = true
		}
		n, d, werr := s.persistFinding(ctx, f)
		if werr != nil {
			return newCount, dupCount, werr
		}
		newCount, dupCount = newCount+n, dupCount+d
	}

	// Pass (b): payments without receipts — the tenant is in the sweep set,
	// so it has ≥1 receipt-bearing capture in the window (pinned 4 gate is
	// satisfied by construction).
	attempts, err := s.loadPaidAttempts(ctx, tenantID, since)
	if err != nil {
		return newCount, dupCount, err
	}
	for _, a := range dedupeCandidates(attempts) {
		if matchedBookings[a.BookingID] {
			continue
		}
		f := findingForOrphanPayment(runID, tenantID, a, s.lookbackDays())
		n, d, werr := s.persistFinding(ctx, f)
		if werr != nil {
			return newCount, dupCount, werr
		}
		newCount, dupCount = newCount+n, dupCount+d
	}
	return newCount, dupCount, nil
}

func (s *Service) lookbackDays() int {
	if s.LookbackDays <= 0 {
		return 30
	}
	return s.LookbackDays
}

// loadCaptures / loadPaidAttempts are the tenant-scoped read transactions
// (withTenant → RLS-scoped) feeding the matcher.
func (s *Service) loadCaptures(ctx context.Context, tenantID uuid.UUID, since time.Time) ([]ReceiptCapture, error) {
	var out []ReceiptCapture
	err := s.Store.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = receiptCaptures(ctx, tx, tenantID, since)
		return err
	})
	return out, err
}

func (s *Service) loadPaidAttempts(ctx context.Context, tenantID uuid.UUID, since time.Time) ([]PaymentCandidate, error) {
	var out []PaymentCandidate
	err := s.Store.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = paidAttemptsInWindow(ctx, tx, tenantID, since)
		return err
	})
	return out, err
}

// matchCapture runs the §3.3 ladder for one receipt-bearing capture,
// prepended by SPEC-W57 rung 0 (P4): payload booking_id → direct booking
// link (payment_receipt captures only, NO time window — staleness is a
// finding, not a filter). A rung-0 hit is exact and short-circuits the
// classic ladder (it can never be ambiguous: one booking id dedupes to one
// candidate); a rung-0 booking with NO paid attempt still "hits" so
// findingForCapture can emit the linked_booking_unpaid signal. Then the
// classic ladder: rung 1 exact (checkin contact chain ONLY) → rung 2
// heuristic (lead phone chain) → rung 3 heuristic (amount+date, skipped
// entirely on a null receipt amount). The highest rung with ≥1 candidate
// decides (see decideLadder).
func (s *Service) matchCapture(ctx context.Context, tenantID uuid.UUID, cap ReceiptCapture) (LadderOutcome, receiptView, error) {
	rv := parseReceiptFields(cap.ReceiptFields)
	anchor := cap.anchorTime()
	var rung0 []PaymentCandidate
	rungs := make([][]PaymentCandidate, 3)
	err := s.Store.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Rung 0 — exact: payload booking_id direct link (payment_receipt
		// only). Returns the booking even with NO paid attempt.
		if bookingID, ok := captureBookingID(cap); ok {
			cands, err := candidatesByBookingID(ctx, tx, tenantID, bookingID)
			if err != nil {
				return fmt.Errorf("rung 0 (booking_id link): %w", err)
			}
			rung0 = dedupeCandidates(cands)
		}
		// Rung 1 — exact: checkin contact chain ONLY.
		if contactID, ok := captureContactID(cap); ok {
			cands, err := candidatesByContact(ctx, tx, tenantID, contactID, anchor, s.matchWindowDays())
			if err != nil {
				return fmt.Errorf("rung 1 (contact chain): %w", err)
			}
			rungs[0] = cands
		}
		// Rung 2 — heuristic: lead phone chain.
		if leadID, ok := captureLeadID(cap); ok {
			phone, err := leadPhone(ctx, tx, tenantID, leadID)
			if err != nil {
				return fmt.Errorf("rung 2 (lead phone): %w", err)
			}
			if phone != "" {
				contacts, err := contactIDsByPhone(ctx, tx, tenantID, phone)
				if err != nil {
					return fmt.Errorf("rung 2 (contacts by phone): %w", err)
				}
				for _, contactID := range contacts {
					cands, err := candidatesByContact(ctx, tx, tenantID, contactID, anchor, s.matchWindowDays())
					if err != nil {
						return fmt.Errorf("rung 2 (bookings by contact): %w", err)
					}
					rungs[1] = append(rungs[1], cands...)
				}
			}
		}
		// Rung 3 — heuristic: amount+date. Null receipt amount (or an
		// unparseable receipt date) skips the rung ENTIRELY (R5 — never
		// guess on null amounts).
		if amountMinor, ok := receiptAmountMinor(rv.AmountMajor); ok && rv.Date != nil {
			cands, err := candidatesByAmountDate(ctx, tx, tenantID, amountMinor, *rv.Date, s.amountWindowDays())
			if err != nil {
				return fmt.Errorf("rung 3 (amount+date): %w", err)
			}
			rungs[2] = cands
		}
		return nil
	})
	if err != nil {
		return LadderOutcome{}, rv, err
	}
	// Rung 0 decides first (P4): single candidate → ConfidenceExact with
	// WinningRung 0; >1 is defensive-only (a single booking id cannot
	// dedupe to >1 candidate) and degrades to an honest ambiguity.
	switch {
	case len(rung0) == 1:
		c := rung0[0]
		return LadderOutcome{Confidence: ConfidenceExact, Candidate: &c, WinningRung: 0}, rv, nil
	case len(rung0) > 1:
		return LadderOutcome{Confidence: ConfidenceNone, WinningRung: 0, Ambiguous: rung0}, rv, nil
	}
	return decideLadder(rungs), rv, nil
}

// findingForCapture maps a ladder outcome to a finding (nil when the pair
// is clean — matched, amounts verified equal). SPEC-W57 §3.5 (P7):
// currency_unknown:true is now CONDITIONAL — carried only when the receipt
// itself has no currency (pre-fold-in behavior); when receipt_fields
// carries a currency the detail says so honestly instead.
func (s *Service) findingForCapture(runID, tenantID uuid.UUID, cap ReceiptCapture, o LadderOutcome, rv receiptView) *Finding {
	base := Finding{
		TenantID:           tenantID,
		RunID:              runID,
		CaptureClientID:    &cap.ClientID,
		ReceiptAmountMajor: rv.AmountMajor,
		ReceiptCurrency:    rv.Currency, // "" when the receipt has none (P7)
	}
	detail := map[string]any{
		"currency_unknown": rv.Currency == "",
		"lookback_days":    s.lookbackDays(),
	}
	if rv.Currency != "" {
		detail["receipt_currency"] = rv.Currency
	}
	if rv.DateRaw != "" {
		detail["receipt_date"] = rv.DateRaw
	}
	if rv.DateRaw != "" && rv.Date == nil {
		detail["receipt_date_unparseable"] = true
	}

	switch {
	case o.Candidate == nil && o.WinningRung == 0:
		// No candidate at any rung → the receipt has no payment.
		base.Kind = KindReceiptWithoutPayment
		base.Confidence = ConfidenceNone
		detail["reason"] = "no_payment_candidate"
		base.Detail = mustJSON(detail)
		return &base
	case o.Candidate == nil:
		// Ambiguous winning rung → confidence 'none' + candidates in detail.
		base.Kind = KindReceiptWithoutPayment
		base.Confidence = ConfidenceNone
		detail["reason"] = "ambiguous_match"
		detail["winning_rung"] = o.WinningRung
		cands := make([]map[string]any, 0, len(o.Ambiguous))
		for _, c := range o.Ambiguous {
			cands = append(cands, map[string]any{"booking_id": c.BookingID.String(), "attempt_tx_ref": c.TxRef})
		}
		detail["candidates"] = cands
		base.Detail = mustJSON(detail)
		return &base
	}

	// Single winning candidate — verify the amount.
	c := o.Candidate
	base.BookingID = &c.BookingID
	base.Confidence = o.Confidence
	detail["winning_rung"] = o.WinningRung

	// SPEC-W57 P4: a rung-0 hit whose booking has NO paid attempt is the
	// cash-collection signal — receipt_without_payment at confidence exact
	// with the booking linked. No amount verification is possible (there is
	// no rail capture to compare against).
	if o.WinningRung == 0 && c.AttemptStatus == "" {
		base.Kind = KindReceiptWithoutPayment
		base.Confidence = ConfidenceExact
		detail["reason"] = "linked_booking_unpaid"
		base.Detail = mustJSON(detail)
		return &base
	}

	if c.TxRef != "" {
		base.AttemptTxRef = &c.TxRef
	}
	detail["attempt_status"] = c.AttemptStatus
	if c.Currency != "" {
		if rv.Currency == "" {
			// R2 honesty: the receipt has no currency; the comparison
			// ASSUMES the rail/offering currency and the detail says so.
			detail["assumed_currency"] = c.Currency
		} else {
			detail["rail_currency"] = c.Currency
		}
	}
	if c.OfferingPriceMinor != nil {
		detail["offering_price_minor"] = *c.OfferingPriceMinor
	}

	divergent, comparable := amountDivergence(rv.AmountMajor, c.AmountMinor)
	// SPEC-W57 §3.5 (P7): when BOTH currencies are present and DIFFER the
	// amounts are NOT comparable — NEVER convert (no FX rates exist in the
	// platform). The pair degrades to the not-comparable path with an
	// explicit currency_mismatch detail.
	currencyMismatch := rv.Currency != "" && c.Currency != "" && !strings.EqualFold(rv.Currency, c.Currency)
	if currencyMismatch {
		comparable = false
		divergent = false
		detail["currency_mismatch"] = true
	}
	// SPEC-W64 field-equality signal: every amount-verification finding
	// carries the honest comparison outcome so downstream consumers (and
	// the admin UI) never re-derive it. amount_comparable is the
	// post-currency-fold comparable; amounts_equal means the pair was
	// comparable AND agreed (a clean pair produces no finding, so
	// amounts_equal:true is only observable in theory — the booleans'
	// value is distinguishing not-comparable from divergent).
	detail["amount_comparable"] = comparable
	detail["amounts_equal"] = comparable && !divergent
	switch {
	case !comparable:
		// The amount cannot be verified. A standalone currency_unknown
		// finding is emitted ONLY when this blocks an otherwise-EXACT pair
		// (SPEC item 2 note); heuristic pairs just carry the flag in any
		// other finding's detail.
		if o.Confidence != ConfidenceExact {
			return nil
		}
		base.Kind = KindCurrencyUnknown
		if currencyMismatch {
			detail["reason"] = "currency_mismatch"
		} else {
			detail["reason"] = "receipt_amount_or_currency_unreadable"
		}
		if c.AmountMinor != nil {
			detail["rail_amount_minor"] = *c.AmountMinor
		}
		base.Detail = mustJSON(detail)
		return &base
	case divergent:
		// Paper-vs-rail divergence: expected = what the rail captured
		// (attempt.amount_minor); captured_amount_minor = the receipt's
		// claimed amount converted at this comparison site.
		receiptMinor, _ := receiptAmountMinor(rv.AmountMajor)
		base.Kind = KindAmountMismatch
		base.ExpectedAmountMinor = c.AmountMinor
		base.CapturedAmountMinor = &receiptMinor
		detail["reason"] = "paper_vs_rail_amount_divergence"
		base.Detail = mustJSON(detail)
		return &base
	default:
		return nil // clean pair — matched and amounts agree
	}
}

// findingForOrphanPayment builds a payment_without_receipt finding for a
// paid attempt whose booking no receipt matched. confidence is 'none'
// (there is no match to be confident about); severity stays OUT of the
// stored row — pinned decision 5's mapping (payment_without_receipt =
// info) drives the downstream ops alert via the event payload's kind.
func findingForOrphanPayment(runID, tenantID uuid.UUID, a PaymentCandidate, lookbackDays int) *Finding {
	detail := map[string]any{
		"currency_unknown": true,
		"reason":           "no_linked_receipt",
		"lookback_days":    lookbackDays,
		"attempt_status":   a.AttemptStatus,
		"paid_at":          a.PaidAt.UTC().Format(time.RFC3339),
	}
	if a.Currency != "" {
		detail["assumed_currency"] = a.Currency
	}
	if a.OfferingPriceMinor != nil {
		detail["offering_price_minor"] = *a.OfferingPriceMinor
	}
	return &Finding{
		TenantID:            tenantID,
		RunID:               runID,
		Kind:                KindPaymentWithoutReceipt,
		BookingID:           &a.BookingID,
		AttemptTxRef:        &a.TxRef,
		CapturedAmountMinor: a.AmountMinor, // what the rail captured (NULL pre-W56)
		ReceiptCurrency:     "",
		Confidence:          ConfidenceNone,
		Detail:              mustJSON(detail),
	}
}

// persistFinding writes the finding + (for NEW rows only) the outbox event
// in one transaction (pinned 7). The finding ID is allocated HERE so the
// outbox payload's finding_id matches the row that commits.
func (s *Service) persistFinding(ctx context.Context, f *Finding) (newCount, dupCount int, err error) {
	f.ID = uuid.New()
	var payload []byte
	if s.EventsTopic != "" {
		payload, err = s.findingEventPayload(ctx, f)
		if err != nil {
			return 0, 0, fmt.Errorf("marshal finding event: %w", err)
		}
	}
	inserted, err := s.Store.InsertFindingWithOutbox(ctx, f, s.EventsTopic, payload)
	if err != nil {
		return 0, 0, err
	}
	if inserted {
		s.log().Info("receipt recon finding",
			zap.String("finding_id", f.ID.String()),
			zap.String("tenant_id", f.TenantID.String()),
			zap.String("kind", f.Kind),
			zap.String("confidence", f.Confidence))
		return 1, 0, nil
	}
	return 0, 1, nil
}

// findingEventPayload builds the com.opendesk.booking.ReceiptReconFinding
// CloudEvent (SPEC item 5): {finding_id, tenant_id, kind, confidence,
// capture_client_id?, booking_id?, attempt_tx_ref?, expected_amount_minor?,
// captured_amount_minor?, receipt_amount_major?, detail}. The payload
// carries kind + confidence (the severity mapping of pinned decision 5 is
// applied downstream from kind by the notification-worker bridge — no
// severity is stored on the row). The payload is pre-marshalled BEFORE the
// insert (f.ID is caller-allocated); a deduped insert simply never writes
// its outbox row, so the event exists exactly once per finding.
func (s *Service) findingEventPayload(ctx context.Context, f *Finding) ([]byte, error) {
	subject := ""
	if s.ResolveSlug != nil {
		subject = s.ResolveSlug(ctx, f.TenantID)
	}
	data := map[string]any{
		"tenant_id":  f.TenantID.String(),
		"run_id":     f.RunID.String(), // SPEC-W64: the sweep run that raised the finding
		"kind":       f.Kind,
		"confidence": f.Confidence,
	}
	if f.CaptureClientID != nil {
		data["capture_client_id"] = *f.CaptureClientID
	}
	if f.BookingID != nil {
		data["booking_id"] = f.BookingID.String()
	}
	if f.AttemptTxRef != nil {
		data["attempt_tx_ref"] = *f.AttemptTxRef
	}
	if f.ExpectedAmountMinor != nil {
		data["expected_amount_minor"] = *f.ExpectedAmountMinor
	}
	if f.CapturedAmountMinor != nil {
		data["captured_amount_minor"] = *f.CapturedAmountMinor
	}
	if f.ReceiptAmountMajor != nil {
		data["receipt_amount_major"] = *f.ReceiptAmountMajor
	}
	var detail map[string]any
	if err := json.Unmarshal(f.Detail, &detail); err == nil {
		data["detail"] = detail
	} else {
		data["detail"] = map[string]any{"currency_unknown": true}
	}
	if f.ID != uuid.Nil {
		data["finding_id"] = f.ID.String()
	}
	return json.Marshal(events.New("booking-service", events.ReceiptReconFindingEventType, subject, f.TenantID.String(), data))
}

// ResolveFinding (SPEC-W64) marks one finding resolved and emits
// com.opendesk.booking.ReceiptReconResolved through the outbox in the SAME
// transaction (Store.ResolveFinding owns the tx). actor is the caller
// identity claim resolved at the HTTP boundary (payment_receipt P3
// server-resolved precedent — never client-supplied). A second resolve of
// the same finding (or an unknown id) returns ErrFindingNotFoundOrResolved
// with NO outbox row — replay-safe idempotence.
func (s *Service) ResolveFinding(ctx context.Context, tenantID, findingID uuid.UUID, actor string) (Finding, error) {
	return s.Store.ResolveFinding(ctx, tenantID, findingID, actor, s.EventsTopic,
		func(f Finding) ([]byte, error) {
			return s.resolvedEventPayload(ctx, f)
		})
}

// resolvedEventPayload builds the com.opendesk.booking.ReceiptReconResolved
// CloudEvent (SPEC-W64): {finding_id, tenant_id, run_id, kind, resolved_at,
// resolved_by}. resolved_at is the database-stamped value from the
// RETURNING clause (never a client clock).
func (s *Service) resolvedEventPayload(ctx context.Context, f Finding) ([]byte, error) {
	subject := ""
	if s.ResolveSlug != nil {
		subject = s.ResolveSlug(ctx, f.TenantID)
	}
	data := map[string]any{
		"finding_id": f.ID.String(),
		"tenant_id":  f.TenantID.String(),
		"run_id":     f.RunID.String(),
		"kind":       f.Kind,
	}
	if f.ResolvedAt != nil {
		data["resolved_at"] = f.ResolvedAt.UTC().Format(time.RFC3339Nano)
	}
	resolvedBy := ""
	if f.ResolvedBy != nil {
		resolvedBy = *f.ResolvedBy
	}
	data["resolved_by"] = resolvedBy
	return json.Marshal(events.New("booking-service", events.ReceiptReconResolvedEventType, subject, f.TenantID.String(), data))
}

// mustJSON marshals detail; a marshal failure degrades to the minimal
// honest object (never a panic in a sweep).
func mustJSON(v map[string]any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"currency_unknown":true}`)
	}
	return raw
}
