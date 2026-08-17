//! Ledger abstraction (SPEC §9, ADR-0007).
//!
//! Payments is ledger-centric: all money movement is modelled as double-entry
//! transfers between accounts, mirroring TigerBeetle's data model (accounts with
//! `debits_pending`/`credits_pending`/`debits_posted`/`credits_posted`, transfers
//! with pending/posted/voided states, idempotent by transfer id).
//!
//! Two implementations exist behind the [`LedgerClient`] trait, selected by the
//! `LEDGER_IMPL` env var:
//! - [`sim::SimLedgerClient`] — default; in-memory double-entry ledger for dev/CI.
//! - `tigerbeetle::TigerBeetleClient` — live client, only compiled with the
//!   `tb-live` cargo feature.
//!
//! ## TigerBeetle code-matching rule (SPEC-W42, recon R1 — live-proven)
//!
//! Real TigerBeetle requires every `POST_PENDING_TRANSFER` /
//! `VOID_PENDING_TRANSFER` leg to carry the SAME `code` as the pending
//! transfer it resolves; a mismatch is rejected with
//! `pending_transfer_has_different_code` (result 30) and the whole LINKED
//! batch rolls back. Deposit holds are [`CODE_DEPOSIT_HOLD`], so every
//! post/void leg of a deposit hold carries [`CODE_DEPOSIT_HOLD`] too. Only
//! auxiliary NON-pending legs linked after the post (the revenue /
//! platform-fee split) keep the operation's own code ([`CODE_CAPTURE`],
//! [`CODE_NO_SHOW_FEE`]). The sim ledger intentionally does NOT enforce this
//! rule (see the note in `sim.rs`).

pub mod sim;
#[cfg(feature = "tb-live")]
pub mod tigerbeetle;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

/// TigerBeetle ledger number (SPEC §9: cluster 0). We use a single ledger.
pub const LEDGER_ID: u32 = 1;

// ---------------------------------------------------------------------------
// Transfer codes (SPEC §9)
// ---------------------------------------------------------------------------
// NOTE (TB code-matching rule, SPEC-W42): a POST_PENDING_TRANSFER or
// VOID_PENDING_TRANSFER leg must carry the SAME code as the pending hold it
// resolves, i.e. CODE_DEPOSIT_HOLD for deposit holds — real TigerBeetle
// rejects a mismatch with `pending_transfer_has_different_code`. Codes
// 101..=104 below apply to plain (non-pending-resolving) transfers only.
pub const CODE_DEPOSIT_HOLD: u16 = 100; // deposit hold (pending) AND every post/void leg resolving it
pub const CODE_CAPTURE: u16 = 101; // auxiliary revenue/fee split legs of a capture (non-pending)
pub const CODE_REFUND: u16 = 102; // posted refund legs (a hold's VOID leg carries 100 instead)
pub const CODE_NO_SHOW_FEE: u16 = 103; // auxiliary split legs of a no-show fee (non-pending)
pub const CODE_PAYOUT: u16 = 104; // payout of tenant earnings (Mojaloop rail)

// ---------------------------------------------------------------------------
// Account codes (user-defined classification)
// ---------------------------------------------------------------------------
pub const ACCOUNT_CODE_TENANT_DEPOSITS: u16 = 10;
pub const ACCOUNT_CODE_TENANT_REVENUE: u16 = 20;
pub const ACCOUNT_CODE_PLATFORM_FEES: u16 = 30;
pub const ACCOUNT_CODE_PLATFORM_CLEARING: u16 = 31;
pub const ACCOUNT_CODE_PLATFORM_PAYOUTS: u16 = 32;

// ---------------------------------------------------------------------------
// Account naming (SPEC §9: `tenant:{id}:deposits`, `tenant:{id}:revenue`,
// plus platform-level accounts)
// ---------------------------------------------------------------------------
pub fn deposits_account(tenant_id: &str) -> String {
    format!("tenant:{tenant_id}:deposits")
}
pub fn revenue_account(tenant_id: &str) -> String {
    format!("tenant:{tenant_id}:revenue")
}
/// Platform revenue from fees.
pub const PLATFORM_FEES_ACCOUNT: &str = "platform:fees";
/// Customer cash received / refunds paid out (asset from the platform view).
pub const PLATFORM_CLEARING_ACCOUNT: &str = "platform:clearing";
/// Clearing account for payouts executed over the Mojaloop rail.
pub const PLATFORM_PAYOUTS_ACCOUNT: &str = "platform:payouts";

/// Accounts on which debits must never exceed credits (no overdraft), matching
/// TigerBeetle's `flags.debits_must_not_exceed_credits`.
pub fn no_overdraft(name: &str) -> bool {
    name.ends_with(":deposits") || name.ends_with(":revenue")
}

/// Deterministic 128-bit account id derived from the account name, so the sim
/// and the live TigerBeetle client agree on ids.
pub fn account_id(name: &str) -> u128 {
    Uuid::new_v5(&Uuid::NAMESPACE_URL, name.as_bytes()).as_u128()
}

// ---------------------------------------------------------------------------
// Data model
// ---------------------------------------------------------------------------
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TransferState {
    Pending,
    Posted,
    Voided,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TransferFlag {
    /// Plain (two-phase commit disabled) transfer.
    None,
    /// Posts a pending transfer (`pending_id` set); resolves the hold.
    PostPending,
    /// Voids a pending transfer (`pending_id` set); releases the hold.
    VoidPending,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Account {
    pub id: u128,
    pub name: String,
    pub ledger: u32,
    pub code: u16,
    pub debits_pending: u64,
    pub credits_pending: u64,
    pub debits_posted: u64,
    pub credits_posted: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Transfer {
    pub id: u128,
    pub debit_account: String,
    pub credit_account: String,
    /// Amount in minor units (cents).
    pub amount: u64,
    pub ledger: u32,
    pub code: u16,
    pub state: TransferState,
    pub flag: TransferFlag,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pending_id: Option<u128>,
    pub created_at: DateTime<Utc>,
}

impl Transfer {
    pub fn id_string(&self) -> String {
        format!("{:032x}", self.id)
    }
}

/// Result of capture / no-show-fee: the posting transfer that resolved the
/// hold plus the revenue/platform-fee split of the captured amount.
#[derive(Debug, Clone, Serialize)]
pub struct CaptureResult {
    pub post: Transfer,
    pub revenue: Transfer,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub platform_fee: Option<Transfer>,
}

#[derive(Debug, Clone, Serialize)]
pub struct AccountBalance {
    pub account: String,
    pub id: String,
    pub debits_pending: u64,
    pub credits_pending: u64,
    pub debits_posted: u64,
    pub credits_posted: u64,
    /// credits_posted - debits_posted (positive on liability-style accounts
    /// such as tenant deposits/revenue means value held on behalf of tenant).
    pub posted_net: i128,
    /// credits_pending - debits_pending.
    pub pending_net: i128,
}

#[derive(Debug, Clone, Serialize)]
pub struct TenantBalance {
    pub tenant_id: String,
    pub accounts: Vec<AccountBalance>,
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------
#[derive(Debug, Error)]
pub enum LedgerError {
    #[error("account not found: {0}")]
    AccountNotFound(String),
    #[error("transfer not found: {0}")]
    TransferNotFound(String),
    #[error("transfer id {0} already exists with different parameters")]
    ExistsWithDifferentParameters(String),
    #[error("transfer {0} is not a pending deposit hold")]
    NotPending(String),
    #[error("transfer {0} is already resolved (posted or voided)")]
    AlreadyResolved(String),
    #[error("amount exceeds the pending hold amount")]
    ExceedsPendingAmount,
    #[error("amount must be greater than zero")]
    InvalidAmount,
    #[error("operation would overdraw account {0} (debits must not exceed credits)")]
    ExceedsCredits(String),
    #[error("ledger backend error: {0}")]
    Backend(String),
}

// ---------------------------------------------------------------------------
// LedgerClient trait — the 7 operations required by SPEC §9 / mission contract
// ---------------------------------------------------------------------------
#[async_trait]
pub trait LedgerClient: Send + Sync {
    /// Idempotently create the per-tenant accounts (deposits, revenue) and the
    /// platform accounts. Returns the account snapshots.
    async fn create_accounts(&self, tenant_id: &str) -> Result<Vec<Account>, LedgerError>;

    /// Two-phase hold: pending transfer `platform:clearing -> tenant:{id}:deposits`
    /// with code 100. Idempotent by `transfer_id`.
    async fn hold_deposit(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError>;

    /// Capture a hold: posts the pending transfer (releasing any remainder),
    /// then splits the captured amount from `tenant:{id}:deposits` into
    /// `tenant:{id}:revenue` (net) and `platform:fees` (fee).
    /// `amount = None` captures the full hold. Idempotent by `hold_id`.
    ///
    /// Codes (TB rule): the POST_PENDING_TRANSFER leg carries the hold's own
    /// code [`CODE_DEPOSIT_HOLD`]; the auxiliary non-pending split legs carry
    /// [`CODE_CAPTURE`].
    async fn capture(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        amount: Option<u64>,
    ) -> Result<CaptureResult, LedgerError>;

    /// Refund. If `hold_id` refers to a still-pending hold, the hold is
    /// voided (flag void_pending, `amount` ignored); per the TB code-matching
    /// rule that VOID leg carries the hold's own code [`CODE_DEPOSIT_HOLD`]
    /// on the wire (the sim records [`CODE_REFUND`] for observability — sim
    /// does not enforce code matching). Otherwise money is moved back:
    /// `tenant:{id}:revenue -> platform:clearing` with [`CODE_REFUND`].
    /// Idempotent by `transfer_id`.
    async fn refund(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        hold_id: Option<Uuid>,
        amount: u64,
    ) -> Result<Transfer, LedgerError>;

    /// No-show fee: posts `amount` of the pending hold (releasing the
    /// remainder) and splits it into tenant revenue / platform fee.
    /// Idempotent by `hold_id`.
    ///
    /// Codes (TB rule): the POST_PENDING_TRANSFER leg carries the hold's own
    /// code [`CODE_DEPOSIT_HOLD`]; the auxiliary non-pending split legs carry
    /// [`CODE_NO_SHOW_FEE`].
    async fn no_show_fee(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<CaptureResult, LedgerError>;

    /// Payout (code 104): `tenant:{id}:revenue -> platform:payouts`. The
    /// clearing credit represents the obligation settled over Mojaloop.
    /// Idempotent by `transfer_id`.
    async fn payout(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError>;

    /// Balance snapshot for the tenant's accounts.
    async fn balance(&self, tenant_id: &str) -> Result<TenantBalance, LedgerError>;
}

/// Derive a deterministic transfer id from an idempotency key (or random when
/// no key is supplied).
pub fn transfer_id_from_key(key: Option<&str>) -> Uuid {
    match key {
        Some(k) if !k.is_empty() => Uuid::new_v5(&Uuid::NAMESPACE_URL, k.as_bytes()),
        _ => Uuid::new_v4(),
    }
}
