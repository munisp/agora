//! In-memory double-entry ledger (ADR-0007 fallback, `LEDGER_IMPL=sim`).
//!
//! Genuinely double-entry: every transfer debits one account and credits
//! another, so Σ debits == Σ credits (both pending and posted) always holds.
//! Semantics mirror TigerBeetle:
//! - two-phase transfers: `pending` → resolved by a `post_pending` or
//!   `void_pending` transfer referencing `pending_id`;
//! - posting an amount smaller than the pending amount releases the remainder;
//! - transfers are idempotent by id: replaying the same id with the same
//!   parameters returns the recorded transfer, with different parameters is an
//!   error (`ExistsWithDifferentParameters`);
//! - liability accounts (`*:deposits`, `*:revenue`) enforce
//!   `debits_posted <= credits_posted`.
//!
//! NOTE (SPEC-W42): the sim deliberately does NOT enforce TigerBeetle's
//! code-matching rule (a POST/VOID_PENDING_TRANSFER leg must carry the
//! pending hold's own code — real TB rejects a mismatch with
//! `pending_transfer_has_different_code`). The sim records the operation
//! code (101/102/103) on post/void legs for observability; its externally
//! visible behavior is unchanged. The live client (`tigerbeetle.rs`) carries
//! `CODE_DEPOSIT_HOLD` on those legs, and its structure tests pin the rule.

use std::collections::HashMap;

use async_trait::async_trait;
use chrono::Utc;
use tokio::sync::Mutex;
use uuid::Uuid;

use super::*;

#[derive(Debug, Default, Clone)]
struct SimState {
    accounts: HashMap<String, Account>,
    transfers: HashMap<u128, Transfer>,
}

impl SimState {
    fn ensure_account(&mut self, ledger: u32, name: &str, code: u16) {
        self.accounts.entry(name.to_string()).or_insert_with(|| Account {
            id: account_id(name),
            name: name.to_string(),
            ledger,
            code,
            debits_pending: 0,
            credits_pending: 0,
            debits_posted: 0,
            credits_posted: 0,
        });
    }

    fn account_mut(&mut self, name: &str) -> Result<&mut Account, LedgerError> {
        self.accounts
            .get_mut(name)
            .ok_or_else(|| LedgerError::AccountNotFound(name.to_string()))
    }

    /// Same id + same parameters => idempotent replay; same id + different
    /// parameters => conflict error (TigerBeetle `exists_with_different_*`).
    fn check_replay(&self, t: &Transfer) -> Result<Option<Transfer>, LedgerError> {
        if let Some(existing) = self.transfers.get(&t.id) {
            let same = existing.debit_account == t.debit_account
                && existing.credit_account == t.credit_account
                && existing.amount == t.amount
                && existing.code == t.code
                && existing.flag == t.flag
                && existing.pending_id == t.pending_id;
            if same {
                return Ok(Some(existing.clone()));
            }
            return Err(LedgerError::ExistsWithDifferentParameters(
                t.id_string(),
            ));
        }
        Ok(None)
    }

    /// Apply a posted transfer to account balances, enforcing no-overdraft.
    fn apply_posted(&mut self, t: &Transfer) -> Result<(), LedgerError> {
        {
            let debit = self.account_mut(&t.debit_account)?;
            debit.debits_posted += t.amount;
        }
        {
            let credit = self.account_mut(&t.credit_account)?;
            credit.credits_posted += t.amount;
        }
        for name in [&t.debit_account, &t.credit_account] {
            if no_overdraft(name) {
                let a = self.accounts.get(name).expect("account exists");
                if a.debits_posted > a.credits_posted {
                    // Roll back to keep the ledger consistent.
                    self.account_mut(&t.debit_account)?.debits_posted -= t.amount;
                    self.account_mut(&t.credit_account)?.credits_posted -= t.amount;
                    return Err(LedgerError::ExceedsCredits(name.to_string()));
                }
            }
        }
        Ok(())
    }

    /// Apply a pending transfer (two-phase commit phase 1).
    fn apply_pending(&mut self, t: &Transfer) -> Result<(), LedgerError> {
        self.account_mut(&t.debit_account)?.debits_pending += t.amount;
        self.account_mut(&t.credit_account)?.credits_pending += t.amount;
        Ok(())
    }

    /// Resolve a pending hold: move `posted_amount` from pending to posted on
    /// both accounts and release the remainder of the pending amounts.
    fn resolve_pending(
        &mut self,
        hold: &Transfer,
        posted_amount: u64,
        void: bool,
    ) -> Result<(), LedgerError> {
        let remainder = hold.amount - posted_amount;
        {
            let debit = self.account_mut(&hold.debit_account)?;
            debit.debits_pending -= hold.amount;
            debit.debits_posted += posted_amount;
        }
        {
            let credit = self.account_mut(&hold.credit_account)?;
            credit.credits_pending -= hold.amount;
            credit.credits_posted += posted_amount;
        }
        let _ = remainder; // remainder is released implicitly above
        if void {
            debug_assert_eq!(posted_amount, 0);
        }
        Ok(())
    }

    fn insert_transfer(&mut self, t: Transfer) -> Transfer {
        self.transfers.insert(t.id, t.clone());
        t
    }

    /// Rebuild a capture result on idempotent replay: find the posting
    /// transfer plus the revenue/fee splits previously recorded for this hold.
    fn rebuild_capture(
        &self,
        tenant_id: &str,
        hold_id: u128,
        code: u16,
    ) -> Result<CaptureResult, LedgerError> {
        let post = self
            .transfers
            .values()
            .find(|t| {
                t.pending_id == Some(hold_id) && t.flag == TransferFlag::PostPending && t.code == code
            })
            .cloned()
            .ok_or_else(|| LedgerError::TransferNotFound(format!("{hold_id:032x}")))?;
        let revenue = self
            .transfers
            .values()
            .find(|t| {
                t.code == code
                    && t.flag == TransferFlag::None
                    && t.debit_account == deposits_account(tenant_id)
                    && t.credit_account == revenue_account(tenant_id)
            })
            .cloned()
            .ok_or_else(|| LedgerError::Backend("capture split missing".into()))?;
        let platform_fee = self
            .transfers
            .values()
            .find(|t| {
                t.code == code
                    && t.flag == TransferFlag::None
                    && t.debit_account == deposits_account(tenant_id)
                    && t.credit_account == PLATFORM_FEES_ACCOUNT
            })
            .cloned();
        Ok(CaptureResult {
            post,
            revenue,
            platform_fee,
        })
    }
}

pub struct SimLedgerClient {
    state: Mutex<SimState>,
    ledger_id: u32,
    fee_bps: u64,
}

impl SimLedgerClient {
    pub fn new(fee_bps: u64) -> Self {
        Self {
            state: Mutex::new(SimState::default()),
            ledger_id: LEDGER_ID,
            fee_bps,
        }
    }

    #[cfg(test)]
    async fn snapshot(&self) -> SimState {
        self.state.lock().await.clone()
    }

    /// Transfer constructor; the nine fields mirror the TigerBeetle transfer
    /// record 1:1, so a params struct would only obscure the mapping.
    #[allow(clippy::too_many_arguments)]
    fn new_transfer(
        &self,
        id: Uuid,
        debit: String,
        credit: String,
        amount: u64,
        code: u16,
        state: TransferState,
        flag: TransferFlag,
        pending_id: Option<u128>,
    ) -> Transfer {
        Transfer {
            id: id.as_u128(),
            debit_account: debit,
            credit_account: credit,
            amount,
            ledger: self.ledger_id,
            code,
            state,
            flag,
            pending_id,
            created_at: Utc::now(),
        }
    }

    /// Shared implementation for capture (code 101) and no-show fee (code 103):
    /// post `post_amount` of the pending hold, then split into revenue/fee.
    async fn capture_like(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        post_amount: u64,
        code: u16,
    ) -> Result<CaptureResult, LedgerError> {
        if post_amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let mut st = self.state.lock().await;

        let hold = st
            .transfers
            .get(&hold_id.as_u128())
            .cloned()
            .ok_or_else(|| LedgerError::TransferNotFound(format!("{}", hold_id)))?;
        if hold.code != CODE_DEPOSIT_HOLD {
            return Err(LedgerError::NotPending(format!("{}", hold_id)));
        }
        match hold.state {
            TransferState::Pending => {}
            TransferState::Voided => {
                return Err(LedgerError::AlreadyResolved(format!("{}", hold_id)))
            }
            TransferState::Posted => {
                // Idempotent replay keyed by hold_id.
                return st.rebuild_capture(tenant_id, hold_id.as_u128(), code);
            }
        }
        if post_amount > hold.amount {
            return Err(LedgerError::ExceedsPendingAmount);
        }

        let deposits = deposits_account(tenant_id);
        let revenue = revenue_account(tenant_id);
        st.ensure_account(self.ledger_id, &deposits, ACCOUNT_CODE_TENANT_DEPOSITS);
        st.ensure_account(self.ledger_id, &revenue, ACCOUNT_CODE_TENANT_REVENUE);
        st.ensure_account(self.ledger_id, PLATFORM_FEES_ACCOUNT, ACCOUNT_CODE_PLATFORM_FEES);

        // t1: posting transfer resolving the hold.
        let t1 = self.new_transfer(
            transfer_id,
            hold.debit_account.clone(),
            hold.credit_account.clone(),
            post_amount,
            code,
            TransferState::Posted,
            TransferFlag::PostPending,
            Some(hold.id),
        );
        if let Some(existing) = st.check_replay(&t1)? {
            // Replay of the same capture request: rebuild the full result.
            let _ = existing;
            return st.rebuild_capture(tenant_id, hold_id.as_u128(), code);
        }
        st.resolve_pending(&hold, post_amount, false)?;
        st.transfers
            .get_mut(&hold.id)
            .expect("hold exists")
            .state = TransferState::Posted;
        let t1 = st.insert_transfer(t1);

        // t2: deposits -> revenue (net of platform fee).
        let fee = post_amount * self.fee_bps / 10_000;
        let net = post_amount - fee;
        let t2_id = Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("capture-revenue:{}", t1.id_string()).as_bytes(),
        );
        let t2 = self.new_transfer(
            t2_id,
            deposits.clone(),
            revenue,
            net,
            code,
            TransferState::Posted,
            TransferFlag::None,
            None,
        );
        st.apply_posted(&t2)?;
        let t2 = st.insert_transfer(t2);

        // t3: deposits -> platform:fees (skipped when fee rounds to zero).
        let t3 = if fee > 0 {
            let t3_id = Uuid::new_v5(
                &Uuid::NAMESPACE_URL,
                format!("capture-fee:{}", t1.id_string()).as_bytes(),
            );
            let t3 = self.new_transfer(
                t3_id,
                deposits,
                PLATFORM_FEES_ACCOUNT.to_string(),
                fee,
                code,
                TransferState::Posted,
                TransferFlag::None,
                None,
            );
            st.apply_posted(&t3)?;
            Some(st.insert_transfer(t3))
        } else {
            None
        };

        Ok(CaptureResult {
            post: t1,
            revenue: t2,
            platform_fee: t3,
        })
    }
}

#[async_trait]
impl LedgerClient for SimLedgerClient {
    async fn create_accounts(&self, tenant_id: &str) -> Result<Vec<Account>, LedgerError> {
        let mut st = self.state.lock().await;
        let names = [
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
        for (name, code) in &names {
            st.ensure_account(self.ledger_id, name, *code);
        }
        Ok(names
            .iter()
            .map(|(name, _)| st.accounts.get(name).expect("just ensured").clone())
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
        let mut st = self.state.lock().await;
        st.ensure_account(
            self.ledger_id,
            &deposits_account(tenant_id),
            ACCOUNT_CODE_TENANT_DEPOSITS,
        );
        st.ensure_account(
            self.ledger_id,
            PLATFORM_CLEARING_ACCOUNT,
            ACCOUNT_CODE_PLATFORM_CLEARING,
        );
        let t = self.new_transfer(
            transfer_id,
            PLATFORM_CLEARING_ACCOUNT.to_string(),
            deposits_account(tenant_id),
            amount,
            CODE_DEPOSIT_HOLD,
            TransferState::Pending,
            TransferFlag::None,
            None,
        );
        if let Some(existing) = st.check_replay(&t)? {
            return Ok(existing);
        }
        st.apply_pending(&t)?;
        Ok(st.insert_transfer(t))
    }

    async fn capture(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        amount: Option<u64>,
    ) -> Result<CaptureResult, LedgerError> {
        let post_amount = match amount {
            Some(a) => a,
            None => {
                let st = self.state.lock().await;
                st.transfers
                    .get(&hold_id.as_u128())
                    .map(|t| t.amount)
                    .ok_or_else(|| LedgerError::TransferNotFound(format!("{}", hold_id)))?
            }
        };
        self.capture_like(tenant_id, hold_id, transfer_id, post_amount, CODE_CAPTURE)
            .await
    }

    async fn refund(
        &self,
        tenant_id: &str,
        transfer_id: Uuid,
        hold_id: Option<Uuid>,
        amount: u64,
    ) -> Result<Transfer, LedgerError> {
        let mut st = self.state.lock().await;

        // Replay by transfer id first.
        if let Some(existing) = st.transfers.get(&transfer_id.as_u128()) {
            if existing.code == CODE_REFUND {
                return Ok(existing.clone());
            }
            return Err(LedgerError::ExistsWithDifferentParameters(
                transfer_id.to_string(),
            ));
        }

        // Path 1: hold still pending -> void it (releases the full hold).
        if let Some(h) = hold_id {
            let hold = st
                .transfers
                .get(&h.as_u128())
                .cloned()
                .ok_or_else(|| LedgerError::TransferNotFound(format!("{}", h)))?;
            match hold.state {
                TransferState::Pending => {
                    let t = self.new_transfer(
                        transfer_id,
                        hold.debit_account.clone(),
                        hold.credit_account.clone(),
                        hold.amount,
                        CODE_REFUND,
                        TransferState::Posted,
                        TransferFlag::VoidPending,
                        Some(hold.id),
                    );
                    st.resolve_pending(&hold, 0, true)?;
                    st.transfers
                        .get_mut(&hold.id)
                        .expect("hold exists")
                        .state = TransferState::Voided;
                    return Ok(st.insert_transfer(t));
                }
                TransferState::Voided => {
                    return Err(LedgerError::AlreadyResolved(format!("{}", h)))
                }
                TransferState::Posted => { /* fall through to posted refund */ }
            }
        }

        // Path 2: refund after capture — move money back to the customer.
        if amount == 0 {
            return Err(LedgerError::InvalidAmount);
        }
        let revenue = revenue_account(tenant_id);
        st.ensure_account(self.ledger_id, &revenue, ACCOUNT_CODE_TENANT_REVENUE);
        st.ensure_account(
            self.ledger_id,
            PLATFORM_CLEARING_ACCOUNT,
            ACCOUNT_CODE_PLATFORM_CLEARING,
        );
        let t = self.new_transfer(
            transfer_id,
            revenue,
            PLATFORM_CLEARING_ACCOUNT.to_string(),
            amount,
            CODE_REFUND,
            TransferState::Posted,
            TransferFlag::None,
            None,
        );
        st.apply_posted(&t)?;
        Ok(st.insert_transfer(t))
    }

    async fn no_show_fee(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
        amount: u64,
    ) -> Result<CaptureResult, LedgerError> {
        self.capture_like(tenant_id, hold_id, transfer_id, amount, CODE_NO_SHOW_FEE)
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
        let mut st = self.state.lock().await;
        let revenue = revenue_account(tenant_id);
        st.ensure_account(self.ledger_id, &revenue, ACCOUNT_CODE_TENANT_REVENUE);
        st.ensure_account(
            self.ledger_id,
            PLATFORM_PAYOUTS_ACCOUNT,
            ACCOUNT_CODE_PLATFORM_PAYOUTS,
        );
        let t = self.new_transfer(
            transfer_id,
            revenue,
            PLATFORM_PAYOUTS_ACCOUNT.to_string(),
            amount,
            CODE_PAYOUT,
            TransferState::Posted,
            TransferFlag::None,
            None,
        );
        if let Some(existing) = st.check_replay(&t)? {
            return Ok(existing);
        }
        st.apply_posted(&t)?;
        Ok(st.insert_transfer(t))
    }

    async fn balance(&self, tenant_id: &str) -> Result<TenantBalance, LedgerError> {
        let mut st = self.state.lock().await;
        st.ensure_account(
            self.ledger_id,
            &deposits_account(tenant_id),
            ACCOUNT_CODE_TENANT_DEPOSITS,
        );
        st.ensure_account(
            self.ledger_id,
            &revenue_account(tenant_id),
            ACCOUNT_CODE_TENANT_REVENUE,
        );
        let prefix = format!("tenant:{tenant_id}:");
        let mut accounts: Vec<AccountBalance> = st
            .accounts
            .values()
            .filter(|a| a.name.starts_with(&prefix))
            .map(|a| AccountBalance {
                account: a.name.clone(),
                id: format!("{:032x}", a.id),
                debits_pending: a.debits_pending,
                credits_pending: a.credits_pending,
                debits_posted: a.debits_posted,
                credits_posted: a.credits_posted,
                posted_net: a.credits_posted as i128 - a.debits_posted as i128,
                pending_net: a.credits_pending as i128 - a.debits_pending as i128,
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
// Unit tests: balance invariants + TigerBeetle-compatible semantics
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    const TENANT: &str = "t-111";

    fn sim(fee_bps: u64) -> SimLedgerClient {
        SimLedgerClient::new(fee_bps)
    }

    /// Global double-entry conservation + no-overdraft invariants.
    async fn assert_invariants(client: &SimLedgerClient) {
        let st = client.snapshot().await;
        let (mut dp, mut cp, mut dpo, mut cpo) = (0u128, 0u128, 0u128, 0u128);
        for a in st.accounts.values() {
            dp += a.debits_pending as u128;
            cp += a.credits_pending as u128;
            dpo += a.debits_posted as u128;
            cpo += a.credits_posted as u128;
            if no_overdraft(&a.name) {
                assert!(
                    a.debits_posted <= a.credits_posted,
                    "overdraft on {}: debits {} > credits {}",
                    a.name,
                    a.debits_posted,
                    a.credits_posted
                );
            }
        }
        assert_eq!(dp, cp, "pending not conserved");
        assert_eq!(dpo, cpo, "posted not conserved");
    }

    #[tokio::test]
    async fn hold_then_capture_splits_fee_and_conserves() {
        let c = sim(1_000); // 10% fee
        c.create_accounts(TENANT).await.unwrap();
        let hold = c
            .hold_deposit(TENANT, Uuid::new_v4(), 10_000)
            .await
            .unwrap();
        assert_eq!(hold.state, TransferState::Pending);
        assert_eq!(hold.code, CODE_DEPOSIT_HOLD);
        assert_invariants(&c).await;

        let res = c
            .capture(TENANT, Uuid::from_u128(hold.id), Uuid::new_v4(), None)
            .await
            .unwrap();
        assert_eq!(res.post.amount, 10_000);
        assert_eq!(res.post.flag, TransferFlag::PostPending);
        assert_eq!(res.revenue.amount, 9_000);
        assert_eq!(res.platform_fee.unwrap().amount, 1_000);
        assert_invariants(&c).await;

        let bal = c.balance(TENANT).await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account == revenue_account(TENANT))
            .unwrap();
        assert_eq!(revenue.posted_net, 9_000);
        assert_eq!(revenue.pending_net, 0);
    }

    #[tokio::test]
    async fn partial_capture_releases_remainder() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 5_000).await.unwrap();
        let res = c
            .capture(TENANT, Uuid::from_u128(hold.id), Uuid::new_v4(), Some(3_000))
            .await
            .unwrap();
        assert_eq!(res.post.amount, 3_000);
        assert_invariants(&c).await;
        let st = c.snapshot().await;
        let deposits = st.accounts.get(&deposits_account(TENANT)).unwrap();
        assert_eq!(deposits.credits_pending, 0);
        assert_eq!(deposits.credits_posted, 3_000);
    }

    #[tokio::test]
    async fn hold_is_idempotent_by_transfer_id() {
        let c = sim(0);
        let id = Uuid::new_v4();
        let t1 = c.hold_deposit(TENANT, id, 1_000).await.unwrap();
        let t2 = c.hold_deposit(TENANT, id, 1_000).await.unwrap();
        assert_eq!(t1.id, t2.id);
        let st = c.snapshot().await;
        let deposits = st.accounts.get(&deposits_account(TENANT)).unwrap();
        assert_eq!(deposits.credits_pending, 1_000, "no double posting");
    }

    #[tokio::test]
    async fn hold_id_conflict_errors() {
        let c = sim(0);
        let id = Uuid::new_v4();
        c.hold_deposit(TENANT, id, 1_000).await.unwrap();
        let err = c.hold_deposit(TENANT, id, 2_000).await.unwrap_err();
        assert!(matches!(
            err,
            LedgerError::ExistsWithDifferentParameters(_)
        ));
    }

    #[tokio::test]
    async fn capture_replay_is_idempotent() {
        let c = sim(500); // 5%
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 2_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        let r1 = c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
        // Second capture with a different transfer id replays by hold_id.
        let r2 = c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
        assert_eq!(r1.post.id, r2.post.id);
        assert_eq!(r1.revenue.id, r2.revenue.id);
        assert_invariants(&c).await;
        let bal = c.balance(TENANT).await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account == revenue_account(TENANT))
            .unwrap();
        assert_eq!(revenue.posted_net, 1_900, "no double capture");
    }

    #[tokio::test]
    async fn void_refund_releases_pending_hold() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 4_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        let void = c
            .refund(TENANT, Uuid::new_v4(), Some(hold_id), 0)
            .await
            .unwrap();
        assert_eq!(void.flag, TransferFlag::VoidPending);
        assert_eq!(void.amount, 4_000);
        assert_invariants(&c).await;
        // Voiding again is rejected as already resolved.
        let err = c
            .refund(TENANT, Uuid::new_v4(), Some(hold_id), 0)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::AlreadyResolved(_)));
        // Capturing a voided hold fails.
        let err = c
            .capture(TENANT, hold_id, Uuid::new_v4(), None)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::AlreadyResolved(_)));
    }

    #[tokio::test]
    async fn posted_refund_moves_money_back_and_enforces_funds() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 6_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
        let r = c
            .refund(TENANT, Uuid::new_v4(), Some(hold_id), 4_000)
            .await
            .unwrap();
        assert_eq!(r.code, CODE_REFUND);
        assert_eq!(r.debit_account, revenue_account(TENANT));
        assert_eq!(r.credit_account, PLATFORM_CLEARING_ACCOUNT);
        assert_invariants(&c).await;
        // Refunding more than earned revenue is rejected (no overdraft).
        let err = c
            .refund(TENANT, Uuid::new_v4(), Some(hold_id), 3_000)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::ExceedsCredits(_)));
        assert_invariants(&c).await;
    }

    #[tokio::test]
    async fn no_show_fee_charges_partial_and_releases_rest() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 10_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        let res = c
            .no_show_fee(TENANT, hold_id, Uuid::new_v4(), 2_500)
            .await
            .unwrap();
        assert_eq!(res.post.code, CODE_NO_SHOW_FEE);
        assert_eq!(res.post.amount, 2_500);
        assert_eq!(res.revenue.amount, 2_500);
        assert_invariants(&c).await;
        // Remainder of the hold was released.
        let st = c.snapshot().await;
        let deposits = st.accounts.get(&deposits_account(TENANT)).unwrap();
        assert_eq!(deposits.credits_pending, 0);
        // Replay by hold_id is idempotent.
        let res2 = c
            .no_show_fee(TENANT, hold_id, Uuid::new_v4(), 2_500)
            .await
            .unwrap();
        assert_eq!(res.post.id, res2.post.id);
    }

    #[tokio::test]
    async fn payout_moves_revenue_to_clearing_and_enforces_funds() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 8_000).await.unwrap();
        c.capture(TENANT, Uuid::from_u128(hold.id), Uuid::new_v4(), None)
            .await
            .unwrap();
        let p = c.payout(TENANT, Uuid::new_v4(), 5_000).await.unwrap();
        assert_eq!(p.code, CODE_PAYOUT);
        assert_eq!(p.debit_account, revenue_account(TENANT));
        assert_eq!(p.credit_account, PLATFORM_PAYOUTS_ACCOUNT);
        assert_invariants(&c).await;
        let err = c.payout(TENANT, Uuid::new_v4(), 9_999).await.unwrap_err();
        assert!(matches!(err, LedgerError::ExceedsCredits(_)));
        // Idempotent replay.
        let id = Uuid::new_v4();
        let p1 = c.payout(TENANT, id, 1_000).await.unwrap();
        let p2 = c.payout(TENANT, id, 1_000).await.unwrap();
        assert_eq!(p1.id, p2.id);
        assert_invariants(&c).await;
        let bal = c.balance(TENANT).await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account == revenue_account(TENANT))
            .unwrap();
        assert_eq!(revenue.posted_net, 8_000 - 5_000 - 1_000);
    }

    #[tokio::test]
    async fn conservation_across_mixed_workflow() {
        let c = sim(250); // 2.5%
        c.create_accounts(TENANT).await.unwrap();
        for i in 0..10u64 {
            let amount = 1_000 + i * 100;
            let hold = c.hold_deposit(TENANT, Uuid::new_v4(), amount).await.unwrap();
            let hold_id = Uuid::from_u128(hold.id);
            match i % 4 {
                0 => {
                    c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
                }
                1 => {
                    c.capture(TENANT, hold_id, Uuid::new_v4(), Some(amount / 2))
                        .await
                        .unwrap();
                }
                2 => {
                    c.refund(TENANT, Uuid::new_v4(), Some(hold_id), 0).await.unwrap();
                }
                _ => {
                    c.no_show_fee(TENANT, hold_id, Uuid::new_v4(), amount / 4)
                        .await
                        .unwrap();
                }
            }
            assert_invariants(&c).await;
        }
    }
}
