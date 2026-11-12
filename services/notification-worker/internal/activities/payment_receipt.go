package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// Payment receipt send (SPEC-W50-B): one customer-facing receipt after
// booking-service's BookingPaymentConfirmed. The payload is the sacred W50
// cross-scope contract (duplicated at the service boundary, not shared).
//
// Channel choice: a phone reaches the customer via the tenant's sms-channel
// binding (twilio / termii / africastalking / whatsapp per the
// ChannelRouter); without a phone the receipt falls back to the email
// (smtp) binding. Both contacts empty is a contract failure — the signals
// bridge already fail-closes such events, and the activity refuses them
// too (never a silent no-op on a payment receipt).
//
// The send reuses the exact low-level helpers of SendConfirmation /
// SendWaitlistClaimNotification (notify's binding dispatch + sender
// rotation) — no new providers, no new dependencies.

// SendPaymentReceipt delivers the payment receipt for one confirmed
// capture. Provider errors surface as activity errors so Temporal retries
// them (retryable); a missing payload (no reachable contact) is also an
// error — the bridge never starts the workflow for such events, so seeing
// one here means a malformed schedule.
func (a *Activities) SendPaymentReceipt(ctx context.Context, in workflows.PacedPaymentReceiptSend) error {
	if in.ContactPhone == "" && in.ContactEmail == "" {
		return fmt.Errorf("payment receipt %s: no reachable contact (contact_phone and contact_email both empty)", in.TxRef)
	}
	text := paymentReceiptMessage(in)

	sender := a.TwilioFrom
	if a.Pacer != nil {
		if n := a.Pacer.NextSender(ctx); n != "" {
			sender = n
		}
	}

	if in.ContactPhone != "" {
		provider := a.Channels.Provider(ChannelSMS, in.TenantSlug)
		if err := a.sendSMS(ctx, provider, in.ContactPhone, text, sender); err != nil {
			return fmt.Errorf("%s binding: %w", provider, err)
		}
		a.Log.Info("payment receipt sent", zap.String("channel", ChannelSMS),
			zap.String("provider", provider), zap.String("phone", in.ContactPhone),
			zap.String("tx_ref", in.TxRef), zap.String("sender_number", sender))
		return nil
	}

	provider := a.Channels.Provider(ChannelEmail, in.TenantSlug)
	if err := a.Dapr.InvokeBinding(ctx, a.BindingName(provider), "create", text, map[string]string{
		"emailTo":      in.ContactEmail,
		"emailFrom":    a.SMTPFrom,
		"subject":      "Payment received",
		"senderNumber": sender,
	}); err != nil {
		return fmt.Errorf("%s binding: %w", provider, err)
	}
	a.Log.Info("payment receipt sent", zap.String("channel", ChannelEmail),
		zap.String("provider", provider), zap.String("email", in.ContactEmail),
		zap.String("tx_ref", in.TxRef))
	return nil
}

// paymentReceiptMessage renders the receipt body:
//
//	"Payment received — NGN 5,000.00 for booking BK-1042. Ref: tx_abc. Thank you!"
//
// booking_ref (the human reference) is preferred; booking_id is the
// fallback when the producer could not resolve one.
func paymentReceiptMessage(in workflows.PacedPaymentReceiptSend) string {
	ref := in.BookingRef
	if ref == "" {
		ref = in.BookingID
	}
	return fmt.Sprintf("Payment received — %s for booking %s. Ref: %s. Thank you!",
		FormatMinorAmount(in.Currency, in.AmountMinor), ref, in.TxRef)
}

// FormatMinorAmount renders a minor-units amount as "<CCY> <major>" with
// thousands separators and two fraction digits, e.g. ("NGN", 500000) →
// "NGN 5,000.00", ("USD", 99) → "USD 0.99". Pure string math — no new
// dependencies.
func FormatMinorAmount(currency string, minor int64) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	major := minor / 100
	frac := minor % 100
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strings.ToUpper(currency))
	b.WriteByte(' ')
	// Major part with thousands separators.
	digits := fmt.Sprintf("%d", major)
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	fmt.Fprintf(&b, ".%02d", frac)
	return b.String()
}
