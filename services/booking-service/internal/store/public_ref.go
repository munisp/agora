package store

// SPEC-W51 (W51-B item B1): human-readable booking public_ref.
//
// Every booking carries a public reference of the form BK-XXXXXXXX — 8
// Crockford-base32 characters (uppercase, alphabet excludes the ambiguous
// I/L/O/U) derived from the booking UUID's first 5 bytes (40 bits). The ref
// is UNIQUE per tenant ((tenant_id, public_ref) partial unique index) and is
// what customers quote at the desk and what the cross-scope payment events
// carry as booking_ref (SPEC-W51 sacred contract delta).
//
// Derivation is deterministic: candidate 0 is pure uuid bytes; on a
// (astronomically rare) intra-tenant collision the counter candidates mix a
// counter into sha256(uuid || counter) and the first free candidate wins —
// the counter loop runs INSIDE the booking's transaction (assignPublicRefTx),
// with the unique index as the authoritative backstop and a whole-tx retry
// in CreateBookingTx/ClaimWaitlistTx for the residual cross-tx race.
//
// ensureBookingPublicRef is the idempotent migration: ADD COLUMN (existing
// DBs), a deterministic backfill of legacy rows (oldest-first so the same
// database always backfills to the same refs), then the unique index.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// publicRefAlphabet is Crockford base32 (uppercase, no I/L/O/U).
const publicRefAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// publicRefMaxCounter bounds the collision-regeneration loop; 64 candidates
// per booking is already ~2^46 intra-tenant collision territory — beyond it
// something is wrong and the create fails loudly instead of looping.
const publicRefMaxCounter = 64

// publicRefRetryAttempts bounds the whole-transaction retry when the unique
// index reports a public_ref race the in-tx EXISTS scan could not see
// (concurrent insert committed between the scan and the insert).
const publicRefRetryAttempts = 4

// encodePublicRef40 renders 5 bytes (40 bits) as 8 Crockford-base32 chars,
// big-endian.
func encodePublicRef40(b []byte) string {
	v := uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
	var out [8]byte
	for i := 7; i >= 0; i-- {
		out[i] = publicRefAlphabet[v&31]
		v >>= 5
	}
	return string(out[:])
}

// publicRefCandidate derives the n-th public-ref candidate for a booking ID.
// Candidate 0 is the pure uuid-bytes form (SPEC-W51 B1); n > 0 deterministically
// mixes the counter in via sha256 (the "counter suffix" regeneration).
func publicRefCandidate(id uuid.UUID, n uint64) string {
	var b [5]byte
	if n == 0 {
		copy(b[:], id[0:5])
	} else {
		var ctr [8]byte
		for i := 0; i < 8; i++ {
			ctr[i] = byte(n >> (8 * i))
		}
		sum := sha256.Sum256(append(append([]byte("opendesk-booking-public-ref:"), id[:]...), ctr[:]...))
		copy(b[:], sum[:5])
	}
	return "BK-" + encodePublicRef40(b[:])
}

// PublicRefForBooking returns the canonical (counter-0) public ref of a
// booking ID — BK-XXXXXXXX. The stored value can differ only when candidate
// 0 collided within the tenant and the counter loop advanced; read the row
// for the authoritative ref.
func PublicRefForBooking(id uuid.UUID) string { return publicRefCandidate(id, 0) }

// isPublicRefUniqueViolation reports whether err is the unique-index race on
// uq_bookings_tenant_public_ref (distinguished from the idempotency-key
// unique violation, which maps to ErrConflict replay semantics).
func isPublicRefUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "uq_bookings_tenant_public_ref"
}

// assignPublicRefTx picks the booking's public_ref inside tx: the preferred
// value when given and free, otherwise the first free counter candidate.
// The (tenant_id, public_ref) unique index is the authoritative backstop —
// this EXISTS scan just makes the common path collision-free without a
// failed statement. Returns the chosen ref.
func assignPublicRefTx(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID, preferred string) (string, error) {
	free := func(ref string) (bool, error) {
		var exists bool
		err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM bookings WHERE tenant_id=$1 AND public_ref=$2)`,
			tenantID, ref).Scan(&exists)
		return !exists, err
	}
	if preferred != "" {
		ok, err := free(preferred)
		if err != nil {
			return "", fmt.Errorf("check public_ref: %w", err)
		}
		if ok {
			return preferred, nil
		}
	}
	for n := uint64(0); n < publicRefMaxCounter; n++ {
		cand := publicRefCandidate(bookingID, n)
		ok, err := free(cand)
		if err != nil {
			return "", fmt.Errorf("check public_ref candidate: %w", err)
		}
		if ok {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free public_ref candidate for booking %s after %d tries", bookingID, publicRefMaxCounter)
}

// ensureBookingPublicRef migrates bookings to carry public_ref (SPEC-W51
// B1). Idempotent, three steps:
//
//  1. ADD COLUMN IF NOT EXISTS public_ref TEXT NOT NULL DEFAULT '' — the ''
//     default keeps rows written by an old binary mid-rollout valid; the
//     partial unique index below ignores them.
//  2. Deterministic backfill: every '' row, oldest first per tenant, gets
//     its first free counter candidate (same candidate logic as the create
//     path). One transaction; re-runnable — a row that already has a ref is
//     never touched.
//  3. CREATE UNIQUE INDEX IF NOT EXISTS uq_bookings_tenant_public_ref ON
//     bookings (tenant_id, public_ref) WHERE public_ref <> '' — the per-tenant
//     uniqueness backstop AND the lookup index.
//
// NOTE (RLS): superuser bootstrap path, intentionally outside withTenant
// (same posture as ensureBookingPaymentColumns).
func (s *Store) ensureBookingPublicRef(ctx context.Context) error {
	const ddl = `
DO $$
BEGIN
    IF to_regclass('public.bookings') IS NOT NULL THEN
        ALTER TABLE bookings ADD COLUMN IF NOT EXISTS public_ref TEXT NOT NULL DEFAULT '';
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure bookings public_ref column: %w", err)
	}
	if err := s.backfillBookingPublicRefs(ctx); err != nil {
		return err
	}
	const idx = `
DO $$
BEGIN
    IF to_regclass('public.bookings') IS NOT NULL THEN
        CREATE UNIQUE INDEX IF NOT EXISTS uq_bookings_tenant_public_ref
            ON bookings (tenant_id, public_ref) WHERE public_ref <> '';
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, idx); err != nil {
		return fmt.Errorf("ensure bookings public_ref index: %w", err)
	}
	return nil
}

// backfillBookingPublicRefs assigns refs to every legacy row (public_ref='')
// in ONE transaction, oldest first per tenant so the result is deterministic
// for a given database state. Idempotent: with nothing to backfill it is a
// no-op.
func (s *Store) backfillBookingPublicRefs(ctx context.Context) error {
	// Same to_regclass guard as the DDL above: a no-op when the bookings
	// table does not exist yet (init scripts create it later; the next
	// store.New backfills).
	var tableExists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.bookings') IS NOT NULL`).Scan(&tableExists); err != nil {
		return fmt.Errorf("backfill public_ref guard: %w", err)
	}
	if !tableExists {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id FROM bookings WHERE public_ref=''
		 ORDER BY tenant_id, created_at, id`)
	if err != nil {
		return fmt.Errorf("backfill public_ref scan: %w", err)
	}
	type legacyRow struct {
		tenantID uuid.UUID
		id       uuid.UUID
	}
	var legacy []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.tenantID, &r.id); err != nil {
			rows.Close()
			return fmt.Errorf("backfill public_ref scan: %w", err)
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill public_ref scan: %w", err)
	}
	for _, r := range legacy {
		ref, err := assignPublicRefTx(ctx, tx, r.tenantID, r.id, "")
		if err != nil {
			return fmt.Errorf("backfill public_ref for booking %s: %w", r.id, err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE bookings SET public_ref=$3 WHERE tenant_id=$1 AND id=$2 AND public_ref=''`,
			r.tenantID, r.id, ref); err != nil {
			return fmt.Errorf("backfill public_ref for booking %s: %w", r.id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("backfill public_ref commit: %w", err)
	}
	return nil
}
