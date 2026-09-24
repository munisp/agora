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
//!
//! SPEC-W45:
//! - K12: POST /v1/refunds gains rail execution — after the (unchanged,
//!   ledger-first) refund commits, a configured Flutterwave rail + a known
//!   provider transaction id triggers `POST /transactions/{id}/refund`;
//!   otherwise the response honestly reports `status: "queued_manual"`.
//! - /v1/transfers: lending disbursement bridge (ledger-first hold -> rail
//!   attempt -> committed / queued_manual, replay returns the original).
//! - K20: payouts strictly above PAYOUT_APPROVAL_THRESHOLD_KOBO (default 0 =
//!   disabled) park in `pending_approval` (funds reserved, no rail call)
//!   until POST /v1/payouts/:id/approve (owner role above the threshold).

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
use crate::transfers::{TransferAttempt, TransferAttemptState, KIND_REFUND, KIND_TRANSFER};
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

    fn not_found(msg: impl Into<String>) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
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
    /// SPEC-W45 K12: Flutterwave transaction id of the original charge, when
    /// the caller knows it. Falls back to the deposit provenance
    /// `psp_reference` (K7) when a `deposit_id` is given.
    pub flutterwave_transaction_id: Option<u64>,
}

/// SPEC-W45 K12 refund response: the ledger transfer PLUS the honest rail
/// outcome. Top-level `id`/`amount` keep the pre-K12 shapes callers parse
/// (booking-service RefundResult), now with `id` rendered as the hex string
/// every other endpoint already uses.
#[derive(Debug, Serialize)]
pub struct RefundResponse {
    /// Refund transfer id (hex string; == `refund_id`).
    pub id: String,
    pub refund_id: String,
    /// Minor units (mirror of `transfer.amount`).
    pub amount: u64,
    pub amount_cents: u64,
    /// "refunded" (provider accepted, or a pending hold was voided with no
    /// provider charge to refund) | "queued_manual" (honest fallback: the
    /// ledger refund committed but the provider refund must be executed
    /// manually — the reason is in `rail_detail`).
    pub status: String,
    /// "flutterwave" when the provider was called, else "none".
    pub rail: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rail_detail: Option<String>,
    /// The ledger refund transfer (full fidelity).
    pub transfer: Transfer,
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
    /// Absent only in the K20 `pending_approval` response (no rail outcome
    /// exists yet); present otherwise, preserving the pre-K20 shape.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mojaloop: Option<PayoutOutcome>,
    /// K20: Some("pending_approval") when the payout awaits approval.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub status: Option<String>,
}

/// SPEC-W45: POST /v1/transfers — the lending disbursement bridge target
/// (booking-service consumer HTTPRail). Accepts A's RailTransfer shape:
/// `{transfer_id, tenant_id, loan_id, application_id, contact_id,
/// amount_kobo, currency, debit_account_code, credit_account_code}` with the
/// `Idempotency-Key` header (body `transfer_id` is the fallback key).
#[derive(Debug, Deserialize)]
pub struct TransferBody {
    pub tenant_id: String,
    /// Amount in KOBO (A's field name); `amount_cents` accepted as an alias.
    pub amount_kobo: Option<u64>,
    pub amount_cents: Option<u64>,
    /// NGN-only guard consistent with the other money routes (P-13).
    pub currency: Option<String>,
    /// Body fallback for the Idempotency-Key header.
    pub transfer_id: Option<String>,
    pub loan_id: Option<String>,
    pub application_id: Option<String>,
    pub contact_id: Option<String>,
    /// Optional explicit rail destination; defaults to a loan/contact alias.
    pub payee: Option<PartyIdInfo>,
    pub reason: Option<String>,
    /// Lending-internal mirror codes (informational; the payments ledger
    /// models the disbursement with its own payout account flow, code 104).
    pub debit_account_code: Option<i64>,
    pub credit_account_code: Option<i64>,
}

/// SPEC-W45 /v1/transfers response. `status` is the honest rail outcome:
/// "committed" (rail COMMITTED, ledger posted) | "queued_manual" (the rail
/// could not execute / its outcome is ambiguous — funds stay RESERVED in the
/// pending ledger hold and the transfer awaits manual execution; verify the
/// rail before acting) | "failed" (explicit rail rejection; hold voided).
#[derive(Debug, Serialize)]
pub struct TransferResponse {
    pub transfer_id: String,
    pub status: String,
    pub amount_cents: u64,
    pub currency: String,
    pub rail: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rail_detail: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ledger_transfer: Option<Transfer>,
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
        .route("/v1/transfers", post(transfer))
        .route("/v1/no-show-fee", post(no_show_fee))
        .route("/v1/accounts/:tenant_id/balance", get(balance))
        .route("/v1/payouts", post(payout))
        .route("/v1/payouts/:id/approve", post(approve_payout))
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
    // SPEC-W46 R3: cached per process — one ledger round trip per tenant.
    st.ensure_accounts(&body.tenant_id).await?;
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

/// SPEC-W45 K12: resolve the Flutterwave transaction id of the original
/// charge — the explicit body field wins; otherwise the deposit provenance
/// `psp_reference` (K7) is consulted when it is a bare numeric provider id.
async fn resolve_fw_tx_id(st: &AppState, body: &RefundBody) -> Option<u64> {
    if let Some(id) = body.flutterwave_transaction_id {
        return Some(id);
    }
    let deposit_id = body.deposit_id?;
    // Provenance is keyed by the ledger id string (hyphenless hex).
    let key = deposit_id.simple().to_string();
    match st.registry.deposit_provenance(&key).await {
        Ok(Some(p)) => p
            .psp_reference
            .as_deref()
            .map(str::trim)
            .and_then(|r| r.parse::<u64>().ok()),
        Ok(None) => None,
        Err(e) => {
            // Best-effort enrichment only: a provenance read failure must
            // never fail the refund (the ledger refund already committed).
            tracing::warn!(error = %e, deposit_id = %deposit_id,
                "K12: provenance lookup for flutterwave tx id failed; treating as no tx id");
            None
        }
    }
}

/// SPEC-W45 K12 refund rail. The ledger refund has ALREADY committed
/// (ledger-first semantics unchanged); this decides the honest rail status:
/// - a voided PENDING hold never reached the provider: nothing to refund;
/// - a posted refund of captured funds attempts `POST /transactions/{id}/refund`
///   when the rail is configured AND a provider tx id is known;
/// - every other case is `queued_manual` with the reason surfaced.
/// Outcomes are recorded durably (rail_attempts) so a replay of the same
/// idempotency key returns the ORIGINAL status instead of re-calling the
/// provider.
async fn refund_rail_status(st: &AppState, body: &RefundBody, t: &Transfer) -> (String, String, Option<String>) {
    // A pending-hold VOID moved no provider money.
    if t.flag == crate::ledger::TransferFlag::VoidPending {
        return (
            "refunded".to_string(),
            "none".to_string(),
            Some("pending hold voided; no provider charge to refund".to_string()),
        );
    }
    let tid = t.id_string();
    // Replay: the durable attempt record is authoritative.
    match st.transfer_attempts.get(&tid).await {
        Ok(Some(att)) => {
            let status = match att.state {
                TransferAttemptState::Committed => "refunded",
                TransferAttemptState::QueuedManual => "queued_manual",
                TransferAttemptState::Failed => "queued_manual",
            };
            let rail = if att.state == TransferAttemptState::Committed {
                "flutterwave"
            } else {
                "none"
            };
            return (status.to_string(), rail.to_string(), att.detail);
        }
        Ok(None) => {}
        Err(e) => {
            tracing::error!(error = %e, refund_id = %tid,
                "K12: rail attempt read failed (replay detection degraded); proceeding without it");
        }
    }
    let record = |state: TransferAttemptState, detail: &str, destination: String| {
        let st = st.clone();
        let tid = tid.clone();
        let detail = detail.to_string();
        async move {
            if let Err(e) = st
                .transfer_attempts
                .record(&TransferAttempt {
                    transfer_id: tid.clone(),
                    kind: KIND_REFUND.to_string(),
                    tenant_id: body.tenant_id.clone(),
                    amount_cents: t.amount,
                    currency: "NGN".to_string(),
                    destination,
                    state,
                    detail: Some(detail),
                    created_at: chrono::Utc::now(),
                    updated_at: chrono::Utc::now(),
                })
                .await
            {
                tracing::error!(error = %e, refund_id = %tid,
                    "K12: refund rail attempt record failed (replay detection degraded)");
            }
        }
    };
    let tx_id = resolve_fw_tx_id(st, body).await;
    if !st.flutterwave.configured() {
        let detail = "flutterwave rail not configured (FLUTTERWAVE_SECRET_KEY unset); \
                      ledger refund committed — manual provider refund required"
            .to_string();
        record(TransferAttemptState::QueuedManual, &detail, String::new()).await;
        return ("queued_manual".to_string(), "none".to_string(), Some(detail));
    }
    let Some(tx_id) = tx_id else {
        let detail = "original charge has no flutterwave transaction id; \
                      ledger refund committed — manual provider refund required"
            .to_string();
        record(TransferAttemptState::QueuedManual, &detail, String::new()).await;
        return ("queued_manual".to_string(), "none".to_string(), Some(detail));
    };
    match st.flutterwave.refund_transaction(tx_id, Some(t.amount)).await {
        Ok(data) => {
            let detail = format!(
                "flutterwave refund accepted (tx {tx_id}, status {})",
                data.status.as_deref().unwrap_or("unknown")
            );
            record(
                TransferAttemptState::Committed,
                &detail,
                tx_id.to_string(),
            )
            .await;
            ("refunded".to_string(), "flutterwave".to_string(), Some(detail))
        }
        Err(e) => {
            let detail = format!(
                "flutterwave refund attempt failed: {e}; \
                 ledger refund committed — manual provider refund required"
            );
            record(
                TransferAttemptState::QueuedManual,
                &detail,
                tx_id.to_string(),
            )
            .await;
            ("queued_manual".to_string(), "none".to_string(), Some(detail))
        }
    }
}

async fn refund(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<RefundBody>,
) -> Result<(StatusCode, Json<RefundResponse>), ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    let key = require_idempotency_key(&body.idempotency_key)?;
    let transfer_id = transfer_id_from_key(Some(&key));
    // Ledger-first (UNCHANGED semantics): hold void / posted refund, TB
    // classifications and replay behavior exactly as before.
    let t = st
        .ledger
        .refund(
            &body.tenant_id,
            transfer_id,
            body.deposit_id,
            body.amount_cents,
        )
        .await?;
    // K12: rail execution AFTER the ledger commit.
    let (status, rail, rail_detail) = refund_rail_status(&st, &body, &t).await;
    st.publish_event(
        "RefundPosted",
        &t.id_string(),
        &body.tenant_id,
        serde_json::json!({
            "refundId": t.id_string(),
            "depositId": body.deposit_id,
            "amountCents": t.amount,
            "reason": body.reason,
            "status": status,
            "rail": rail,
            "railDetail": rail_detail,
            "ledgerRef": t.id_string(),
        }),
    )
    .await;
    Ok((
        StatusCode::CREATED,
        Json(RefundResponse {
            id: t.id_string(),
            refund_id: t.id_string(),
            amount: t.amount,
            amount_cents: t.amount,
            status,
            rail,
            rail_detail,
            transfer: t,
        }),
    ))
}

/// SPEC-W45: POST /v1/transfers — the lending disbursement bridge.
///
/// Auth: K6 money-role gate + X-Internal-Token acceptance for service
/// callers (require_money_mutation — the lending consumer authenticates with
/// the internal token; a human gateway caller needs a money role).
/// Ordering (C3 ledger-first): the pending hold RESERVES the funds before
/// the rail is called; an over-limit transfer is rejected with no rail side
/// effect. Rail outcomes mirror the payout pattern: COMMITTED posts the
/// hold; an explicit rejection voids it (failed); an unreachable/ambiguous
/// rail leaves the hold pending and records `queued_manual` (honest — the
/// funds are reserved and the transfer awaits manual execution, surfaced in
/// the response JSON with a verify-before-manual caveat). The
/// `Idempotency-Key` header (or body `transfer_id`) makes replays return the
/// ORIGINAL recorded outcome.
async fn transfer(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<TransferBody>,
) -> Result<(StatusCode, Json<TransferResponse>), ApiError> {
    require_safe_tenant(&body.tenant_id)?;
    require_money_mutation(&st, &headers, &body.tenant_id)?;
    let amount = body
        .amount_kobo
        .or(body.amount_cents)
        .filter(|a| *a > 0)
        .ok_or_else(|| ApiError::bad_request("amount_kobo (kobo, > 0) is required"))?;
    require_ngn(body.currency.as_deref())?;
    let currency = body
        .currency
        .clone()
        .unwrap_or_else(|| SUPPORTED_CURRENCY.to_string());
    // Idempotency-Key header honored; body transfer_id is the fallback.
    let key = headers
        .get("idempotency-key")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|k| !k.is_empty())
        .map(|k| k.to_string())
        .or_else(|| {
            body.transfer_id
                .as_ref()
                .map(|k| k.trim().to_string())
                .filter(|k| !k.is_empty())
        })
        .ok_or_else(|| {
            ApiError::bad_request(
                "Idempotency-Key header (or body transfer_id) is required on /v1/transfers",
            )
        })?;
    let tid = transfer_id_from_key(Some(&key));
    let pid = tid.to_string();
    let destination = match (&body.payee, &body.loan_id, &body.contact_id) {
        (Some(p), _, _) => serde_json::to_string(p).unwrap_or_default(),
        (None, Some(l), _) => format!("loan:{l}"),
        (None, None, Some(c)) => format!("contact:{c}"),
        (None, None, None) => format!("transfer:{pid}"),
    };

    let response = |status: &str, code: StatusCode, detail: Option<String>, t: Option<Transfer>| {
        (
            code,
            Json(TransferResponse {
                transfer_id: pid.clone(),
                status: status.to_string(),
                amount_cents: amount,
                currency: currency.clone(),
                rail: "mojaloop".to_string(),
                rail_detail: detail,
                ledger_transfer: t,
            }),
        )
    };

    // Idempotent replay: the durable attempt record returns the ORIGINAL
    // outcome (same status + same HTTP code) without re-executing anything.
    if let Some(att) = st
        .transfer_attempts
        .get(&pid)
        .await
        .map_err(|e| ApiError::bad_gateway(format!("transfer attempt store error: {e}")))?
    {
        let t = st.ledger.get_transfer(tid).await.ok();
        return Ok(match att.state {
            TransferAttemptState::Committed => {
                response("committed", StatusCode::CREATED, att.detail, t)
            }
            TransferAttemptState::QueuedManual => {
                response("queued_manual", StatusCode::BAD_GATEWAY, att.detail, t)
            }
            TransferAttemptState::Failed => {
                response("failed", StatusCode::BAD_GATEWAY, att.detail, t)
            }
        });
    }

    let record = |state: TransferAttemptState, detail: String| {
        let st = st.clone();
        let pid = pid.clone();
        let destination = destination.clone();
        let currency = currency.clone();
        let tenant = body.tenant_id.clone();
        async move {
            if let Err(e) = st
                .transfer_attempts
                .record(&TransferAttempt {
                    transfer_id: pid.clone(),
                    kind: KIND_TRANSFER.to_string(),
                    tenant_id: tenant,
                    amount_cents: amount,
                    currency,
                    destination,
                    state,
                    detail: Some(detail),
                    created_at: chrono::Utc::now(),
                    updated_at: chrono::Utc::now(),
                })
                .await
            {
                tracing::error!(error = %e, transfer_id = %pid,
                    "transfer rail attempt record failed (replay detection degraded)");
            }
        }
    };

    // C3 LEDGER-FIRST: the pending hold reserves the funds BEFORE the rail
    // is called (reuses the two-phase payout account flow: tenant revenue ->
    // platform:payouts, code 104; an over-limit transfer is rejected here
    // with no rail side effect). SPEC-W46 R3: account ensure is cached per
    // process (one ledger round trip per tenant).
    st.ensure_accounts(&body.tenant_id).await?;
    let hold = st
        .ledger
        .payout_hold(&body.tenant_id, tid, amount)
        .await?;
    match hold.state {
        TransferState::Pending => {}
        // Hold replay without an attempt row (the record write failed on the
        // first try): Posted is a committed replay; Voided a prior failure —
        // never re-execute the rail on the same transfer id.
        TransferState::Posted => {
            return Ok(response(
                "committed",
                StatusCode::CREATED,
                Some("ledger replay: transfer already posted".to_string()),
                Some(hold),
            ))
        }
        TransferState::Voided => {
            return Ok(response(
                "failed",
                StatusCode::CONFLICT,
                Some(format!(
                    "transfer {pid} was voided after a prior rail failure; \
                     use a new idempotency key"
                )),
                Some(hold),
            ))
        }
    }

    // Rail attempt (quote -> transfer; only explicit COMMITTED counts).
    let payee = body.payee.clone().unwrap_or(PartyIdInfo {
        party_id_type: "ALIAS".to_string(),
        party_identifier: destination.clone(),
    });
    let instruction = PayoutInstruction {
        transfer_id: tid,
        amount_cents: amount,
        currency: currency.clone(),
        payee,
        payer: PartyIdInfo {
            party_id_type: "ALIAS".to_string(),
            party_identifier: format!("tenant:{}", body.tenant_id),
        },
    };
    match st.mojaloop.execute_payout(&instruction).await {
        PayoutRailOutcome::Committed(outcome) => {
            match st
                .ledger
                .payout_post(&body.tenant_id, tid, payout_post_id(&pid))
                .await
            {
                Ok(t) => {
                    let detail = format!("mojaloop transfer {} committed", outcome.transfer_id);
                    record(TransferAttemptState::Committed, detail.clone()).await;
                    st.publish_event(
                        "TransferExecuted",
                        &pid,
                        &body.tenant_id,
                        serde_json::json!({
                            "transferId": pid,
                            "loanId": body.loan_id,
                            "applicationId": body.application_id,
                            "contactId": body.contact_id,
                            "amountCents": amount,
                            "currency": currency,
                            "status": "committed",
                            "reason": body.reason,
                            "debitAccountCode": body.debit_account_code,
                            "creditAccountCode": body.credit_account_code,
                            "mojaloopTransferId": outcome.transfer_id,
                            "ledgerRef": t.id_string(),
                        }),
                    )
                    .await;
                    Ok(response("committed", StatusCode::CREATED, Some(detail), Some(t)))
                }
                Err(e) => {
                    tracing::error!(error = %e, transfer_id = %pid,
                        "CRITICAL: mojaloop transfer committed but ledger post failed");
                    let detail = format!(
                        "rail committed but the ledger post failed ({e}); funds are RESERVED \
                         in the pending hold — reconcile before any manual execution"
                    );
                    record(TransferAttemptState::QueuedManual, detail.clone()).await;
                    Ok(response("queued_manual", StatusCode::BAD_GATEWAY, Some(detail), Some(hold)))
                }
            }
        }
        PayoutRailOutcome::Failed(reason) => {
            // Explicit rail rejection: void the hold (funds released).
            if let Err(e) = st
                .ledger
                .payout_void(&body.tenant_id, tid, payout_void_id(&pid))
                .await
            {
                tracing::error!(error = %e, transfer_id = %pid,
                    "CRITICAL: rail rejected transfer but ledger void failed");
            }
            let detail = format!("transfer rail rejected: {reason}");
            record(TransferAttemptState::Failed, detail.clone()).await;
            st.publish_event(
                "TransferExecuted",
                &pid,
                &body.tenant_id,
                serde_json::json!({
                    "transferId": pid,
                    "loanId": body.loan_id,
                    "amountCents": amount,
                    "currency": currency,
                    "status": "failed",
                    "railDetail": detail,
                }),
            )
            .await;
            Ok(response("failed", StatusCode::BAD_GATEWAY, Some(detail), None))
        }
        PayoutRailOutcome::Unknown(reason) => {
            // Unreachable/ambiguous rail: the hold STAYS PENDING (funds
            // reserved) and the transfer is honestly queued for manual
            // execution. 502 so the caller's rail client does NOT treat the
            // disbursement as delivered; the replay returns this same
            // recorded outcome.
            let detail = format!(
                "rail outcome unknown ({reason}); funds are RESERVED in the pending \
                 ledger hold and the transfer is queued for manual execution — VERIFY \
                 the rail did not execute before acting"
            );
            record(TransferAttemptState::QueuedManual, detail.clone()).await;
            st.publish_event(
                "TransferExecuted",
                &pid,
                &body.tenant_id,
                serde_json::json!({
                    "transferId": pid,
                    "loanId": body.loan_id,
                    "amountCents": amount,
                    "currency": currency,
                    "status": "queued_manual",
                    "railDetail": detail,
                }),
            )
            .await;
            Ok(response(
                "queued_manual",
                StatusCode::BAD_GATEWAY,
                Some(detail),
                Some(hold),
            ))
        }
    }
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
                        mojaloop: Some(recorded_outcome(&attempt)),
                        status: None,
                    }),
                ))
            }
            AttemptState::PendingApproval => {
                // K20: parked above the approval threshold — replay the
                // original 202 (the funds are still reserved; no rail call).
                let t = st.ledger.get_transfer(payout_id).await?;
                Ok((
                    StatusCode::ACCEPTED,
                    Json(PayoutResponse {
                        payout_id: pid.clone(),
                        ledger_transfer: t,
                        mojaloop: None,
                        status: Some("pending_approval".to_string()),
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
    // effect). SPEC-W46 R3: account ensure is cached per process.
    st.ensure_accounts(&body.tenant_id).await?;
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
                    mojaloop: Some(PayoutOutcome {
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
                    }),
                    status: None,
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

    // K20: approval gate. When PAYOUT_APPROVAL_THRESHOLD_KOBO > 0 and the
    // amount is STRICTLY ABOVE it, park the payout: the funds are RESERVED
    // (pending hold above) but the rail is NOT called until
    // POST /v1/payouts/:id/approve. Default threshold 0 = disabled.
    if st.config.payout_approval_threshold_cents > 0
        && body.amount_cents > st.config.payout_approval_threshold_cents
    {
        let detail = format!(
            "amount {} kobo exceeds approval threshold {} kobo; awaiting owner approval",
            body.amount_cents, st.config.payout_approval_threshold_cents
        );
        if let Err(e) = record(AttemptState::PendingApproval, Some(detail.clone())).await {
            // Without the durable record the approve endpoint could not find
            // the payout: fail closed and release the reservation.
            tracing::error!(error = %e, payout_id = %pid,
                "pending_approval record failed; voiding the hold and failing the request");
            let _ = st
                .ledger
                .payout_void(&body.tenant_id, payout_id, payout_void_id(&pid))
                .await;
            return Err(ApiError::bad_gateway(format!(
                "payout approval record failed; no funds moved: {e}"
            )));
        }
        st.publish_event(
            "PayoutPendingApproval",
            &pid,
            &body.tenant_id,
            serde_json::json!({
                "payoutId": pid,
                "amountCents": body.amount_cents,
                "currency": body.currency,
                "payee": payee,
                "beneficiaryId": beneficiary_id,
                "approvalThresholdCents": st.config.payout_approval_threshold_cents,
                "ledgerRef": hold.id_string(),
            }),
        )
        .await;
        return Ok((
            StatusCode::ACCEPTED,
            Json(PayoutResponse {
                payout_id: pid,
                ledger_transfer: hold,
                mojaloop: None,
                status: Some("pending_approval".to_string()),
            }),
        ));
    }

    dispatch_payout_rail(
        &st,
        &pid,
        payout_id,
        &body.tenant_id,
        body.amount_cents,
        &body.currency,
        &payee,
        Some(beneficiary_id),
    )
    .await
}

/// W43 C3 / W45 K20 shared rail dispatch: pending hold is already reserved;
/// attempt the rail, then post (COMMITTED) or void (FAILED/UNKNOWN) and
/// record the durable attempt. Used by POST /v1/payouts (immediate
/// dispatch) and POST /v1/payouts/:id/approve (gated dispatch).
#[allow(clippy::too_many_arguments)]
async fn dispatch_payout_rail(
    st: &AppState,
    pid: &str,
    payout_id: Uuid,
    tenant_id: &str,
    amount_cents: u64,
    currency: &str,
    payee: &PartyIdInfo,
    beneficiary_id: Option<Uuid>,
) -> Result<(StatusCode, Json<PayoutResponse>), ApiError> {
    let record = |state: AttemptState, detail: Option<String>| {
        let pid = pid.to_string();
        let st = st.clone();
        let tenant_id = tenant_id.to_string();
        let currency = currency.to_string();
        let payee = payee.clone();
        async move {
            st.payout_attempts
                .record(&PayoutAttempt {
                    payout_id: pid,
                    tenant_id,
                    amount_cents,
                    currency,
                    payee: serde_json::to_value(payee).unwrap_or(serde_json::Value::Null),
                    state,
                    detail,
                    created_at: chrono::Utc::now(),
                    updated_at: chrono::Utc::now(),
                })
                .await
        }
    };

    // 2. Rail execution (quote -> transfer; only explicit COMMITTED counts).
    let instruction = PayoutInstruction {
        transfer_id: payout_id,
        amount_cents,
        currency: currency.to_string(),
        payee: payee.clone(),
        payer: PartyIdInfo {
            party_id_type: "ALIAS".to_string(),
            party_identifier: format!("tenant:{tenant_id}"),
        },
    };
    match st.mojaloop.execute_payout(&instruction).await {
        PayoutRailOutcome::Committed(outcome) => {
            // 3a. Rail committed: post the pending payout in full.
            match st
                .ledger
                .payout_post(tenant_id, payout_id, payout_post_id(pid))
                .await
            {
                Ok(t) => {
                    if let Err(e) = record(
                        AttemptState::Committed,
                        Some(format!(
                            "mojaloop transfer {} committed",
                            outcome.transfer_id
                        )),
                    )
                    .await
                    {
                        tracing::error!(error = %e, payout_id = %pid,
                            "payout committed but attempt record failed (replay detection degraded)");
                    }
                    st.payouts_committed
                        .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    st.publish_event(
                        "PayoutPosted",
                        pid,
                        tenant_id,
                        serde_json::json!({
                            "payoutId": pid,
                            "amountCents": amount_cents,
                            "currency": currency,
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
                            payout_id: pid.to_string(),
                            ledger_transfer: t,
                            mojaloop: Some(outcome),
                            status: None,
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
                    Err(ApiError::bad_gateway(
                        "payout rail committed but ledger post failed; recorded for reconciliation",
                    ))
                }
            }
        }
        PayoutRailOutcome::Failed(reason) => {
            // 3b. Rail failure: void the pending hold, record durably.
            if let Err(e) = st
                .ledger
                .payout_void(tenant_id, payout_id, payout_void_id(pid))
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
                .payout_void(tenant_id, payout_id, payout_void_id(pid))
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

/// SPEC-W45 K20: POST /v1/payouts/:id/approve — dispatch a payout that was
/// parked in `pending_approval`. K6 money roles required; when the recorded
/// amount is above the (enabled) threshold the caller must additionally be
/// an `owner` (internal-token service callers exempt, per require_owner_role).
/// The dispatch is ledger-first + durable, identical to the immediate path.
async fn approve_payout(
    State(st): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<String>,
) -> Result<(StatusCode, Json<PayoutResponse>), ApiError> {
    let payout_id = Uuid::parse_str(id.trim())
        .map_err(|_| ApiError::bad_request("payout id must be a uuid"))?;
    let attempt = st
        .payout_attempts
        .get(&payout_id.to_string())
        .await
        .map_err(|e| ApiError::bad_gateway(format!("payout attempt store error: {e}")))?
        .ok_or_else(|| ApiError::not_found(format!("payout {payout_id} not found")))?;
    require_safe_tenant(&attempt.tenant_id)?;
    st.auth
        .authorize_tenant(&headers, &attempt.tenant_id)
        .map_err(auth_err)?;
    st.auth.require_money_role(&headers).map_err(auth_err)?;
    // K20: above-threshold approvals require the owner role specifically.
    if st.config.payout_approval_threshold_cents > 0
        && attempt.amount_cents > st.config.payout_approval_threshold_cents
    {
        st.auth.require_owner_role(&headers).map_err(auth_err)?;
    }
    let pid = payout_id.to_string();
    match attempt.state {
        AttemptState::PendingApproval => {}
        // Idempotent replay of an already-approved payout.
        AttemptState::Committed | AttemptState::ResolvedCommitted => {
            let t = match st.ledger.get_transfer(payout_post_id(&pid)).await {
                Ok(t) => t,
                Err(_) => st.ledger.get_transfer(payout_id).await?,
            };
            return Ok((
                StatusCode::OK,
                Json(PayoutResponse {
                    payout_id: pid,
                    ledger_transfer: t,
                    mojaloop: Some(PayoutOutcome {
                        quote_id: String::new(),
                        transfer_id: payout_id.to_string(),
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
                    }),
                    status: Some("approved_replay".to_string()),
                }),
            ));
        }
        other => {
            return Err(ApiError::conflict(format!(
                "payout {pid} is in state '{}' and cannot be approved",
                other.as_str()
            )))
        }
    }
    // The reservation must still be pending before the rail is called.
    let hold = st.ledger.get_transfer(payout_id).await?;
    match hold.state {
        TransferState::Pending => {}
        TransferState::Posted => {
            // A concurrent approval already dispatched; mark + replay.
            let _ = st
                .payout_attempts
                .mark(&pid, AttemptState::Committed, Some("approved (concurrent dispatch replay)"))
                .await;
            let t = st
                .ledger
                .get_transfer(payout_post_id(&pid))
                .await
                .unwrap_or(hold);
            return Ok((
                StatusCode::OK,
                Json(PayoutResponse {
                    payout_id: pid,
                    ledger_transfer: t,
                    mojaloop: Some(PayoutOutcome {
                        quote_id: String::new(),
                        transfer_id: payout_id.to_string(),
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
                    }),
                    status: Some("approved_replay".to_string()),
                }),
            ));
        }
        TransferState::Voided => {
            return Err(ApiError::conflict(format!(
                "payout {pid} hold was voided; it cannot be approved (use a new payout)"
            )))
        }
    }
    let payee: PartyIdInfo = serde_json::from_value(attempt.payee.clone()).map_err(|e| {
        ApiError::bad_gateway(format!("payout {pid} stored payee is undecodable: {e}"))
    })?;
    st.payouts_attempted
        .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
    dispatch_payout_rail(
        &st,
        &pid,
        payout_id,
        &attempt.tenant_id,
        attempt.amount_cents,
        &attempt.currency,
        &payee,
        None,
    )
    .await
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
    // SPEC-W46 R3: cached per process — one ledger round trip per tenant.
    st.ensure_accounts(&tenant).await?;
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
