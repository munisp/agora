package store

// Tenant data deletion prep (SPEC-W45 CODER-A item 10 / K9): the
// tenant.deleted cascade consumer anonymizes booking PII and cancels open
// bookings BEFORE the row-level deletion proceeds — the booking rows are
// the tenant's financial/audit history, so they are pseudonymized, not
// wiped (mirrors AnonymizeContacts' GDPR tombstone posture).

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// TenantDeleteSummary reports the cascade prep counts.
type TenantDeleteSummary struct {
	ContactsAnonymized int64 `json:"contacts_anonymized"`
	BookingsCancelled  int64 `json:"bookings_cancelled"`
}

// DeleteTenantData anonymizes every contact of the tenant (same salted
// SHA-256 tombstone as the GDPR erasure path) and cancels its open
// (pending|confirmed) bookings. Idempotent: anonymized contacts carry
// name='erased' and are skipped on replay; cancelled bookings are terminal
// for this update. Completed/no_show/cancelled rows are untouched.
func (s *Store) DeleteTenantData(ctx context.Context, tenantID uuid.UUID) (TenantDeleteSummary, error) {
	var sum TenantDeleteSummary
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE contacts
			 SET name='erased', notes='',
			     phone=CASE WHEN phone IS NOT NULL AND phone <> ''
			                THEN 'sha256:' || encode(sha256(('opendesk-gdpr-erase:' || phone)::bytea), 'hex')
			                ELSE phone END,
			     email=CASE WHEN email IS NOT NULL AND email <> ''
			                THEN 'sha256:' || encode(sha256(('opendesk-gdpr-erase:' || email)::bytea), 'hex')
			                ELSE email END
			 WHERE tenant_id=$1 AND name <> 'erased'`, tenantID)
		if err != nil {
			return fmt.Errorf("anonymize tenant contacts: %w", err)
		}
		sum.ContactsAnonymized = tag.RowsAffected()

		tag, err = tx.Exec(ctx,
			`UPDATE bookings SET status='cancelled', updated_at=now()
			 WHERE tenant_id=$1 AND status IN ('pending','confirmed')`, tenantID)
		if err != nil {
			return fmt.Errorf("cancel open tenant bookings: %w", err)
		}
		sum.BookingsCancelled = tag.RowsAffected()
		return nil
	})
	return sum, err
}

// DeleteTenantDataBySlug resolves the tenant via the sites registry (the
// cascade carries the tenant SLUG; deleted tenants may no longer resolve
// through identity-service, and the sites row outlives them). Works on
// published AND unpublished sites (deletion must not depend on publish
// state).
func (s *Store) DeleteTenantDataBySlug(ctx context.Context, slug string) (TenantDeleteSummary, error) {
	var tenantID uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id FROM sites WHERE slug=$1 ORDER BY created_at LIMIT 1`, slug).Scan(&tenantID)
	if err != nil {
		return TenantDeleteSummary{}, ErrNotFound
	}
	return s.DeleteTenantData(ctx, tenantID)
}
