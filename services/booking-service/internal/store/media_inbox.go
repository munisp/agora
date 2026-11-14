package store

// SPEC-W61 (Scope 2): media_inbox — WhatsApp media booking anchor + staff
// review queue. Inbound WhatsApp media lands in ocr-service (W60); the
// gateway then POSTs an idempotent ingest to booking-service, which parks
// the row or anchors it to a UNIQUE recent booking of the digits-matched
// contact (resolve-or-park, single tx). Staff later link/discard parked
// rows. The binding W61 ruling: the ingest path NEVER creates contacts or
// leads (G4) — unknown senders are simply parked.
//
// Phone normalization (G3): canonical form is DIGITS-ONLY with a single
// leading "00" stripped. Matching is BOTH-SIDES normalized SQL-side
// (regexp_replace(phone,'\D','','g') + '^00' strip) so legacy UNNORMALIZED
// contacts.phone rows ('+234 801 ...', '00234...') still match Meta's
// digits-only sender ids. Stored rows stay byte-identical; only matching
// is normalized. The sender_phone_digits column is re-derived SQL-side
// from the raw sender_phone on ingest (trust-but-verify: the
// gateway-computed digits in MediaInboxIngest are never authoritative).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/events"
)

// ReceiptIngestedEventType is the CloudEvent type emitted on
// opendesk.booking.events for each fresh media-inbox ingest (SPEC-W64
// Scope 3; the G-C pinned string is free of fraud-engine substring
// needles). LOCAL const: the canonical const lives in
// internal/events/cloudevents.go (single-owner A this wave) — defined
// locally so this package builds before A's hunk lands; the integrator may
// switch to the shared const (the type STRING is pinned by SPEC-W64 G-C
// either way).
const ReceiptIngestedEventType = "com.opendesk.booking.ReceiptIngested"

// mediaInboxParkedCap bounds the per-tenant parked review queue (SPEC-W61
// pinned constant — NO env knob). On ingest, if parked rows for the tenant
// exceed the cap, the oldest excess parked rows are auto-discarded with
// anchor_method='auto_cap'.
const mediaInboxParkedCap = 500

// MediaInboxItem mirrors booking.media_inbox.
type MediaInboxItem struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	Channel           string     `json:"channel"`
	ProviderMessageID string     `json:"provider_message_id"`
	DocumentID        uuid.UUID  `json:"document_id"`
	SenderPhone       string     `json:"sender_phone"`
	SenderPhoneDigits string     `json:"sender_phone_digits"`
	Mime              string     `json:"mime"`
	Bytes             int64      `json:"bytes"`
	SHA256Prefix      string     `json:"sha256_prefix"`
	State             string     `json:"state"`
	AnchorMethod      string     `json:"anchor_method,omitempty"`
	BookingID         *uuid.UUID `json:"booking_id,omitempty"`
	ContactID         *uuid.UUID `json:"contact_id,omitempty"`
	LinkedBy          string     `json:"linked_by,omitempty"`
	// BookingRefHint (SPEC-W66 WS2) is the normalized BK-XXXXXXXX token the
	// sender quoted in the media caption — the TOKEN ONLY, never the
	// caption (P8). NULL when the caption carried no/ambiguous/malformed
	// token; NULLed again by subject erasure (G7).
	BookingRefHint    string     `json:"booking_ref_hint,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// MediaInboxIngest is the input of IngestMediaInbox. SenderPhoneDigits is
// the gateway-computed digits form; the store re-derives the canonical
// digits SQL-side from SenderPhone (trust-but-verify) and stores ITS OWN
// derivation. BookingRefHint (SPEC-W66 WS2) is the optional caption-extracted
// BK-ref token; it is normalized + validated store-side (trust-but-verify
// again) and a malformed hint is treated as ABSENT, never an error.
type MediaInboxIngest struct {
	TenantID          uuid.UUID
	Channel           string
	ProviderMessageID string
	DocumentID        uuid.UUID
	SenderPhone       string
	SenderPhoneDigits string
	Mime              string
	Bytes             int64
	SHA256Prefix      string
	BookingRefHint    string
}

// mediaInboxCols is the media_inbox projection (own column const — the
// shared bookingCols in bookings.go is HANDS OFF, W57 incident).
// booking_ref_hint is the SPEC-W66 WS2 additive column.
const mediaInboxCols = `id, tenant_id, channel, provider_message_id, document_id, sender_phone, sender_phone_digits, mime, bytes, sha256_prefix, state, anchor_method, booking_id, contact_id, linked_by, booking_ref_hint, created_at, updated_at`

// mediaInboxDigitsSQL renders the pinned G3 digits-only canonical form of
// a SQL text expression: strip non-digits, then strip ONE leading "00".
func mediaInboxDigitsSQL(expr string) string {
	return `regexp_replace(regexp_replace(` + expr + `,'\D','','g'),'^00','')`
}

// bookingRefHintAlphabet is the SPEC-W51 public_ref alphabet (Crockford
// base32, uppercase, no I/L/O/U) — mirrored from public_ref.go's
// publicRefAlphabet for the WS2 hint validation (the hint IS a public_ref).
const mediaInboxHintAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// normalizeBookingRefHint renders the canonical hint form: trim, uppercase,
// then validate "BK-" + 8 Crockford-alphabet chars. A malformed hint
// returns "" (treated as ABSENT — best-effort, never an error/400).
func normalizeBookingRefHint(raw string) string {
	h := strings.ToUpper(strings.TrimSpace(raw))
	if len(h) != 11 || !strings.HasPrefix(h, "BK-") {
		return ""
	}
	for i := 3; i < 11; i++ {
		c := h[i]
		if c >= 0x80 || !strings.ContainsRune(mediaInboxHintAlphabet, rune(c)) {
			return ""
		}
	}
	return h
}

func scanMediaInbox(row pgx.Row) (MediaInboxItem, error) {
	var it MediaInboxItem
	var anchorMethod, linkedBy, bookingRefHint *string
	err := row.Scan(&it.ID, &it.TenantID, &it.Channel, &it.ProviderMessageID, &it.DocumentID,
		&it.SenderPhone, &it.SenderPhoneDigits, &it.Mime, &it.Bytes, &it.SHA256Prefix,
		&it.State, &anchorMethod, &it.BookingID, &it.ContactID, &linkedBy, &bookingRefHint, &it.CreatedAt, &it.UpdatedAt)
	if anchorMethod != nil {
		it.AnchorMethod = *anchorMethod
	}
	if linkedBy != nil {
		it.LinkedBy = *linkedBy
	}
	if bookingRefHint != nil {
		it.BookingRefHint = *bookingRefHint
	}
	return it, err
}

// ensureMediaInboxDDL bootstraps the media_inbox table idempotently, with
// RLS + tenant policy + FORCE mirroring the ensure* chain (booking_reviews
// pattern). The contacts functional digits index (G3) is guarded on
// contacts existing: store.New runs before the bare contacts fixture in
// embedded-postgres tests, and partial deployments may lack it.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureMediaInboxDDL(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS media_inbox (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    channel             TEXT NOT NULL,
    provider_message_id TEXT NOT NULL,
    document_id         UUID NOT NULL,
    sender_phone        TEXT NOT NULL,
    sender_phone_digits TEXT NOT NULL,
    mime                TEXT NOT NULL,
    bytes               BIGINT NOT NULL,
    sha256_prefix       TEXT NOT NULL,
    state               TEXT NOT NULL DEFAULT 'parked'
                        CHECK (state IN ('parked','anchored','linked','discarded')),
    anchor_method       TEXT,
    booking_id          UUID,
    contact_id          UUID,
    linked_by           TEXT,
    booking_ref_hint    TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, channel, provider_message_id)
);
-- SPEC-W66 WS2: additive nullable caption-hint column (converges tables
-- created by the pre-W66 CREATE TABLE above — both-sides idempotent).
ALTER TABLE media_inbox ADD COLUMN IF NOT EXISTS booking_ref_hint TEXT;
CREATE INDEX IF NOT EXISTS idx_media_inbox_tenant_state_created ON media_inbox (tenant_id, state, created_at DESC);
ALTER TABLE media_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE media_inbox FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'media_inbox' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON media_inbox
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;
DO $$
BEGIN
    IF to_regclass('public.contacts') IS NOT NULL THEN
        CREATE INDEX IF NOT EXISTS idx_contacts_phone_digits ON contacts
            (tenant_id, (regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')));
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure media_inbox table: %w", err)
	}
	return nil
}

// IngestMediaInbox records one inbound media message idempotently and
// resolves-or-parks it in ONE tenant transaction (SPEC-W61 §Resolve-or-park):
//
//  1. Idempotency: UNIQUE(tenant_id, channel, provider_message_id) with
//     ON CONFLICT DO NOTHING RETURNING — a replayed ingest (N-05
//     redelivery) returns the EXISTING row unchanged.
//  2. Resolve: candidate bookings = bookings of the tenant whose contact's
//     phone matches the sender BOTH-SIDES digits-normalized, with status IN
//     ('pending','confirmed','rescheduled') AND starts_at in
//     [now-7d, now+30d]. EXACTLY ONE candidate => state='anchored',
//     anchor_method='phone_recency', booking_id + contact_id set. Zero or
//     >1 => parked. Anchoring NEVER fires on ambiguity.
//  2b. SPEC-W66 WS2 caption hint: the optional booking_ref_hint token NEVER
//     anchors without phone corroboration and NEVER creates anything (G6).
//     It only CORROBORATES or NARROWS within the phone-candidate set:
//       - 1 candidate AND (hint absent OR hint == that booking's public_ref)
//         => anchored, anchor_method='phone_recency' (unchanged).
//       - >1 candidates AND hint matches exactly one => anchored,
//         anchor_method='phone_recency+ref_hint' (the hint narrowed within
//         the phone-corroborated set).
//       - >=1 candidates AND hint matches NONE => PARKED,
//         anchor_method='ref_hint_mismatch' (lead ruling (c); the booking
//         row is NOT touched).
//       - 0 candidates => parked, hint recorded, anchor_method stays NULL.
//     The normalized hint is recorded on the row in every case (NULL when
//     absent/malformed).
//  3. Parked cap: after a parked insert, if the tenant's parked rows exceed
//     mediaInboxParkedCap, the oldest excess parked rows are auto-discarded
//     (state='discarded', anchor_method='auto_cap').
//
// The ingest path NEVER inserts into contacts or leads (G4 binding ruling).
//
// SPEC-W64 Scope 3: a FRESH insert also writes one ReceiptIngested
// CloudEvent {tenant_id, channel, document_id, media_inbox_id,
// sha256_prefix, mime, bytes, state, booking_id?} to the outbox in the SAME
// withTenant tx; the replay branch (ON CONFLICT fetch) emits NOTHING so
// bronze event_id dedupe stays honest. eventsTopic == "" disables emission
// (house idiom). The payload is PII-safe by construction: it never carries
// the sender phone in any form (raw or digits).
func (s *Store) IngestMediaInbox(ctx context.Context, in MediaInboxIngest, eventsTopic string) (MediaInboxItem, error) {
	var item MediaInboxItem
	emitted := false
	err := s.withTenant(ctx, in.TenantID, func(tx pgx.Tx) error {
		// Resolve-or-park, decided BEFORE the insert so the row lands with
		// its final state in one statement. The JOIN folds the contact
		// match (step 2 of the SPEC) into the candidate query: zero
		// matching contacts naturally yield zero candidates => parked.
		// The digits needle is re-derived SQL-side from the RAW sender
		// phone (trust-but-verify), never from in.SenderPhoneDigits.
		// SPEC-W66 WS2: the hint is normalized+validated Go-side
		// (trust-but-verify again — a malformed hint is ABSENT); the
		// candidate query selects b.public_ref so the hint can corroborate
		// or narrow WITHIN the phone-matched set only.
		hint := normalizeBookingRefHint(in.BookingRefHint)
		var hintArg *string
		if hint != "" {
			hintArg = &hint
		}
		state := "parked"
		var anchorMethod *string
		var bookingID, contactID *uuid.UUID
		rows, err := tx.Query(ctx,
			`SELECT b.id, b.contact_id, b.public_ref
			   FROM bookings b
			   JOIN contacts c ON c.tenant_id = b.tenant_id AND c.id = b.contact_id
			  WHERE b.tenant_id = $1
			    AND `+mediaInboxDigitsSQL("c.phone")+` = `+mediaInboxDigitsSQL("$2")+`
			    AND b.status IN ('pending','confirmed','rescheduled')
			    AND b.starts_at BETWEEN now() - interval '7 days' AND now() + interval '30 days'
			  ORDER BY b.starts_at DESC`,
			in.TenantID, in.SenderPhone)
		if err != nil {
			return fmt.Errorf("media inbox resolve candidates: %w", err)
		}
		var candBooking uuid.UUID
		var candContact *uuid.UUID
		candidates := 0
		var hintBooking uuid.UUID
		var hintContact *uuid.UUID
		hintMatches := 0
		for rows.Next() {
			var bID uuid.UUID
			var cID *uuid.UUID
			var ref string
			if err := rows.Scan(&bID, &cID, &ref); err != nil {
				rows.Close()
				return fmt.Errorf("media inbox resolve scan: %w", err)
			}
			candidates++
			if candidates == 1 {
				candBooking, candContact = bID, cID
			}
			if hint != "" && ref == hint {
				hintMatches++
				if hintMatches == 1 {
					hintBooking, hintContact = bID, cID
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("media inbox resolve: %w", err)
		}
		switch {
		case candidates == 1 && (hint == "" || hintMatches == 1):
			// EXACTLY ONE candidate, corroborated (or unhinted): anchors as
			// before — anchor_method 'phone_recency' unchanged (WS2).
			state = "anchored"
			method := "phone_recency"
			anchorMethod = &method
			b := candBooking
			bookingID = &b
			contactID = candContact
		case candidates > 1 && hintMatches == 1:
			// The hint NARROWED the ambiguous phone set to exactly one
			// phone-corroborated booking.
			state = "anchored"
			method := "phone_recency+ref_hint"
			anchorMethod = &method
			b := hintBooking
			bookingID = &b
			contactID = hintContact
		case candidates >= 1 && hint != "" && hintMatches == 0:
			// The hint CONTRADICTS every phone candidate: park + flag
			// (lead ruling (c)); the booking row is NOT touched.
			method := "ref_hint_mismatch"
			anchorMethod = &method
		}
		// default: parked with NO anchor columns (zero candidates, or
		// ambiguous with no hint — ambiguity never anchors).

		item, err = scanMediaInbox(tx.QueryRow(ctx,
			`INSERT INTO media_inbox
			    (tenant_id, channel, provider_message_id, document_id, sender_phone, sender_phone_digits,
			     mime, bytes, sha256_prefix, state, anchor_method, booking_id, contact_id, booking_ref_hint)
			 VALUES ($1,$2,$3,$4,$5,`+mediaInboxDigitsSQL("$5")+`,$6,$7,$8,$9,$10,$11,$12,$13)
			 ON CONFLICT (tenant_id, channel, provider_message_id) DO NOTHING
			 RETURNING `+mediaInboxCols,
			in.TenantID, in.Channel, in.ProviderMessageID, in.DocumentID, in.SenderPhone,
			in.Mime, in.Bytes, in.SHA256Prefix, state, anchorMethod, bookingID, contactID, hintArg))
		if errors.Is(err, pgx.ErrNoRows) {
			// Replay (N-05 redelivery): the unique key already exists —
			// return the EXISTING row unchanged (same state).
			item, err = scanMediaInbox(tx.QueryRow(ctx,
				`SELECT `+mediaInboxCols+` FROM media_inbox
				  WHERE tenant_id=$1 AND channel=$2 AND provider_message_id=$3`,
				in.TenantID, in.Channel, in.ProviderMessageID))
			if err != nil {
				return fmt.Errorf("media inbox replay fetch: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert media inbox: %w", err)
		}

		if item.State == "parked" {
			// Parked cap: keep the mediaInboxParkedCap NEWEST parked rows;
			// auto-discard the oldest excess. The fresh row is the newest,
			// so the cap never discards the row just inserted.
			if _, err := tx.Exec(ctx,
				`UPDATE media_inbox
				    SET state='discarded', anchor_method='auto_cap', updated_at=now()
				  WHERE tenant_id=$1 AND state='parked' AND id IN (
				      SELECT id FROM media_inbox
				       WHERE tenant_id=$1 AND state='parked'
				       ORDER BY created_at DESC, id DESC
				       OFFSET $2
				  )`, in.TenantID, mediaInboxParkedCap); err != nil {
				return fmt.Errorf("media inbox parked cap: %w", err)
			}
		}

		// SPEC-W64 Scope 3: exactly one ReceiptIngested per FRESH ingest,
		// in the same tx. The replay branch returned above, so reaching
		// this point implies the row is newly inserted. The fresh row is
		// the newest parked row, so the cap above never discarded it —
		// item.State is final.
		if eventsTopic != "" {
			data := map[string]any{
				"tenant_id":      item.TenantID.String(),
				"channel":        item.Channel,
				"document_id":    item.DocumentID.String(),
				"media_inbox_id": item.ID.String(),
				"sha256_prefix":  item.SHA256Prefix,
				"mime":           item.Mime,
				"bytes":          item.Bytes,
				"state":          item.State,
			}
			if item.BookingID != nil {
				data["booking_id"] = item.BookingID.String()
			}
			payload, err := json.Marshal(events.New("booking-service", ReceiptIngestedEventType, "", item.TenantID.String(), data))
			if err != nil {
				return fmt.Errorf("marshal receipt ingested event: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
				item.ID, eventsTopic, payload); err != nil {
				return fmt.Errorf("insert media inbox outbox: %w", err)
			}
			emitted = true
		}
		return nil
	})
	if err == nil && emitted {
		s.signalOutboxFlush() // W46-A item 6: flush-on-commit
	}
	return item, err
}

// ListMediaInbox returns the tenant's media inbox rows, newest first.
// state "" lists all states; otherwise it filters on the exact state.
// limit <= 0 defaults to 50 and is capped at 500; offset < 0 clamps to 0.
func (s *Store) ListMediaInbox(ctx context.Context, tenantID uuid.UUID, state string, limit, offset int) ([]MediaInboxItem, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	var out []MediaInboxItem
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		q := `SELECT ` + mediaInboxCols + ` FROM media_inbox WHERE tenant_id=$1`
		args := []any{tenantID}
		if state != "" {
			q += ` AND state=$2`
			args = append(args, state)
		}
		q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanMediaInbox(rows)
			if err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// LinkMediaInbox is the staff override: it validates that bookingID exists
// in the tenant, then marks the inbox row state='linked',
// anchor_method='staff', records linked_by and fills booking_id +
// contact_id (from the booking). ErrNotFound when the inbox row is missing
// OR the booking is not in the tenant.
func (s *Store) LinkMediaInbox(ctx context.Context, tenantID, id, bookingID uuid.UUID, linkedBy string) (MediaInboxItem, error) {
	var item MediaInboxItem
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var contactID *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT contact_id FROM bookings WHERE tenant_id=$1 AND id=$2`,
			tenantID, bookingID).Scan(&contactID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("link media inbox: validate booking: %w", err)
		}
		item, err = scanMediaInbox(tx.QueryRow(ctx,
			`UPDATE media_inbox
			    SET state='linked', anchor_method='staff', booking_id=$3, contact_id=$4,
			        linked_by=$5, updated_at=now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+mediaInboxCols,
			tenantID, id, bookingID, contactID, linkedBy))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("link media inbox: %w", err)
		}
		return nil
	})
	return item, err
}

// DiscardMediaInbox marks the inbox row state='discarded' and records the
// staff member (linked_by). ErrNotFound when the row is missing.
func (s *Store) DiscardMediaInbox(ctx context.Context, tenantID, id uuid.UUID, linkedBy string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE media_inbox SET state='discarded', linked_by=$3, updated_at=now()
			  WHERE tenant_id=$1 AND id=$2`, tenantID, id, linkedBy)
		if err != nil {
			return fmt.Errorf("discard media inbox: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// MediaDocumentIDsForSubject returns the ocr document_ids bound to a data
// subject's phone digits (SPEC-W61 G2: the privacy consumer erases the ocr
// bytes for each after the row tombstone). An empty needle matches NOTHING
// — tombstoned rows carry sender_phone_digits='' and must not be
// re-collected.
func (s *Store) MediaDocumentIDsForSubject(ctx context.Context, tenantID uuid.UUID, phoneDigits string) ([]string, error) {
	if phoneDigits == "" {
		return nil, nil
	}
	var out []string
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT document_id FROM media_inbox
			  WHERE tenant_id=$1 AND sender_phone_digits=$2
			  ORDER BY created_at`, tenantID, phoneDigits)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id.String())
		}
		return rows.Err()
	})
	return out, err
}
