// store_queries.go — read/query methods of Store (split from store.go for transport-size limits; no behavior change).
package lending

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListProducts enumerates the tenant's products (newest-updated first).
// includeInactive=false restricts to active products (the application form).
func (s *Store) ListProducts(ctx context.Context, tenantID uuid.UUID, includeInactive bool) ([]Product, error) {
	q := `SELECT ` + productCols + ` FROM loan_products WHERE tenant_id=$1`
	if !includeInactive {
		q += ` AND active`
	}
	q += ` ORDER BY updated_at DESC LIMIT 200`
	out := []Product{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProduct(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// GetProduct loads one product scoped to the tenant (ErrNotFound when
// missing or cross-tenant).
func (s *Store) GetProduct(ctx context.Context, tenantID, id uuid.UUID) (Product, error) {
	var p Product
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanProduct(tx.QueryRow(ctx,
			`SELECT `+productCols+` FROM loan_products WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		p = row
		return nil
	})
	return p, err
}

// GetApplication loads one application scoped to the tenant.
func (s *Store) GetApplication(ctx context.Context, tenantID, id uuid.UUID) (Application, error) {
	var a Application
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanApplication(tx.QueryRow(ctx,
			`SELECT `+applicationCols+` FROM loan_applications WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		a = row
		return nil
	})
	return a, err
}

// ApplicationFilters scopes ListApplications ("" disables a filter).
type ApplicationFilters struct {
	Status    string
	ContactID *uuid.UUID
	Limit     int // 0 → 200 (default cap)
}

// ListApplications returns the tenant's applications (newest first).
// Backs GET /v1/lending/applications.
func (s *Store) ListApplications(ctx context.Context, tenantID uuid.UUID, f ApplicationFilters) ([]Application, error) {
	q := `SELECT ` + applicationCols + ` FROM loan_applications WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	if f.ContactID != nil {
		n++
		q += fmt.Sprintf(` AND contact_id=$%d`, n)
		args = append(args, *f.ContactID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	n++
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, n)
	args = append(args, limit)
	out := []Application{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanApplication(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Scoring signals (defensive reads over contacts/bookings — missing tables
// or columns contribute 0, never a 500; SPEC-W20 "code defensively")
// ---------------------------------------------------------------------------

// ComputeScore gathers the naive-score signals for one contact and computes
// the 0..100 score. Every signal query is independent and best-effort.
func (s *Store) ComputeScore(ctx context.Context, tenantID, contactID uuid.UUID) (int, ScoreSignals) {
	var sig ScoreSignals
	now := time.Now().UTC()

	// Tenure: earliest known contact activity. The canonical contacts table
	// carries no created_at — try it first (future schemas), then fall back
	// to the first booking's created_at.
	var firstSeen *time.Time
	_ = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var ts *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT created_at FROM contacts WHERE tenant_id=$1 AND id=$2`,
			tenantID, contactID).Scan(&ts); err == nil && ts != nil {
			firstSeen = ts
		}
		return nil // defensive: any failure leaves firstSeen nil
	})
	if firstSeen == nil {
		_ = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			var ts *time.Time
			if err := tx.QueryRow(ctx,
				`SELECT min(created_at) FROM bookings WHERE tenant_id=$1 AND contact_id=$2`,
				tenantID, contactID).Scan(&ts); err == nil && ts != nil {
				firstSeen = ts
			}
			return nil
		})
	}
	if firstSeen != nil {
		sig.TenureDays = int(now.Sub(*firstSeen).Hours() / 24)
	}

	_ = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM bookings WHERE tenant_id=$1 AND contact_id=$2 AND status='completed'`,
			tenantID, contactID).Scan(&n); err == nil {
			sig.CompletedBookings = n
		}
		return nil
	})

	_ = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM loan_applications WHERE tenant_id=$1 AND contact_id=$2 AND status='repaid'`,
			tenantID, contactID).Scan(&n); err == nil {
			sig.RepaidLoans = n
		}
		return nil
	})

	return Score(sig), sig
}

// GetLoan loads one loan account scoped to the tenant.
func (s *Store) GetLoan(ctx context.Context, tenantID, id uuid.UUID) (LoanAccount, error) {
	var l LoanAccount
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanLoan(tx.QueryRow(ctx,
			`SELECT `+loanCols+` FROM loan_accounts WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		l = row
		return nil
	})
	return l, err
}

// LoanFilters scopes ListLoans ("" / nil disables a filter).
type LoanFilters struct {
	Status        string // active|repaid|defaulted ("" = all)
	ApplicationID *uuid.UUID
	ContactID     *uuid.UUID
	Limit         int // 0 → 200 (default cap)
}

// ListLoans returns the tenant's loan accounts (newest disbursed first).
// Backs GET /v1/lending/loans — the collection lookup the UI needs to
// browse the book / resolve a loan from its application (SPEC lists only
// GET /loans/{id}; this is the documented collection addition).
func (s *Store) ListLoans(ctx context.Context, tenantID uuid.UUID, f LoanFilters) ([]LoanAccount, error) {
	q := `SELECT ` + loanCols + ` FROM loan_accounts WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	if f.ApplicationID != nil {
		n++
		q += fmt.Sprintf(` AND application_id=$%d`, n)
		args = append(args, *f.ApplicationID)
	}
	if f.ContactID != nil {
		n++
		q += fmt.Sprintf(` AND contact_id=$%d`, n)
		args = append(args, *f.ContactID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	n++
	q += fmt.Sprintf(` ORDER BY disbursed_at DESC LIMIT $%d`, n)
	args = append(args, limit)
	out := []LoanAccount{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLoan(rows)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// ListRepayments returns one loan's repayments (oldest first).
func (s *Store) ListRepayments(ctx context.Context, tenantID, loanID uuid.UUID) ([]Repayment, error) {
	out := []Repayment{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, loan_id, amount_kobo, ref_id, paid_at
			   FROM repayments WHERE tenant_id=$1 AND loan_id=$2
			  ORDER BY paid_at, id LIMIT 500`, tenantID, loanID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Repayment
			if err := rows.Scan(&r.ID, &r.TenantID, &r.LoanID, &r.AmountKobo, &r.RefID, &r.PaidAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Portfolio
// ---------------------------------------------------------------------------

// Portfolio aggregates the tenant's book (PAR30 over ACTIVE loans; see the
// Portfolio doc). Backs GET /v1/lending/portfolio.
func (s *Store) Portfolio(ctx context.Context, tenantID uuid.UUID, now time.Time) (Portfolio, error) {
	var p Portfolio
	p.ComputedAt = now.UTC()
	par30Cutoff := now.AddDate(0, 0, -30)
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(outstanding_kobo),0), count(*)
			   FROM loan_accounts WHERE tenant_id=$1 AND status='active'`,
			tenantID).Scan(&p.TotalOutstandingKobo, &p.ActiveCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM loan_accounts WHERE tenant_id=$1 AND status='repaid'`,
			tenantID).Scan(&p.RepaidCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM loan_accounts WHERE tenant_id=$1 AND status='defaulted'`,
			tenantID).Scan(&p.DefaultedCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(outstanding_kobo),0)
			   FROM loan_accounts
			  WHERE tenant_id=$1 AND status='active' AND due_at < $2`,
			tenantID, par30Cutoff).Scan(&p.PAR30OutstandingKobo); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return p, err
	}
	if p.TotalOutstandingKobo > 0 {
		ratio := float64(p.PAR30OutstandingKobo) / float64(p.TotalOutstandingKobo)
		p.PAR30 = &ratio
	}
	return p, nil
}

// LedgerBalance returns (credits − debits) of one account for one
// beneficiary ("" = house side), in kobo (backs PostgresLedger.Balance).
func (s *Store) LedgerBalance(ctx context.Context, tenantID uuid.UUID, accountCode int, beneficiaryID string) (int64, error) {
	var bal int64
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(credit_kobo),0) - COALESCE(SUM(debit_kobo),0)
			   FROM lending_ledger
			  WHERE tenant_id=$1 AND account_code=$2 AND beneficiary_id=$3`,
			tenantID, accountCode, beneficiaryID).Scan(&bal)
	})
	return bal, err
}

// ListLedgerEntries lists ledger rows of a tenant in [from,to] (nil =
// unbounded), oldest first. beneficiaryID "" = all beneficiaries.
func (s *Store) ListLedgerEntries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time, beneficiaryID string) ([]LedgerEntry, error) {
	q := `SELECT ` + ledgerCols + ` FROM lending_ledger WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if beneficiaryID != "" {
		n++
		q += fmt.Sprintf(` AND beneficiary_id=$%d`, n)
		args = append(args, beneficiaryID)
	}
	if from != nil {
		n++
		q += fmt.Sprintf(` AND created_at >= $%d`, n)
		args = append(args, from.UTC())
	}
	if to != nil {
		n++
		q += fmt.Sprintf(` AND created_at <= $%d`, n)
		args = append(args, to.UTC())
	}
	q += ` ORDER BY created_at, id LIMIT 1000`
	out := []LedgerEntry{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanLedgerEntry(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}
