//! REST API (SPEC §9) + Temporal activity HTTP handlers (SPEC §6).
//!
//! SPEC-W43 hardening:
//! - P-09/C1: every tenant-scoped route authorizes via `AuthConfig`
//!   (internal token OR gateway `X-Tenant-Slugs` binding; dev escape);
//!   `/activities/*` and `/v1/internal/*` require the internal token.
//! - P-12/C5: money-moving endpoints REQUIRE a non-empty `idempotency_key`
//!   (400 when absent). Capture is exempt: its transfer id is derived from
//!   the deposit id, so it is idempotent by construction.
//! - P-13: holds and payouts are NGN-only until multi-currency lands
//!   (400 otherwise).
//! - P-01/C3: payouts are ledger-first (pending hold -> rail -> post/void)
//!   with a durable `payout_attempts` record for the reconciler.
//!
//! SPEC-W44 hardening (closes S1-F7-01 "mint-and-drain"):
//! - K6: every money MUTATION additionally requires a money role
//!   (`X-User-Roles` ∩ `MONEY_ROLES`; internal-token callers exempt) —
//!   tenant membership alone never moves money.
//! - K7: payouts reference a registered tenant-owned `beneficiary_id` (raw
//!   per-call `payee` is rejected 422); human deposits record provenance
//!   (`declared_by` = gateway `X-User-Id`, optional `psp_reference`).
//! - K5: activity payloads accept `tenant_slug` (preferred; uuid-only
//!   `tenant_id` logs a WARN), and tenant values are path-safety checked.

use axum::{
    extract::{Path, Query, State},
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::auth::AuthRejection;
use crate::ledger::{
    transfer_id_from_key, CaptureResult, LedgerError, TenantBalance, Transfer, TransferState,
};
use crate::mojaloop::{Money, PartyIdInfo, PayoutInstruction, PayoutOutcome, PayoutRailOutcome};
use crate::payouts::{payout_post_id, payout_void_id, AttemptState, PayoutAttempt};
use crate::registry::{Beneficiary, DepositProvenance};
use crate::AppState;

/// P-13: NGN-only until multi-currency lands (documented in README).
const SUPPORTED_CURRENCY: &str = "NGN";

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

    fn conflict(msg: impl Into<String>) -> Self {
        Self {
            status: StatusCode::CONFLICT,
            message: msg.into(),
        }
    }

    fn bad_gateway(msg: impl Into<String>) -> Self {
        Self {
            status: StatusCode::BAD_GATEWAY,
            message: msg.into(),
        }
    }

    fn unprocessable(msg: impl Into<String>) -> Self {
        Self {
            status: StatusCode::UNPROCESSABLE_ENTITY,
            message: msg.into(),
        }
    }
}

fn auth_err(r: AuthRejection) -> ApiError {
    ApiError {
        status: r.status(),
        message: r.message().to_string(),
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
            // P-11: partial refund amount against a pending hold is a 400.
            LedgerError::AmountMismatch(_) => StatusCode::BAD_REQUEST,
            // P-06: cross-tenant money operations are 403.
            LedgerError::TenantMismatch(_) => StatusCode::FORBIDDEN,
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
    /// P-12/C5: REQUIRED on money-moving endpoints (400 when absent/empty).
    pub idempotency_key: Option<String>,
    /// SPEC-W44 K7: optional PSP reference recorded in the deposit provenance
    /// alongside `declared_by` (gateway X-User-Id).
    pub psp_reference: Option<String>,
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
    /// P-12/C5: REQUIRED (400 when absent/empty).
    pub idempotency_key: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct NoShowFeeBody {
    pub tenant_id: String,
    pub deposit_id: Uuid,
    pub amount_cents: u64,
    pub booking_id: Option<String>,
    /// P-12/C5: REQUIRED (400 when absent/empty).
    pub idempotency_key: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct PayoutBody {
    pub tenant_id: String,
    pub amount_cents: u64,
    pub currency: String,
    /// SPEC-W44 K7: REJECTED (422). The payout destination must be a
    /// registered tenant beneficiary — a raw per-call payee let any tenant
    /// member drain revenue to an arbitrary Mojaloop party (S1-F7-01).
    pub payee: Option<PartyIdInfo>,
    /// K7: REQUIRED. Must reference a `payout_beneficiaries` row owned by
    /// `tenant_id` and not disabled (422 otherwise).
    pub beneficiary_id: Option<Uuid>,
    /// P-12/C5: REQUIRED (400 when absent/empty).
    pub idempotency_key: Option<String>,
}

/// K7: register a vetted payout destination (K6-gated, tenant-bound).
#[derive(Debug, Deserialize)]
pub struct BeneficiaryBody {
    pub tenant_id: String,
    pub label: String,
    pub party_id_info: PartyIdInfo,
}

#[derive(Debug, Deserialize)]
pub struct BeneficiaryListParams {
    pub tenant_id: String,
}

/// K7: disable a beneficiary (soft-delete; payouts to it are then 422).
#[derive(Debug, Deserialize)]
pub struct BeneficiaryDisableBody {
    pub tenant_id: String,
}

#[derive(Debug, Serialize)]
pub struct PayoutResponse {
    pub payout_id: String,
    pub ledger_transfer: Transfer,
    pub mojaloop: PayoutOutcome,
}

/// P-10: explicit account provisioning (internal token required).
#[derive(Debug, Deserialize)]
pub struct ProvisionBody {
    pub tenant_id: String,
}

// Temporal activity bodies (SPEC §6: BookingSagaWorkflow HoldDeposit/VoidHold).
// SPEC-W44 K5: `tenant_slug` (Keycloak slug, the ledger namespace) is
// PREFERRED; legacy uuid-only `tenant_id` payloads are accepted with a WARN
// while saga callers migrate (CODER-B2/N2 switch them to TenantSlug).
#[derive(Debug, Deserialize)]
pub struct HoldDepositActivityBody {
    pub tenant_id: Option<String>,
    pub tenant_slug: Option<String>,
    pub booking_id: String,
    pub amount_cents: u64,
    pub currency: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct VoidHoldActivityBody {
    pub tenant_id: Option<String>,
    pub tenant_slug: Option<String>,
    pub deposit_id: Option<Uuid>,
    pub booking_id: Option<String>,
}

// ---------------------------------------------------------------------------
// Shared guards
// ---------------------------------------------------------------------------

/// P-12/C5: money-moving endpoints require a non-empty idempotency key.
fn require_idempotency_key(key: &Option<String>) -> Result<String, ApiError> {
    key.as_ref()
        .map(|k| k.trim())
        .filter(|k| !k.is_empty())
        .map(|k| k.to_string())
        .ok_or_else(|| {
            ApiError::bad_request("idempotency_key is required on money-moving endpoints")
        })
}

/// P-13: NGN-only guard (400 until multi-currency lands).
fn require_ngn(currency: Option<&str>) -> Result<(), ApiError> {
    match currency {
        Some(c) if c != SUPPORTED_CURRENCY => Err(ApiError::bad_request(format!(
            "unsupported currency '{c}': payments are {SUPPORTED_CURRENCY}-only \
             until multi-currency lands"
        ))),
        _ => Ok(()),
    }
}

/// SPEC-W44 K6: money-mutation gate (tenant binding first, then role).
fn require_money_mutation(st: &AppState, headers: &HeaderMap, tenant: &str) -> Result<(), ApiError> {
    st.auth.authorize_tenant(headers, tenant).map_err(auth_err)?;
    st.auth.require_money_role(headers).map_err(auth_err)
}

/// SPEC-W44 K5 (W-P item 2): tenant values are ledger account-name segments
/// (`tenant:{id}:deposits`) and URL path segments — reject anything that
/// could smuggle a path/account separator or traversal (400).
fn require_safe_tenant(tenant: &str) -> Result<(), ApiError> {
    let bad = tenant.is_empty()
        || tenant.len() > 128
        || tenant.contains("..")
        || tenant
            .chars()
            .any(|c| c.is_control() || c.is_whitespace() || c == '/' || c == '\\' || c == ':');
    if bad {
        return Err(ApiError::bad_request(
            "invalid tenant id: must be a non-empty slug without path separators",
        ));
    }
    Ok(())
}

/// SPEC-W44 K5: resolve the tenant for an activity payload — `tenant_slug`
/// wins; a bare uuid `tenant_id` is accepted with a WARN (legacy saga
/// callers) so the two-namespace split cannot silently reappear.
fn resolve_activity_tenant(
    tenant_slug: &Option<String>,
    tenant_id: &Option<String>,
) -> Result<String, ApiError> {
    if let Some(s) = tenant_slug.as_ref().map(|s| s.trim()).filter(|s| !s.is_empty()) {
        require_safe_tenant(s)?;
        return Ok(s.to_string());
    }
    match tenant_id.as_ref().map(|s| s.trim()).filter(|s| !s.is_empty()) {
        Some(t) => {
            if Uuid::parse_str(t).is_ok() {
                tracing::warn!(
                    tenant_id = t,
                    "K5: activity payload carries a uuid-only tenant_id (no tenant_slug); \
                     the ledger namespace is the Keycloak slug — migrate the caller"
                );
            }
            require_safe_tenant(t)?;
            Ok(t.to_string())
        }
        None => Err(ApiError::bad_request(
            "tenant_slug (preferred) or tenant_id is required",
        )),
    }
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------
pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/metrics", get(metrics))
        .route("/v1/deposits", post(hold_deposit))
        .route("/v1/deposits/:id/capture", post(capture_deposit))
        .route("/v1/refunds", post(refund))
        .route("/v1/no-show-fee", post(no_show_fee))
        .route("/v1/accounts/:tenant_id/balance", get(balance))
        .route("/v1/payouts", post(payout))
        .route("/v1/beneficiaries", get(list_beneficiaries))
        .route("/v1/beneficiaries", post(create_beneficiary))
        .route("/v1/beneficiaries/:id/disable", post(disable_beneficiary))
        .route("/v1/internal/accounts/provision", post(provision_accounts))
        .route("/activities/hold-deposit", post(activity_hold_deposit))
        .route("/activities/void-hold", post(activity_void_hold))
        .with_state(state)
}

/// SPEC-W44 F15-03: dependency-aware liveness. `status: "degraded"` + 503
/// when a real dependency check fails (ledger probe — TigerBeetle when
/// tb-live; Postgres ping with a 2s budget when a DSN-backed registry is
/// configured) or when commands have been dead-lettered
/// (`commands_dead_lettered > 0`). The DLQ producer state is REPORTED but
/// does not flip the status: with the producer down the consumer fails
/// closed (offsets uncommitted, redelivery) rather than serving wrong.
async fn healthz(State(st): State<AppState>) -> Response {
    use std::sync::atomic::Ordering;
    let budget = std::time::Duration::from_secs(2);

    let ledger_ok = match tokio::time::timeout(budget, st.ledger.ping()).await {
        Ok(Ok(())) => true,
        Ok(Err(e)) => {
            tracing::warn!(error = %e, "healthz: ledger probe failed");
            false
        }
        Err(_) => {
            tracing::warn!("healthz: ledger probe timed out (2s budget)");
            false
        }
    };
    // PG ping only when the registry is actually Postgres-backed (the mem
    // fallback ping is a tautology and would hide "no DSN configured").
    let pg_configured = st.config.database_url.is_some();
    let pg_ok = if pg_configured {
        match tokio::time::timeout(budget, st.registry.ping()).await {
            Ok(Ok(())) => true,
            Ok(Err(e)) => {
                tracing::warn!(error = %e, "healthz: postgres ping failed");
                false
            }
            Err(_) => {
                tracing::warn!("healthz: postgres ping timed out (2s budget)");
                false
            }
        }
    } else {
        true
    };
    let dead_lettered = st.commands_dead_lettered.load(Ordering::Relaxed);
    let degraded = !ledger_ok || !pg_ok || dead_lettered > 0;
    let status = if degraded { "degraded" } else { "ok" };
    let code = if degraded {
        StatusCode::SERVICE_UNAVAILABLE
    } else {
        StatusCode::OK
    };
    (
        code,
        Json(serde_json::json!({
            "status": status,
            "service": "payments-service",
            "ledger_impl": st.config.ledger_impl,
            "checks": {
                "ledger": if ledger_ok { "ok" } else { "fail" },
                "postgres": if !pg_configured { "not-configured" } else if pg_ok { "ok" } else { "fail" },
                "dlq_producer": if st.dlq.available() { "up" } else { "down" },
            },
            "commands_dead_lettered": dead_lettered,
        })),
    )
        .into_response()
}

/// SPEC-W44 F15-03: minimal Prometheus text exposition (hand-rolled; no new
/// dependency so the pinned Cargo.lock stays byte-identical).
async fn metrics(State(st): State<AppState>) -> Response {
    use std::sync::atomic::Ordering;
    let body = format!(
        "# HELP payments_commands_processed_total Payments commands handled successfully.\n\
         # TYPE payments_commands_processed_total counter\n\
         payments_commands_processed_total {}\n\
         # HELP payments_commands_dead_lettered Commands dead-lettered after bounded retries.\n\
         # TYPE payments_commands_dead_lettered gauge\n\
         payments_commands_dead_lettered {}\n\
         # HELP payments_payout_attempts_total Payout attempts by rail outcome.\n\
         # TYPE payments_payout_attempts_total counter\n\
         payments_payout_attempts_total{{outcome=\"attempted\"}} {}\n\
         payments_payout_attempts_total{{outcome=\"committed\"}} {}\n\
         payments_payout_attempts_total{{outcome=\"failed\"}} {}\n\
         payments_payout_attempts_total{{outcome=\"unknown\"}} {}\n\
         # HELP payments_events_published_total Outbox events published (best-effort).\n\
         # TYPE payments_events_published_total counter\n\
         payments_events_published_total {}\n\
         # HELP payments_events_failed_total Outbox publish failures (reconciler republishes).\n\
         # TYPE payments_events_failed_total counter\n\
         payments_events_failed_total {}\n",
        st.commands_processed.load(Ordering::Relaxed),
        st.commands_dead_lettered.load(Ordering::Relaxed),
        st.payouts_attempted.load(Ordering::Relaxed),
        st.payouts_committed.load(Ordering::Relaxed),
        st.payouts_failed.load(Ordering::Relaxed),
        st.payouts_unknown.load(Ordering::Relaxed),
        st.events_published.load(Ordering::Relaxed),
        st.events_failed.load(Ordering::Relaxed),
    );
    (
        [(header::CONTENT_TYPE, "text/plain; version=0.0.4; charset=utf-8")],
        body,
    )
        .into_response()
}

async fn hold_deposit(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<HoldDepositBody>,
) -> Result<(StatusCode, Json<DepositResponse>), ApiError> {
    // K7: this is the HUMAN deposit path — K6-gated and provenance-recorded.
    // (Deposit creation by internal/verified-payment paths — activities and
    // the Kafka commands consumer — is unchanged.)
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    require_ngn(body.currency.as_deref())?;
    let key = require_idempotency_key(&body.idempotency_key)?;
    // P-10: auto-provision the tenant accounts on first hold (idempotent,
    // exists-ok) so the live ledger never rejects a first-time tenant.
    st.ledger.create_accounts(&body.tenant_id).await?;
    let transfer_id = transfer_id_from_key(Some(&key));
    let t = st
        .ledger
        .hold_deposit(&body.tenant_id, transfer_id, body.amount_cents)
        .await?;
    // K7: provenance — who declared this deposit (gateway X-User-Id) and an
    // optional PSP reference. Write-once (idempotent replay keeps the first
    // record). Best-effort after the ledger commit: the money is already
    // held; a provenance write failure is logged loudly, not rolled back.
    let declared_by = st
        .auth
        .user_id(&headers)
        .unwrap_or("unknown")
        .to_string();
    if let Err(e) = st
        .registry
        .record_deposit_provenance(&DepositProvenance {
            deposit_id: t.id_string(),
            tenant_id: body.tenant_id.clone(),
            declared_by: declared_by.clone(),
            psp_reference: body
                .psp_reference
                .as_ref()
                .map(|r| r.trim().to_string())
                .filter(|r| !r.is_empty()),
            created_at: chrono::Utc::now(),
        })
        .await
    {
        tracing::error!(error = %e, deposit_id = %t.id_string(),
            "K7: deposit provenance record failed (audit gap)");
    }
    st.publish_event(
        "DepositHeld",
        body.booking_id.as_deref().unwrap_or(&body.tenant_id),
        &body.tenant_id,
        serde_json::json!({
            "depositId": t.id_string(),
            "bookingId": body.booking_id,
            "amountCents": body.amount_cents,
            "currency": body.currency,
            "declaredBy": declared_by,
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
    headers: HeaderMap,
    Path(id): Path<Uuid>,
    Json(body): Json<CaptureBody>,
) -> Result<Json<CaptureResponse>, ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    // Deterministic capture transfer id => idempotent retries (P-12: capture
    // is idempotent by construction from the deposit id).
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
    headers: HeaderMap,
    Json(body): Json<RefundBody>,
) -> Result<(StatusCode, Json<Transfer>), ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    let key = require_idempotency_key(&body.idempotency_key)?;
    let transfer_id = transfer_id_from_key(Some(&key));
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
    headers: HeaderMap,
    Json(body): Json<NoShowFeeBody>,
) -> Result<(StatusCode, Json<CaptureResult>), ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    let key = require_idempotency_key(&body.idempotency_key)?;
    let fee_id = transfer_id_from_key(Some(&key));
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
    headers: HeaderMap,
    Path(tenant_id): Path<String>,
) -> Result<Json<TenantBalance>, ApiError> {
    // P-09: balance reads require tenant binding too.
    require_safe_tenant(&tenant_id)?;
    st.auth
        .authorize_tenant(&headers, &tenant_id)
        .map_err(auth_err)?;
    let bal = st.ledger.balance(&tenant_id).await?;
    Ok(Json(bal))
}

/// P-10: explicit idempotent account provisioning (internal token only).
async fn provision_accounts(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<ProvisionBody>,
) -> Result<Json<serde_json::Value>, ApiError> {
    st.auth.require_internal(&headers).map_err(auth_err)?;
    require_safe_tenant(&body.tenant_id)?;
    let accounts = st.ledger.create_accounts(&body.tenant_id).await?;
    // Note: render ids as hex strings — serde_json::to_value (used by the
    // json! macro) rejects u128 ("number out of range").
    let accounts: Vec<serde_json::Value> = accounts
        .iter()
        .map(|a| {
            serde_json::json!({
                "id": format!("{:032x}", a.id),
                "name": a.name,
                "ledger": a.ledger,
                "code": a.code,
            })
        })
        .collect();
    Ok(Json(serde_json::json!({
        "tenant_id": body.tenant_id,
        "accounts": accounts,
    })))
}

// ---------------------------------------------------------------------------
// K7: payee registry (beneficiaries) — K6-gated, tenant-bound
// ---------------------------------------------------------------------------

/// POST /v1/beneficiaries — register a vetted payout destination.
async fn create_beneficiary(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<BeneficiaryBody>,
) -> Result<(StatusCode, Json<Beneficiary>), ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    let label = body.label.trim();
    if label.is_empty() || label.len() > 200 {
        return Err(ApiError::bad_request("label must be 1..=200 chars"));
    }
    if body.party_id_info.party_id_type.trim().is_empty()
        || body.party_id_info.party_identifier.trim().is_empty()
    {
        return Err(ApiError::bad_request(
            "party_id_info.partyIdType and partyIdentifier are required",
        ));
    }
    let b = Beneficiary {
        id: Uuid::new_v4(),
        tenant_id: body.tenant_id.clone(),
        label: label.to_string(),
        party_id_info: serde_json::to_value(&body.party_id_info)
            .map_err(|e| ApiError::bad_request(format!("invalid party_id_info: {e}")))?,
        created_by: st.auth.user_id(&headers).unwrap_or("unknown").to_string(),
        created_at: chrono::Utc::now(),
        disabled_at: None,
    };
    st.registry
        .create_beneficiary(&b)
        .await
        .map_err(|e| ApiError::bad_gateway(format!("beneficiary store error: {e}")))?;
    Ok((StatusCode::CREATED, Json(b)))
}

/// GET /v1/beneficiaries?tenant_id=… — list the tenant's beneficiaries
/// (disabled rows included, flagged via `disabled_at`).
async fn list_beneficiaries(
    State(st): State<AppState>,
    headers: HeaderMap,
    Query(params): Query<BeneficiaryListParams>,
) -> Result<Json<Vec<Beneficiary>>, ApiError> {
    require_safe_tenant(&params.tenant_id)?;
    require_money_mutation(&st, &headers, &params.tenant_id)?;
    let list = st
        .registry
        .list_beneficiaries(&params.tenant_id)
        .await
        .map_err(|e| ApiError::bad_gateway(format!("beneficiary store error: {e}")))?;
    Ok(Json(list))
}

/// POST /v1/beneficiaries/:id/disable — soft-disable (idempotent). Payouts
/// to a disabled beneficiary are rejected 422.
async fn disable_beneficiary(
    State(st): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
    Json(body): Json<BeneficiaryDisableBody>,
) -> Result<Json<Beneficiary>, ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    match st
        .registry
        .disable_beneficiary(id, &body.tenant_id)
        .await
        .map_err(|e| ApiError::bad_gateway(format!("beneficiary store error: {e}")))?
    {
        Some(b) => Ok(Json(b)),
        // Unknown or foreign id: indistinguishable (no cross-tenant oracle).
        None => Err(ApiError::unprocessable(format!(
            "beneficiary {id} not found for this tenant"
        ))),
    }
}

/// K7: resolve the payout destination from the beneficiary registry. A raw
/// per-call `payee` is ALWAYS rejected (422); the beneficiary must exist,
/// belong to the tenant, and not be disabled.
async fn resolve_payee(
    st: &AppState,
    body: &PayoutBody,
) -> Result<(Uuid, PartyIdInfo), ApiError> {
    let beneficiary_id = match body.beneficiary_id {
        Some(id) => id,
        None => {
            return Err(ApiError::unprocessable(if body.payee.is_some() {
                "a raw per-call payee is no longer accepted (SPEC-W44 K7) — \
                 register the destination first via POST /v1/beneficiaries and \
                 pass beneficiary_id"
            } else {
                "beneficiary_id is required (SPEC-W44 K7) — register the payout \
                 destination first via POST /v1/beneficiaries"
            }))
        }
    };
    // V1: unknown, foreign, and disabled beneficiaries are
    // INDISTINGUISHABLE — one uniform 422 body, matching the disable path's
    // no-cross-tenant-oracle posture (registry.rs disable_beneficiary).
    // Distinguishing "does not belong to this tenant" from "unknown" would
    // be a cross-tenant beneficiary-id existence oracle.
    let invalid =
        || ApiError::unprocessable(format!("invalid beneficiary {beneficiary_id}"));
    let b = st
        .registry
        .get_beneficiary(beneficiary_id)
        .await
        .map_err(|e| ApiError::bad_gateway(format!("beneficiary store error: {e}")))?
        .ok_or_else(invalid)?;
    if b.tenant_id != body.tenant_id {
        return Err(invalid());
    }
    if b.disabled_at.is_some() {
        return Err(invalid());
    }
    let payee: PartyIdInfo = serde_json::from_value(b.party_id_info.clone()).map_err(|e| {
        ApiError::unprocessable(format!("beneficiary {beneficiary_id} party_id_info invalid: {e}"))
    })?;
    Ok((beneficiary_id, payee))
}

async fn payout(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<PayoutBody>,
) -> Result<(StatusCode, Json<PayoutResponse>), ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    require_ngn(Some(&body.currency))?;
    let key = require_idempotency_key(&body.idempotency_key)?;
    // Deterministic id => retries of the same payout key are safe.
    let payout_id = transfer_id_from_key(Some(&key));
    let pid = payout_id.to_string();

    let recorded_outcome = |attempt: &PayoutAttempt| PayoutOutcome {
        quote_id: String::new(),
        transfer_id: pid.clone(),
        state: "COMMITTED".to_string(),
        completed_at: None,
        amount: Money {
            currency: attempt.currency.clone(),
            amount: format!(
                "{}.{:02}",
                attempt.amount_cents / 100,
                attempt.amount_cents % 100
            ),
        },
    };

    // Idempotent replay: a durable attempt record short-circuits the rail.
    if let Some(attempt) = st
        .payout_attempts
        .get(&pid)
        .await
        .map_err(|e| ApiError::bad_gateway(format!("payout attempt store error: {e}")))?
    {
        return match attempt.state {
            AttemptState::Committed | AttemptState::ResolvedCommitted => {
                // Previously settled: replay the stored outcome; the response
                // reflects the ledger's actual stored transfer.
                let t = match st.ledger.get_transfer(payout_post_id(&pid)).await {
                    Ok(t) => t,
                    Err(_) => st.ledger.get_transfer(payout_id).await?,
                };
                Ok((
                    StatusCode::CREATED,
                    Json(PayoutResponse {
                        payout_id: pid.clone(),
                        ledger_transfer: t,
                        mojaloop: recorded_outcome(&attempt),
                    }),
                ))
            }
            AttemptState::Unknown => Err(ApiError::conflict(format!(
                "payout {pid} outcome is unknown and being reconciled; \
                 do not retry with the same idempotency key"
            ))),
            AttemptState::Failed | AttemptState::ResolvedFailed => Err(ApiError::conflict(format!(
                "payout {pid} previously failed on the rail ({}); use a new idempotency key",
                attempt.detail.unwrap_or_default()
            ))),
        };
    }

    // K7: registry-resolved destination (raw payee rejected 422). Resolved
    // AFTER the idempotent-replay short-circuit (a replay replays the
    // originally recorded attempt) but BEFORE any ledger/rail side effect.
    let (beneficiary_id, payee) = resolve_payee(&st, &body).await?;
    st.payouts_attempted
        .fetch_add(1, std::sync::atomic::Ordering::Relaxed);

    let record = |state: AttemptState, detail: Option<String>| {
        let body = &body;
        let payee = &payee;
        let pid = pid.clone();
        let st = st.clone();
        async move {
            st.payout_attempts
                .record(&PayoutAttempt {
                    payout_id: pid,
                    tenant_id: body.tenant_id.clone(),
                    amount_cents: body.amount_cents,
                    currency: body.currency.clone(),
                    payee: serde_json::to_value(payee).unwrap_or(serde_json::Value::Null),
                    state,
                    detail,
                    created_at: chrono::Utc::now(),
                    updated_at: chrono::Utc::now(),
                })
                .await
        }
    };

    // C3 LEDGER-FIRST: 1. pending payout hold reserves the funds BEFORE the
    // rail is called (over-limit payouts are rejected here, with no rail side
    // effect).
    st.ledger.create_accounts(&body.tenant_id).await?;
    let hold = st
        .ledger
        .payout_hold(&body.tenant_id, payout_id, body.amount_cents)
        .await?;
    match hold.state {
        TransferState::Pending => {}
        // Hold replay without an attempt row (e.g. the record write failed on
        // the first try): Posted is a committed replay; Voided means a prior
        // failure/unknown the reconciler owns — never re-execute the rail on
        // the same payout id.
        TransferState::Posted => {
            let t = st
                .ledger
                .get_transfer(payout_post_id(&pid))
                .await
                .unwrap_or(hold);
            return Ok((
                StatusCode::CREATED,
                Json(PayoutResponse {
                    payout_id: pid.clone(),
                    ledger_transfer: t,
                    mojaloop: PayoutOutcome {
                        quote_id: String::new(),
                        transfer_id: pid,
                        state: "COMMITTED".to_string(),
                        completed_at: None,
                        amount: Money {
                            currency: body.currency.clone(),
                            amount: format!(
                                "{}.{:02}",
                                body.amount_cents / 100,
                                body.amount_cents % 100
                            ),
                        },
                    },
                }),
            ));
        }
        TransferState::Voided => {
            return Err(ApiError::conflict(format!(
                "payout {pid} was voided after a rail failure/unknown outcome; \
                 use a new idempotency key"
            )))
        }
    }

    // 2. Rail execution (quote -> transfer; only explicit COMMITTED counts).
    let instruction = PayoutInstruction {
        transfer_id: payout_id,
        amount_cents: body.amount_cents,
        currency: body.currency.clone(),
        payee: payee.clone(),
        payer: PartyIdInfo {
            party_id_type: "ALIAS".to_string(),
            party_identifier: format!("tenant:{}", body.tenant_id),
        },
    };
    match st.mojaloop.execute_payout(&instruction).await {
        PayoutRailOutcome::Committed(outcome) => {
            // 3a. Rail committed: post the pending payout in full.
            match st
                .ledger
                .payout_post(&body.tenant_id, payout_id, payout_post_id(&pid))
                .await
            {
                Ok(t) => {
                    if let Err(e) = record(AttemptState::Committed, Some(format!(
                        "mojaloop transfer {} committed",
                        outcome.transfer_id
                    )))
                    .await
                    {
                        tracing::error!(error = %e, payout_id = %pid,
                            "payout committed but attempt record failed (replay detection degraded)");
                    }
                    st.payouts_committed
                        .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    st.publish_event(
                        "PayoutPosted",
                        &pid,
                        &body.tenant_id,
                        serde_json::json!({
                            "payoutId": pid,
                            "amountCents": body.amount_cents,
                            "currency": body.currency,
                            "payee": payee,
                            "beneficiaryId": beneficiary_id,
                            "mojaloopTransferId": outcome.transfer_id,
                            "mojaloopState": outcome.state,
                            "ledgerRef": t.id_string(),
                        }),
                    )
                    .await;
                    Ok((
                        StatusCode::CREATED,
                        Json(PayoutResponse {
                            payout_id: pid,
                            ledger_transfer: t,
                            mojaloop: outcome,
                        }),
                    ))
                }
                Err(e) => {
                    // The rail committed but the ledger post failed: record as
                    // UNKNOWN so the reconciler settles the pending hold.
                    tracing::error!(error = %e, payout_id = %pid,
                        "CRITICAL: mojaloop transfer committed but ledger payout post failed");
                    let _ = record(
                        AttemptState::Unknown,
                        Some(format!("rail committed; ledger post failed: {e}")),
                    )
                    .await;
                    Err(ApiError::bad_gateway(format!(
                        "payout rail committed but ledger post failed; recorded for reconciliation"
                    )))
                }
            }
        }
        PayoutRailOutcome::Failed(reason) => {
            // 3b. Rail failure: void the pending hold, record durably.
            if let Err(e) = st
                .ledger
                .payout_void(&body.tenant_id, payout_id, payout_void_id(&pid))
                .await
            {
                tracing::error!(error = %e, payout_id = %pid,
                    "CRITICAL: rail rejected payout but ledger void failed");
            }
            let _ = record(AttemptState::Failed, Some(reason.clone())).await;
            st.payouts_failed
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            Err(ApiError::bad_gateway(format!("payout rail rejected: {reason}")))
        }
        PayoutRailOutcome::Unknown(reason) => {
            // 3c. Unknown: void the pending hold, record for the reconciler.
            if let Err(e) = st
                .ledger
                .payout_void(&body.tenant_id, payout_id, payout_void_id(&pid))
                .await
            {
                tracing::error!(error = %e, payout_id = %pid,
                    "CRITICAL: rail outcome unknown and ledger void failed; hold still pending");
            }
            let _ = record(AttemptState::Unknown, Some(reason.clone())).await;
            st.payouts_unknown
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            Err(ApiError::bad_gateway(format!(
                "payout rail outcome unknown; recorded for reconciliation: {reason}"
            )))
        }
    }
}

// ---------------------------------------------------------------------------
// Temporal activity handlers (BookingSagaWorkflow: HoldDeposit / VoidHold)
// P-09: internal token required.
// ---------------------------------------------------------------------------
async fn activity_hold_deposit(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<HoldDepositActivityBody>,
) -> Result<(StatusCode, Json<DepositResponse>), ApiError> {
    st.auth.require_internal(&headers).map_err(auth_err)?;
    if body.amount_cents == 0 {
        return Err(ApiError::bad_request("amount_cents must be > 0"));
    }
    require_ngn(body.currency.as_deref())?;
    // K5: tenant_slug preferred; uuid-only tenant_id accepted with a WARN.
    let tenant = resolve_activity_tenant(&body.tenant_slug, &body.tenant_id)?;
    // P-10: auto-provision on first hold (idempotent, exists-ok).
    st.ledger.create_accounts(&tenant).await?;
    // Deterministic per booking => saga retries are idempotent.
    let transfer_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("saga-hold:{}", body.booking_id).as_bytes(),
    );
    let t = st
        .ledger
        .hold_deposit(&tenant, transfer_id, body.amount_cents)
        .await?;
    st.publish_event(
        "DepositHeld",
        &body.booking_id,
        &tenant,
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
    headers: HeaderMap,
    Json(body): Json<VoidHoldActivityBody>,
) -> Result<Json<Transfer>, ApiError> {
    st.auth.require_internal(&headers).map_err(auth_err)?;
    // K5: tenant_slug preferred; uuid-only tenant_id accepted with a WARN.
    let tenant = resolve_activity_tenant(&body.tenant_slug, &body.tenant_id)?;
    let deposit_id = match (body.deposit_id, &body.booking_id) {
        (Some(d), _) => d,
        (None, Some(b)) => Uuid::new_v5(
            &Uuid::NAMESPACE_URL,
            format!("saga-void:{b}").as_bytes(),
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
        .refund(&tenant, transfer_id, Some(deposit_id), 0)
        .await?;
    st.publish_event(
        "HoldVoided",
        &deposit_id.to_string(),
        &tenant,
        serde_json::json!({
            "depositId": deposit_id.to_string(),
            "ledgerRef": t.id_string(),
            "via": "temporal-activity",
        }),
    )
    .await;
    Ok(Json(t))
}
