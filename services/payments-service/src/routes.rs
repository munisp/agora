//! REST API (SPEC §9) + Temporal activity HTTP handlers (SPEC §6).

use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::ledger::{
    transfer_id_from_key, CaptureResult, LedgerError, TenantBalance, Transfer,
};
use crate::mojaloop::{PartyIdInfo, PayoutInstruction};
use crate::AppState;

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------
#[derive(Debug)]
pub struct ApiError {
    status: StatusCode,
    message: String,
}

impl ApiError {
    fn bad_request(msg: impl Into<String>) -> Self {
        Self {
            status: StatusCode::BAD_REQUEST,
            message: msg.into(),
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (
            self.status,
            Json(serde_json::json!({ "error": self.message })),
        )
            .into_response()
    }
}

impl From<LedgerError> for ApiError {
    fn from(e: LedgerError) -> Self {
        let status = match &e {
            LedgerError::AccountNotFound(_) | LedgerError::TransferNotFound(_) => {
                StatusCode::NOT_FOUND
            }
            LedgerError::ExistsWithDifferentParameters(_)
            | LedgerError::NotPending(_)
            | LedgerError::AlreadyResolved(_) => StatusCode::CONFLICT,
            LedgerError::ExceedsPendingAmount
            | LedgerError::InvalidAmount
            | LedgerError::ExceedsCredits(_) => StatusCode::UNPROCESSABLE_ENTITY,
            LedgerError::Backend(_) => StatusCode::BAD_GATEWAY,
        };
        Self {
            status,
            message: e.to_string(),
        }
    }
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------
#[derive(Debug, Deserialize)]
pub struct HoldDepositBody {
    pub tenant_id: String,
    pub booking_id: Option<String>,
    pub amount_cents: u64,
    pub currency: Option<String>,
    pub idempotency_key: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct DepositResponse {
    pub deposit_id: String,
    pub state: crate::ledger::TransferState,
    pub amount_cents: u64,
    pub transfer: Transfer,
}

#[derive(Debug, Deserialize)]
pub struct CaptureBody {
    pub tenant_id: String,
    pub amount_cents: Option<u64>,
}

#[derive(Debug, Serialize)]
pub struct CaptureResponse {
    pub deposit_id: String,
    pub result: CaptureResult,
}

#[derive(Debug, Deserialize)]
pub struct RefundBody {
    pub tenant_id: String,
    pub deposit_id: Option<Uuid>,
    #[serde(default)]
    pub amount_cents: u64,
    pub reason: Option<String>,
    pub idempotency_key: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct NoShowFeeBody {
    pub tenant_id: String,
    pub deposit_id: Uuid,
    pub amount_cents: u64,
    pub booking_id: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct PayoutBody {
    pub tenant_id: String,
    pub amount_cents: u64,
    pub currency: String,
    pub payee: PartyIdInfo,
    pub idempotency_key: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct PayoutResponse {
    pub payout_id: String,
    pub ledger_transfer: Transfer,
    pub mojaloop: crate::mojaloop::PayoutOutcome,
}

// Temporal activity bodies (SPEC §6: BookingSagaWorkflow HoldDeposit/VoidHold).
#[derive(Debug, Deserialize)]
pub struct HoldDepositActivityBody {
    pub tenant_id: String,
    pub booking_id: String,
    pub amount_cents: u64,
    pub currency: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct VoidHoldActivityBody {
    pub tenant_id: String,
    pub deposit_id: Option<Uuid>,
    pub booking_id: Option<String>,
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------
pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/v1/deposits", post(hold_deposit))
        .route("/v1/deposits/:id/capture", post(capture_deposit))
        .route("/v1/refunds", post(refund))
        .route("/v1/no-show-fee", post(no_show_fee))
        .route("/v1/accounts/:tenant_id/balance", get(balance))
        .route("/v1/payouts", post(payout))
        .route("/activities/hold-deposit", post(activity_hold_deposit))
        .route("/activities/void-hold", post(activity_void_hold))
        .with_state(state)
}

async fn healthz(State(st): State<AppState>) -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "status": "ok",
        "service": "payments-service",
        "ledger_impl": st.config.ledger_impl,
        "commands_dead_lettered": st
            .commands_dead_lettered
            .load(std::sync::atomic::Ordering::Relaxed),
    }))
}

async fn hold_deposit(
    State(st): State<AppState>,
    Json(body): Json<HoldDepositBody>,
) -> Result<(StatusCode, Json<DepositResponse>), ApiError> {
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    let key = body.idempotency_key.clone().or_else(|| {
        body.booking_id
            .as_ref()
            .map(|b| format!("hold:{b}"))
    });
    let transfer_id = transfer_id_from_key(key.as_deref());
    let t = st
        .ledger
        .hold_deposit(&body.tenant_id, transfer_id, body.amount_cents)
        .await?;
    st.publish_event(
        "DepositHeld",
        body.booking_id.as_deref().unwrap_or(&body.tenant_id),
        &body.tenant_id,
        serde_json::json!({
            "depositId": t.id_string(),
            "bookingId": body.booking_id,
            "amountCents": body.amount_cents,
            "currency": body.currency,
            "ledgerRef": t.id_string(),
        }),
    )
    .await;
    Ok((
        StatusCode::CREATED,
        Json(DepositResponse {
            deposit_id: t.id_string(),
            state: t.state,
            amount_cents: t.amount,
            transfer: t,
        }),
    ))
}

async fn capture_deposit(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    Json(body): Json<CaptureBody>,
) -> Result<Json<CaptureResponse>, ApiError> {
    // Deterministic capture transfer id => idempotent retries.
    let capture_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("capture:{}", id).as_bytes(),
    );
    let result = st
        .ledger
        .capture(&body.tenant_id, id, capture_id, body.amount_cents)
        .await?;
    st.publish_event(
        "DepositCaptured",
        &id.to_string(),
        &body.tenant_id,
        serde_json::json!({
            "depositId": id.to_string(),
            "postedAmountCents": result.post.amount,
            "revenueCents": result.revenue.amount,
            "platformFeeCents": result.platform_fee.as_ref().map(|t| t.amount),
            "ledgerRef": result.post.id_string(),
        }),
    )
    .await;
    Ok(Json(CaptureResponse {
        deposit_id: id.to_string(),
        result,
    }))
}

async fn refund(
    State(st): State<AppState>,
    Json(body): Json<RefundBody>,
) -> Result<(StatusCode, Json<Transfer>), ApiError> {
    let key = body.idempotency_key.clone().or_else(|| {
        body.deposit_id
            .as_ref()
            .map(|d| format!("refund:{d}:{}", body.amount_cents))
    });
    let transfer_id = transfer_id_from_key(key.as_deref());
    let t = st
        .ledger
        .refund(
            &body.tenant_id,
            transfer_id,
            body.deposit_id,
            body.amount_cents,
        )
        .await?;
    st.publish_event(
        "RefundPosted",
        &t.id_string(),
        &body.tenant_id,
        serde_json::json!({
            "refundId": t.id_string(),
            "depositId": body.deposit_id,
            "amountCents": t.amount,
            "reason": body.reason,
            "ledgerRef": t.id_string(),
        }),
    )
    .await;
    Ok((StatusCode::CREATED, Json(t)))
}

async fn no_show_fee(
    State(st): State<AppState>,
    Json(body): Json<NoShowFeeBody>,
) -> Result<(StatusCode, Json<CaptureResult>), ApiError> {
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    let fee_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("no-show-fee:{}", body.deposit_id).as_bytes(),
    );
    let result = st
        .ledger
        .no_show_fee(&body.tenant_id, body.deposit_id, fee_id, body.amount_cents)
        .await?;
    st.publish_event(
        "NoShowFeePosted",
        body.booking_id.as_deref().unwrap_or(&body.tenant_id),
        &body.tenant_id,
        serde_json::json!({
            "depositId": body.deposit_id.to_string(),
            "feeCents": body.amount_cents,
            "ledgerRef": result.post.id_string(),
        }),
    )
    .await;
    Ok((StatusCode::CREATED, Json(result)))
}

async fn balance(
    State(st): State<AppState>,
    Path(tenant_id): Path<String>,
) -> Result<Json<TenantBalance>, ApiError> {
    let bal = st.ledger.balance(&tenant_id).await?;
    Ok(Json(bal))
}

async fn payout(
    State(st): State<AppState>,
    Json(body): Json<PayoutBody>,
) -> Result<(StatusCode, Json<PayoutResponse>), ApiError> {
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    // Deterministic id => retries of the same payout are idempotent end-to-end.
    let payout_id = transfer_id_from_key(body.idempotency_key.as_deref());

    // 1. Mojaloop rail first (quote -> transfer). If the rail rejects the
    //    payout, no ledger movement happens.
    let instruction = PayoutInstruction {
        transfer_id: payout_id,
        amount_cents: body.amount_cents,
        currency: body.currency.clone(),
        payee: body.payee.clone(),
        payer: PartyIdInfo {
            party_id_type: "ALIAS".to_string(),
            party_identifier: format!("tenant:{}", body.tenant_id),
        },
    };
    let outcome = st
        .mojaloop
        .execute_payout(&instruction)
        .await
        .map_err(|e| ApiError {
            status: StatusCode::BAD_GATEWAY,
            message: format!("mojaloop payout failed: {e}"),
        })?;

    // 2. Ledger payout transfer (code 104): revenue -> platform:payouts.
    let t = st
        .ledger
        .payout(&body.tenant_id, payout_id, body.amount_cents)
        .await
        .map_err(|e| {
            // The rail already committed; this requires operator reconciliation.
            tracing::error!(
                error = %e,
                payout_id = %payout_id,
                "CRITICAL: mojaloop transfer committed but ledger payout failed"
            );
            ApiError::from(e)
        })?;

    st.publish_event(
        "PayoutPosted",
        &payout_id.to_string(),
        &body.tenant_id,
        serde_json::json!({
            "payoutId": payout_id.to_string(),
            "amountCents": body.amount_cents,
            "currency": body.currency,
            "payee": body.payee,
            "mojaloopTransferId": outcome.transfer_id,
            "mojaloopState": outcome.state,
            "ledgerRef": t.id_string(),
        }),
    )
    .await;

    Ok((
        StatusCode::CREATED,
        Json(PayoutResponse {
            payout_id: payout_id.to_string(),
            ledger_transfer: t,
            mojaloop: outcome,
        }),
    ))
}

// ---------------------------------------------------------------------------
// Temporal activity handlers (BookingSagaWorkflow: HoldDeposit / VoidHold)
// ---------------------------------------------------------------------------
async fn activity_hold_deposit(
    State(st): State<AppState>,
    Json(body): Json<HoldDepositActivityBody>,
) -> Result<(StatusCode, Json<DepositResponse>), ApiError> {
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    // Deterministic per booking => saga retries are idempotent.
    let transfer_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("saga-hold:{}", body.booking_id).as_bytes(),
    );
    let t = st
        .ledger
        .hold_deposit(&body.tenant_id, transfer_id, body.amount_cents)
        .await?;
    st.publish_event(
        "DepositHeld",
        &body.booking_id,
        &body.tenant_id,
        serde_json::json!({
            "depositId": t.id_string(),
            "bookingId": body.booking_id,
            "amountCents": body.amount_cents,
            "currency": body.currency,
            "ledgerRef": t.id_string(),
            "via": "temporal-activity",
        }),
    )
    .await;
    Ok((
        StatusCode::CREATED,
        Json(DepositResponse {
            deposit_id: t.id_string(),
            state: t.state,
            amount_cents: t.amount,
            transfer: t,
        }),
    ))
}

async fn activity_void_hold(
    State(st): State<AppState>,
    Json(body): Json<VoidHoldActivityBody>,
) -> Result<Json<Transfer>, ApiError> {
    let deposit_id = match (body.deposit_id, &body.booking_id) {
        (Some(d), _) => d,
        (None, Some(b)) => Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("saga-hold:{b}").as_bytes(),
        ),
        (None, None) => {
            return Err(ApiError::bad_request(
                "either deposit_id or booking_id is required",
            ))
        }
    };
    let transfer_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("saga-void:{deposit_id}").as_bytes(),
    );
    let t = st
        .ledger
        .refund(&body.tenant_id, transfer_id, Some(deposit_id), 0)
        .await?;
    st.publish_event(
        "HoldVoided",
        &deposit_id.to_string(),
        &body.tenant_id,
        serde_json::json!({
            "depositId": deposit_id.to_string(),
            "ledgerRef": t.id_string(),
            "via": "temporal-activity",
        }),
    )
    .await;
    Ok(Json(t))
}
