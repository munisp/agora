//! Rating math + invoice persistence (SPEC-W7 B2).
//!
//! Rating rule: per metric, `billable = max(0, total - included_quota)` and
//! `amount = billable * unit_price_cents`. All money is integer minor units,
//! so multiplication is exact (no float rounding anywhere).

use chrono::{DateTime, NaiveDate, TimeZone, Utc};
use sqlx::{Postgres, Row, Transaction};
use thiserror::Error;
use uuid::Uuid;

use crate::models::{Invoice, InvoiceStatus, LineItem};

#[derive(Debug, Error)]
pub enum BillingError {
    #[error("bad period '{0}' (expected YYYY-MM)")]
    BadPeriod(String),
    #[error("a non-draft invoice already exists for tenant {tenant_id} period {period}")]
    Conflict { tenant_id: String, period: String },
    #[error("invoice not found: {0}")]
    NotFound(String),
    #[error("illegal transition {from} -> {to}")]
    IllegalTransition { from: String, to: String },
    #[error("mixed currencies in rate cards for metric '{metric}': expected {expected}, found {found}")]
    MixedCurrency {
        expected: String,
        found: String,
        metric: String,
    },
    #[error("database error: {0}")]
    Db(String),
}

impl From<sqlx::Error> for BillingError {
    fn from(e: sqlx::Error) -> Self {
        Self::Db(e.to_string())
    }
}

// ---------------------------------------------------------------------------
// Pure rating helpers (unit-tested below; no I/O).
// ---------------------------------------------------------------------------

/// Parse "YYYY-MM" into the half-open UTC window [start, end).
pub fn parse_period(period: &str) -> Result<(DateTime<Utc>, DateTime<Utc>), BillingError> {
    let parts: Vec<&str> = period.split('-').collect();
    if parts.len() != 2 || parts[0].len() != 4 || parts[1].len() != 2 {
        return Err(BillingError::BadPeriod(period.to_string()));
    }
    let year: i32 = parts[0]
        .parse()
        .map_err(|_| BillingError::BadPeriod(period.to_string()))?;
    let month: u32 = parts[1]
        .parse()
        .map_err(|_| BillingError::BadPeriod(period.to_string()))?;
    if !(1..=12).contains(&month) {
        return Err(BillingError::BadPeriod(period.to_string()));
    }
    let start_date = NaiveDate::from_ymd_opt(year, month, 1)
        .ok_or_else(|| BillingError::BadPeriod(period.to_string()))?;
    let (ny, nm) = if month == 12 { (year + 1, 1) } else { (year, month + 1) };
    let end_date = NaiveDate::from_ymd_opt(ny, nm, 1)
        .ok_or_else(|| BillingError::BadPeriod(period.to_string()))?;
    let start = Utc.from_utc_datetime(
        &start_date.and_hms_opt(0, 0, 0).expect("midnight is a valid time"),
    );
    let end = Utc.from_utc_datetime(
        &end_date.and_hms_opt(0, 0, 0).expect("midnight is a valid time"),
    );
    Ok((start, end))
}

/// Rate one metric line. Integer math only: `amount` is exact by
/// construction; `saturating_mul` guards against pathological inputs instead
/// of panicking on overflow.
pub fn rate_line(
    metric: &str,
    total: i64,
    unit_price_cents: i64,
    included_quota: i64,
) -> LineItem {
    let billable = (total - included_quota).max(0);
    LineItem {
        metric: metric.to_string(),
        quantity: total,
        included_quota,
        billable,
        unit_price_cents,
        amount_cents: billable.saturating_mul(unit_price_cents),
    }
}

// ---------------------------------------------------------------------------
// Persistence (SPEC-W34 GF6)
//
// Every function takes an open transaction whose `app.tenant_id` GUC has
// already been set transaction-locally by the caller (see tenant.rs) — the
// 0002 RLS policies are fail-closed without it. Multi-statement operations
// (generate, transitions) now also run atomically inside that transaction.
// ---------------------------------------------------------------------------

struct RateCardRow {
    metric: String,
    unit_price_cents: i64,
    included_quota: i64,
    currency: String,
}

/// Aggregate a tenant's usage for the period, rate it against the tenant's
/// rate cards, and create (or replace a draft) invoice.
pub async fn generate_invoice(
    tx: &mut Transaction<'_, Postgres>,
    tenant_id: Uuid,
    period: &str,
) -> Result<Invoice, BillingError> {
    let (start, end) = parse_period(period)?;

    // Existing non-void invoice for (tenant, period)?
    let existing = sqlx::query(
        "SELECT id, status FROM invoices WHERE tenant_id = $1 AND period = $2 AND status <> 'void'",
    )
    .bind(tenant_id)
    .bind(period)
    .fetch_optional(&mut **tx)
    .await?;
    let existing_id: Option<Uuid> = match &existing {
        Some(row) => {
            let status: String = row.try_get("status")?;
            if status != InvoiceStatus::Draft.as_str() {
                return Err(BillingError::Conflict {
                    tenant_id: tenant_id.to_string(),
                    period: period.to_string(),
                });
            }
            Some(row.try_get("id")?)
        }
        None => None,
    };

    // Aggregate usage for the period. SUM(bigint) is numeric in Postgres, so
    // cast back to bigint explicitly for sqlx decoding.
    let usage_rows = sqlx::query(
        "SELECT metric, CAST(COALESCE(SUM(value), 0) AS bigint) AS total \
         FROM usage_records \
         WHERE tenant_id = $1 AND ts >= $2 AND ts < $3 \
         GROUP BY metric ORDER BY metric",
    )
    .bind(tenant_id)
    .bind(start)
    .bind(end)
    .fetch_all(&mut **tx)
    .await?;

    let card_rows = sqlx::query(
        "SELECT metric, unit_price_cents, included_quota, currency \
         FROM rate_cards WHERE tenant_id = $1 ORDER BY metric",
    )
    .bind(tenant_id)
    .fetch_all(&mut **tx)
    .await?;
    let cards: Vec<RateCardRow> = card_rows
        .iter()
        .map(|r| {
            Ok(RateCardRow {
                metric: r.try_get("metric")?,
                unit_price_cents: r.try_get("unit_price_cents")?,
                included_quota: r.try_get("included_quota")?,
                currency: r.try_get("currency")?,
            })
        })
        .collect::<Result<Vec<_>, sqlx::Error>>()?;

    // Only metrics with a rate card are billed (others stay metered but free).
    let mut line_items: Vec<LineItem> = Vec::new();
    let mut currency: Option<String> = None;
    for row in &usage_rows {
        let metric: String = row.try_get("metric")?;
        let total: i64 = row.try_get("total")?;
        if let Some(card) = cards.iter().find(|c| c.metric == metric) {
            // SPEC-W43 B-05: an invoice must be single-currency. The rate-card
            // upsert gate (routes.rs) already rejects mixed currencies at
            // write time, so this branch is defense in depth for rows written
            // before the gate existed — refuse loudly instead of silently
            // rating different metrics in different currencies and stamping
            // the invoice with whichever card came last.
            if let Some(existing) = &currency {
                if *existing != card.currency {
                    tracing::error!(
                        expected = %existing,
                        found = %card.currency,
                        metric = %metric,
                        tenant_id = %tenant_id,
                        "refusing to generate a mixed-currency invoice"
                    );
                    return Err(BillingError::MixedCurrency {
                        expected: existing.clone(),
                        found: card.currency.clone(),
                        metric,
                    });
                }
            } else {
                currency = Some(card.currency.clone());
            }
            line_items.push(rate_line(
                &metric,
                total,
                card.unit_price_cents,
                card.included_quota,
            ));
        }
    }
    let currency = currency.unwrap_or_else(|| "USD".to_string());
    let subtotal: i64 = line_items
        .iter()
        .fold(0i64, |acc, li| acc.saturating_add(li.amount_cents));
    let line_items_json = serde_json::to_value(&line_items)
        .map_err(|e| BillingError::Db(format!("line items serialize: {e}")))?;

    let id = match existing_id {
        // Regenerate = replace the draft in place (keep the invoice id stable).
        Some(id) => {
            sqlx::query(
                "UPDATE invoices SET line_items = $1, subtotal_cents = $2, currency = $3 \
                 WHERE id = $4 AND status = 'draft'",
            )
            .bind(&line_items_json)
            .bind(subtotal)
            .bind(&currency)
            .bind(id)
            .execute(&mut **tx)
            .await?;
            id
        }
        None => {
            sqlx::query(
                "INSERT INTO invoices (tenant_id, period, status, subtotal_cents, currency, line_items) \
                 VALUES ($1, $2, 'draft', $3, $4, $5) RETURNING id",
            )
            .bind(tenant_id)
            .bind(period)
            .bind(subtotal)
            .bind(&currency)
            .bind(&line_items_json)
            .fetch_one(&mut **tx)
            .await?
            .try_get("id")?
        }
    };

    get_invoice(tx, id)
        .await?
        .ok_or_else(|| BillingError::NotFound(id.to_string()))
}

/// Load one invoice by id.
pub async fn get_invoice(
    tx: &mut Transaction<'_, Postgres>,
    id: Uuid,
) -> Result<Option<Invoice>, BillingError> {
    let row = sqlx::query(
        "SELECT id, tenant_id, period, status, subtotal_cents, currency, line_items, \
                payment_ref, created_at, issued_at, paid_at \
         FROM invoices WHERE id = $1",
    )
    .bind(id)
    .fetch_optional(&mut **tx)
    .await?;
    row.map(|r| invoice_from_row(&r)).transpose()
}

/// List invoices, optionally filtered by status, always tenant-scoped.
pub async fn list_invoices(
    tx: &mut Transaction<'_, Postgres>,
    tenant_id: Uuid,
    status: Option<InvoiceStatus>,
) -> Result<Vec<Invoice>, BillingError> {
    let rows = match status {
        Some(st) => {
            sqlx::query(
                "SELECT id, tenant_id, period, status, subtotal_cents, currency, line_items, \
                        payment_ref, created_at, issued_at, paid_at \
                 FROM invoices WHERE tenant_id = $1 AND status = $2 \
                 ORDER BY created_at DESC",
            )
            .bind(tenant_id)
            .bind(st.as_str())
            .fetch_all(&mut **tx)
            .await?
        }
        None => {
            sqlx::query(
                "SELECT id, tenant_id, period, status, subtotal_cents, currency, line_items, \
                        payment_ref, created_at, issued_at, paid_at \
                 FROM invoices WHERE tenant_id = $1 \
                 ORDER BY created_at DESC",
            )
            .bind(tenant_id)
            .fetch_all(&mut **tx)
            .await?
        }
    };
    rows.iter().map(invoice_from_row).collect()
}

fn invoice_from_row(row: &sqlx::postgres::PgRow) -> Result<Invoice, BillingError> {
    let status_str: String = row.try_get("status")?;
    let status = InvoiceStatus::parse(&status_str)
        .ok_or_else(|| BillingError::Db(format!("unknown invoice status '{status_str}'")))?;
    let line_items_json: serde_json::Value = row.try_get("line_items")?;
    let line_items: Vec<LineItem> = serde_json::from_value(line_items_json)
        .map_err(|e| BillingError::Db(format!("line items decode: {e}")))?;
    Ok(Invoice {
        id: row.try_get("id")?,
        tenant_id: row.try_get("tenant_id")?,
        period: row.try_get("period")?,
        status,
        subtotal_cents: row.try_get("subtotal_cents")?,
        currency: row.try_get("currency")?,
        line_items,
        payment_ref: row.try_get("payment_ref")?,
        created_at: row.try_get("created_at")?,
        issued_at: row.try_get("issued_at")?,
        paid_at: row.try_get("paid_at")?,
    })
}

/// Apply a state-machine transition. Returns the previous status on success
/// (callers use it for ledger/event side effects). `IllegalTransition` when
/// the current state does not allow the move; `NotFound` when the id is
/// unknown. The `expected_from` guard in SQL makes concurrent transitions
/// safe: exactly one caller wins.
pub async fn transition_invoice(
    tx: &mut Transaction<'_, Postgres>,
    id: Uuid,
    to: InvoiceStatus,
) -> Result<InvoiceStatus, BillingError> {
    let current = match get_invoice(tx, id).await? {
        Some(inv) => inv.status,
        None => return Err(BillingError::NotFound(id.to_string())),
    };
    if !current.can_transition_to(to) {
        return Err(BillingError::IllegalTransition {
            from: current.as_str().to_string(),
            to: to.as_str().to_string(),
        });
    }
    let now_clause = match to {
        InvoiceStatus::Issued => ", issued_at = now()",
        InvoiceStatus::Paid => ", paid_at = now()",
        _ => "",
    };
    let sql = format!(
        "UPDATE invoices SET status = $1{now_clause} WHERE id = $2 AND status = $3"
    );
    let res = sqlx::query(&sql)
        .bind(to.as_str())
        .bind(id)
        .bind(current.as_str())
        .execute(&mut **tx)
        .await?;
    if res.rows_affected() == 0 {
        // Lost a race with a concurrent transition; re-read to report the
        // state we actually saw.
        let seen = get_invoice(tx, id)
            .await?
            .map(|inv| inv.status.as_str().to_string())
            .unwrap_or_else(|| "missing".to_string());
        return Err(BillingError::IllegalTransition {
            from: seen,
            to: to.as_str().to_string(),
        });
    }
    Ok(current)
}

/// Mark an invoice paid idempotently (Paystack webhook path, B3): an
/// already-paid invoice is a 200 replay, not an error. Returns
/// Ok(Some(prev)) when this call performed the transition, Ok(None) when the
/// invoice was already paid.
pub async fn mark_paid_idempotent(
    tx: &mut Transaction<'_, Postgres>,
    id: Uuid,
) -> Result<Option<InvoiceStatus>, BillingError> {
    match get_invoice(tx, id).await? {
        None => Err(BillingError::NotFound(id.to_string())),
        Some(inv) if inv.status == InvoiceStatus::Paid => Ok(None),
        Some(inv) => transition_invoice(tx, id, InvoiceStatus::Paid)
            .await
            .map(|prev| {
                debug_assert_eq!(prev, inv.status);
                Some(prev)
            }),
    }
}

/// Store the payment reference (Paystack reference or static payload ref).
pub async fn set_payment_ref(
    tx: &mut Transaction<'_, Postgres>,
    id: Uuid,
    payment_ref: &str,
) -> Result<(), BillingError> {
    let res = sqlx::query("UPDATE invoices SET payment_ref = $1 WHERE id = $2 AND status <> 'void'")
        .bind(payment_ref)
        .bind(id)
        .execute(&mut **tx)
        .await?;
    if res.rows_affected() == 0 {
        return Err(BillingError::NotFound(id.to_string()));
    }
    Ok(())
}

/// Dunning sweep (B3): issued invoices whose issued_at is older than
/// `due_days` become past_due. Returns the number transitioned.
pub async fn mark_overdue(
    tx: &mut Transaction<'_, Postgres>,
    due_days: i64,
) -> Result<u64, BillingError> {
    let res = sqlx::query(
        "UPDATE invoices SET status = 'past_due' \
         WHERE status = 'issued' AND issued_at IS NOT NULL \
           AND issued_at < now() - ($1 || ' days')::interval",
    )
    .bind(due_days.to_string())
    .execute(&mut **tx)
    .await?;
    Ok(res.rows_affected())
}

// ---------------------------------------------------------------------------
// Unit tests: rating math (quota boundaries, rounding), period parsing.
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Datelike;

    #[test]
    fn rate_line_bills_overage_only() {
        // total 1200, quota 1000, 50c/unit => 200 * 50 = 10_000c.
        let li = rate_line("booking", 1200, 50, 1000);
        assert_eq!(li.quantity, 1200);
        assert_eq!(li.billable, 200);
        assert_eq!(li.amount_cents, 10_000);
    }

    #[test]
    fn rate_line_quota_boundary_is_free() {
        // Exactly at the quota boundary nothing is billed.
        let at = rate_line("booking", 1000, 50, 1000);
        assert_eq!(at.billable, 0);
        assert_eq!(at.amount_cents, 0);
        // One unit over the boundary bills exactly one unit.
        let over = rate_line("booking", 1001, 50, 1000);
        assert_eq!(over.billable, 1);
        assert_eq!(over.amount_cents, 50);
    }

    #[test]
    fn rate_line_under_quota_never_negative() {
        let li = rate_line("message", 10, 1, 1000);
        assert_eq!(li.billable, 0);
        assert_eq!(li.amount_cents, 0);
    }

    #[test]
    fn rate_line_integer_math_is_exact_no_rounding_drift() {
        // Odd quantity x odd price must be exact (integer cents, no floats):
        // 333 x 7 = 2331.
        let li = rate_line("call_minutes", 343, 7, 10);
        assert_eq!(li.billable, 333);
        assert_eq!(li.amount_cents, 2_331);
        // Zero unit price bills nothing regardless of volume.
        assert_eq!(rate_line("booking", 1_000_000, 0, 0).amount_cents, 0);
    }

    #[test]
    fn rate_line_saturates_instead_of_overflowing() {
        let li = rate_line("booking", i64::MAX, i64::MAX, 0);
        assert_eq!(li.amount_cents, i64::MAX);
    }

    #[test]
    fn parse_period_builds_half_open_month_window() {
        let (start, end) = parse_period("2026-03").unwrap();
        assert_eq!(start.year(), 2026);
        assert_eq!(start.month(), 3);
        assert_eq!(start.day(), 1);
        assert_eq!(end.year(), 2026);
        assert_eq!(end.month(), 4);
        assert_eq!(end.day(), 1);
        // December rolls the year.
        let (start, end) = parse_period("2025-12").unwrap();
        assert_eq!((start.year(), start.month()), (2025, 12));
        assert_eq!((end.year(), end.month()), (2026, 1));
    }

    #[test]
    fn parse_period_rejects_malformed_input() {
        for bad in ["2026-3", "26-03", "2026-13", "2026-00", "2026/03", "", "abcd-ef"] {
            assert!(parse_period(bad).is_err(), "expected '{bad}' to fail");
        }
    }
}
