//! Billing ledger (SPEC-W7 B2): receivables double-entry behind the same
//! SimLedgerClient trait pattern as payments-service (ADR-0007).
//!
//! Accounts (ledger codes per SPEC-W7):
//!   200 = AR-control      `tenant:{id}:ar`        (asset: money owed to us)
//!   201 = revenue         `tenant:{id}:revenue`   (income)
//!   202 = payments-clearing `platform:billing:clearing` (cash received)
//!
//! Postings:
//!   invoice issued -> DR AR-control   / CR revenue           (code 200)
//!   invoice paid   -> DR clearing     / CR AR-control        (code 202)
//!   invoice voided -> DR revenue      / CR AR-control        (code 201,
//!   reversal of the issued entry for issued/past_due voids — SPEC-W43 B-02)
//!
//! Transfers are posted (single-phase) and idempotent by transfer id, which
//! callers derive deterministically from the invoice id — webhook retries and
//! consumer redeliveries replay without double-posting.
//!
//! SPEC-W43 B-03: the postgres backend can post INSIDE the caller's open
//! sqlx transaction (`*_in_tx` variants), so the ledger write commits
//! atomically with the invoice state transition — a crash between the two
//! can no longer strand an issued/paid/void invoice without its ledger
//! entry. The sim backend cannot enlist (dev/CI only, non-durable anyway);
//! its `*_in_tx` variants return `Ok(None)` and the caller posts after
//! commit as before.

use std::collections::HashMap;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::Serialize;
use sqlx::{PgPool, Postgres, Row, Transaction};
use thiserror::Error;
use tokio::sync::Mutex;
use uuid::Uuid;

pub const LEDGER_ID: u32 = 1;

// Account codes (SPEC-W7 B2).
pub const ACCOUNT_CODE_AR_CONTROL: u16 = 200;
pub const ACCOUNT_CODE_REVENUE: u16 = 201;
pub const ACCOUNT_CODE_CLEARING: u16 = 202;

// Transfer codes: the debited control account's code.
pub const CODE_INVOICE_ISSUED: u16 = 200; // DR AR / CR revenue
pub const CODE_INVOICE_PAID: u16 = 202; // DR clearing / CR AR
pub const CODE_INVOICE_VOIDED: u16 = 201; // DR revenue / CR AR (reversal)

pub fn ar_account(tenant_id: &str) -> String {
    format!("tenant:{tenant_id}:ar")
}
pub fn revenue_account(tenant_id: &str) -> String {
    format!("tenant:{tenant_id}:revenue")
}
pub const CLEARING_ACCOUNT: &str = "platform:billing:clearing";

#[derive(Debug, Clone, Serialize)]
pub struct Account {
    pub id: u128,
    pub name: String,
    pub ledger: u32,
    pub code: u16,
    pub debits_posted: u64,
    pub credits_posted: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct Transfer {
    pub id: u128,
    pub debit_account: String,
    pub credit_account: String,
    /// Amount in minor units (cents).
    pub amount: u64,
    pub ledger: u32,
    pub code: u16,
    pub created_at: DateTime<Utc>,
}

impl Transfer {
    pub fn id_string(&self) -> String {
        format!("{:032x}", self.id)
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct AccountBalance {
    pub account: String,
    pub debits_posted: u64,
    pub credits_posted: u64,
    /// credits_posted - debits_posted.
    pub posted_net: i128,
}

#[derive(Debug, Clone, Serialize)]
pub struct TenantBalance {
    pub tenant_id: String,
    pub accounts: Vec<AccountBalance>,
}

#[derive(Debug, Error)]
pub enum LedgerError {
    #[error("transfer id {0} already exists with different parameters")]
    ExistsWithDifferentParameters(String),
    #[error("amount must be greater than zero")]
    InvalidAmount,
    #[error("ledger backend error: {0}")]
    Backend(String),
}

/// The receivables posting interface. `post` is the primitive; the two
/// helpers encode the SPEC-W7 posting rules.
#[async_trait]
pub trait BillingLedger: Send + Sync {
    async fn post(
        &self,
        transfer_id: Uuid,
        debit: &str,
        credit: &str,
        amount: u64,
        code: u16,
    ) -> Result<Transfer, LedgerError>;

    async fn balance(&self, tenant_id: &str) -> Result<TenantBalance, LedgerError>;

    /// Invoice issued: DR AR-control / CR revenue (code 200).
    async fn invoice_issued(
        &self,
        tenant_id: &str,
        invoice_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        let id = Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("billing-issued:{invoice_id}").as_bytes(),
        );
        self.post(
            id,
            &ar_account(tenant_id),
            &revenue_account(tenant_id),
            amount,
            CODE_INVOICE_ISSUED,
        )
        .await
    }

    /// Invoice paid: DR payments-clearing / CR AR-control (code 202).
    async fn invoice_paid(
        &self,
        tenant_id: &str,
        invoice_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        let id = Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("billing-paid:{invoice_id}").as_bytes(),
        );
        self.post(
            id,
            CLEARING_ACCOUNT,
            &ar_account(tenant_id),
            amount,
            CODE_INVOICE_PAID,
        )
        .await
    }

    /// Invoice voided after issuance (SPEC-W43 B-02): reversing entry
    /// DR revenue / CR AR-control (code 201), deterministic transfer id
    /// `billing-void:{invoice_id}` so void retries replay idempotently.
    /// Only meaningful for voids from issued/past_due; voiding a draft has
    /// no ledger effect (the caller skips this entirely).
    async fn invoice_voided(
        &self,
        tenant_id: &str,
        invoice_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        let id = Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("billing-void:{invoice_id}").as_bytes(),
        );
        self.post(
            id,
            &revenue_account(tenant_id),
            &ar_account(tenant_id),
            amount,
            CODE_INVOICE_VOIDED,
        )
        .await
    }

    // -----------------------------------------------------------------------
    // Transaction-enlisting variants (SPEC-W43 B-03, crash safety).
    //
    // The postgres backend overrides these to post INSIDE the caller's open
    // transaction, so the ledger entry and the invoice status transition
    // commit — or roll back — atomically. They return `Ok(Some(transfer))`
    // when the posting was enlisted. The default returns `Ok(None)`
    // ("cannot enlist"): the sim backend has no sqlx transaction to join
    // (dev/CI only, non-durable by design), and callers fall back to the
    // post-commit `invoice_*` helpers above.
    // -----------------------------------------------------------------------

    /// In-transaction variant of [`BillingLedger::invoice_issued`].
    async fn invoice_issued_in_tx(
        &self,
        _tx: &mut Transaction<'_, Postgres>,
        _tenant_id: &str,
        _invoice_id: Uuid,
        _amount: u64,
    ) -> Result<Option<Transfer>, LedgerError> {
        Ok(None)
    }

    /// In-transaction variant of [`BillingLedger::invoice_paid`].
    async fn invoice_paid_in_tx(
        &self,
        _tx: &mut Transaction<'_, Postgres>,
        _tenant_id: &str,
        _invoice_id: Uuid,
        _amount: u64,
    ) -> Result<Option<Transfer>, LedgerError> {
        Ok(None)
    }

    /// In-transaction variant of [`BillingLedger::invoice_voided`].
    async fn invoice_voided_in_tx(
        &self,
        _tx: &mut Transaction<'_, Postgres>,
        _tenant_id: &str,
        _invoice_id: Uuid,
        _amount: u64,
    ) -> Result<Option<Transfer>, LedgerError> {
        Ok(None)
    }
}

/// Deterministic 128-bit account id from the account name (same derivation as
/// payments-service, so a future TigerBeetle backend can share ids).
fn account_id(name: &str) -> u128 {
    Uuid::new_v5(&Uuid::NAMESPACE_URL, name.as_bytes()).as_u128()
}

/// Account code inferred from the account name (SPEC-W7 B2 naming).
fn code_for_account(name: &str) -> u16 {
    if name.ends_with(":ar") {
        ACCOUNT_CODE_AR_CONTROL
    } else if name.ends_with(":revenue") {
        ACCOUNT_CODE_REVENUE
    } else {
        ACCOUNT_CODE_CLEARING
    }
}

// ---------------------------------------------------------------------------
// SimLedgerClient: in-memory double-entry default (ADR-0007 fallback).
// ---------------------------------------------------------------------------

#[derive(Debug, Default, Clone)]
struct SimState {
    accounts: HashMap<String, Account>,
    transfers: HashMap<u128, Transfer>,
}

pub struct SimLedgerClient {
    state: Mutex<SimState>,
    ledger_id: u32,
}

impl SimLedgerClient {
    pub fn new() -> Self {
        Self {
            state: Mutex::new(SimState::default()),
            ledger_id: LEDGER_ID,
        }
    }

    #[cfg(test)]
    async fn snapshot(&self) -> SimState {
        self.state.lock().await.clone()
    }
}

impl Default for SimLedgerClient {
    fn default() -> Self {
        Self::new()
    }
}

#[async_trait]
impl BillingLedger for SimLedgerClient {
    async fn post(
        &self,
        transfer_id: Uuid,
        debit: &str,
        credit: &str,
        amount: u64,
        code: u16,
    ) -> Result<Transfer, LedgerError> {
        if amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let mut st = self.state.lock().await;

        // Idempotent replay: same id + same parameters returns the recorded
        // transfer; same id + different parameters is a conflict.
        if let Some(existing) = st.transfers.get(&transfer_id.as_u128()) {
            let same = existing.debit_account == debit
                && existing.credit_account == credit
                && existing.amount == amount
                && existing.code == code;
            if same {
                return Ok(existing.clone());
            }
            return Err(LedgerError::ExistsWithDifferentParameters(
                transfer_id.to_string(),
            ));
        }

        for name in [debit, credit] {
            st.accounts.entry(name.to_string()).or_insert_with(|| Account {
                id: account_id(name),
                name: name.to_string(),
                ledger: self.ledger_id,
                code: code_for_account(name),
                debits_posted: 0,
                credits_posted: 0,
            });
        }

        let t = Transfer {
            id: transfer_id.as_u128(),
            debit_account: debit.to_string(),
            credit_account: credit.to_string(),
            amount,
            ledger: self.ledger_id,
            code,
            created_at: Utc::now(),
        };
        st.accounts
            .get_mut(debit)
            .expect("account just ensured")
            .debits_posted += amount;
        st.accounts
            .get_mut(credit)
            .expect("account just ensured")
            .credits_posted += amount;
        st.transfers.insert(t.id, t.clone());
        Ok(t)
    }

    async fn balance(&self, tenant_id: &str) -> Result<TenantBalance, LedgerError> {
        let st = self.state.lock().await;
        let prefix = format!("tenant:{tenant_id}:");
        let mut accounts: Vec<AccountBalance> = st
            .accounts
            .values()
            .filter(|a| a.name.starts_with(&prefix))
            .map(|a| AccountBalance {
                account: a.name.clone(),
                debits_posted: a.debits_posted,
                credits_posted: a.credits_posted,
                posted_net: a.credits_posted as i128 - a.debits_posted as i128,
            })
            .collect();
        accounts.sort_by(|a, b| a.account.cmp(&b.account));
        Ok(TenantBalance {
            tenant_id: tenant_id.to_string(),
            accounts,
        })
    }
}

// ---------------------------------------------------------------------------
// PgLedgerClient: durable Postgres-backed implementation (SIM-029 default).
// ---------------------------------------------------------------------------

/// Postgres-backed receivables ledger (SIM-029). Selected by default via
/// BILLING_LEDGER_IMPL=postgres; the same double-entry posting rules and
/// idempotency-by-transfer-id semantics as the sim, but durable across
/// restarts. Posting runs in one transaction: replay check, account
/// upserts, balance updates, and the transfer insert commit atomically.
pub struct PgLedgerClient {
    pool: PgPool,
    ledger_id: u32,
}

fn pg_err(context: &str, e: sqlx::Error) -> LedgerError {
    LedgerError::Backend(format!("{context}: {e}"))
}

impl PgLedgerClient {
    /// Fail-closed construction: verifies the ledger tables exist (migration
    /// 0003) so a mis-provisioned database fails the boot loudly instead of
    /// failing the first posting.
    pub async fn new(pool: PgPool) -> Result<Self, LedgerError> {
        sqlx::query("SELECT id FROM ledger_transfers WHERE false")
            .fetch_optional(&pool)
            .await
            .map_err(|e| pg_err("ledger tables unavailable (migration 0003 applied?)", e))?;
        Ok(Self {
            pool,
            ledger_id: LEDGER_ID,
        })
    }
}

impl PgLedgerClient {
    /// Post one transfer inside `tx`. The replay check, account upserts,
    /// balance updates, and the transfer insert all run on the caller's
    /// transaction; committing (or rolling back) is the caller's job, which
    /// is what lets the invoice state machine and the ledger commit
    /// atomically (SPEC-W43 B-03).
    async fn post_in(
        &self,
        tx: &mut Transaction<'_, Postgres>,
        transfer_id: Uuid,
        debit: &str,
        credit: &str,
        amount: u64,
        code: u16,
    ) -> Result<Transfer, LedgerError> {
        if amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let amt = i64::try_from(amount).map_err(|_| LedgerError::InvalidAmount)?;

        // Idempotent replay: same id + same parameters returns the recorded
        // transfer; same id + different parameters is a conflict.
        let existing = sqlx::query(
            "SELECT debit_account, credit_account, amount, code, created_at \
             FROM ledger_transfers WHERE id = $1",
        )
        .bind(transfer_id)
        .fetch_optional(&mut **tx)
        .await
        .map_err(|e| pg_err("replay check", e))?;
        if let Some(row) = existing {
            let ex_debit: String = row.try_get("debit_account").map_err(|e| pg_err("decode", e))?;
            let ex_credit: String = row.try_get("credit_account").map_err(|e| pg_err("decode", e))?;
            let ex_amount: i64 = row.try_get("amount").map_err(|e| pg_err("decode", e))?;
            let ex_code: i32 = row.try_get("code").map_err(|e| pg_err("decode", e))?;
            let ex_created: DateTime<Utc> =
                row.try_get("created_at").map_err(|e| pg_err("decode", e))?;
            let same = ex_debit == debit
                && ex_credit == credit
                && ex_amount == amt
                && ex_code == i32::from(code);
            if same {
                return Ok(Transfer {
                    id: transfer_id.as_u128(),
                    debit_account: ex_debit,
                    credit_account: ex_credit,
                    amount,
                    ledger: self.ledger_id,
                    code,
                    created_at: ex_created,
                });
            }
            return Err(LedgerError::ExistsWithDifferentParameters(
                transfer_id.to_string(),
            ));
        }

        for name in [debit, credit] {
            sqlx::query(
                "INSERT INTO ledger_accounts (name, id, ledger, code) \
                 VALUES ($1, $2, $3, $4) ON CONFLICT (name) DO NOTHING",
            )
            .bind(name)
            .bind(format!("{:032x}", account_id(name)))
            .bind(self.ledger_id as i32)
            .bind(i32::from(code_for_account(name)))
            .execute(&mut **tx)
            .await
            .map_err(|e| pg_err("ensure account", e))?;
        }
        sqlx::query("UPDATE ledger_accounts SET debits_posted = debits_posted + $2 WHERE name = $1")
            .bind(debit)
            .bind(amt)
            .execute(&mut **tx)
            .await
            .map_err(|e| pg_err("debit update", e))?;
        sqlx::query("UPDATE ledger_accounts SET credits_posted = credits_posted + $2 WHERE name = $1")
            .bind(credit)
            .bind(amt)
            .execute(&mut **tx)
            .await
            .map_err(|e| pg_err("credit update", e))?;

        let created_at = Utc::now();
        sqlx::query(
            "INSERT INTO ledger_transfers \
             (id, debit_account, credit_account, amount, ledger, code, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7)",
        )
        .bind(transfer_id)
        .bind(debit)
        .bind(credit)
        .bind(amt)
        .bind(self.ledger_id as i32)
        .bind(i32::from(code))
        .bind(created_at)
        .execute(&mut **tx)
        .await
        .map_err(|e| pg_err("insert transfer", e))?;

        Ok(Transfer {
            id: transfer_id.as_u128(),
            debit_account: debit.to_string(),
            credit_account: credit.to_string(),
            amount,
            ledger: self.ledger_id,
            code,
            created_at,
        })
    }
}

#[async_trait]
impl BillingLedger for PgLedgerClient {
    async fn post(
        &self,
        transfer_id: Uuid,
        debit: &str,
        credit: &str,
        amount: u64,
        code: u16,
    ) -> Result<Transfer, LedgerError> {
        let mut tx = self.pool.begin().await.map_err(|e| pg_err("begin", e))?;
        let transfer = self
            .post_in(&mut tx, transfer_id, debit, credit, amount, code)
            .await?;
        tx.commit().await.map_err(|e| pg_err("commit", e))?;
        Ok(transfer)
    }

    async fn invoice_issued_in_tx(
        &self,
        tx: &mut Transaction<'_, Postgres>,
        tenant_id: &str,
        invoice_id: Uuid,
        amount: u64,
    ) -> Result<Option<Transfer>, LedgerError> {
        let id = Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("billing-issued:{invoice_id}").as_bytes(),
        );
        self.post_in(
            tx,
            id,
            &ar_account(tenant_id),
            &revenue_account(tenant_id),
            amount,
            CODE_INVOICE_ISSUED,
        )
        .await
        .map(Some)
    }

    async fn invoice_paid_in_tx(
        &self,
        tx: &mut Transaction<'_, Postgres>,
        tenant_id: &str,
        invoice_id: Uuid,
        amount: u64,
    ) -> Result<Option<Transfer>, LedgerError> {
        let id = Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("billing-paid:{invoice_id}").as_bytes(),
        );
        self.post_in(
            tx,
            id,
            CLEARING_ACCOUNT,
            &ar_account(tenant_id),
            amount,
            CODE_INVOICE_PAID,
        )
        .await
        .map(Some)
    }

    async fn invoice_voided_in_tx(
        &self,
        tx: &mut Transaction<'_, Postgres>,
        tenant_id: &str,
        invoice_id: Uuid,
        amount: u64,
    ) -> Result<Option<Transfer>, LedgerError> {
        let id = Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("billing-void:{invoice_id}").as_bytes(),
        );
        self.post_in(
            tx,
            id,
            &revenue_account(tenant_id),
            &ar_account(tenant_id),
            amount,
            CODE_INVOICE_VOIDED,
        )
        .await
        .map(Some)
    }

    async fn balance(&self, tenant_id: &str) -> Result<TenantBalance, LedgerError> {
        // Tenant ids are UUIDs in billing, so the prefix contains no LIKE
        // metacharacters.
        let rows = sqlx::query(
            "SELECT name, debits_posted, credits_posted FROM ledger_accounts \
             WHERE name LIKE $1 ORDER BY name",
        )
        .bind(format!("tenant:{tenant_id}:%"))
        .fetch_all(&self.pool)
        .await
        .map_err(|e| pg_err("balance", e))?;
        let accounts = rows
            .iter()
            .map(|r| -> Result<AccountBalance, LedgerError> {
                let debits: i64 = r.try_get("debits_posted").map_err(|e| pg_err("decode", e))?;
                let credits: i64 = r.try_get("credits_posted").map_err(|e| pg_err("decode", e))?;
                Ok(AccountBalance {
                    account: r.try_get("name").map_err(|e| pg_err("decode", e))?,
                    debits_posted: debits as u64,
                    credits_posted: credits as u64,
                    posted_net: credits as i128 - debits as i128,
                })
            })
            .collect::<Result<Vec<_>, _>>()?;
        Ok(TenantBalance {
            tenant_id: tenant_id.to_string(),
            accounts,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TENANT: &str = "t-1";

    async fn assert_conservation(client: &SimLedgerClient) {
        let st = client.snapshot().await;
        let (mut d, mut c) = (0u128, 0u128);
        for a in st.accounts.values() {
            d += a.debits_posted as u128;
            c += a.credits_posted as u128;
        }
        assert_eq!(d, c, "double-entry conservation violated");
    }

    #[tokio::test]
    async fn issued_then_paid_moves_ar_to_clearing() {
        let c = SimLedgerClient::new();
        let invoice = Uuid::new_v4();
        let t1 = c.invoice_issued(TENANT, invoice, 12_500).await.unwrap();
        assert_eq!(t1.code, CODE_INVOICE_ISSUED);
        assert_eq!(t1.debit_account, ar_account(TENANT));
        assert_eq!(t1.credit_account, revenue_account(TENANT));
        assert_conservation(&c).await;

        let t2 = c.invoice_paid(TENANT, invoice, 12_500).await.unwrap();
        assert_eq!(t2.code, CODE_INVOICE_PAID);
        assert_eq!(t2.debit_account, CLEARING_ACCOUNT);
        assert_eq!(t2.credit_account, ar_account(TENANT));
        assert_conservation(&c).await;

        // AR nets to zero once paid; revenue carries the income.
        let bal = c.balance(TENANT).await.unwrap();
        let ar = bal
            .accounts
            .iter()
            .find(|a| a.account == ar_account(TENANT))
            .unwrap();
        assert_eq!(ar.posted_net, 0);
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account == revenue_account(TENANT))
            .unwrap();
        assert_eq!(revenue.posted_net, 12_500);
    }

    #[tokio::test]
    async fn void_reverses_the_issued_posting_idempotently() {
        // SPEC-W43 B-02: voiding an issued invoice posts DR revenue / CR AR
        // keyed billing-void:{invoice_id}; AR and revenue net back to zero
        // and replays do not double-post.
        let c = SimLedgerClient::new();
        let invoice = Uuid::new_v4();
        c.invoice_issued(TENANT, invoice, 7_500).await.unwrap();
        let t = c.invoice_voided(TENANT, invoice, 7_500).await.unwrap();
        assert_eq!(t.code, CODE_INVOICE_VOIDED);
        assert_eq!(t.debit_account, revenue_account(TENANT));
        assert_eq!(t.credit_account, ar_account(TENANT));
        // Replay (void retry) is absorbed.
        c.invoice_voided(TENANT, invoice, 7_500).await.unwrap();
        assert_conservation(&c).await;
        let st = c.snapshot().await;
        assert_eq!(st.transfers.len(), 2, "issued + one reversal only");
        let bal = c.balance(TENANT).await.unwrap();
        for a in &bal.accounts {
            assert_eq!(a.posted_net, 0, "{} must net to zero after void", a.account);
        }
        // A conflicting replay (different amount) is rejected, not absorbed.
        let err = c.invoice_voided(TENANT, invoice, 7_501).await.unwrap_err();
        assert!(matches!(err, LedgerError::ExistsWithDifferentParameters(_)));
    }

    #[tokio::test]
    async fn postings_are_idempotent_by_transfer_id() {
        let c = SimLedgerClient::new();
        let invoice = Uuid::new_v4();
        // Same invoice issued/paid twice (webhook retry): same derived
        // transfer ids replay without double-posting.
        c.invoice_issued(TENANT, invoice, 5_000).await.unwrap();
        c.invoice_issued(TENANT, invoice, 5_000).await.unwrap();
        c.invoice_paid(TENANT, invoice, 5_000).await.unwrap();
        c.invoice_paid(TENANT, invoice, 5_000).await.unwrap();
        assert_conservation(&c).await;
        let st = c.snapshot().await;
        assert_eq!(st.transfers.len(), 2, "no duplicate transfers");
        let clearing = st.accounts.get(CLEARING_ACCOUNT).unwrap();
        assert_eq!(clearing.debits_posted, 5_000);
    }

    #[tokio::test]
    async fn conflicting_replay_and_zero_amount_are_rejected() {
        let c = SimLedgerClient::new();
        let id = Uuid::new_v4();
        c.post(id, "a:x:ar", "a:x:revenue", 100, CODE_INVOICE_ISSUED)
            .await
            .unwrap();
        let err = c
            .post(id, "a:x:ar", "a:x:revenue", 200, CODE_INVOICE_ISSUED)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::ExistsWithDifferentParameters(_)));
        let err = c
            .post(Uuid::new_v4(), "a:x:ar", "a:x:revenue", 0, CODE_INVOICE_ISSUED)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::InvalidAmount));
    }
}
