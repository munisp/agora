package store

// Reverse CRM sync support (SPEC-CRM §B, Twenty -> OpenDesk direction):
//   - contacts gain nullable `source` / `external_id` columns so rows created
//     from a CRM person can be traced back (and re-matched) to the external
//     system;
//   - bookings gain a `crm_notes` JSONB array column that the crm-sync reverse
//     worker appends to when a Twenty task is marked DONE.
//
// LOOP PREVENTION: neither path writes an outbox event — contacts have no
// outbox at all (only bookings do, and only via CreateBookingTx /
// SetBookingStatus / RescheduleBooking), so a reverse-synced contact or CRM
// note can never re-enter the forward (OpenDesk -> Twenty) event flow. This
// is inherent to the outbox design; keep it that way.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ensureCRMColumns bootstraps the reverse-sync columns idempotently.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant (see ensureSitesTable).
func (s *Store) ensureCRMColumns(ctx context.Context) error {
	// ALTER TABLE IF EXISTS / regclass guards keep this a no-op when the base
	// schema has not been applied yet (e.g. tests that create the tables only
	// after store.New and then re-run ensureCRMColumns).
	const ddl = `
ALTER TABLE IF EXISTS contacts ADD COLUMN IF NOT EXISTS source TEXT;
ALTER TABLE IF EXISTS contacts ADD COLUMN IF NOT EXISTS external_id TEXT;
ALTER TABLE IF EXISTS bookings ADD COLUMN IF NOT EXISTS crm_notes JSONB NOT NULL DEFAULT '[]'::jsonb;
DO $$
BEGIN
  IF to_regclass('contacts') IS NOT NULL THEN
    CREATE INDEX IF NOT EXISTS idx_contacts_tenant_external_id ON contacts (tenant_id, external_id);
  END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure crm columns: %w", err)
	}
	return nil
}

// MergeExternalContact folds an inbound external (CRM) contact into the
// existing row: non-empty inbound fields win, empty fields keep the stored
// value. Pure function — unit-tested without a database.
func MergeExternalContact(existing, in Contact) Contact {
	out := existing
	if in.Name != "" {
		out.Name = in.Name
	}
	if in.Phone != "" {
		out.Phone = in.Phone
	}
	if in.Email != "" {
		out.Email = in.Email
	}
	if in.Notes != "" {
		out.Notes = in.Notes
	}
	if in.Source != "" {
		out.Source = in.Source
	}
	if in.ExternalID != "" {
		out.ExternalID = in.ExternalID
	}
	return out
}

// FindContactByPhoneOrEmail locates a contact within a tenant by phone OR
// e-mail (phone match preferred when both could hit different rows). Empty
// needles never match. Returns ErrNotFound when nothing matches.
func (s *Store) FindContactByPhoneOrEmail(ctx context.Context, tenantID uuid.UUID, phone, email string) (Contact, error) {
	var c Contact
	if phone == "" && email == "" {
		return c, ErrNotFound
	}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// SPEC-W88: the phone needle matches on the pinned W61 G3 digits
		// canonical form BOTH SIDES (same idiom as GetContactByChannel),
		// so a CRM/admin '+'-form and a WhatsApp wa_id digits-form of the
		// SAME subscriber dedupe to one contact instead of accumulating
		// parallel rows (the raw uq_contacts_tenant_phone unique index
		// permits both forms). E-mail stays exact. The digits<>''
		// guards keep no-digit needles/rows from matching each other.
		d := mediaInboxDigitsSQL
		phoneHit := `($2 <> '' AND ` + d(`phone`) + ` <> '' AND ` + d(`phone`) + ` = ` + d(`$2`) + `)`
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, phone, email, notes,
			        COALESCE(source, ''), COALESCE(external_id, '')
			 FROM contacts
			 WHERE tenant_id=$1
			   AND (`+phoneHit+` OR ($3 <> '' AND email=$3))
			 ORDER BY `+phoneHit+` DESC, name
			 LIMIT 1`,
			tenantID, phone, email).
			Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email, &c.Notes, &c.Source, &c.ExternalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// UpsertExternalContact applies a reverse-synced CRM contact: when a contact
// with the same phone OR e-mail already exists in the tenant it is merged and
// updated (created=false); otherwise a new contact is created with the given
// source/external_id (created=true). The caller sets Source ("twenty") and
// ExternalID (the CRM person id).
//
// No outbox event is written — see the package-level loop-prevention note.
func (s *Store) UpsertExternalContact(ctx context.Context, tenantID uuid.UUID, in *Contact) (bool, error) {
	in.TenantID = tenantID
	existing, err := s.FindContactByPhoneOrEmail(ctx, tenantID, in.Phone, in.Email)
	if errors.Is(err, ErrNotFound) {
		if in.ID == uuid.Nil {
			in.ID = uuid.New()
		}
		if err := s.CreateContact(ctx, in); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	merged := MergeExternalContact(existing, *in)
	if err := s.updateExternalContact(ctx, &merged); err != nil {
		return false, err
	}
	*in = merged
	return false, nil
}

// updateExternalContact persists a merged external contact incl. the
// source/external_id columns (which UpdateContact leaves alone).
func (s *Store) updateExternalContact(ctx context.Context, c *Contact) error {
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE contacts SET name=$3, phone=$4, email=$5, notes=$6,
			        source=NULLIF($7,''), external_id=NULLIF($8,'')
			 WHERE tenant_id=$1 AND id=$2`,
			c.TenantID, c.ID, c.Name, c.Phone, c.Email, c.Notes, c.Source, c.ExternalID)
		if err != nil {
			return fmt.Errorf("update external contact: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CRMNote is one entry of the bookings.crm_notes JSONB array, appended by the
// crm-sync reverse worker when a Twenty task linked to the booking is closed.
type CRMNote struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"` // e.g. "twenty"
	Text   string    `json:"text"`
}

// AppendBookingCRMNote appends one note to bookings.crm_notes. It never
// writes an outbox event (loop prevention: the note is CRM-originated).
// Returns ErrNotFound when the booking does not exist in the tenant.
func (s *Store) AppendBookingCRMNote(ctx context.Context, tenantID, bookingID uuid.UUID, note CRMNote) error {
	b, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("marshal crm note: %w", err)
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE bookings SET crm_notes = crm_notes || $3::jsonb, updated_at=now()
			 WHERE tenant_id=$1 AND id=$2`,
			tenantID, bookingID, string(b))
		if err != nil {
			return fmt.Errorf("append crm note: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListBookingCRMNotes returns the crm_notes array of a booking (helper for
// tests and future read endpoints).
func (s *Store) ListBookingCRMNotes(ctx context.Context, tenantID, bookingID uuid.UUID) ([]CRMNote, error) {
	var raw []byte
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT crm_notes FROM bookings WHERE tenant_id=$1 AND id=$2`,
			tenantID, bookingID).Scan(&raw)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var notes []CRMNote
	if err := json.Unmarshal(raw, &notes); err != nil {
		return nil, fmt.Errorf("decode crm_notes: %w", err)
	}
	return notes, nil
}
