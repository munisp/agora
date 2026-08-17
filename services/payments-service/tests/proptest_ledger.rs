//! Property-based tests (W41-6) over the in-memory double-entry ledger
//! (`SimLedgerClient`, `LEDGER_IMPL=sim`, ADR-0007 fallback).
//!
//! The crate is binary-only (no lib target), so the ledger modules are
//! included directly into this integration-test crate via `#[path]`. Only
//! default features are used: the `tb-live` TigerBeetle gate stays OFF.
//!
//! Properties under test (SPEC-W41 W41-6):
//! 1. Random sequences of hold/capture/refund/no-show-fee ops conserve the
//!    double-entry invariant: every transfer debits exactly one account and
//!    credits exactly one account, and the per-account counters recorded by
//!    the ledger exactly match a shadow model (Σ debits == Σ credits, both
//!    pending and posted, always holds).
//! 2. Replaying a transfer with the same id and the same parameters is
//!    idempotent: Ok with the same transfer id and no state change.
//! 3. The no-overdraft invariant (`debits_posted <= credits_posted` on
//!    `*:deposits` / `*:revenue` accounts) is never violated, and failed
//!    operations leave the recorded balances untouched (clean rollback).
//! 4. `transfer_id_from_key` is deterministic for a fixed key and distinct
//!    keys map to distinct ids.
//!
//! Note on visibility: the ledger's only public read API is
//! `balance(tenant_id)`, which exposes the per-tenant `*:deposits` and
//! `*:revenue` accounts. Platform accounts (`platform:*`) are therefore
//! cross-checked through the shadow model plus the structural fact that a
//! `Transfer` carries a single amount applied to one debit and one credit
//! account (asserted for every transfer returned by the ledger).

#[path = "../src/ledger/mod.rs"]
mod ledger;

use std::collections::HashMap;

use ledger::sim::SimLedgerClient;
use ledger::{
    deposits_account, no_overdraft, revenue_account, transfer_id_from_key, LedgerClient,
    TenantBalance, Transfer, CODE_CAPTURE, CODE_DEPOSIT_HOLD, CODE_NO_SHOW_FEE, CODE_REFUND,
    PLATFORM_CLEARING_ACCOUNT, PLATFORM_FEES_ACCOUNT,
};
use proptest::prelude::*;
use uuid::Uuid;

const TENANTS: [&str; 3] = ["prop-tenant-a", "prop-tenant-b", "prop-tenant-c"];

// ---------------------------------------------------------------------------
// Shadow model
// ---------------------------------------------------------------------------

/// Four TigerBeetle-style counters per account, tracked in the shadow model.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
struct Acct {
    debits_pending: u128,
    credits_pending: u128,
    debits_posted: u128,
    credits_posted: u128,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum HoldState {
    Pending,
    /// Resolved by capture (code 101) or no-show fee (code 103); the u16 is
    /// the code that resolved it (needed to model code-keyed replay).
    Posted(u16),
    Voided,
}

#[derive(Debug, Clone)]
struct HoldInfo {
    id: Uuid,
    amount: u64,
    state: HoldState,
    /// Transfer id of the posting transfer (t1) once resolved.
    post_id: Option<u128>,
}

#[derive(Debug, Default)]
struct Model {
    accounts: HashMap<String, Acct>,
    /// Holds per tenant (op `hold` selectors index into these vecs).
    holds: HashMap<usize, Vec<HoldInfo>>,
}

impl Model {
    fn acct(&mut self, name: &str) -> &mut Acct {
        self.accounts.entry(name.to_string()).or_default()
    }

    fn get(&self, name: &str) -> Acct {
        self.accounts.get(name).copied().unwrap_or_default()
    }

    /// Global conservation over every account the ledger told us about,
    /// including the platform accounts that `balance()` does not expose.
    fn assert_conserved(&self) {
        let (mut dp, mut cp, mut dpo, mut cpo) = (0u128, 0u128, 0u128, 0u128);
        for a in self.accounts.values() {
            dp += a.debits_pending;
            cp += a.credits_pending;
            dpo += a.debits_posted;
            cpo += a.credits_posted;
        }
        assert_eq!(dp, cp, "model: pending debits != pending credits");
        assert_eq!(dpo, cpo, "model: posted debits != posted credits");
    }

    fn hold_at(&self, tenant: usize, sel: u16) -> Option<(usize, HoldInfo)> {
        let holds = self.holds.get(&tenant)?;
        if holds.is_empty() {
            return None;
        }
        let idx = (sel as usize) % holds.len();
        Some((idx, holds[idx].clone()))
    }
}

/// Canonical, comparable view of a `TenantBalance` (the ledger type does not
/// derive PartialEq).
fn bal_key(b: &TenantBalance) -> Vec<(String, u64, u64, u64, u64)> {
    let mut v: Vec<_> = b
        .accounts
        .iter()
        .map(|a| {
            (
                a.account.clone(),
                a.debits_pending,
                a.credits_pending,
                a.debits_posted,
                a.credits_posted,
            )
        })
        .collect();
    v.sort();
    v
}

/// Structural double-entry check on a transfer the ledger reported: it must
/// move a positive amount between two distinct accounts.
fn assert_transfer_shape(t: &Transfer) {
    assert!(t.amount > 0, "transfer with zero amount: {t:?}");
    assert_ne!(
        t.debit_account, t.credit_account,
        "self-transfer would break double-entry: {t:?}"
    );
}

fn apply_hold_model(model: &mut Model, tenant: &str, amount: u64) {
    model.acct(PLATFORM_CLEARING_ACCOUNT).debits_pending += amount as u128;
    model.acct(&deposits_account(tenant)).credits_pending += amount as u128;
}

/// Mirror of `SimState::resolve_pending` + the revenue/fee split in
/// `capture_like`, driven entirely by the transfers the ledger returned.
fn apply_capture_model(
    model: &mut Model,
    tenant: &str,
    hold_amount: u64,
    post_amount: u64,
    fee: u64,
) {
    let deposits = deposits_account(tenant);
    let revenue = revenue_account(tenant);
    let net = post_amount - fee;
    // t1: pending -> posted on the hold's accounts.
    model.acct(PLATFORM_CLEARING_ACCOUNT).debits_pending -= hold_amount as u128;
    model.acct(PLATFORM_CLEARING_ACCOUNT).debits_posted += post_amount as u128;
    model.acct(&deposits).credits_pending -= hold_amount as u128;
    model.acct(&deposits).credits_posted += post_amount as u128;
    // t2: deposits -> revenue (net of fee).
    model.acct(&deposits).debits_posted += net as u128;
    model.acct(&revenue).credits_posted += net as u128;
    // t3: deposits -> platform:fees.
    if fee > 0 {
        model.acct(&deposits).debits_posted += fee as u128;
        model.acct(PLATFORM_FEES_ACCOUNT).credits_posted += fee as u128;
    }
}

fn apply_void_model(model: &mut Model, tenant: &str, hold_amount: u64) {
    model.acct(PLATFORM_CLEARING_ACCOUNT).debits_pending -= hold_amount as u128;
    model.acct(&deposits_account(tenant)).credits_pending -= hold_amount as u128;
}

fn apply_posted_refund_model(model: &mut Model, tenant: &str, amount: u64) {
    model.acct(&revenue_account(tenant)).debits_posted += amount as u128;
    model.acct(PLATFORM_CLEARING_ACCOUNT).credits_posted += amount as u128;
}

// ---------------------------------------------------------------------------
// Operation strategy
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
enum Op {
    Hold { tenant: usize, amount: u64 },
    Capture { tenant: usize, hold: u16, amount: Option<u64> },
    Refund { tenant: usize, hold: Option<u16>, amount: u64 },
    NoShowFee { tenant: usize, hold: u16, amount: u64 },
}

fn op_strategy() -> impl Strategy<Value = Op> {
    // Bounded amounts keep u64 arithmetic far from overflow and make
    // overdraft failures (refund of more than earned revenue) frequent.
    prop_oneof![
        4 => (0usize..TENANTS.len(), 1u64..=50_000u64)
            .prop_map(|(tenant, amount)| Op::Hold { tenant, amount }),
        3 => (0usize..TENANTS.len(), any::<u16>(), proptest::option::of(0u64..=60_000u64))
            .prop_map(|(tenant, hold, amount)| Op::Capture { tenant, hold, amount }),
        2 => (0usize..TENANTS.len(), proptest::option::of(any::<u16>()), 0u64..=60_000u64)
            .prop_map(|(tenant, hold, amount)| Op::Refund { tenant, hold, amount }),
        2 => (0usize..TENANTS.len(), any::<u16>(), 0u64..=60_000u64)
            .prop_map(|(tenant, hold, amount)| Op::NoShowFee { tenant, hold, amount }),
    ]
}

fn seq_strategy() -> impl Strategy<Value = (u64, Vec<Op>)> {
    (0u64..=2_000u64, prop::collection::vec(op_strategy(), 1..40))
}

// ---------------------------------------------------------------------------
// Driver
// ---------------------------------------------------------------------------

async fn balances(client: &SimLedgerClient) -> HashMap<usize, Vec<(String, u64, u64, u64, u64)>> {
    let mut out = HashMap::new();
    for (i, t) in TENANTS.iter().enumerate() {
        let b = client.balance(t).await.expect("balance must succeed");
        assert_eq!(b.tenant_id, *t);
        out.insert(i, bal_key(&b));
    }
    out
}

/// Assert that the balances recorded by the ledger match the shadow model
/// exactly for every tenant account, and that no-overdraft holds on them.
fn assert_state_matches_model(
    model: &Model,
    balances: &HashMap<usize, Vec<(String, u64, u64, u64, u64)>>,
    ctx: &str,
) {
    for (i, accounts) in balances {
        let tenant = TENANTS[*i];
        for (name, dp, cp, dpo, cpo) in accounts {
            let m = model.get(name);
            assert_eq!(
                (*dp as u128, *cp as u128),
                (m.debits_pending, m.credits_pending),
                "{ctx}: pending mismatch on {name}"
            );
            assert_eq!(
                (*dpo as u128, *cpo as u128),
                (m.debits_posted, m.credits_posted),
                "{ctx}: posted mismatch on {name}"
            );
            if no_overdraft(name) {
                assert!(
                    dpo <= cpo,
                    "{ctx}: no-overdraft violated on {name}: debits {dpo} > credits {cpo}"
                );
            }
        }
        // The model must not track tenant accounts the ledger does not report.
        for name in model.accounts.keys() {
            if name.starts_with(&format!("tenant:{tenant}:")) {
                assert!(
                    accounts.iter().any(|(n, ..)| n == name),
                    "{ctx}: ledger missing account {name}"
                );
            }
        }
    }
}

/// What a successful op produced: the primary transfer id returned by the
/// ledger plus the resolved hold id (if the op referenced one), so replays
/// use the exact original parameters.
#[derive(Debug, Clone, Copy)]
struct OpRecord {
    primary_id: u128,
    hold_id: Option<Uuid>,
}

/// Execute one op against the ledger with the caller-supplied transfer id,
/// keeping the shadow model in sync and asserting the per-op semantics.
/// Returns the record on success; None means the ledger rejected the op (or
/// no op was possible), in which case the caller asserts the state is
/// byte-identical to before the call.
async fn run_op(
    client: &SimLedgerClient,
    model: &mut Model,
    fee_bps: u64,
    op: &Op,
    tid: Uuid,
    step: usize,
) -> Option<OpRecord> {
    let ctx = || format!("step {step} op {op:?}");
    match *op {
        Op::Hold { tenant, amount } => {
            let tname = TENANTS[tenant];
            match client.hold_deposit(tname, tid, amount).await {
                Ok(t) => {
                    assert_transfer_shape(&t);
                    assert_eq!(t.code, CODE_DEPOSIT_HOLD, "{}", ctx());
                    assert_eq!(t.amount, amount, "{}", ctx());
                    assert_eq!(t.id, tid.as_u128(), "{}", ctx());
                    apply_hold_model(model, tname, amount);
                    model.holds.entry(tenant).or_default().push(HoldInfo {
                        id: tid,
                        amount,
                        state: HoldState::Pending,
                        post_id: None,
                    });
                    Some(OpRecord { primary_id: t.id, hold_id: None })
                }
                Err(e) => panic!("{}: hold of valid amount failed: {e}", ctx()),
            }
        }
        Op::Capture { tenant, hold, amount } => {
            let tname = TENANTS[tenant];
            let (idx, h) = model.hold_at(tenant, hold)?;
            match client.capture(tname, h.id, tid, amount).await {
                Ok(res) => {
                    assert_transfer_shape(&res.post);
                    assert_transfer_shape(&res.revenue);
                    if let Some(f) = &res.platform_fee {
                        assert_transfer_shape(f);
                    }
                    match h.state {
                        HoldState::Pending => {
                            // Fresh capture: t1/t2/t3 are constructed together,
                            // so the split must be internally consistent.
                            if let Some(f) = &res.platform_fee {
                                assert_eq!(
                                    res.post.amount,
                                    res.revenue.amount + f.amount,
                                    "{}",
                                    ctx()
                                );
                            } else {
                                assert_eq!(res.post.amount, res.revenue.amount, "{}", ctx());
                            }
                            let post = amount.unwrap_or(h.amount);
                            let fee = post * fee_bps / 10_000;
                            assert_eq!(res.post.code, CODE_CAPTURE, "{}", ctx());
                            assert_eq!(res.post.id, tid.as_u128(), "{}", ctx());
                            assert_eq!(res.post.amount, post, "{}", ctx());
                            assert_eq!(res.revenue.amount, post - fee, "{}", ctx());
                            assert_eq!(
                                res.platform_fee.as_ref().map(|f| f.amount),
                                (fee > 0).then_some(fee),
                                "{}",
                                ctx()
                            );
                            apply_capture_model(model, tname, h.amount, post, fee);
                            let m = &mut model.holds.get_mut(&tenant).unwrap()[idx];
                            m.state = HoldState::Posted(CODE_CAPTURE);
                            m.post_id = Some(res.post.id);
                        }
                        // Idempotent replay keyed by hold_id: the fresh tid is
                        // discarded by the ledger and the recorded posting
                        // transfer is returned. NOTE (sim wart, reported, not
                        // fixed — src/ledger/sim.rs is outside W41-6 Rust
                        // ownership): `SimState::rebuild_capture` looks the
                        // revenue/fee split up by (code, accounts) only, so on
                        // replay of one of several captured holds of the same
                        // tenant those transfers may be attributed to a
                        // different hold. The posting transfer (keyed by
                        // pending_id) and — critically — the ledger STATE are
                        // exact; we assert the unambiguous parts here.
                        HoldState::Posted(CODE_CAPTURE) => {
                            assert_eq!(Some(res.post.id), h.post_id, "{}", ctx());
                            assert_eq!(res.post.code, CODE_CAPTURE, "{}", ctx());
                        }
                        other => panic!(
                            "{}: capture succeeded on hold in state {other:?}",
                            ctx()
                        ),
                    }
                    Some(OpRecord { primary_id: res.post.id, hold_id: Some(h.id) })
                }
                Err(e) => {
                    // Legal failures: zero/over-hold amount on a pending hold,
                    // already-voided hold, or a code mismatch when replaying a
                    // hold resolved by a different code.
                    let post = amount.unwrap_or(h.amount);
                    let legal = match h.state {
                        HoldState::Pending => post == 0 || post > h.amount,
                        HoldState::Voided => true,
                        HoldState::Posted(code) => code != CODE_CAPTURE,
                    };
                    assert!(legal, "{}: unexpected capture error: {e}", ctx());
                    None
                }
            }
        }
        Op::Refund { tenant, hold, amount } => {
            let tname = TENANTS[tenant];
            let h = hold.and_then(|sel| model.hold_at(tenant, sel));
            match client
                .refund(tname, tid, h.as_ref().map(|(_, x)| x.id), amount)
                .await
            {
                Ok(t) => {
                    assert_transfer_shape(&t);
                    assert_eq!(t.code, CODE_REFUND, "{}", ctx());
                    match h.as_ref().map(|(_, x)| x.state) {
                        Some(HoldState::Pending) => {
                            // Void of a pending hold; `amount` ignored.
                            let (idx, hh) = h.clone().expect("pending hold is Some");
                            assert_eq!(t.amount, hh.amount, "{}", ctx());
                            apply_void_model(model, tname, hh.amount);
                            model.holds.get_mut(&tenant).unwrap()[idx].state = HoldState::Voided;
                        }
                        _ => {
                            // Posted refund: revenue -> platform:clearing.
                            assert!(amount > 0, "{}", ctx());
                            assert_eq!(t.amount, amount, "{}", ctx());
                            apply_posted_refund_model(model, tname, amount);
                        }
                    }
                    Some(OpRecord {
                        primary_id: t.id,
                        hold_id: h.as_ref().map(|(_, x)| x.id),
                    })
                }
                Err(e) => {
                    // Legal failures: voided hold, zero amount on the posted
                    // path, or refund exceeding earned revenue (no overdraft).
                    let legal = match h.as_ref().map(|(_, x)| x.state) {
                        Some(HoldState::Voided) => true,
                        _ => {
                            amount == 0 || {
                                let rev = model.get(&revenue_account(tname));
                                amount as u128 > rev.credits_posted - rev.debits_posted
                            }
                        }
                    };
                    assert!(legal, "{}: unexpected refund error: {e}", ctx());
                    None
                }
            }
        }
        Op::NoShowFee { tenant, hold, amount } => {
            let tname = TENANTS[tenant];
            let (idx, h) = model.hold_at(tenant, hold)?;
            match client.no_show_fee(tname, h.id, tid, amount).await {
                Ok(res) => {
                    assert_transfer_shape(&res.post);
                    match h.state {
                        HoldState::Pending => {
                            let fee = amount * fee_bps / 10_000;
                            assert_eq!(res.post.code, CODE_NO_SHOW_FEE, "{}", ctx());
                            assert_eq!(res.post.id, tid.as_u128(), "{}", ctx());
                            assert_eq!(res.post.amount, amount, "{}", ctx());
                            assert_eq!(res.revenue.amount, amount - fee, "{}", ctx());
                            apply_capture_model(model, tname, h.amount, amount, fee);
                            let m = &mut model.holds.get_mut(&tenant).unwrap()[idx];
                            m.state = HoldState::Posted(CODE_NO_SHOW_FEE);
                            m.post_id = Some(res.post.id);
                        }
                        HoldState::Posted(CODE_NO_SHOW_FEE) => {
                            assert_eq!(Some(res.post.id), h.post_id, "{}", ctx());
                        }
                        other => panic!(
                            "{}: no-show fee succeeded on hold in state {other:?}",
                            ctx()
                        ),
                    }
                    Some(OpRecord { primary_id: res.post.id, hold_id: Some(h.id) })
                }
                Err(e) => {
                    let legal = match h.state {
                        HoldState::Pending => amount == 0 || amount > h.amount,
                        HoldState::Voided => true,
                        HoldState::Posted(code) => code != CODE_NO_SHOW_FEE,
                    };
                    assert!(legal, "{}: unexpected no-show-fee error: {e}", ctx());
                    None
                }
            }
        }
    }
}

/// Replay one previously-successful op with the exact same transfer id and
/// resolved hold id, asserting idempotence: Ok, same primary transfer id,
/// and (via the caller) no state change.
async fn replay_op(client: &SimLedgerClient, op: &Op, tid: Uuid, rec: OpRecord, step: usize) {
    let ctx = || format!("replay of step {step} op {op:?}");
    match *op {
        Op::Hold { tenant, amount } => {
            let t = client
                .hold_deposit(TENANTS[tenant], tid, amount)
                .await
                .unwrap_or_else(|e| panic!("{}: {e}", ctx()));
            assert_eq!(t.id, rec.primary_id, "{}", ctx());
        }
        Op::Capture { tenant, amount, .. } => {
            let res = client
                .capture(TENANTS[tenant], rec.hold_id.expect("recorded"), tid, amount)
                .await
                .unwrap_or_else(|e| panic!("{}: {e}", ctx()));
            assert_eq!(res.post.id, rec.primary_id, "{}", ctx());
        }
        Op::Refund { tenant, amount, .. } => {
            let t = client
                .refund(TENANTS[tenant], tid, rec.hold_id, amount)
                .await
                .unwrap_or_else(|e| panic!("{}: {e}", ctx()));
            assert_eq!(t.id, rec.primary_id, "{}", ctx());
        }
        Op::NoShowFee { tenant, amount, .. } => {
            let res = client
                .no_show_fee(TENANTS[tenant], rec.hold_id.expect("recorded"), tid, amount)
                .await
                .unwrap_or_else(|e| panic!("{}: {e}", ctx()));
            assert_eq!(res.post.id, rec.primary_id, "{}", ctx());
        }
    }
}

/// Execute a random op sequence against a fresh ledger, checking conservation
/// + model match + no-overdraft after every step and clean rollback on every
/// failed op. When `replay` is set, every successful op is additionally
/// replayed (same id + params) immediately after execution, and the whole
/// journal is replayed again at the end; replays must never move the state.
async fn run_sequence(fee_bps: u64, ops: &[Op], replay: bool) {
    let client = SimLedgerClient::new(fee_bps);
    let mut model = Model::default();

    // Journal of successful ops: (op, transfer id used, record returned).
    // Transfer ids are unique per op with overwhelming probability (random
    // v4), and id-keyed replay is exactly what is under test.
    let mut journal: Vec<(Op, Uuid, OpRecord)> = Vec::new();

    for (step, op) in ops.iter().enumerate() {
        let tid = Uuid::new_v4();
        let pre = balances(&client).await;
        let ok = run_op(&client, &mut model, fee_bps, op, tid, step).await;
        let post = balances(&client).await;

        match ok {
            Some(rec) => {
                journal.push((op.clone(), tid, rec));
                if replay {
                    replay_op(&client, op, tid, rec, step).await;
                    let after = balances(&client).await;
                    assert_eq!(post, after, "step {step}: inline replay changed state: {op:?}");
                }
            }
            None => {
                // Property 3b: failed ops roll back cleanly.
                assert_eq!(pre, post, "step {step}: failed op changed state: {op:?}");
            }
        }

        // Properties 1 + 3a: conservation, model match, no-overdraft.
        model.assert_conserved();
        assert_state_matches_model(&model, &post, &format!("step {step}"));
    }

    // Property 2 (whole-sequence): replay every successful op once more at
    // the end; the ledger state must not move at all.
    if replay {
        let before = balances(&client).await;
        for (step, (op, tid, rec)) in journal.iter().enumerate() {
            replay_op(&client, op, *tid, *rec, step).await;
        }
        let after = balances(&client).await;
        assert_eq!(before, after, "end-of-sequence replay changed state");
    }
}

// ---------------------------------------------------------------------------
// Properties
// ---------------------------------------------------------------------------

proptest! {
    #![proptest_config(ProptestConfig::with_cases(128))]

    /// P1: random hold/capture/refund/no-show sequences conserve Σ debits ==
    /// Σ credits (pending and posted) at every step, matching a shadow model
    /// of the TigerBeetle semantics.
    #[test]
    fn prop_double_entry_conserved((fee_bps, ops) in seq_strategy()) {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(run_sequence(fee_bps, &ops, false));
    }

    /// P2: replaying any successful op with the same id + params is
    /// idempotent (Ok, same transfer id, no state change) — checked inline
    /// after every op and again over the whole sequence journal.
    #[test]
    fn prop_replay_is_idempotent((fee_bps, ops) in seq_strategy()) {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(run_sequence(fee_bps, &ops, true));
    }

    /// P3: no-overdraft never violated; failed ops roll back cleanly. Biased
    /// towards over-drawing posted-path refunds (fresh/large amounts).
    #[test]
    fn prop_no_overdraft_and_clean_rollback(
        fee_bps in 0u64..=2_000u64,
        ops in prop::collection::vec(
            (
                0usize..TENANTS.len(),
                0u64..=100_000u64,
                any::<bool>(),
                proptest::option::of(any::<u16>()),
            )
                .prop_map(|(tenant, amount, do_hold, hold)| {
                    if do_hold {
                        Op::Hold { tenant, amount: amount.max(1) }
                    } else {
                        Op::Refund { tenant, hold, amount }
                    }
                }),
            1..40
        )
    ) {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(run_sequence(fee_bps, &ops, false));
    }

    /// P4a: `transfer_id_from_key` is deterministic for any fixed non-empty
    /// key (UUID v5); an empty key falls back to random v4.
    #[test]
    fn prop_transfer_id_deterministic(key in ".*") {
        let a = transfer_id_from_key(Some(&key));
        let b = transfer_id_from_key(Some(&key));
        if key.is_empty() {
            prop_assert_eq!(a.get_version(), Some(uuid::Version::Random));
            prop_assert_eq!(b.get_version(), Some(uuid::Version::Random));
        } else {
            prop_assert_eq!(a, b);
            prop_assert_eq!(a.get_version(), Some(uuid::Version::Sha1));
        }
    }

    /// P4b: distinct non-empty keys map to distinct transfer ids.
    #[test]
    fn prop_transfer_id_distinct_keys(
        k1 in "[ -~]{1,64}",
        k2 in "[ -~]{1,64}",
    ) {
        prop_assume!(k1 != k2);
        prop_assert_ne!(
            transfer_id_from_key(Some(&k1)),
            transfer_id_from_key(Some(&k2))
        );
    }

    /// P4c: None and empty keys fall back to random (v4) ids.
    #[test]
    fn prop_transfer_id_random_fallback(_dummy in 0..8u8) {
        let a = transfer_id_from_key(None);
        let b = transfer_id_from_key(Some(""));
        prop_assert_ne!(a, b);
        prop_assert_eq!(a.get_version(), Some(uuid::Version::Random));
        prop_assert_eq!(b.get_version(), Some(uuid::Version::Random));
    }
}
