package store

// Tenant deletion prep (SPEC-W45 CODER-A item 10; K9 TenantDeleted
// cascade): when identity publishes TenantDeleted, the booking consumer
// (CODER-E, internal/consumer) calls DeleteTenantData to anonymize the
// tenant's contacts and cancel its open bookings. The helper is idempotent
// (a redelivered event is a no-op) and runs inside the tenant's RLS
// transaction so it can never touch another tenant's rows.
//
// SPEC-W47 (W47-A item 6, A-DB DB-02): the cascade now covers EVERY
// PII-bearing booking-DB table, not just contacts/bookings. Per-table
// actions (see tenantPIIPurgeSteps): cleartext phone/name columns are
// replaced by the W3 salted SHA-256 tombstone ('sha256:<hex>') or
// NULLed/'erased', free-text PII carriers (crm notes, survey answers,
// field-capture payloads, CSAT comments) are blanked, and capability /
// device / geo rows that would keep working or keep PII for a dead tenant
// (portal tokens, CSAT tokens, device tokens, survey invites, contact
// locations, QR scans, geo send ledgers) are DELETED. Financial and audit
// rows (bookings, campaign spend, commission/loyalty/lending ledgers,
// ticket timelines) are preserved with their PII links anonymized.
//
// Every step is idempotent: anonymizing UPDATEs skip rows already
// tombstoned ('sha256:%' / 'erased' guards), DELETEs naturally converge.
// Steps whose table does not exist in this database (partial deployments —
// e.g. PostGIS-less installs lack the geo tables) are skipped after a
// to_regclass probe; the full-schema coverage test in internal/consumer
// (W47) fails when a tenant-scoped table exists without a classification.

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
	// Details maps each covered table (SPEC-W47 extended cascade) to the
	// rows anonymized or deleted. Tables absent from this database
	// (partial deployments) do not appear.
	Details map[string]int64 `json:"details,omitempty"`
}

// piiPurgeStep is one idempotent anonymize/delete statement of the extended
// TenantDeleted cascade. $1 is always the tenant id.
type piiPurgeStep struct {
	table  string
	action string // human-readable action for the coverage map
	sql    string
}

// hashExpr renders the W3 salted SHA-256 tombstone expression for a column
// (pgcrypto digest — the base schema already depends on it for the contacts
// tombstone).
func hashExpr(col string) string {
	return `'sha256:' || encode(digest('opendesk-gdpr-erase:' || ` + col + `, 'sha256'), 'hex')`
}

// tenantPIIPurgeSteps is the SPEC-W47 (A-DB DB-02) cascade coverage. Order
// is irrelevant; every statement is guarded so a redelivery is a no-op.
// Kept in one place so the coverage test can diff it against the live
// information_schema inventory.
var tenantPIIPurgeSteps = []piiPurgeStep{
	{"leads", "anonymize phone_e164",
		`UPDATE leads SET phone_e164=` + hashExpr("phone_e164") + `
		 WHERE tenant_id=$1 AND phone_e164 <> '' AND phone_e164 NOT LIKE 'sha256:%'`},
	{"promo_redemptions", "anonymize phone_e164",
		`UPDATE promo_redemptions SET phone_e164=` + hashExpr("phone_e164") + `
		 WHERE tenant_id=$1 AND phone_e164 <> '' AND phone_e164 NOT LIKE 'sha256:%'`},
	{"waitlist", "anonymize contact_name/contact_phone",
		`UPDATE waitlist
		 SET contact_name=CASE WHEN contact_name NOT IN ('','erased') THEN 'erased' ELSE contact_name END,
		     contact_phone=CASE WHEN contact_phone <> '' AND contact_phone NOT LIKE 'sha256:%'
		                        THEN ` + hashExpr("contact_phone") + ` ELSE contact_phone END
		 WHERE tenant_id=$1
		   AND (contact_name NOT IN ('','erased')
		        OR (contact_phone <> '' AND contact_phone NOT LIKE 'sha256:%'))`},
	{"referral_agents", "anonymize name/phone",
		`UPDATE referral_agents SET name='erased', phone=` + hashExpr("phone") + `, updated_at=now()
		 WHERE tenant_id=$1 AND name <> 'erased'`},
	{"referrals", "anonymize referee_phone",
		`UPDATE referrals SET referee_phone=` + hashExpr("referee_phone") + `
		 WHERE tenant_id=$1 AND referee_phone <> '' AND referee_phone NOT LIKE 'sha256:%'`},
	{"civic_cases", "null reporter phone/name",
		`UPDATE civic_cases SET reporter_phone_e164=NULL, reporter_name=NULL, updated_at=now()
		 WHERE tenant_id=$1 AND (reporter_phone_e164 IS NOT NULL OR reporter_name IS NOT NULL)`},
	{"tickets", "null csat free-text comment",
		`UPDATE tickets SET csat_comment=NULL, updated_at=now()
		 WHERE tenant_id=$1 AND csat_comment IS NOT NULL`},
	{"survey_responses", "blank answers, detach contact",
		`UPDATE survey_responses SET answers='{}'::jsonb, contact_id=NULL
		 WHERE tenant_id=$1 AND (answers <> '{}'::jsonb OR contact_id IS NOT NULL)`},
	{"crm_notes", "blank note body/author",
		`UPDATE crm_notes SET body='[erased]', author='', updated_at=now()
		 WHERE tenant_id=$1 AND body <> '[erased]'`},
	{"field_captures", "blank queued payload/gps",
		`UPDATE field_captures SET payload='{}'::jsonb, gps=NULL
		 WHERE tenant_id=$1 AND (payload <> '{}'::jsonb OR gps IS NOT NULL)`},
	{"field_checkins", "null geo, blank note/payload",
		`UPDATE field_checkins SET lat=NULL, lng=NULL, note='', payload='{}'::jsonb
		 WHERE tenant_id=$1 AND (lat IS NOT NULL OR lng IS NOT NULL OR note <> '' OR payload <> '{}'::jsonb)`},
	// Hard deletes: capability tokens, device handles, geo/scan PII and
	// replay ledgers of a dead tenant must not survive (they would keep
	// working or keep PII with no owner).
	{"contact_locations", "delete contact geo", `DELETE FROM contact_locations WHERE tenant_id=$1`},
	{"survey_invites", "delete invite capability tokens", `DELETE FROM survey_invites WHERE tenant_id=$1`},
	{"device_tokens", "delete push tokens", `DELETE FROM device_tokens WHERE tenant_id=$1`},
	{"portal_tokens", "delete portal login tokens", `DELETE FROM portal_tokens WHERE tenant_id=$1`},
	{"csat_tokens", "delete CSAT capability tokens", `DELETE FROM csat_tokens WHERE tenant_id=$1`},
	{"qr_scans", "delete scan events", `DELETE FROM qr_scans WHERE tenant_id=$1`},
	{"geo_campaign_sends", "delete send ledger", `DELETE FROM geo_campaign_sends WHERE tenant_id=$1`},
	// SPEC-W48 A4: reviews carry reviewer PII (reviewer_name) and have no
	// owner after tenant deletion — hard-deleted like the other PII rows.
	{"booking_reviews", "delete reviews (reviewer PII)", `DELETE FROM booking_reviews WHERE tenant_id=$1`},
	// SPEC-W61 (G1/G2): media_inbox rows are sender-PII + ocr anchor
	// pointers with no owner after tenant deletion — hard-deleted like the
	// other PII/capability rows (the ocr bytes themselves are hard-deleted
	// ocr-side by the existing W58 TenantDeleted erasure).
	{"media_inbox", "delete media inbox rows (sender PII + anchors)", `DELETE FROM media_inbox WHERE tenant_id=$1`},
	// The outbox has no tenant_id column (A-DB DB-13) and no RLS by design
	// (cross-tenant dispatcher); payload->>'tenant_id' is the only handle.
	// Rows whose payloads lack the key are untouched (documented limit).
	{"outbox", "delete tenant outbox rows (payload key)", `DELETE FROM outbox WHERE payload->>'tenant_id'=$1`},
}

// TenantCascadeTables enumerates every booking-DB table the TenantDeleted
// cascade touches, mapped to the action taken ("anonymize ...", "delete ...",
// "cancel open bookings"). Exported for the W47 cascade-coverage test
// (internal/consumer), which fails when a tenant-scoped table exists in the
// full schema without either a cascade entry here or a documented keep-list
// entry there.
func TenantCascadeTables() map[string]string {
	out := map[string]string{
		"contacts": "anonymize name/phone/email/notes",
		"bookings": "cancel open bookings (terminal rows kept as audit)",
	}
	for _, st := range tenantPIIPurgeSteps {
		out[st.table] = st.action
	}
	return out
}

// tableExists probes the information_schema (RLS-free catalog) for a table.
func tableExists(ctx context.Context, tx pgx.Tx, name string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&ok)
	return ok, err
}

// DeleteTenantData anonymizes ALL contacts of the tenant (name='erased',
// phone/email replaced by their salted SHA-256 tombstones — the W3
// AnonymizeContacts scheme), cancels every OPEN booking
// (pending|confirmed → cancelled) and runs the SPEC-W47 extended PII
// cascade (tenantPIIPurgeSteps) over every other PII-bearing booking-DB
// table. Terminal bookings (cancelled|no_show|completed), already-erased
// rows and financial/audit records are untouched, so the helper is
// idempotent. No outbox events are emitted: this is a cascade reaction to
// TenantDeleted, not an operator action — the deletion event itself is the
// audit anchor (K9).
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

		// SPEC-W47 (W47-A item 6, A-DB DB-02): extended PII cascade.
		for _, st := range tenantPIIPurgeSteps {
			exists, err := tableExists(ctx, tx, st.table)
			if err != nil {
				return fmt.Errorf("probe table %s: %w", st.table, err)
			}
			if !exists {
				continue // partial deployment (e.g. no PostGIS geo tables)
			}
			tag, err := tx.Exec(ctx, st.sql, tenantID)
			if err != nil {
				return fmt.Errorf("tenant cascade step %s (%s): %w", st.table, st.action, err)
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
