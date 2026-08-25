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
        // SPEC-W43 P-01 (C3): pending debits RESERVE funds — on no-overdraft
        // accounts (deposits/revenue) debits_pending + debits_posted must
        // never exceed credits_posted, matching TigerBeetle's
        // debits_must_not_exceed_credits (which covers pending debits).
        for name in [&t.debit_account, &t.credit_account] {
            if no_overdraft(name) {
                let a = self.accounts.get(name).expect("account exists");
                if a.debits_pending + a.debits_posted > a.credits_posted {
                    // Roll back to keep the ledger consistent.
                    self.account_mut(&t.debit_account)?.debits_pending -= t.amount;
                    self.account_mut(&t.credit_account)?.credits_pending -= t.amount;
                    return Err(LedgerError::ExceedsCredits(name.to_string()));
                }
            }
        }
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
        // P-06: cross-tenant guard — the hold's credit account pins the
        // owning tenant; a capture/no-show-fee under another tenant is 403.
        if hold.credit_account != deposits_account(tenant_id) {
            return Err(LedgerError::TenantMismatch(format!(
                "hold {hold_id} does not belong to tenant {tenant_id}"
            )));
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
        // P-05: checked fee math BEFORE any mutation (overflow / out-of-range
        // fee_bps => InvalidAmount, never a wrap).
        let (net, fee) = fee_split(post_amount, self.fee_bps)?;
        // A 100% fee split (fee == post) would make the revenue leg a
        // zero-amount transfer, which the double-entry invariant (and
        // TigerBeetle's wire protocol, amount_must_not_be_zero) forbids.
        // Reject BEFORE any mutation, like the other checks above.
        if net == 0 {
            return Err(LedgerError::InvalidAmount);
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

        // P-06 atomicity: pre-validate EVERY leg against the post-resolution
        // balances before the first mutation, so a rejected capture leaves
        // zero mutations. Account existence is guaranteed by the
        // ensure_account calls above; the only remaining failure mode of
        // apply_posted is the no-overdraft rule on the deposits account.
        {
            let dep = st.accounts.get(&deposits).expect("ensured above");
            let debits_after = dep.debits_posted + net + fee;
            let credits_after = dep.credits_posted + post_amount;
            if debits_after > credits_after {
                return Err(LedgerError::ExceedsCredits(deposits.clone()));
            }
        }

        // Mutation phase: no validation failures remain below.
        st.resolve_pending(&hold, post_amount, false)?;
        st.transfers
            .get_mut(&hold.id)
            .expect("hold exists")
            .state = TransferState::Posted;
        let t1 = st.insert_transfer(t1);

        // t2: deposits -> revenue (net of platform fee).
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

        // Replay by transfer id first, WITH kind matching (P-11 regression):
        // the stored record must be a refund-shaped transfer consistent with
        // this request; anything else is a parameter conflict, never a
        // silent replay of a different operation.
        if let Some(existing) = st.transfers.get(&transfer_id.as_u128()) {
            let matches = existing.code == CODE_REFUND
                && match existing.flag {
                    TransferFlag::VoidPending => {
                        // Void leg: keyed by the hold it resolved; the amount
                        // argument is ignored on the void path, exactly as on
                        // the original call.
                        hold_id.map(|h| h.as_u128()) == existing.pending_id
                    }
                    TransferFlag::None => {
                        existing.amount == amount
                            && existing.debit_account == revenue_account(tenant_id)
                            && existing.credit_account == PLATFORM_CLEARING_ACCOUNT
                    }
                    TransferFlag::PostPending => false,
                };
            if matches {
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
            // P-06: cross-tenant guard — the hold's credit account pins the
            // owning tenant; a void/refund under another tenant is 403.
            if hold.credit_account != deposits_account(tenant_id) {
                return Err(LedgerError::TenantMismatch(format!(
                    "hold {h} does not belong to tenant {tenant_id}"
                )));
            }
            match hold.state {
                TransferState::Pending => {
                    // P-11: a partial amount against a pending hold is
                    // rejected; only 0 (full void) or exactly the hold amount
                    // may void it — never a silent full void of a partial
                    // request.
                    if amount != 0 && amount != hold.amount {
                        return Err(LedgerError::AmountMismatch(format!(
                            "refund amount {amount} != pending hold amount {}",
                            hold.amount
                        )));
                    }
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

    async fn payout_hold(
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
            TransferState::Pending,
            TransferFlag::None,
            None,
        );
        if let Some(existing) = st.check_replay(&t)? {
            return Ok(existing);
        }
        // apply_pending reserves the funds (pending debits count against the
        // no-overdraft rule), so an over-limit payout is rejected BEFORE any
        // rail side effect (C3).
        st.apply_pending(&t)?;
        Ok(st.insert_transfer(t))
    }

    async fn payout_post(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
    ) -> Result<Transfer, LedgerError> {
        let mut st = self.state.lock().await;
        let hold = st
            .transfers
            .get(&hold_id.as_u128())
            .cloned()
            .ok_or_else(|| LedgerError::TransferNotFound(format!("{}", hold_id)))?;
        if hold.code != CODE_PAYOUT {
            return Err(LedgerError::NotPending(format!("{}", hold_id)));
        }
        // P-06: tenant consistency — the payout hold debits the tenant's
        // revenue account.
        if hold.debit_account != revenue_account(tenant_id) {
            return Err(LedgerError::TenantMismatch(format!(
                "payout hold {hold_id} does not belong to tenant {tenant_id}"
            )));
        }
        match hold.state {
            TransferState::Voided => {
                return Err(LedgerError::AlreadyResolved(format!("{}", hold_id)))
            }
            TransferState::Posted => {
                // Idempotent replay: return the stored posting leg.
                if let Some(post) = st
                    .transfers
                    .values()
                    .find(|t| {
                        t.pending_id == Some(hold.id)
                            && t.flag == TransferFlag::PostPending
                            && t.code == CODE_PAYOUT
                    })
                    .cloned()
                {
                    return Ok(post);
                }
                return Err(LedgerError::Backend("payout posting leg missing".into()));
            }
            TransferState::Pending => {}
        }
        let t = self.new_transfer(
            transfer_id,
            hold.debit_account.clone(),
            hold.credit_account.clone(),
            hold.amount,
            CODE_PAYOUT,
            TransferState::Posted,
            TransferFlag::PostPending,
            Some(hold.id),
        );
        if let Some(existing) = st.check_replay(&t)? {
            return Ok(existing);
        }
        st.resolve_pending(&hold, hold.amount, false)?;
        st.transfers
            .get_mut(&hold.id)
            .expect("hold exists")
            .state = TransferState::Posted;
        Ok(st.insert_transfer(t))
    }

    async fn payout_void(
        &self,
        tenant_id: &str,
        hold_id: Uuid,
        transfer_id: Uuid,
    ) -> Result<Transfer, LedgerError> {
        let mut st = self.state.lock().await;
        let hold = st
            .transfers
            .get(&hold_id.as_u128())
            .cloned()
            .ok_or_else(|| LedgerError::TransferNotFound(format!("{}", hold_id)))?;
        if hold.code != CODE_PAYOUT {
            return Err(LedgerError::NotPending(format!("{}", hold_id)));
        }
        if hold.debit_account != revenue_account(tenant_id) {
            return Err(LedgerError::TenantMismatch(format!(
                "payout hold {hold_id} does not belong to tenant {tenant_id}"
            )));
        }
        match hold.state {
            TransferState::Voided => {
                // Idempotent replay: return the stored void leg.
                if let Some(void) = st
                    .transfers
                    .values()
                    .find(|t| {
                        t.pending_id == Some(hold.id)
                            && t.flag == TransferFlag::VoidPending
                            && t.code == CODE_PAYOUT
                    })
                    .cloned()
                {
                    return Ok(void);
                }
                return Err(LedgerError::AlreadyResolved(format!("{}", hold_id)));
            }
            TransferState::Posted => {
                return Err(LedgerError::AlreadyResolved(format!("{}", hold_id)))
            }
            TransferState::Pending => {}
        }
        let t = self.new_transfer(
            transfer_id,
            hold.debit_account.clone(),
            hold.credit_account.clone(),
            hold.amount,
            CODE_PAYOUT,
            TransferState::Posted,
            TransferFlag::VoidPending,
            Some(hold.id),
        );
        if let Some(existing) = st.check_replay(&t)? {
            return Ok(existing);
        }
        st.resolve_pending(&hold, 0, true)?;
        st.transfers
            .get_mut(&hold.id)
            .expect("hold exists")
            .state = TransferState::Voided;
        Ok(st.insert_transfer(t))
    }

    async fn get_transfer(&self, transfer_id: Uuid) -> Result<Transfer, LedgerError> {
        let st = self.state.lock().await;
        st.transfers
            .get(&transfer_id.as_u128())
            .cloned()
            .ok_or_else(|| LedgerError::TransferNotFound(format!("{}", transfer_id)))
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

    // ------------------------------------------------------------------
    // SPEC-W43 regression tests
    // ------------------------------------------------------------------

    /// Comparable snapshot of funds-relevant state (P-06 zero-mutation
    /// assertions): sorted (account, counters) + transfer count.
    async fn funds_snapshot(client: &SimLedgerClient) -> (Vec<(String, u64, u64, u64, u64)>, usize) {
        let st = client.snapshot().await;
        let mut accts: Vec<_> = st
            .accounts
            .values()
            .map(|a| {
                (
                    a.name.clone(),
                    a.debits_pending,
                    a.credits_pending,
                    a.debits_posted,
                    a.credits_posted,
                )
            })
            .collect();
        accts.sort();
        (accts, st.transfers.len())
    }

    /// C5 (FIN-H2 parity anchor): replaying a posted refund with the SAME
    /// transfer id and SAME parameters returns the stored refund verbatim —
    /// same id, no double refund. This is the sim behavior the live
    /// TigerBeetle client's replay short-circuit must match.
    #[tokio::test]
    async fn refund_replay_same_key_returns_stored_refund() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 6_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
        let rid = Uuid::new_v4();
        let r1 = c.refund(TENANT, rid, Some(hold_id), 4_000).await.unwrap();
        let pre = funds_snapshot(&c).await;
        let r2 = c.refund(TENANT, rid, Some(hold_id), 4_000).await.unwrap();
        assert_eq!(r1.id, r2.id, "replay returns the same refund id");
        assert_eq!(r2.code, CODE_REFUND);
        assert_eq!(r2.amount, 4_000);
        assert_eq!(r2.debit_account, revenue_account(TENANT));
        assert_eq!(r2.credit_account, PLATFORM_CLEARING_ACCOUNT);
        assert_eq!(funds_snapshot(&c).await, pre, "replay moved nothing");
        assert_invariants(&c).await;
    }

    /// P-11: replaying a posted refund id with a DIFFERENT amount is a
    /// parameter conflict (409), not a silent replay.
    #[tokio::test]
    async fn refund_replay_with_different_amount_conflicts() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 6_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
        let rid = Uuid::new_v4();
        c.refund(TENANT, rid, Some(hold_id), 4_000).await.unwrap();
        let err = c
            .refund(TENANT, rid, Some(hold_id), 4_500)
            .await
            .unwrap_err();
        assert!(matches!(
            err,
            LedgerError::ExistsWithDifferentParameters(_)
        ));
        assert_invariants(&c).await;
    }

    /// P-11 (kind matching): a transfer id already used by a non-refund
    /// operation must conflict when presented as a refund id.
    #[tokio::test]
    async fn refund_transfer_id_reused_by_other_kind_conflicts() {
        let c = sim(0);
        let id = Uuid::new_v4();
        c.hold_deposit(TENANT, id, 1_000).await.unwrap();
        let err = c.refund(TENANT, id, None, 1_000).await.unwrap_err();
        assert!(matches!(
            err,
            LedgerError::ExistsWithDifferentParameters(_)
        ));
        assert_invariants(&c).await;
    }

    /// P-11: refund(amount < hold.amount) on a still-pending hold => rejected
    /// (400 via AmountMismatch), NO silent full void, zero mutations.
    #[tokio::test]
    async fn partial_refund_of_pending_hold_is_rejected() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 4_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        let pre = funds_snapshot(&c).await;
        let err = c
            .refund(TENANT, Uuid::new_v4(), Some(hold_id), 1_000)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::AmountMismatch(_)));
        // The hold is still pending (not silently voided) and nothing moved.
        let h = c.get_transfer(hold_id).await.unwrap();
        assert_eq!(h.state, TransferState::Pending);
        assert_eq!(funds_snapshot(&c).await, pre, "zero mutations on 400");
        assert_invariants(&c).await;
    }

    /// P-11: refund(amount == hold.amount) on a pending hold voids it in full.
    #[tokio::test]
    async fn full_amount_refund_of_pending_hold_voids() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 4_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        let t = c
            .refund(TENANT, Uuid::new_v4(), Some(hold_id), 4_000)
            .await
            .unwrap();
        assert_eq!(t.flag, TransferFlag::VoidPending);
        assert_eq!(t.amount, 4_000);
        let h = c.get_transfer(hold_id).await.unwrap();
        assert_eq!(h.state, TransferState::Voided);
        assert_invariants(&c).await;
    }

    /// P-06: cross-tenant capture/void/refund are rejected (TenantMismatch =>
    /// HTTP 403 at the route layer) and leave ZERO mutations behind.
    #[tokio::test]
    async fn cross_tenant_ops_are_rejected_without_mutation() {
        let c = sim(250);
        let other = "t-222";
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 5_000).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        let pre = funds_snapshot(&c).await;

        // Cross-tenant capture.
        let err = c
            .capture(other, hold_id, Uuid::new_v4(), None)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::TenantMismatch(_)), "{err}");
        // Cross-tenant void (refund of the pending hold).
        let err = c
            .refund(other, Uuid::new_v4(), Some(hold_id), 0)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::TenantMismatch(_)), "{err}");
        // Cross-tenant no-show fee.
        let err = c
            .no_show_fee(other, hold_id, Uuid::new_v4(), 1_000)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::TenantMismatch(_)), "{err}");
        assert_eq!(funds_snapshot(&c).await, pre, "zero mutations so far");

        // Same-tenant capture succeeds; then a cross-tenant POSTED refund of
        // that hold is still rejected.
        c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
        let pre2 = funds_snapshot(&c).await;
        let err = c
            .refund(other, Uuid::new_v4(), Some(hold_id), 1_000)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::TenantMismatch(_)), "{err}");
        assert_eq!(funds_snapshot(&c).await, pre2, "zero mutations on posted path");
        assert_invariants(&c).await;
    }

    /// C4: capture(amount=None) resolves the pending hold's amount (sim side).
    #[tokio::test]
    async fn capture_amount_none_resolves_pending_amount() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 7_500).await.unwrap();
        let hold_id = Uuid::from_u128(hold.id);
        let res = c.capture(TENANT, hold_id, Uuid::new_v4(), None).await.unwrap();
        assert_eq!(res.post.amount, 7_500, "None resolves to the hold amount");
        assert_eq!(res.revenue.amount, 7_500);
        assert_invariants(&c).await;
    }

    /// P-05: fee_split checked math — bounds, exactness, overflow safety.
    #[test]
    fn fee_split_bounds_and_overflow() {
        // fee never exceeds the amount; net + fee == amount always.
        for (amount, bps) in [(10_000u64, 1_000u64), (1, 10_000), (0, 250), (u64::MAX / 10_000, 10_000)] {
            let (net, fee) = fee_split(amount, bps).unwrap();
            assert!(fee <= amount);
            assert_eq!(net + fee, amount);
        }
        assert_eq!(fee_split(10_000, 1_000).unwrap(), (9_000, 1_000));
        assert_eq!(fee_split(10_000, 10_000).unwrap(), (0, 10_000));
        // Out-of-range fee_bps is rejected (defense in depth; config validates
        // at boot too).
        assert!(matches!(
            fee_split(1_000, 10_001),
            Err(LedgerError::InvalidAmount)
        ));
        // Overflowing multiplication (huge amount) is an error, not a wrap.
        // u64::MAX * 10_000 overflows; fee_split must not panic.
        let r = fee_split(u64::MAX, 10_000);
        assert!(matches!(r, Err(LedgerError::InvalidAmount)));
    }

    /// P-01 (C3): two-phase payout — hold reserves, post settles after the
    /// rail commits, void releases on rail failure/unknown.
    #[tokio::test]
    async fn payout_two_phase_happy_path_and_overdraft() {
        let c = sim(0);
        let hold = c.hold_deposit(TENANT, Uuid::new_v4(), 8_000).await.unwrap();
        c.capture(TENANT, Uuid::from_u128(hold.id), Uuid::new_v4(), None)
            .await
            .unwrap();

        // Over-limit payout hold is rejected BEFORE any rail side effect.
        let err = c
            .payout_hold(TENANT, Uuid::new_v4(), 9_999)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::ExceedsCredits(_)), "{err}");
        assert_invariants(&c).await;

        // Ledger-first: pending hold reserves the funds.
        let pid = Uuid::new_v4();
        let ph = c.payout_hold(TENANT, pid, 5_000).await.unwrap();
        assert_eq!(ph.state, TransferState::Pending);
        assert_eq!(ph.code, CODE_PAYOUT);
        // A second payout hold for the remaining revenue beyond the reserved
        // amount is now rejected (funds are reserved).
        let err = c
            .payout_hold(TENANT, Uuid::new_v4(), 3_001)
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::ExceedsCredits(_)), "{err}");

        // Rail COMMITTED -> post in full (deterministic post id).
        let post_id = Uuid::new_v5(&Uuid::NAMESPACE_URL, b"payout-post:test");
        let posted = c
            .payout_post(TENANT, pid, post_id)
            .await
            .unwrap();
        assert_eq!(posted.flag, TransferFlag::PostPending);
        assert_eq!(posted.amount, 5_000);
        // Post replay is idempotent.
        let replay = c.payout_post(TENANT, pid, post_id).await.unwrap();
        assert_eq!(replay.id, posted.id);
        assert_invariants(&c).await;

        // Second payout: rail failure -> void releases the reservation.
        let pid2 = Uuid::new_v4();
        c.payout_hold(TENANT, pid2, 3_000).await.unwrap();
        let err = c.payout_hold(TENANT, Uuid::new_v4(), 1).await.unwrap_err();
        assert!(matches!(err, LedgerError::ExceedsCredits(_)), "{err}");
        let void_id = Uuid::new_v5(&Uuid::NAMESPACE_URL, b"payout-void:test");
        let v = c.payout_void(TENANT, pid2, void_id).await.unwrap();
        assert_eq!(v.flag, TransferFlag::VoidPending);
        // Funds released: the remaining 3_000 is payable again.
        let pid3 = Uuid::new_v4();
        c.payout_hold(TENANT, pid3, 3_000).await.unwrap();
        assert_invariants(&c).await;
        // Cross-tenant payout post is rejected.
        let err = c
            .payout_post("t-222", pid3, Uuid::new_v4())
            .await
            .unwrap_err();
        assert!(matches!(err, LedgerError::TenantMismatch(_)), "{err}");
        assert_invariants(&c).await;
    }
}
