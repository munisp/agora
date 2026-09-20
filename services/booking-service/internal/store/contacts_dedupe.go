package store

// Contact dedupe (SPEC-W45 CODER-A item 5, STK O8): contacts are unique per
// (tenant_id, phone) — the same person must not accumulate parallel contact
// rows across booking channels. This file carries:
//
//  1. ensureContactDedupe — the idempotent bootstrap migration for EXISTING
//     databases: duplicate (tenant_id, phone) rows are folded into one
//     keeper row (MIN(id::text)::uuid — Postgres has no MIN(uuid) aggregate;
//     contacts have no created_at column, so the deterministic survivor is
//     the smallest uuid by text sort cast back to uuid; dependent FK/soft-FK
//     references are re-pointed first), the losers are deleted, and the
//     partial UNIQUE index uq_contacts_tenant_phone is created (phone IS
//     NOT NULL — phone-less rows are exempt). Fresh installs get the index
//     directly from 01-booking-schema.sql.
//  2. UpsertBookingContact — the match-or-create used by the booking write
//     path (bookingops.Create), mirroring UpsertExternalContact: match on
//     phone (then email), merge non-empty inbound fields, create otherwise;
//     a lost unique race re-reads the winner.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ensureContactDedupe folds duplicate (tenant_id, phone) contact rows and
// creates the dedupe UNIQUE index. Idempotent; a no-op when the contacts
// table does not exist yet.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant (see ensureSitesTable).
func (s *Store) ensureContactDedupe(ctx context.Context) error {
	const ddl = `
DO $$
BEGIN
    IF to_regclass('public.contacts') IS NOT NULL THEN
        -- Re-point hard FK references (bookings.contact_id) from duplicate
        -- rows to the keeper (MIN(id::text)::uuid per tenant+phone —
        -- Postgres has no MIN(uuid) aggregate).
        IF to_regclass('public.bookings') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, phone))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND phone <> ''
            )
            UPDATE bookings b SET contact_id = r.keeper
            FROM ranked r
            WHERE b.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        -- Re-point soft references held by the lending app (no FK, but the
        -- linkage must survive the fold).
        IF to_regclass('public.loan_applications') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, phone))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND phone <> ''
            )
            UPDATE loan_applications a SET contact_id = r.keeper
            FROM ranked r
            WHERE a.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        IF to_regclass('public.loan_accounts') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, phone))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND phone <> ''
            )
            UPDATE loan_accounts a SET contact_id = r.keeper
            FROM ranked r
            WHERE a.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        -- Drop the losers (their references were re-pointed above).
        DELETE FROM contacts c
        WHERE c.phone IS NOT NULL AND c.phone <> '' AND c.id <> (
            SELECT MIN(id::text)::uuid FROM contacts k
            WHERE k.tenant_id = c.tenant_id AND k.phone = c.phone
        );
        CREATE UNIQUE INDEX IF NOT EXISTS uq_contacts_tenant_phone
            ON contacts (tenant_id, phone) WHERE phone IS NOT NULL AND phone <> '';
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure contacts dedupe: %w", err)
	}
	return nil
}

// UpsertBookingContact applies the booking write path's inline contact
// (SPEC-W45): when a contact with the same phone (preferred) or e-mail
// already exists in the tenant it is merged and updated (created=false);
// otherwise a new contact is created (created=true). A lost race against
// the uq_contacts_tenant_phone unique index re-reads the winner instead of
// failing the booking.
func (s *Store) UpsertBookingContact(ctx context.Context, tenantID uuid.UUID, in *Contact) (bool, error) {
	in.TenantID = tenantID
	existing, err := s.FindContactByPhoneOrEmail(ctx, tenantID, in.Phone, in.Email)
	if errors.Is(err, ErrNotFound) {
		if in.ID == uuid.Nil {
			in.ID = uuid.New()
		}
		if err := s.CreateContact(ctx, in); err != nil {
			if isUniqueViolation(err) {
				// Concurrent writer won the (tenant_id, phone) race — fold
				// into their row.
				winner, findErr := s.FindContactByPhoneOrEmail(ctx, tenantID, in.Phone, in.Email)
				if findErr != nil {
					return false, fmt.Errorf("re-read contact after unique race: %w", findErr)
				}
				merged := MergeExternalContact(winner, *in)
				merged.Source, merged.ExternalID = winner.Source, winner.ExternalID
				if uErr := s.UpdateContact(ctx, &merged); uErr != nil {
					return false, uErr
				}
				*in = merged
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	merged := MergeExternalContact(existing, *in)
	// Regular booking contacts never overwrite reverse-CRM provenance.
	merged.Source, merged.ExternalID = existing.Source, existing.ExternalID
	if err := s.UpdateContact(ctx, &merged); err != nil {
		return false, err
	}
	*in = merged
	return false, nil
}

// GetContactByPhone fetches one contact by (tenant_id, phone) — the dedupe
// key. Returns ErrNotFound when no row carries that phone.
func (s *Store) GetContactByPhone(ctx context.Context, tenantID uuid.UUID, phone string) (Contact, error) {
	var c Contact
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, phone, email, notes FROM contacts
			 WHERE tenant_id=$1 AND phone=$2 ORDER BY id LIMIT 1`,
			tenantID, phone).Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email, &c.Notes)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}
