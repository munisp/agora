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
//
//     SPEC-W89 (closure, lead ruling): the dedupe key is now the pinned
//     W61 G3 DIGITS canonical form — a '+234…' row and a '234…' row are
//     the same subscriber and fold to one keeper. The re-point set is
//     extended (media_inbox, contact_locations, geo_campaign_sends) and
//     the TTL-ephemeral loser rows (verification_grants, portal_tokens)
//     are DELETED (fail-open safe: worst case one re-OTP). The unique
//     index is swapped to the digits EXPRESSION so cross-format
//     duplicates are structurally impossible afterwards.
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

        -- SPEC-W89: DIGITS-equal fold (W61 G3 canonical form both sides).
        -- A '+234…' row and a '234…' row are the same subscriber; the raw
        -- fold above cannot see them. Same keeper rule, wider partition.
        IF to_regclass('public.bookings') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            UPDATE bookings b SET contact_id = r.keeper
            FROM ranked r
            WHERE b.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        IF to_regclass('public.loan_applications') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            UPDATE loan_applications a SET contact_id = r.keeper
            FROM ranked r
            WHERE a.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        IF to_regclass('public.loan_accounts') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            UPDATE loan_accounts a SET contact_id = r.keeper
            FROM ranked r
            WHERE a.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        -- media_inbox: re-point the anchor lineage to the keeper.
        IF to_regclass('public.media_inbox') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            UPDATE media_inbox m SET contact_id = r.keeper
            FROM ranked r
            WHERE m.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        -- contact_locations: keeper's location wins on conflict (PK
        -- (tenant_id, contact_id)); non-conflicting loser rows re-point.
        IF to_regclass('public.contact_locations') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            DELETE FROM contact_locations l
            USING ranked r
            WHERE l.contact_id = r.id AND r.id <> r.keeper
              AND EXISTS (SELECT 1 FROM contact_locations k
                          WHERE k.tenant_id = l.tenant_id AND k.contact_id = r.keeper);
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            UPDATE contact_locations l SET contact_id = r.keeper
            FROM ranked r
            WHERE l.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        -- geo_campaign_sends: a conflict means the campaign already sent
        -- to the keeper — the loser ledger row is a true duplicate and is
        -- deleted; non-conflicting rows re-point.
        IF to_regclass('public.geo_campaign_sends') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            DELETE FROM geo_campaign_sends g
            USING ranked r
            WHERE g.contact_id = r.id AND r.id <> r.keeper
              AND EXISTS (SELECT 1 FROM geo_campaign_sends k
                          WHERE k.campaign_id = g.campaign_id AND k.contact_id = r.keeper);
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            UPDATE geo_campaign_sends g SET contact_id = r.keeper
            FROM ranked r
            WHERE g.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        -- TTL-ephemeral loser rows are DELETED (fail-open safe: worst
        -- case one re-OTP / one new portal code). Their UNIQUE
        -- (tenant_id, contact_id) constraints make re-pointing unsafe.
        IF to_regclass('public.verification_grants') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            DELETE FROM verification_grants g
            USING ranked r
            WHERE g.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        IF to_regclass('public.portal_tokens') IS NOT NULL THEN
            WITH ranked AS (
                SELECT id, (MIN(id::text) OVER (PARTITION BY tenant_id, regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))::uuid AS keeper
                FROM contacts WHERE phone IS NOT NULL AND regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> ''
            )
            DELETE FROM portal_tokens t
            USING ranked r
            WHERE t.contact_id = r.id AND r.id <> r.keeper;
        END IF;
        -- Drop the digits-loser contacts (all references handled above).
        DELETE FROM contacts c
        WHERE c.phone IS NOT NULL
          AND regexp_replace(regexp_replace(c.phone,'\D','','g'),'^00','') <> ''
          AND c.id <> (
            SELECT MIN(id::text)::uuid FROM contacts k
            WHERE k.tenant_id = c.tenant_id
              AND regexp_replace(regexp_replace(k.phone,'\D','','g'),'^00','')
                = regexp_replace(regexp_replace(c.phone,'\D','','g'),'^00','')
        );

        -- SPEC-W89: swap the RAW unique index for the digits EXPRESSION —
        -- cross-format duplicates are structurally impossible afterwards.
        -- The DROP is required (IF NOT EXISTS would no-op on the old
        -- same-named raw index).
        DROP INDEX IF EXISTS uq_contacts_tenant_phone;
        CREATE UNIQUE INDEX IF NOT EXISTS uq_contacts_tenant_phone
            ON contacts (tenant_id, (regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')))
            WHERE regexp_replace(regexp_replace(phone,'\D','','g'),'^00','') <> '';
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
			`SELECT id, tenant_id, name, COALESCE(phone, ''), COALESCE(email, ''), COALESCE(notes, '') FROM contacts
			 WHERE tenant_id=$1 AND phone=$2 ORDER BY id LIMIT 1`,
			tenantID, phone).Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email, &c.Notes)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}
