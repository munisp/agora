//! Live TigerBeetle client (`LEDGER_IMPL=tigerbeetle`), compiled only with the
//! `tb-live` cargo feature (ADR-0007).
//!
//! Written against the pinned `tigerbeetle-unofficial 0.8.0+0.16.28` crate
//! (community-published official Rust client for TigerBeetle); the relevant
//! surface, verified against the vendored crate source:
//!   - `tigerbeetle_unofficial::Client::new(cluster_id: u128, addresses: impl AsRef<[u8]>)`
//!   - `Client::{create_accounts, create_transfers}(Vec<_>) -> Result<(), {CreateAccounts,CreateTransfers}Error>`
//!     where the `Api` variant carries per-item errors with `.kind()`
//!     (`error::Create{Account,Transfer}ErrorKind`); `exists` means idempotent replay.
//!   - `Account::new(id: u128, ledger: u32, code: u16)`
//!   - `Transfer::new(id: u128)` with builders `.with_debit_account_id(..)`,
//!     `.with_credit_account_id(..)`, `.with_amount(u128)`, `.with_ledger(..)`,
//!     `.with_code(..)`, `.with_pending_id(..)`, `.with_flags(transfer::Flags::{LINKED,
//!     PENDING, POST_PENDING_TRANSFER, VOID_PENDING_TRANSFER, ..})` (bitflags).
//!   - `Client::lookup_accounts(Vec<u128>)`; balance accessors return `u128`.
//! If the pinned crate version drifts from this surface, this module is the
//! single integration point to adjust. The default build does NOT compile it
//! (the sys crate's build script needs the Zig toolchain/network), so the
//! service always builds green (ADR-0007 fallback to the sim ledger).
//!
//! SPEC-W34 GF11 hardening:
//!   * `submit()` treats TigerBeetle's `exists` result as SUCCESS (idempotent
//!     replay of the same transfer id) — previously any per-item result was
//!     reported as an error, so at-least-once replays of a committed transfer
//!     looked like ledger failures and got dead-lettered;
//!   * SPEC-W42 R2: the idempotent-replay acceptance also covers the LINKED-
//!     batch replay signature (live-proven against TigerBeetle 0.16.28): a
//!     verbatim replay of an already-committed linked capture batch returns
//!     `exists` on leg 0 and `linked_event_failed` on every leg linked after
//!     it (ANY non-`ok` result breaks the chain). `is_idempotent_replay()`
//!     accepts exactly an {Exists, LinkedEventFailed} error set anchored by
//!     at least one Exists; any other error kind still fails (mutation
//!     safety — see the helper's docs).
//!   * capture/no-show-fee post + revenue/fee split is ONE linked
//!     `create_transfers` batch — TigerBeetle applies linked batches
//!     atomically, so a partial capture (hold posted, split lost) can no
//!     longer happen.
//!
//! SPEC-W42 transfer-code correctness (recon R1, live-proven):
//!   * TigerBeetle enforces that a `POST_PENDING_TRANSFER` /
//!     `VOID_PENDING_TRANSFER` leg carries the SAME `code` as the pending
//!     transfer it resolves; a mismatch is rejected with
//!     `pending_transfer_has_different_code` (result 30) and the whole
//!     LINKED batch rolls back. Deposit holds are created with
//!     `CODE_DEPOSIT_HOLD`, so every post/void leg of a deposit hold carries
//!     `CODE_DEPOSIT_HOLD` too. Only the auxiliary NON-pending legs linked
//!     after the post (the revenue / platform-fee split) keep the
//!     operation's own code (`CODE_CAPTURE` / `CODE_NO_SHOW_FEE`).
//!   * The transfer batches are built by pure constructors
//!     (`build_hold_transfer`, `build_void_hold_transfer`,
//!     `build_capture_batch`) so the exact structure sent on the wire is
//!     unit-testable without a server (see the `tests` module below).

use async_trait::async_trait;
use tigerbeetle_unofficial as tb;
use uuid::Uuid;

use super::*;
use tb::error::{CreateAccountErrorKind, CreateAccountsError, CreateTransferErrorKind, CreateTransfersError};
use tb::transfer::Flags as TbFlags;

pub struct TigerBeetleClient {
    client: tb::Client,
    ledger_id: u32,
    fee_bps: u64,
}

fn map_err<E: std::fmt::Display>(e: E) -> LedgerError {
    LedgerError::Backend(e.to_string())
}

// ---------------------------------------------------------------------------
// Pure transfer constructors (no server needed — unit-tested below)
// ---------------------------------------------------------------------------

/// Shared transfer constructor.
fn tb_transfer(
    ledger_id: u32,
    id: u128,
    debit: &str,
    credit: &str,
    amount: u64,
    code: u16,
) -> tb::Transfer {
    tb::Transfer::new(id)
        .with_debit_account_id(account_id(debit))
        .with_credit_account_id(account_id(credit))
        .with_amount(amount as u128)
        .with_ledger(ledger_id)
        .with_code(code)
}

/// Two-phase hold leg: pending `platform:clearing -> tenant:{id}:deposits`
/// with `CODE_DEPOSIT_HOLD` (flag PENDING).
fn build_hold_transfer(
    ledger_id: u32,
    transfer_id: Uuid,
    tenant_id: &str,
    amount: u64,
) -> tb::Transfer {
    tb_transfer(
        ledger_id,
        transfer_id.as_u128(),
        PLATFORM_CLEARING_ACCOUNT,
        &deposits_account(tenant_id),
        amount,
        CODE_DEPOSIT_HOLD,
    )
    .with_flags(TbFlags::PENDING)
}

/// VOID_PENDING_TRANSFER leg resolving a deposit hold.
///
/// TB code-matching rule (SPEC-W42): the void leg MUST carry the hold's own
/// code (`CODE_DEPOSIT_HOLD`), not `CODE_REFUND` — a real server rejects a
/// mismatch with `pending_transfer_has_different_code` and the hold is left
/// unresolved.
///
/// Amount semantics (P-11): 0 voids the full pending amount; a nonzero
/// amount must equal the pending amount exactly — TigerBeetle rejects a
/// partial void with `pending_transfer_has_different_amount`, which the
/// caller maps to `AmountMismatch` (400). A partial amount therefore never
/// silently voids the full hold.
fn build_void_hold_transfer(
    ledger_id: u32,
    transfer_id: u128,
    hold_id: u128,
    debit: &str,
    credit: &str,
    amount: u64,
) -> tb::Transfer {
    tb_transfer(ledger_id, transfer_id, debit, credit, amount, CODE_DEPOSIT_HOLD)
        .with_pending_id(hold_id)
        .with_flags(TbFlags::VOID_PENDING_TRANSFER)
}

/// The fully-constructed linked capture/no-show-fee batch plus the amounts
/// and derived ids needed to wrap the result after submit. Built by a pure
/// function so the exact wire structure is unit-testable (SPEC-W42).
#[derive(Debug)]
struct CaptureBatch {
    transfers: Vec<tb::Transfer>,
    posted: u64,
    net: u64,
    fee: u64,
    rev_id: u128,
    fee_id: u128,
}

/// GF11: post + revenue split (+ platform fee) as ONE linked
/// `create_transfers` batch — TigerBeetle applies linked batches atomically
/// (all-or-nothing), eliminating the previous two-batch window where the
/// hold was posted but the split was lost.
///
/// TB code-matching rule (SPEC-W42): leg 0 (the POST_PENDING_TRANSFER leg)
/// carries `CODE_DEPOSIT_HOLD` — the hold's own code — while the auxiliary
/// non-pending split legs carry the operation code (`code`: `CODE_CAPTURE`
/// or `CODE_NO_SHOW_FEE`).
fn build_capture_batch(
    ledger_id: u32,
    fee_bps: u64,
    tenant_id: &str,
    hold_id: Uuid,
    transfer_id: Uuid,
    posted: u64,
    code: u16,
) -> Result<CaptureBatch, LedgerError> {
    if posted == 0 {
        return Err(LedgerError::InvalidAmount);
    }
    let deposits = deposits_account(tenant_id);
    let revenue = revenue_account(tenant_id);
    // P-05: checked fee math (overflow / out-of-range fee_bps => InvalidAmount).
    let (net, fee) = fee_split(posted, fee_bps)?;
    // A 100% fee split (fee == posted) would make the revenue leg
    // zero-amount; TB rejects it on the wire (amount_must_not_be_zero),
    // failing the whole linked batch. Reject up front, mirroring the sim.
    if net == 0 {
        return Err(LedgerError::InvalidAmount);
    }

    let rev_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("capture-revenue:{:032x}", transfer_id.as_u128()).as_bytes(),
    )
    .as_u128();
    let fee_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("capture-fee:{:032x}", transfer_id.as_u128()).as_bytes(),
    )
    .as_u128();

    let mut transfers = Vec::with_capacity(3);
    // Leg 0: posting transfer resolving the hold. Carries the hold's code
    // (CODE_DEPOSIT_HOLD) — NOT the operation code — per the TB rule above.
    let post = tb_transfer(
        ledger_id,
        transfer_id.as_u128(),
        PLATFORM_CLEARING_ACCOUNT,
        &deposits,
        posted,
        CODE_DEPOSIT_HOLD,
    )
    .with_pending_id(hold_id.as_u128())
    .with_flags(TbFlags::POST_PENDING_TRANSFER | TbFlags::LINKED);
    transfers.push(post);
    // Leg 1: deposits -> revenue (net of platform fee). Auxiliary NON-pending
    // leg: keeps the operation code.
    let mut rev = tb_transfer(ledger_id, rev_id, &deposits, &revenue, net, code);
    if fee > 0 {
        // Not the last event in the chain: keep the link open.
        rev = rev.with_flags(TbFlags::LINKED);
    }
    transfers.push(rev);
    // Leg 2 (skipped when fee rounds to zero): deposits -> platform:fees.
    if fee > 0 {
        transfers.push(tb_transfer(
            ledger_id,
            fee_id,
            &deposits,
            PLATFORM_FEES_ACCOUNT,
            fee,
            code,
        ));
    }
    Ok(CaptureBatch {
        transfers,
        posted,
        net,
        fee,
        rev_id,
        fee_id,
    })
}

/// GF11 idempotent-replay acceptance, extended in SPEC-W42 R2 for LINKED
/// batches (live-proven against TigerBeetle 0.16.28).
///
/// TigerBeetle linked-batch replay semantics: when a verbatim replay of an
/// already-committed LINKED `create_transfers` batch is submitted, the first
/// leg of the chain reports `exists` and — because ANY non-`ok` result
/// breaks the link chain — every leg linked after it reports
/// `linked_event_failed`. The committed batch's fund state is already exactly
/// what this batch puts there, so this error pattern is an idempotent-replay
/// SUCCESS, not a ledger failure.
///
/// Acceptance rule: EVERY returned error kind must be `Exists` or
/// `LinkedEventFailed`, AND at least one must be `Exists`. The `Exists`
/// anchor is mandatory: `linked_event_failed` alone never proves a prior
/// commit — it also trails genuine first-leg rejections.
///
/// Mutation safety: any other error kind in the set fails the check. E.g. a
/// capture batch whose post leg carries the wrong code is rejected by a real
/// server as `pending_transfer_has_different_code` + `linked_event_failed`,
/// which is NOT accepted here, so a mutated batch can never masquerade as an
/// idempotent replay. (The error-kind enums implement no `PartialEq` —
/// use `matches!`.)
/// P-12: TigerBeetle `exists_with_different_*` per-item results are 409
/// CONFLICTS (idempotency-key misuse), never 502 backend errors.
fn is_exists_with_different(k: &CreateTransferErrorKind) -> bool {
    use CreateTransferErrorKind as K;
    matches!(
        k,
        K::ExistsWithDifferentFlags
            | K::ExistsWithDifferentPendingId
            | K::ExistsWithDifferentTimeout
            | K::ExistsWithDifferentDebitAccountId
            | K::ExistsWithDifferentCreditAccountId
            | K::ExistsWithDifferentAmount
            | K::ExistsWithDifferentUserData128
            | K::ExistsWithDifferentUserData64
            | K::ExistsWithDifferentUserData32
            | K::ExistsWithDifferentLedger
            | K::ExistsWithDifferentCode
    )
}

/// P-02: classification of a void-hold attempt's per-item results. ONLY
/// `pending_transfer_not_pending` and `pending_transfer_already_posted`
/// (FIN-H: real TigerBeetle 0.16.28 returns result 33
/// `pending_transfer_already_posted` when voiding an ALREADY-POSTED pending
/// transfer — state_machine.zig:2522 — instead of
/// `pending_transfer_not_pending`; both mean "hold already resolved") may
/// fall through to the posted-refund path; `pending_transfer_not_found` is a
/// 404 and everything else is a backend error — nothing is swallowed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum VoidClassification {
    /// Void committed (or idempotent replay of a committed void).
    Committed,
    /// Hold already resolved => fall through to the posted-refund path.
    NotPending,
    /// Hold unknown to the ledger => 404.
    NotFound,
    /// P-11: the requested amount is a nonzero partial of the pending hold
    /// => 400 (never a silent full void).
    AmountMismatch,
    /// Genuine backend failure.
    Backend,
}

fn classify_void_kinds(kinds: &[CreateTransferErrorKind]) -> VoidClassification {
    use CreateTransferErrorKind as K;
    if is_idempotent_replay(kinds) {
        return VoidClassification::Committed;
    }
    if kinds.iter().any(|k| matches!(k, K::PendingTransferNotFound)) {
        return VoidClassification::NotFound;
    }
    if kinds.iter().any(|k| {
        matches!(
            k,
            K::PendingTransferNotPending | K::PendingTransferAlreadyPosted
        )
    }) {
        return VoidClassification::NotPending;
    }
    if kinds
        .iter()
        .any(|k| matches!(k, K::PendingTransferHasDifferentAmount))
    {
        return VoidClassification::AmountMismatch;
    }
    VoidClassification::Backend
}

fn is_idempotent_replay(kinds: &[CreateTransferErrorKind]) -> bool {
    if kinds.is_empty() {
        // Vacuous case: an Api error with no per-item results cannot come
        // from a real server; preserve the previous all-`Exists` guard's
        // vacuous truth on an empty slice so the success path is untouched.
        return true;
    }
    let anchored = kinds
        .iter()
        .any(|k| matches!(k, CreateTransferErrorKind::Exists));
    let only_replay_kinds = kinds.iter().all(|k| {
        matches!(
            k,
            CreateTransferErrorKind::Exists | CreateTransferErrorKind::LinkedEventFailed
        )
    });
    anchored && only_replay_kinds
}

/// FIN-H2 / C5 replay parity with the sim (`sim.rs` refund): does a STORED
/// transfer found under the deterministic refund transfer id represent the
/// SAME refund this request would commit?
///
/// The sim does a replay-by-transfer-id short-circuit FIRST: a stored
/// refund-shaped transfer consistent with the request is returned verbatim
/// (idempotent replay, 201-class); anything else stored under the same id
/// is a P-12 parameter conflict (409-class), never a silent success and
/// never a backend error. This helper is the live-ledger equivalent of the
/// sim's `matches` check, applied to the wire shape TigerBeetle actually
/// stores:
///
///   * Posted refund (refund after capture): a plain posted transfer with
///     `CODE_REFUND`, `tenant:{id}:revenue -> platform:clearing`, matching
///     amount and ledger. The revenue account pins the tenant, so a
///     cross-tenant replay does not match (conflict, exactly like the
///     sim's account comparison). `hold_id` is irrelevant on this arm,
///     mirroring the sim's `TransferFlag::None` arm.
///   * Void leg (refund of a still-pending hold): a VOID_PENDING_TRANSFER
///     keyed by `pending_id == hold_id`. Per the SPEC-W42 code-matching
///     rule the live void leg carries the hold's own code
///     (`CODE_DEPOSIT_HOLD`) — the sim records `CODE_REFUND` on its void
///     record, so the code check here is the live analog of the sim's
///     `VoidPending` arm; it also distinguishes a deposit-hold refund void
///     from a payout void (`CODE_PAYOUT`). The amount argument is ignored
///     on the void path, exactly as on the original call.
///
/// Pending holds, capture post legs (`POST_PENDING_TRANSFER`), payout legs
/// and any transfer under a different ledger never match: presenting their
/// id as a refund id is a parameter conflict.
fn refund_replay_matches(
    stored: &tb::Transfer,
    ledger_id: u32,
    tenant_id: &str,
    hold_id: Option<Uuid>,
    amount: u64,
) -> bool {
    if stored.ledger() != ledger_id {
        return false;
    }
    let flags = stored.flags();
    if flags.contains(TbFlags::VOID_PENDING_TRANSFER) {
        return stored.code() == CODE_DEPOSIT_HOLD
            && hold_id.map(|h| h.as_u128()) == Some(stored.pending_id());
    }
    if flags.contains(TbFlags::POST_PENDING_TRANSFER) || flags.contains(TbFlags::PENDING) {
        return false;
    }
    stored.code() == CODE_REFUND
        && stored.amount() == amount as u128
        && stored.debit_account_id() == account_id(&revenue_account(tenant_id))
        && stored.credit_account_id() == account_id(PLATFORM_CLEARING_ACCOUNT)
}

impl TigerBeetleClient {
    pub async fn connect(
        addresses: &str,
        cluster_id: u128,
        ledger_id: u32,
        fee_bps: u64,
    ) -> Result<Self, LedgerError> {
        let client = tb::Client::new(cluster_id, addresses).map_err(map_err)?;
        Ok(Self {
            client,
            ledger_id,
            fee_bps,
        })
    }

    /// Submit one batch. GF11: TigerBeetle reports idempotent replays of an
    /// already-committed transfer id as per-item `exists` results — those are
    /// SUCCESS (the money is already where this batch puts it). SPEC-W42 R2:
    /// for a LINKED batch the replay surfaces as `exists` on the chain-
    /// breaking leg plus `linked_event_failed` on every leg linked after it
    /// (see `is_idempotent_replay`). Only genuine rejections (including
    /// `exists_with_different_*` conflicts and code mismatches) are errors.
    async fn submit(&self, transfers: Vec<tb::Transfer>) -> Result<(), LedgerError> {
        match self.client.create_transfers(transfers).await {
            Ok(()) => Ok(()),
            Err(CreateTransfersError::Api(api)) => {
                let kinds: Vec<CreateTransferErrorKind> =
                    api.as_slice().iter().map(|e| e.kind()).collect();
                if is_idempotent_replay(&kinds) {
                    return Ok(());
                }
                // P-12: exists_with_different_* => 409 (was swallowed into
                // 502 Backend before).
                if let Some(k) = kinds.iter().find(|k| is_exists_with_different(k)) {
                    return Err(LedgerError::ExistsWithDifferentParameters(format!(
                        "{k:?}"
                    )));
                }
                // Insufficient funds => 422, not 502 (payout holds rejected
                // for over-limit amounts must surface as a client error).
                if kinds
                    .iter()
                    .any(|k| matches!(k, CreateTransferErrorKind::ExceedsCredits))
                {
                    return Err(LedgerError::ExceedsCredits(
                        "tigerbeetle account (debits must not exceed credits)".to_string(),
                    ));
                }
                Err(LedgerError::Backend(format!(
                    "tigerbeetle transfer rejected: {kinds:?}"
                )))
            }
            Err(e) => Err(LedgerError::Backend(format!(
                "tigerbeetle transfer rejected: {e}"
            ))),
        }
    }

    /// Look up exactly one transfer by id (404 when unknown).
    async fn lookup_one(&self, id: Uuid) -> Result<tb::Transfer, LedgerError> {
        let found = self
            .client
            .lookup_transfers(vec![id.as_u128()])
            .await
            .map_err(map_err)?;
        found
            .into_iter()
            .next()
            .ok_or_else(|| LedgerError::TransferNotFound(id.to_string()))
    }

    /// Best-effort account-name resolution for a stored account id: platform
    /// accounts always; the tenant-scoped accounts when the tenant is known.
    fn account_name(&self, id: u128, tenant_id: Option<&str>) -> String {
        let mut candidates = vec![
            PLATFORM_FEES_ACCOUNT.to_string(),
            PLATFORM_CLEARING_ACCOUNT.to_string(),
            PLATFORM_PAYOUTS_ACCOUNT.to_string(),
        ];
        if let Some(t) = tenant_id {
            candidates.push(deposits_account(t));
            candidates.push(revenue_account(t));
        }
        for cand in candidates {
            if account_id(&cand) == id {
                return cand;
            }
        }
        format!("tb:{id:032x}")
    }

    /// P-11: build a response snapshot from the ACTUAL stored transfer
    /// (deterministic-id lookup), so responses/events match ledger reality —
    /// including idempotent replays.
    fn wrap_stored(&self, stored: &tb::Transfer, tenant_id: Option<&str>) -> Transfer {
        let flags = stored.flags();
        let flag = if flags.contains(TbFlags::VOID_PENDING_TRANSFER) {
            TransferFlag::VoidPending
        } else if flags.contains(TbFlags::POST_PENDING_TRANSFER) {
            TransferFlag::PostPending
        } else {
            TransferFlag::None
        };
        let state = if flags.contains(TbFlags::PENDING) {
            TransferState::Pending
        } else {
            TransferState::Posted
        };
        Transfer {
            id: stored.id(),
            debit_account: self.account_name(stored.debit_account_id(), tenant_id),
            credit_account: self.account_name(stored.credit_account_id(), tenant_id),
            // TB counters are u128; public model is u64 (see balance()).
            amount: stored.amount() as u64,
            ledger: stored.ledger(),
            code: stored.code(),
            state,
            flag,
            pending_id: (stored.pending_id() != 0).then_some(stored.pending_id()),
            created_at: chrono::Utc::now(),
        }
    }

    fn wrap(
        &self,
        id: u128,
        debit: &str,
        credit: &str,
        amount: u64,
        code: u16,
        state: TransferState,
        flag: TransferFlag,
        pending_id: Option<u128>,
    ) -> Transfer {
        Transfer {
            id,
            debit_account: debit.to_string(),
            credit_account: credit.to_string(),
            amount,
            ledger: self.ledger_id,
            code,
            state,
            flag,
            pending_id,
            created_at: chrono::Utc::now(),
        }
    }
}

#[async_trait]
impl LedgerClient for TigerBeetleClient {
    async fn create_accounts(&self, tenant_id: &str) -> Result<Vec<Account>, LedgerError> {
        let defs = [
            (deposits_account(tenant_id), ACCOUNT_CODE_TENANT_DEPOSITS),
            (revenue_account(tenant_id), ACCOUNT_CODE_TENANT_REVENUE),
            (PLATFORM_FEES_ACCOUNT.to_string(), ACCOUNT_CODE_PLATFORM_FEES),
            (
                PLATFORM_CLEARING_ACCOUNT.to_string(),
                ACCOUNT_CODE_PLATFORM_CLEARING,
            ),
            (
                PLATFORM_PAYOUTS_ACCOUNT.to_string(),
                ACCOUNT_CODE_PLATFORM_PAYOUTS,
            ),
        ];
        let tb_accounts: Vec<tb::Account> = defs
            .iter()
            .map(|(name, code)| {
                let a = tb::Account::new(account_id(name), self.ledger_id, *code);
                // P-03: enforce debits_must_not_exceed_credits on the tenant
                // deposits/revenue accounts, matching the sim's
                // no_overdraft() rule (previously omitted — overdrafts were
                // only blocked in the sim, not on the live ledger).
                if no_overdraft(name) {
                    a.with_flags(tb::account::Flags::DEBITS_MUST_NOT_EXCEED_CREDITS)
                } else {
                    a
                }
            })
            .collect();
        match self.client.create_accounts(tb_accounts).await {
            Ok(()) => {}
            // `exists` results are expected on idempotent re-creation;
            // anything else is a real error.
            Err(CreateAccountsError::Api(api))
                if api
                    .as_slice()
                    .iter()
                    .all(|e| matches!(e.kind(), CreateAccountErrorKind::Exists)) => {}
            Err(e) => {
                return Err(LedgerError::Backend(format!(
                    "tigerbeetle account creation failed: {e}"
                )))
            }
        }
        Ok(defs
            .iter()
            .map(|(name, code)| Account {
                id: account_id(name),
                name: name.clone(),
                ledger: self.ledger_id,
                code: *code,
                debits_pending: 0,
                credits_pending: 0,
                debits_posted: 0,
                credits_posted: 0,
            })
            .collect())
    }

    async fn hold_deposit(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        if amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let debit = PLATFORM_CLEARING_ACCOUNT.to_string();
        let credit = deposits_account(tenant_id);
        let t = build_hold_transfer(self.ledger_id, transfer_id, tenant_id, amount);
        self.submit(vec![t]).await?;
        Ok(self.wrap(
            transfer_id.as_u128(),
            &debit,
            &credit,
            amount,
            CODE_DEPOSIT_HOLD,
            TransferState::Pending,
            TransferFlag::None,
            None,
        ))
    }

    async fn capture(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        amount: Option<u64>,
    ) -> Result<CaptureResult, LedgerError> {
        self.capture_like_tb(tenant_id, hold_id, transfer_id, amount, CODE_CAPTURE)
            .await
    }

    async fn refund(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        hold_id: Option<Uuid>,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        // FIN-H2 / C5: replay by transfer id FIRST, mirroring the sim's
        // short-circuit (`sim.rs` refund). The first call already committed
        // the deterministic refund transfer id, so replaying the same
        // idempotency key must return the stored refund-shaped transfer
        // (201-class) instead of re-attempting the void/posted leg: a real
        // server rejects a re-attempted VOID leg whose stored transfer is
        // the committed POSTED refund with `exists_with_different_flags`,
        // which used to surface as a 502 (callers could not safely retry).
        // A stored transfer with the same id but different
        // amount/accounts/flags semantics stays a P-12 conflict (409),
        // never a silent success; `exists_with_different_*` handling in
        // submit()/classify_void_kinds() is otherwise unchanged.
        match self.lookup_one(transfer_id).await {
            Ok(stored) => {
                if refund_replay_matches(&stored, self.ledger_id, tenant_id, hold_id, amount) {
                    return Ok(self.wrap_stored(&stored, Some(tenant_id)));
                }
                return Err(LedgerError::ExistsWithDifferentParameters(
                    transfer_id.to_string(),
                ));
            }
            // First call: the id is unknown to the ledger — proceed.
            Err(LedgerError::TransferNotFound(_)) => {}
            Err(e) => return Err(e),
        }
        if let Some(h) = hold_id {
            // P-06: resolve the hold first — its credit account pins the
            // owning tenant (cross-tenant refund => TenantMismatch => 403).
            let hold = self.lookup_one(h).await?;
            let credit = deposits_account(tenant_id);
            if hold.credit_account_id() != account_id(&credit) {
                return Err(LedgerError::TenantMismatch(format!(
                    "hold {h} does not belong to tenant {tenant_id}"
                )));
            }
            // P-11: the void leg carries the REQUESTED amount. TigerBeetle
            // void semantics: 0 voids the full pending amount, a nonzero
            // amount must equal it exactly, and a partial amount is rejected
            // with `pending_transfer_has_different_amount` (mapped to
            // AmountMismatch => 400) BEFORE any state change — never a
            // silent full void of a partial request.
            // Void the pending hold. TB fails with `pending_transfer_not_pending`
            // (or, on a real 0.16.28 server voiding an ALREADY-POSTED pending
            // transfer, `pending_transfer_already_posted` — FIN-H) if the
            // hold was already resolved (then — and ONLY then — we fall
            // through to a posted refund). A replayed void reports `exists`
            // and is an idempotent-replay success (GF11).
            //
            // TB code-matching rule (SPEC-W42): the void leg carries the
            // hold's own code (CODE_DEPOSIT_HOLD), not CODE_REFUND — a real
            // server rejects the mismatch with
            // `pending_transfer_has_different_code`.
            //
            // P-02: classification is explicit — pending_transfer_not_found
            // => TransferNotFound (404, matching the sim); every other
            // rejection => Backend (502); nothing is swallowed.
            let debit = PLATFORM_CLEARING_ACCOUNT.to_string();
            let t = build_void_hold_transfer(
                self.ledger_id,
                transfer_id.as_u128(),
                h.as_u128(),
                &debit,
                &credit,
                amount,
            );
            match self.client.create_transfers(vec![t]).await {
                Ok(()) => {
                    let stored = self.lookup_one(transfer_id).await?;
                    return Ok(self.wrap_stored(&stored, Some(tenant_id)));
                }
                Err(CreateTransfersError::Api(api)) => {
                    let kinds: Vec<CreateTransferErrorKind> =
                        api.as_slice().iter().map(|e| e.kind()).collect();
                    match classify_void_kinds(&kinds) {
                        VoidClassification::Committed => {
                            let stored = self.lookup_one(transfer_id).await?;
                            return Ok(self.wrap_stored(&stored, Some(tenant_id)));
                        }
                        VoidClassification::NotPending => {
                            // Hold already resolved (captured) => posted
                            // refund below.
                        }
                        VoidClassification::NotFound => {
                            return Err(LedgerError::TransferNotFound(h.to_string()));
                        }
                        VoidClassification::AmountMismatch => {
                            return Err(LedgerError::AmountMismatch(format!(
                                "refund amount {amount} is a nonzero partial of                                  pending hold {h}"
                            )));
                        }
                        VoidClassification::Backend => {
                            return Err(LedgerError::Backend(format!(
                                "tigerbeetle void transfer rejected: {kinds:?}"
                            )));
                        }
                    }
                }
                Err(e) => {
                    return Err(LedgerError::Backend(format!(
                        "tigerbeetle transfer rejected: {e}"
                    )))
                }
            }
        }
        if amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let debit = revenue_account(tenant_id);
        let credit = PLATFORM_CLEARING_ACCOUNT.to_string();
        let t = tb_transfer(
            self.ledger_id,
            transfer_id.as_u128(),
            &debit,
            &credit,
            amount,
            CODE_REFUND,
        );
        self.submit(vec![t]).await?;
        // P-11: respond with the actual stored transfer (deterministic-id
        // lookup), so the response/event equal ledger reality even on
        // idempotent replays.
        let stored = self.lookup_one(transfer_id).await?;
        Ok(self.wrap_stored(&stored, Some(tenant_id)))
    }

    async fn no_show_fee(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<CaptureResult, LedgerError> {
        self.capture_like_tb(tenant_id, hold_id, transfer_id, Some(amount), CODE_NO_SHOW_FEE)
            .await
    }

    async fn payout(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        if amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let debit = revenue_account(tenant_id);
        let credit = PLATFORM_PAYOUTS_ACCOUNT.to_string();
        let t = tb_transfer(
            self.ledger_id,
            transfer_id.as_u128(),
            &debit,
            &credit,
            amount,
            CODE_PAYOUT,
        );
        self.submit(vec![t]).await?;
        Ok(self.wrap(
            transfer_id.as_u128(),
            &debit,
            &credit,
            amount,
            CODE_PAYOUT,
            TransferState::Posted,
            TransferFlag::None,
            None,
        ))
    }

    async fn payout_hold(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        if amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let debit = revenue_account(tenant_id);
        let credit = PLATFORM_PAYOUTS_ACCOUNT.to_string();
        // C3 ledger-first phase 1: PENDING payout reserves the funds. The
        // revenue account carries debits_must_not_exceed_credits (P-03), and
        // TigerBeetle counts pending debits against it — an over-limit payout
        // hold is rejected (ExceedsCredits => 422) BEFORE any rail call.
        let t = tb_transfer(
            self.ledger_id,
            transfer_id.as_u128(),
            &debit,
            &credit,
            amount,
            CODE_PAYOUT,
        )
        .with_flags(TbFlags::PENDING);
        self.submit(vec![t]).await?;
        Ok(self.wrap(
            transfer_id.as_u128(),
            &debit,
            &credit,
            amount,
            CODE_PAYOUT,
            TransferState::Pending,
            TransferFlag::None,
            None,
        ))
    }

    async fn payout_post(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
    ) -> Result<Transfer, LedgerError> {
        let hold = self.lookup_one(hold_id).await?;
        if hold.code() != CODE_PAYOUT {
            return Err(LedgerError::NotPending(hold_id.to_string()));
        }
        let debit = revenue_account(tenant_id);
        if hold.debit_account_id() != account_id(&debit) {
            return Err(LedgerError::TenantMismatch(format!(
                "payout hold {hold_id} does not belong to tenant {tenant_id}"
            )));
        }
        let amount = hold.amount() as u64;
        // Post leg carries the hold's own code (TB code-matching rule).
        let t = tb_transfer(
            self.ledger_id,
            transfer_id.as_u128(),
            &debit,
            PLATFORM_PAYOUTS_ACCOUNT,
            amount,
            CODE_PAYOUT,
        )
        .with_pending_id(hold_id.as_u128())
        .with_flags(TbFlags::POST_PENDING_TRANSFER);
        self.submit(vec![t]).await?;
        let stored = self.lookup_one(transfer_id).await?;
        Ok(self.wrap_stored(&stored, Some(tenant_id)))
    }

    async fn payout_void(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
    ) -> Result<Transfer, LedgerError> {
        let hold = self.lookup_one(hold_id).await?;
        if hold.code() != CODE_PAYOUT {
            return Err(LedgerError::NotPending(hold_id.to_string()));
        }
        let debit = revenue_account(tenant_id);
        if hold.debit_account_id() != account_id(&debit) {
            return Err(LedgerError::TenantMismatch(format!(
                "payout hold {hold_id} does not belong to tenant {tenant_id}"
            )));
        }
        let t = tb_transfer(
            self.ledger_id,
            transfer_id.as_u128(),
            &debit,
            PLATFORM_PAYOUTS_ACCOUNT,
            0,
            CODE_PAYOUT,
        )
        .with_pending_id(hold_id.as_u128())
        .with_flags(TbFlags::VOID_PENDING_TRANSFER);
        self.submit(vec![t]).await?;
        let stored = self.lookup_one(transfer_id).await?;
        Ok(self.wrap_stored(&stored, Some(tenant_id)))
    }

    async fn get_transfer(&self, transfer_id: Uuid) -> Result<Transfer, LedgerError> {
        let stored = self.lookup_one(transfer_id).await?;
        // Tenant is unknown at this level; platform accounts resolve by name,
        // tenant-scoped accounts render as `tb:{id}` (callers that need the
        // amount/state only — e.g. the Flutterwave verify-before-capture
        // check — are unaffected).
        Ok(self.wrap_stored(&stored, None))
    }

    /// SPEC-W44 F15-03: /healthz liveness probe — a lookup round-trip proves
    /// the TB connection is alive (a missing account is fine; reachability is
    /// what matters).
    async fn ping(&self) -> Result<(), LedgerError> {
        self.client
            .lookup_accounts(vec![account_id(PLATFORM_FEES_ACCOUNT)])
            .await
            .map(|_| ())
            .map_err(map_err)
    }

    async fn balance(&self, tenant_id: &str) -> Result<TenantBalance, LedgerError> {
        let names = [deposits_account(tenant_id), revenue_account(tenant_id)];
        let ids: Vec<u128> = names.iter().map(|n| account_id(n)).collect();
        let accounts = self.client.lookup_accounts(ids).await.map_err(map_err)?;
        let mut out = Vec::new();
        for (i, a) in accounts.iter().enumerate() {
            // TB balance counters are u128; the public model uses u64 (values
            // far below 2^64 in any realistic deployment).
            let debits_posted = a.debits_posted() as u64;
            let credits_posted = a.credits_posted() as u64;
            let debits_pending = a.debits_pending() as u64;
            let credits_pending = a.credits_pending() as u64;
            out.push(AccountBalance {
                account: names[i].clone(),
                id: format!("{:032x}", a.id()),
                debits_pending,
                credits_pending,
                debits_posted,
                credits_posted,
                posted_net: credits_posted as i128 - debits_posted as i128,
                pending_net: credits_pending as i128 - debits_pending as i128,
            });
        }
        Ok(TenantBalance {
            tenant_id: tenant_id.to_string(),
            accounts: out,
        })
    }
}

impl TigerBeetleClient {
    async fn capture_like_tb(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        amount: Option<u64>,
        code: u16,
    ) -> Result<CaptureResult, LedgerError> {
        // C4/P-04: look up the pending hold FIRST. This resolves the posted
        // amount when the caller passes None (previously rejected with a
        // backend error) and gives the cross-tenant guard (P-06): the hold's
        // credit account pins the owning tenant.
        let hold = self.lookup_one(hold_id).await?;
        let deposits = deposits_account(tenant_id);
        if hold.credit_account_id() != account_id(&deposits) {
            return Err(LedgerError::TenantMismatch(format!(
                "hold {hold_id} does not belong to tenant {tenant_id}"
            )));
        }
        let posted = match amount {
            Some(a) => a,
            // C4: resolve the pending transfer amount via lookup before
            // building the batch.
            None => hold.amount() as u64,
        };
        let batch = build_capture_batch(
            self.ledger_id,
            self.fee_bps,
            tenant_id,
            hold_id,
            transfer_id,
            posted,
            code,
        )?;
        let CaptureBatch {
            transfers,
            posted,
            net,
            fee,
            rev_id,
            fee_id,
        } = batch;
        self.submit(transfers).await?;

        let revenue = revenue_account(tenant_id);
        Ok(CaptureResult {
            // The post leg carries the hold's code on the wire (TB code-
            // matching rule), so the snapshot reports CODE_DEPOSIT_HOLD.
            post: self.wrap(
                transfer_id.as_u128(),
                PLATFORM_CLEARING_ACCOUNT,
                &deposits,
                posted,
                CODE_DEPOSIT_HOLD,
                TransferState::Posted,
                TransferFlag::PostPending,
                Some(hold_id.as_u128()),
            ),
            revenue: self.wrap(
                rev_id,
                &deposits,
                &revenue,
                net,
                code,
                TransferState::Posted,
                TransferFlag::None,
                None,
            ),
            platform_fee: if fee > 0 {
                Some(self.wrap(
                    fee_id,
                    &deposits,
                    PLATFORM_FEES_ACCOUNT,
                    fee,
                    code,
                    TransferState::Posted,
                    TransferFlag::None,
                    None,
                ))
            } else {
                None
            },
        })
    }
}

// ---------------------------------------------------------------------------
// Unit tests: structure assertions on the constructed wire transfers — no
// TigerBeetle server needed. These pin the SPEC-W42 code-matching rule:
// every POST_PENDING_TRANSFER / VOID_PENDING_TRANSFER leg of a deposit hold
// carries CODE_DEPOSIT_HOLD (the hold's code). Mutation check: reverting any
// post/void leg to CODE_CAPTURE / CODE_REFUND must fail these tests, exactly
// as a real server rejects the batch with pending_transfer_has_different_code.
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    const TENANT: &str = "t-codes";
    const FEE_BPS: u64 = 1_000; // 10%

    fn hold_id() -> Uuid {
        Uuid::from_u128(0x001d)
    }

    #[test]
    fn hold_leg_is_pending_with_deposit_hold_code() {
        let id = Uuid::from_u128(0x1001);
        let t = build_hold_transfer(LEDGER_ID, id, TENANT, 5_000);
        assert_eq!(t.id(), id.as_u128());
        assert_eq!(t.code(), CODE_DEPOSIT_HOLD);
        assert!(t.flags().contains(TbFlags::PENDING));
        assert!(!t.flags().contains(TbFlags::POST_PENDING_TRANSFER));
        assert!(!t.flags().contains(TbFlags::VOID_PENDING_TRANSFER));
        assert!(!t.flags().contains(TbFlags::LINKED));
        assert_eq!(t.ledger(), LEDGER_ID);
        assert_eq!(t.amount(), 5_000);
        assert_eq!(t.debit_account_id(), account_id(PLATFORM_CLEARING_ACCOUNT));
        assert_eq!(t.credit_account_id(), account_id(&deposits_account(TENANT)));
    }

    #[test]
    fn capture_post_leg_carries_the_holds_code() {
        let cap_id = Uuid::from_u128(0x2002);
        let b = build_capture_batch(LEDGER_ID, FEE_BPS, TENANT, hold_id(), cap_id, 10_000, CODE_CAPTURE)
            .unwrap();
        let post = &b.transfers[0];
        // THE SPEC-W42 rule: a POST_PENDING_TRANSFER leg must carry the
        // pending hold's code, not the operation code.
        assert_eq!(
            post.code(),
            CODE_DEPOSIT_HOLD,
            "POST_PENDING_TRANSFER leg must carry CODE_DEPOSIT_HOLD (real TB \
             rejects a mismatch with pending_transfer_has_different_code and \
             rolls back the linked batch)"
        );
        assert!(post.flags().contains(TbFlags::POST_PENDING_TRANSFER));
        assert!(post.flags().contains(TbFlags::LINKED), "linked to the split legs");
        assert!(!post.flags().contains(TbFlags::PENDING));
        assert_eq!(post.pending_id(), hold_id().as_u128());
        assert_eq!(post.id(), cap_id.as_u128());
        assert_eq!(post.amount(), 10_000);
        assert_eq!(post.debit_account_id(), account_id(PLATFORM_CLEARING_ACCOUNT));
        assert_eq!(post.credit_account_id(), account_id(&deposits_account(TENANT)));
    }

    #[test]
    fn capture_split_legs_keep_the_operation_code() {
        let cap_id = Uuid::from_u128(0x2003);
        let b = build_capture_batch(LEDGER_ID, FEE_BPS, TENANT, hold_id(), cap_id, 10_000, CODE_CAPTURE)
            .unwrap();
        assert_eq!(b.transfers.len(), 3, "post + revenue + platform fee");
        let rev = &b.transfers[1];
        let fee = &b.transfers[2];
        // Auxiliary NON-pending legs keep the operation's own code.
        assert_eq!(rev.code(), CODE_CAPTURE);
        assert_eq!(fee.code(), CODE_CAPTURE);
        for aux in [rev, fee] {
            assert!(!aux.flags().contains(TbFlags::POST_PENDING_TRANSFER));
            assert!(!aux.flags().contains(TbFlags::VOID_PENDING_TRANSFER));
            assert!(!aux.flags().contains(TbFlags::PENDING));
            assert_eq!(aux.pending_id(), 0, "non-pending legs have no pending_id");
        }
        // Link chain: post LINKED, rev LINKED, fee closes the chain.
        assert!(rev.flags().contains(TbFlags::LINKED));
        assert!(!fee.flags().contains(TbFlags::LINKED), "last leg closes the chain");
        // 10% fee split: net 9000 / fee 1000.
        assert_eq!(rev.amount(), 9_000);
        assert_eq!(fee.amount(), 1_000);
        assert_eq!(rev.credit_account_id(), account_id(&revenue_account(TENANT)));
        assert_eq!(fee.credit_account_id(), account_id(PLATFORM_FEES_ACCOUNT));
        // Deterministic derived ids (idempotent replay of the split).
        assert_eq!(rev.id(), b.rev_id);
        assert_eq!(fee.id(), b.fee_id);
    }

    #[test]
    fn no_show_fee_post_leg_carries_the_holds_code() {
        let fee_id = Uuid::from_u128(0x2004);
        let b = build_capture_batch(LEDGER_ID, FEE_BPS, TENANT, hold_id(), fee_id, 2_500, CODE_NO_SHOW_FEE)
            .unwrap();
        let post = &b.transfers[0];
        assert_eq!(post.code(), CODE_DEPOSIT_HOLD);
        assert!(post.flags().contains(TbFlags::POST_PENDING_TRANSFER));
        assert_eq!(post.pending_id(), hold_id().as_u128());
        // Split legs keep CODE_NO_SHOW_FEE.
        assert_eq!(b.transfers[1].code(), CODE_NO_SHOW_FEE);
        assert_eq!(b.transfers[2].code(), CODE_NO_SHOW_FEE);
    }

    #[test]
    fn zero_fee_batch_has_two_legs_and_a_closed_link() {
        let cap_id = Uuid::from_u128(0x2005);
        let b = build_capture_batch(LEDGER_ID, 0, TENANT, hold_id(), cap_id, 4_000, CODE_CAPTURE)
            .unwrap();
        assert_eq!(b.transfers.len(), 2, "no platform-fee leg when fee rounds to zero");
        assert_eq!(b.fee, 0);
        assert_eq!(b.net, 4_000);
        let rev = &b.transfers[1];
        assert_eq!(rev.code(), CODE_CAPTURE);
        assert!(!rev.flags().contains(TbFlags::LINKED), "last leg closes the chain");
    }

    #[test]
    fn capture_batch_rejects_zero_amount() {
        let err = build_capture_batch(LEDGER_ID, 0, TENANT, hold_id(), Uuid::from_u128(0x2006), 0, CODE_CAPTURE)
            .unwrap_err();
        assert!(matches!(err, LedgerError::InvalidAmount));
        // A 100% fee split leaves a zero-amount revenue leg, which TB would
        // reject on the wire (amount_must_not_be_zero): rejected up front.
        let err = build_capture_batch(LEDGER_ID, 10_000, TENANT, hold_id(), Uuid::from_u128(0x2007), 4_000, CODE_CAPTURE)
            .unwrap_err();
        assert!(matches!(err, LedgerError::InvalidAmount));
    }

    #[test]
    fn void_leg_carries_the_holds_code() {
        let debit = PLATFORM_CLEARING_ACCOUNT.to_string();
        let credit = deposits_account(TENANT);
        let t = build_void_hold_transfer(LEDGER_ID, 0x3003, hold_id().as_u128(), &debit, &credit, 0);
        // THE SPEC-W42 rule: a VOID_PENDING_TRANSFER leg must carry the
        // pending hold's code, not CODE_REFUND.
        assert_eq!(
            t.code(),
            CODE_DEPOSIT_HOLD,
            "VOID_PENDING_TRANSFER leg must carry CODE_DEPOSIT_HOLD (real TB \
             rejects a mismatch with pending_transfer_has_different_code)"
        );
        assert!(t.flags().contains(TbFlags::VOID_PENDING_TRANSFER));
        assert!(!t.flags().contains(TbFlags::POST_PENDING_TRANSFER));
        assert!(!t.flags().contains(TbFlags::LINKED));
        assert_eq!(t.pending_id(), hold_id().as_u128());
        assert_eq!(t.amount(), 0, "amount 0 voids the full pending hold");
        assert_eq!(t.ledger(), LEDGER_ID);
    }

    #[test]
    fn hold_post_and_void_legs_share_one_code() {
        // Cross-check the invariant end to end: hold, capture-post and void
        // legs for the SAME deposit hold all carry CODE_DEPOSIT_HOLD.
        let hold = build_hold_transfer(LEDGER_ID, Uuid::from_u128(0x4001), TENANT, 7_000);
        let b = build_capture_batch(
            LEDGER_ID,
            FEE_BPS,
            TENANT,
            Uuid::from_u128(0x4002),
            Uuid::from_u128(0x4003),
            7_000,
            CODE_CAPTURE,
        )
        .unwrap();
        let void = build_void_hold_transfer(
            LEDGER_ID,
            0x4004,
            0x4002,
            PLATFORM_CLEARING_ACCOUNT,
            &deposits_account(TENANT),
            0,
        );
        assert_eq!(hold.code(), b.transfers[0].code());
        assert_eq!(hold.code(), void.code());
        assert_eq!(hold.code(), CODE_DEPOSIT_HOLD);
    }

    // -----------------------------------------------------------------------
    // GF11 idempotent-replay acceptance (SPEC-W42 R2): synthetic error-kind
    // vectors against `is_idempotent_replay` — pure, no server needed.
    // Mutation check: widening the accepted kind set (e.g. accepting
    // PendingTransferHasDifferentCode) or dropping the Exists anchor must
    // fail these tests.
    // -----------------------------------------------------------------------

    #[test]
    fn replay_all_exists_is_accepted() {
        // Single-leg replay (hold / void / refund / payout batches).
        assert!(is_idempotent_replay(&[CreateTransferErrorKind::Exists]));
    }

    #[test]
    fn replay_linked_exists_then_linked_event_failed_is_accepted() {
        use CreateTransferErrorKind as K;
        // Live-proven replay signature of a committed 3-leg LINKED capture
        // batch: leg 0 `exists`, legs 1..n `linked_event_failed`.
        assert!(is_idempotent_replay(&[
            K::Exists,
            K::LinkedEventFailed,
            K::LinkedEventFailed,
        ]));
        // Two-leg variant (fee rounds to zero).
        assert!(is_idempotent_replay(&[K::Exists, K::LinkedEventFailed]));
    }

    #[test]
    fn linked_event_failed_without_exists_anchor_is_rejected() {
        // `linked_event_failed` alone never proves a prior commit — it also
        // trails genuine first-leg rejections.
        assert!(!is_idempotent_replay(&[
            CreateTransferErrorKind::LinkedEventFailed
        ]));
        assert!(!is_idempotent_replay(&[
            CreateTransferErrorKind::LinkedEventFailed,
            CreateTransferErrorKind::LinkedEventFailed,
        ]));
    }

    #[test]
    fn mutant_different_code_plus_linked_event_failed_is_rejected() {
        use CreateTransferErrorKind as K;
        // The W42 mutant (post leg carrying the operation code instead of
        // CODE_DEPOSIT_HOLD) is rejected by a real server with
        // `pending_transfer_has_different_code` on leg 0, which breaks the
        // link chain: this MUST NOT be accepted as an idempotent replay.
        assert!(!is_idempotent_replay(&[
            K::PendingTransferHasDifferentCode,
            K::LinkedEventFailed,
        ]));
        // Anchor present elsewhere does not rescue a genuine rejection.
        assert!(!is_idempotent_replay(&[
            K::Exists,
            K::PendingTransferHasDifferentCode,
        ]));
    }

    #[test]
    fn any_genuine_error_kind_defeats_replay_acceptance() {
        use CreateTransferErrorKind as K;
        assert!(!is_idempotent_replay(&[K::Exists, K::ExceedsCredits]));
        assert!(!is_idempotent_replay(&[
            K::ExceedsCredits,
            K::LinkedEventFailed,
        ]));
    }

    // -----------------------------------------------------------------------
    // P-02: void-attempt classification — fall through to the posted refund
    // ONLY on pending_transfer_not_pending / pending_transfer_already_posted
    // (FIN-H: a live 0.16.28 server returns the latter for an already-posted
    // hold); not_found => 404; partial amount => 400 (P-11); everything else
    // => Backend (nothing swallowed).
    // -----------------------------------------------------------------------

    #[test]
    fn void_classification_not_pending_falls_through() {
        assert_eq!(
            classify_void_kinds(&[CreateTransferErrorKind::PendingTransferNotPending]),
            VoidClassification::NotPending
        );
    }

    #[test]
    fn void_classification_already_posted_falls_through() {
        // FIN-H: live TigerBeetle 0.16.28 rejects the void of an
        // ALREADY-POSTED pending transfer (hold -> capture -> refund) with
        // `pending_transfer_already_posted` (result 33), not
        // `pending_transfer_not_pending`. It MUST classify identically so
        // refund-after-capture falls through to the posted-refund path
        // (sim/TB parity — the sim already resolves this flow).
        assert_eq!(
            classify_void_kinds(&[CreateTransferErrorKind::PendingTransferAlreadyPosted]),
            VoidClassification::NotPending
        );
    }

    #[test]
    fn void_classification_not_found_is_404() {
        assert_eq!(
            classify_void_kinds(&[CreateTransferErrorKind::PendingTransferNotFound]),
            VoidClassification::NotFound
        );
    }

    #[test]
    fn void_classification_partial_amount_is_400() {
        assert_eq!(
            classify_void_kinds(&[CreateTransferErrorKind::PendingTransferHasDifferentAmount]),
            VoidClassification::AmountMismatch
        );
    }

    #[test]
    fn void_classification_replay_is_committed() {
        assert_eq!(
            classify_void_kinds(&[CreateTransferErrorKind::Exists]),
            VoidClassification::Committed
        );
    }

    #[test]
    fn void_classification_anything_else_is_backend() {
        use CreateTransferErrorKind as K;
        // Code mismatch / funds errors / account errors must NOT fall through.
        assert_eq!(
            classify_void_kinds(&[K::PendingTransferHasDifferentCode]),
            VoidClassification::Backend
        );
        assert_eq!(
            classify_void_kinds(&[K::ExceedsCredits]),
            VoidClassification::Backend
        );
        assert_eq!(
            classify_void_kinds(&[K::CreditAccountNotFound]),
            VoidClassification::Backend
        );
    }

    #[test]
    fn void_leg_carries_requested_amount_for_partial_rejection() {
        // P-11: a nonzero amount on the void leg makes a real server reject
        // partial voids with pending_transfer_has_different_amount.
        let debit = PLATFORM_CLEARING_ACCOUNT.to_string();
        let credit = deposits_account(TENANT);
        let t = build_void_hold_transfer(LEDGER_ID, 0x3004, hold_id().as_u128(), &debit, &credit, 1_000);
        assert_eq!(t.amount(), 1_000);
        assert_eq!(t.code(), CODE_DEPOSIT_HOLD);
        assert!(t.flags().contains(TbFlags::VOID_PENDING_TRANSFER));
    }

    // -----------------------------------------------------------------------
    // P-12: exists_with_different_* classification (409, not 502).
    // -----------------------------------------------------------------------

    #[test]
    fn exists_with_different_kinds_are_classified() {
        use CreateTransferErrorKind as K;
        for k in [
            K::ExistsWithDifferentFlags,
            K::ExistsWithDifferentPendingId,
            K::ExistsWithDifferentTimeout,
            K::ExistsWithDifferentDebitAccountId,
            K::ExistsWithDifferentCreditAccountId,
            K::ExistsWithDifferentAmount,
            K::ExistsWithDifferentUserData128,
            K::ExistsWithDifferentUserData64,
            K::ExistsWithDifferentUserData32,
            K::ExistsWithDifferentLedger,
            K::ExistsWithDifferentCode,
        ] {
            assert!(is_exists_with_different(&k), "{k:?} must classify");
        }
        assert!(!is_exists_with_different(&K::Exists));
        assert!(!is_exists_with_different(&K::ExceedsCredits));
    }

    // -----------------------------------------------------------------------
    // P-05: fee math overflow safety in the batch constructor.
    // -----------------------------------------------------------------------

    #[test]
    fn capture_batch_fee_math_never_overflows() {
        // fee_bps above 100% is rejected, not wrapped.
        let err = build_capture_batch(
            LEDGER_ID,
            10_001,
            TENANT,
            hold_id(),
            Uuid::from_u128(0x2007),
            1_000,
            CODE_CAPTURE,
        )
        .unwrap_err();
        assert!(matches!(err, LedgerError::InvalidAmount));
        // 100% fee (net == 0) is rejected up front: the revenue leg would be
        // zero-amount, which TB rejects on the wire (amount_must_not_be_zero)
        // — mirrors the sim (capture_batch_rejects_zero_amount asserts the
        // same). The fee math itself never overflows/wraps first.
        let err = build_capture_batch(
            LEDGER_ID,
            10_000,
            TENANT,
            hold_id(),
            Uuid::from_u128(0x2008),
            5_000,
            CODE_CAPTURE,
        )
        .unwrap_err();
        assert!(matches!(err, LedgerError::InvalidAmount));
        // Boundary below 100% still computes exactly.
        let b = build_capture_batch(
            LEDGER_ID,
            9_999,
            TENANT,
            hold_id(),
            Uuid::from_u128(0x2009),
            5_000,
            CODE_CAPTURE,
        )
        .unwrap();
        assert_eq!(b.net, 1);
        assert_eq!(b.fee, 4_999);
    }

    #[test]
    fn empty_error_set_keeps_prior_vacuous_acceptance() {
        // Success path unaffected: no per-item errors (the Ok(()) arm handles
        // real success; this preserves the old all-Exists guard's vacuous
        // truth on an empty slice).
        assert!(is_idempotent_replay(&[]));
    }

    // -----------------------------------------------------------------------
    // FIN-H2 / C5: refund replay-by-transfer-id classification (sim parity).
    // Pure shape checks against constructed wire transfers — no server
    // needed. Mutation check: accepting a non-refund kind, a mismatched
    // amount/account/ledger, or the wrong hold on the void arm must fail
    // these tests.
    // -----------------------------------------------------------------------

    fn posted_refund(id: u128, tenant: &str, amount: u64) -> tb::Transfer {
        tb_transfer(
            LEDGER_ID,
            id,
            &revenue_account(tenant),
            PLATFORM_CLEARING_ACCOUNT,
            amount,
            CODE_REFUND,
        )
    }

    #[test]
    fn refund_replay_matches_the_stored_posted_refund() {
        // The FIN-H2 defect shape: hold -> capture -> refund committed a
        // plain posted CODE_REFUND transfer under the deterministic id;
        // replaying the same idempotency key must match it (201-class).
        let stored = posted_refund(0x5001, TENANT, 4_000);
        // hold_id is irrelevant on the posted-refund arm (mirrors the sim's
        // TransferFlag::None arm).
        assert!(refund_replay_matches(
            &stored,
            LEDGER_ID,
            TENANT,
            Some(hold_id()),
            4_000
        ));
        assert!(refund_replay_matches(&stored, LEDGER_ID, TENANT, None, 4_000));
    }

    #[test]
    fn refund_replay_posted_refund_parameter_drift_conflicts() {
        let stored = posted_refund(0x5002, TENANT, 4_000);
        // Different amount (P-11), different tenant (P-06 — the revenue
        // account pins the tenant) and different ledger are all conflicts,
        // never silent replays.
        assert!(!refund_replay_matches(&stored, LEDGER_ID, TENANT, None, 4_500));
        assert!(!refund_replay_matches(&stored, LEDGER_ID, "t-other", None, 4_000));
        assert!(!refund_replay_matches(
            &stored,
            LEDGER_ID + 1,
            TENANT,
            None,
            4_000
        ));
        // A refund-shaped transfer of a DIFFERENT tenant never satisfies
        // this tenant's replay.
        let other_tenant_refund = posted_refund(0x5002, "t-other", 4_000);
        assert!(!refund_replay_matches(
            &other_tenant_refund,
            LEDGER_ID,
            TENANT,
            None,
            4_000
        ));
    }

    #[test]
    fn refund_replay_matches_the_stored_void_leg() {
        // Refund of a still-pending hold commits a VOID_PENDING_TRANSFER
        // carrying the hold's own code (SPEC-W42); replaying the same key
        // must match it instead of re-attempting the void.
        let stored = build_void_hold_transfer(
            LEDGER_ID,
            0x5003,
            hold_id().as_u128(),
            PLATFORM_CLEARING_ACCOUNT,
            &deposits_account(TENANT),
            0,
        );
        // Keyed by the hold it resolved; the amount argument is ignored on
        // the void path, exactly as on the original call (sim parity).
        assert!(refund_replay_matches(&stored, LEDGER_ID, TENANT, Some(hold_id()), 0));
        assert!(refund_replay_matches(
            &stored,
            LEDGER_ID,
            TENANT,
            Some(hold_id()),
            4_000
        ));
        // A different or missing hold is a conflict, not a replay.
        assert!(!refund_replay_matches(
            &stored,
            LEDGER_ID,
            TENANT,
            Some(Uuid::from_u128(0x9999)),
            0
        ));
        assert!(!refund_replay_matches(&stored, LEDGER_ID, TENANT, None, 0));
    }

    #[test]
    fn refund_replay_rejects_non_refund_kinds() {
        // A transfer id already used by a non-refund operation presented as
        // a refund id is a parameter conflict (P-11/P-12), never a replay.
        let hold = build_hold_transfer(LEDGER_ID, Uuid::from_u128(0x5004), TENANT, 1_000);
        assert!(!refund_replay_matches(&hold, LEDGER_ID, TENANT, None, 1_000));
        let cap = build_capture_batch(
            LEDGER_ID,
            0,
            TENANT,
            hold_id(),
            Uuid::from_u128(0x5005),
            4_000,
            CODE_CAPTURE,
        )
        .unwrap();
        // Capture post leg (POST_PENDING_TRANSFER) and split legs.
        assert!(!refund_replay_matches(
            &cap.transfers[0],
            LEDGER_ID,
            TENANT,
            Some(hold_id()),
            4_000
        ));
        assert!(!refund_replay_matches(
            &cap.transfers[1],
            LEDGER_ID,
            TENANT,
            None,
            4_000
        ));
        let payout = tb_transfer(
            LEDGER_ID,
            0x5006,
            &revenue_account(TENANT),
            PLATFORM_PAYOUTS_ACCOUNT,
            1_000,
            CODE_PAYOUT,
        );
        assert!(!refund_replay_matches(&payout, LEDGER_ID, TENANT, None, 1_000));
        // A payout VOID leg shares the VOID_PENDING_TRANSFER flag but
        // carries CODE_PAYOUT: not a deposit-hold refund void.
        let payout_void = tb_transfer(
            LEDGER_ID,
            0x5007,
            &revenue_account(TENANT),
            PLATFORM_PAYOUTS_ACCOUNT,
            1_000,
            CODE_PAYOUT,
        )
        .with_pending_id(hold_id().as_u128())
        .with_flags(TbFlags::VOID_PENDING_TRANSFER);
        assert!(!refund_replay_matches(
            &payout_void,
            LEDGER_ID,
            TENANT,
            Some(hold_id()),
            1_000
        ));
    }
}
