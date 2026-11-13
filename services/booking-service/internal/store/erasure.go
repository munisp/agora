package store

// Per-subject GDPR/NDPA erasure (SPEC-W47 W47-A item 6, A-DB DB-03):
// PrivacyEraseRequested used to tombstone ONLY the contacts row — the same
// phone number survived verbatim in leads, promo_redemptions, waitlist,
// referral_agents, referrals and civic_cases, making the erasure
// ineffective. AnonymizeSubject now hash-tombstones the subject's
// phone/name columns in EVERY subject-keyed booking-DB table inside ONE
// tenant transaction (the consumer privacy path calls it on
// PrivacyEraseRequested).

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SubjectEraseSummary reports what AnonymizeSubject changed.
type SubjectEraseSummary struct {
	// ContactsAnonymized is the historical return value of
	// AnonymizeContacts (kept for the privacy consumer's log line).
	ContactsAnonymized int64 `json:"contacts_anonymized"`
	// Details maps each subject-keyed table to the rows tombstoned.
	Details map[string]int64 `json:"details,omitempty"`
}

// subjectPiiStep is one per-subject tombstone statement. $1 = tenant id,
// $2 = cleartext phone, $3 = the phone's salted SHA-256 tombstone.
// Statements are idempotent (already-tombstoned rows are excluded).
type subjectPiiStep struct {
	table string
	sql   string
}

// subjectPiiSteps covers every subject-keyed (phone-bearing) booking-DB
// table beyond contacts (A-DB DB-03). Deterministic tombstones keep
// uniqueness invariants intact: equal phones hash equally, and two distinct
// phones never collide (SHA-256), so the promo_redemptions PK and the
// referral_agents/referrals phone unique indexes stay satisfied.
var subjectPiiSteps = []subjectPiiStep{
	{"leads",
		`UPDATE leads SET phone_e164=$3
		 WHERE tenant_id=$1 AND phone_e164=$2`},
	{"promo_redemptions",
		`UPDATE promo_redemptions SET phone_e164=$3
		 WHERE tenant_id=$1 AND phone_e164=$2`},
	{"waitlist",
		`UPDATE waitlist SET contact_name='erased', contact_phone=$3
		 WHERE tenant_id=$1 AND contact_phone=$2`},
	{"referral_agents",
		`UPDATE referral_agents SET name='erased', phone=$3, updated_at=now()
		 WHERE tenant_id=$1 AND phone=$2`},
	{"referrals",
		`UPDATE referrals SET referee_phone=$3
		 WHERE tenant_id=$1 AND referee_phone=$2`},
	{"civic_cases",
		`UPDATE civic_cases SET reporter_phone_e164=$3, reporter_name=NULL, updated_at=now()
		 WHERE tenant_id=$1 AND reporter_phone_e164=$2`},
	// SPEC-W61 (G1): media_inbox binds document_id ↔ sender phone, so the
	// subject tombstone must reach it. Keyed on the subject's phone DIGITS
	// (the pinned G3 normalization applied to $2 SQL-side, identical to the
	// ingest match): sender_phone gets the same salted 'sha256:' tombstone
	// as the sibling steps, sender_phone_digits is blanked, and the anchor
	// columns (document_id, booking_id, contact_id, linked_by, state) are
	// preserved for audit. The sender_phone_digits <> '' guard keeps the
	// step idempotent and prevents an all-non-digit needle from matching
	// already-tombstoned rows.
	{"media_inbox",
		`UPDATE media_inbox SET sender_phone=$3, sender_phone_digits=''
		 WHERE tenant_id=$1 AND sender_phone_digits <> ''
		   AND sender_phone_digits = regexp_replace(regexp_replace($2,'\D','','g'),'^00','')`},
}

// AnonymizeSubject applies the GDPR right-to-erasure tombstone for ONE data
// subject (phone and/or email) across every subject-keyed booking-DB table
// of the tenant in a single RLS-scoped transaction: contacts get the W3
// tombstone (name='erased', phone/email → salted SHA-256) and the tables in
// subjectPiiSteps get their phone/name columns hash-tombstoned or NULLed.
// Booking history, audit rows and financial records stay referentially
// intact. Idempotent: a repeated erase for the same subject matches nothing
// (the tombstoned values no longer equal the cleartext needles). Empty
// phone/email never match NULL/'' columns.
func (s *Store) AnonymizeSubject(ctx context.Context, tenantID uuid.UUID, phone, email string) (SubjectEraseSummary, error) {
	var sum SubjectEraseSummary
	if phone == "" && email == "" {
		return sum, nil
	}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		affected, err := anonymizeContactsTx(ctx, tx, tenantID, phone, email)
		if err != nil {
			return err
		}
		sum.ContactsAnonymized = affected
		if phone == "" {
			return nil
		}
		tombstone := hashPii(phone)
		for _, st := range subjectPiiSteps {
			exists, err := tableExists(ctx, tx, st.table)
			if err != nil {
				return fmt.Errorf("probe table %s: %w", st.table, err)
			}
			if !exists {
				continue // partial deployment
			}
			tag, err := tx.Exec(ctx, st.sql, tenantID, phone, tombstone)
			if err != nil {
				return fmt.Errorf("subject erase step %s: %w", st.table, err)
			}
			if sum.Details == nil {
				sum.Details = map[string]int64{}
			}
			sum.Details[st.table] = tag.RowsAffected()
		}
		return nil
	})
	return sum, err
}
