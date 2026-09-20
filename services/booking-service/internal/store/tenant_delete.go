package store

// Tenant deletion prep (SPEC-W45 CODER-A item 10; K9 TenantDeleted
// cascade): when identity publishes TenantDeleted, the booking consumer
// (CODER-E, internal/consumer) calls DeleteTenantData to anonymize the
// tenant's contacts and cancel its open bookings. The helper is idempotent
// (a redelivered event is a no-op) and runs inside the tenant's RLS
// transaction so it can never touch another tenant's rows.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TenantDeletionSummary reports what DeleteTenantData changed.
type TenantDeletionSummary struct {
	ContactsAnonymized int64 `json:"contacts_anonymized"`
	BookingsCancelled  int64 `json:"bookings_cancelled"`
}

// DeleteTenantData anonymizes ALL contacts of the tenant (name='erased',
// phone/email replaced by their salted SHA-256 tombstones — the W3
// AnonymizeContacts scheme) and cancels every OPEN booking
// (pending|confirmed → cancelled). Terminal bookings
// (cancelled|no_show|completed) and already-erased contacts are untouched,
// so the helper is idempotent. No outbox events are emitted: this is a
// cascade reaction to TenantDeleted, not an operator action — the deletion
// event itself is the audit anchor (K9).
func (s *Store) DeleteTenantData(ctx context.Context, tenantID uuid.UUID) (TenantDeletionSummary, error) {
	var sum TenantDeletionSummary
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE contacts
			 SET name='erased', notes='',
			     phone=CASE WHEN phone IS NOT NULL AND phone <> ''
			                THEN 'sha256:' || encode(digest('opendesk-gdpr-erase:' || phone, 'sha256'), 'hex')
			                ELSE phone END,
			     email=CASE WHEN email IS NOT NULL AND email <> ''
			                THEN 'sha256:' || encode(digest('opendesk-gdpr-erase:' || email, 'sha256'), 'hex')
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

// DeleteTenantDataBySlug resolves the tenant id from the site registry and
// runs DeleteTenantData — the K9 TenantDeleted payload carries tenant_slug;
// consumers that only hold the slug use this wrapper. Returns ErrNotFound
// when the slug never had a site row. Unlike GetSiteBySlug this resolves
// UNPUBLISHED sites too: a tenant being deleted must be purged regardless
// of its site's publish state.
//
// NOTE (RLS): the sites table has no RLS policy (public slug-resolution
// registry — see GetSiteBySlug), so the lookup runs outside withTenant;
// the purge itself is tenant-scoped.
func (s *Store) DeleteTenantDataBySlug(ctx context.Context, slug string) (TenantDeletionSummary, error) {
	var tenantID uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id FROM sites WHERE slug=$1 OR tenant_slug=$1
		 ORDER BY created_at LIMIT 1`, slug).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantDeletionSummary{}, ErrNotFound
	}
	if err != nil {
		return TenantDeletionSummary{}, err
	}
	return s.DeleteTenantData(ctx, tenantID)
}
