//! REST API (SPEC-W7 B2/B3): rate cards, invoicing, payment links/QR, and the
//! public Paystack webhook.
//!
//! Auth contract (SPEC-W44 K1/K2/K6 — the W43-era header-trust contract is
//! RETIRED: APISIX now strips caller-sent x-tenant-id/x-user-roles on OIDC
//! routes, so trusting them would be trusting nothing):
//! - **Service callers (K2)**: a valid `x-internal-token` (vs
//!   BILLING_INTERNAL_TOKEN, constant-time; boot fails closed when unset)
//!   fully authenticates the call; the tenant comes from the caller's
//!   path/query/body param.
//! - **Human calls via the gateway (K1)**: no internal token; APISIX injects
//!   `X-Tenant-Slugs` (verified JWT claim; SPEC-W44 F1: the gateway NEVER
//!   injects x-internal-token — the W39-era injection silently authenticated
//!   every human call as a service caller) and the caller-supplied tenant
//!   param must bind to the claim: directly, or — since billing tenants are
//!   uuid-keyed while the claim carries slugs — by resolving each slug to
//!   its uuid via identity-service (see `bind_tenant` / src/identity.rs;
//!   fail-closed on identity errors).
//! - **Money mutations (K6)**: rate-card upsert, generate/issue/void and
//!   payment-link additionally require `X-User-Roles` ∩ `MONEY_ROLES`
//!   (default owner/admin; internal-token callers exempt; fail-closed when
//!   the header is absent).
//! - `/webhooks/paystack` is exempt from both: it authenticates via the
//!   Paystack HMAC signature instead. `/healthz` + `/metrics` are exempt
//!   liveness/observability probes.
//! - Dev escape `OPENDESK_TRUST_DIRECT_TENANT=1` (standalone dev only,
//!   never set in compose) bypasses binding/roles for headerless calls.

use axum::{
    body::Bytes,
    extract::{Path, Query, Request, State},
    http::{header, HeaderMap, StatusCode},
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, patch, post, put},
    Json, Router,
};
use serde::Deserialize;
use uuid::Uuid;

use sqlx::{Postgres, Transaction};

use crate::invoices::{self, BillingError};
use crate::models::{Invoice, InvoiceStatus, RateCard};
use crate::outbox;
use crate::payments_qr;
use crate::tenant;
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
    fn new(status: StatusCode, msg: impl Into<String>) -> Self {
        Self {
            status,
            message: msg.into(),
        }
    }
    fn bad_request(msg: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, msg)
    }
    fn forbidden(msg: impl Into<String>) -> Self {
        Self::new(StatusCode::FORBIDDEN, msg)
    }
    fn internal(msg: impl Into<String>) -> Self {
        Self::new(StatusCode::INTERNAL_SERVER_ERROR, msg)
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

impl From<BillingError> for ApiError {
    fn from(e: BillingError) -> Self {
        match &e {
            BillingError::BadPeriod(_) => ApiError::new(StatusCode::BAD_REQUEST, e.to_string()),
            BillingError::Conflict { .. } | BillingError::IllegalTransition { .. } => {
                ApiError::new(StatusCode::CONFLICT, e.to_string())
            }
            BillingError::NotFound(_) => ApiError::new(StatusCode::NOT_FOUND, e.to_string()),
            // MixedCurrency is a defensive invariant violation (the rate-card
            // upsert gate already rejects mixed currencies at write time), so
            // it surfaces as a 500, not a client error (B-05).
            BillingError::MixedCurrency { .. } | BillingError::Db(_) => {
                ApiError::internal(e.to_string())
            }
        }
    }
}

impl From<sqlx::Error> for ApiError {
    fn from(e: sqlx::Error) -> Self {
        ApiError::internal(e.to_string())
    }
}

// ---------------------------------------------------------------------------
// Auth helpers (SPEC-W44 K1/K2/K6)
// ---------------------------------------------------------------------------

/// Gateway-injected tenant slugs (K1: csv of the verified JWT's
/// `tenant_slugs` claim; APISIX strips caller-sent copies). An absent or
/// empty header means the request is NOT a gateway call.
fn gateway_slugs(headers: &HeaderMap) -> Option<Vec<String>> {
    let raw = headers
        .get("x-tenant-slugs")
        .and_then(|v| v.to_str().ok())?;
    let slugs: Vec<String> = raw
        .split(',')
        .map(|t| t.trim().to_string())
        .filter(|t| !t.is_empty())
        .collect();
    if slugs.is_empty() {
        None
    } else {
        Some(slugs)
    }
}

/// K1/K2 tenant binding: the caller-supplied tenant (path/query/body param)
/// is trusted for internal-token service callers; gateway (human) calls must
/// bind it to the verified `X-Tenant-Slugs` claim (403 on mismatch). The
/// claim carries Keycloak SLUGS while billing tenants are uuid-keyed, so
/// binding is two-stage (SPEC-W44 F1):
///   1. direct match — the param equals a claimed slug (slug-keyed tenants,
///      or a uuid-string slug), no network call;
///   2. otherwise each claimed slug is resolved to its tenant uuid via
///      identity-service `GET /v1/tenants/{slug}` (src/identity.rs, TTL
///      cache); the binding succeeds when any resolved id == the param.
/// Resolution is FAIL-CLOSED: identity unreachable/erroring is a 503 (never
/// a silent allow), a resolved-but-different id is a 403, and resolution
/// disabled (IDENTITY_BASE_URL empty) is a 403. Dev escape
/// `OPENDESK_TRUST_DIRECT_TENANT=1` allows headerless standalone calls.
/// This bound tenant is what the RLS GUC is pinned to (GF6).
async fn bind_tenant(st: &AppState, headers: &HeaderMap, tenant_id: Uuid) -> Result<(), ApiError> {
    // K2: internal-token service callers are fully authenticated.
    if internal_token_matches(&st.config.internal_token, headers) {
        return Ok(());
    }
    // K1: gateway call — the tenant must be listed in the verified claim.
    if let Some(slugs) = gateway_slugs(headers) {
        let t = tenant_id.to_string();
        if slugs.iter().any(|s| *s == t) {
            return Ok(());
        }
        if !st.identity.configured() {
            return Err(ApiError::forbidden(
                "tenant is not bound to the caller (X-Tenant-Slugs membership; \
                 slug-to-uuid resolution disabled: IDENTITY_BASE_URL unset)",
            ));
        }
        for slug in &slugs {
            match st.identity.resolve(slug).await {
                Ok(Some(id)) if id == tenant_id => return Ok(()),
                Ok(Some(_)) | Ok(None) => {}
                Err(e) => {
                    // Fail CLOSED: an identity outage must never widen
                    // access; 503 (retryable) rather than a 403 the caller
                    // cannot distinguish from a real non-membership.
                    tracing::warn!(error = %e, tenant = %tenant_id, "K1 binding: identity resolution failed");
                    return Err(ApiError::new(
                        StatusCode::SERVICE_UNAVAILABLE,
                        format!("tenant binding unavailable: {e}"),
                    ));
                }
            }
        }
        return Err(ApiError::forbidden(
            "tenant is not bound to the caller (X-Tenant-Slugs membership / identity resolution)",
        ));
    }
    if st.config.trust_direct_tenant {
        return Ok(());
    }
    Err(ApiError::forbidden(
        "tenant binding required: no internal token and no gateway-injected X-Tenant-Slugs",
    ))
}

/// K6 money-role gate for mutations (rate-card upsert, generate, issue,
/// void, payment-link): `X-User-Roles` (gateway-injected, csv) must
/// intersect `MONEY_ROLES`; internal-token service callers (K2) are exempt;
/// fail-closed when the header is absent unless the dev escape is on.
fn require_money_role(st: &AppState, headers: &HeaderMap) -> Result<(), ApiError> {
    if internal_token_matches(&st.config.internal_token, headers) {
        return Ok(());
    }
    let roles: Vec<String> = headers
        .get("x-user-roles")
        .and_then(|v| v.to_str().ok())
        .map(|s| {
            s.split([',', ' '])
                .map(|t| t.trim().to_ascii_lowercase())
                .filter(|t| !t.is_empty())
                .collect()
        })
        .unwrap_or_default();
    if roles
        .iter()
        .any(|r| st.config.money_roles.iter().any(|m| m == r))
    {
        return Ok(());
    }
    if st.config.trust_direct_tenant && !headers.contains_key("x-user-roles") {
        return Ok(());
    }
    Err(ApiError::forbidden(
        "a money role is required for this operation (X-User-Roles intersect MONEY_ROLES)",
    ))
}

/// Begin a tenant-scoped transaction for an invoice-ID-addressed route.
/// The caller does not supply the tenant for these routes, so the invoice is
/// looked up on the internal pool (role-gated cross-tenant read, GF6), its
/// OWN tenant is then bound to the caller (K1: X-Tenant-Slugs membership;
/// K2: internal token), and the request transaction runs with the RLS GUC
/// pinned to that tenant — a cross-tenant id is a 403 after a successful
/// lookup, never a leak.
///
/// SPEC-W46 R8: ONE transaction, ONE query roundtrip — the combined
/// `set_config + SELECT` (invoices::get_invoice_pin_tenant) replaces the old
/// lookup-tx + commit + bind + second-tx + re-read chain (6-8 RTTs, two
/// BEGIN/COMMIT pairs per request). Write paths then use
/// UPDATE...RETURNING (transition_invoice / mark_paid_idempotent), so the
/// row fetched here is never re-read on the hot path either.
async fn begin_scoped_invoice_tx(
    st: &AppState,
    headers: &HeaderMap,
    id: Uuid,
) -> Result<(Transaction<'static, Postgres>, Invoice), ApiError> {
    let mut tx = tenant::begin_internal_tx(&st.internal_pool).await?;
    let inv = invoices::get_invoice_pin_tenant(&mut tx, id)
        .await?
        .ok_or_else(|| ApiError::new(StatusCode::NOT_FOUND, format!("invoice not found: {id}")))?;
    bind_tenant(st, headers, inv.tenant_id).await?;
    Ok((tx, inv))
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------
#[derive(Debug, Deserialize)]
pub struct RateCardBody {
    pub metric: String,
    pub unit_price_cents: i64,
    #[serde(default)]
    pub included_quota: i64,
    #[serde(default = "default_currency")]
    pub currency: String,
    /// VAT-ready tax basis points (0..=10000, default 0).
    #[serde(default)]
    pub tax_bps: i64,
}

fn default_currency() -> String {
    "USD".to_string()
}

#[derive(Debug, Deserialize)]
pub struct GenerateBody {
    pub tenant_id: Uuid,
    pub period: String,
    /// Optional billing contact; stored on the invoice (COALESCE-kept on
    /// regenerate when absent).
    #[serde(default)]
    pub billing_email: Option<String>,
}

/// SPEC-W45 K18: identity-service pushes the tenant's plan here on plan
/// change (TenantPlanChanged); billing is the plan's rating-side source.
#[derive(Debug, Deserialize)]
pub struct PlanBody {
    pub plan: String,
}

/// SPEC-W45: PATCH the invoice billing contact. `null` clears it.
#[derive(Debug, Deserialize)]
pub struct BillingEmailBody {
    pub billing_email: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ListParams {
    pub tenant_id: Uuid,
    pub status: Option<String>,
    /// SPEC-W46 R10: page size (default 50, max 200) and keyset cursor from
    /// the previous page's last item (`{unix_micros}:{uuid}`, see
    /// invoices::InvoiceCursor). Both optional — old clients get the same
    /// response shape, just bounded.
    pub limit: Option<i64>,
    pub cursor: Option<String>,
}

/// R10 list bounds: unbounded invoice lists grew linearly with tenant age.
pub const LIST_INVOICES_DEFAULT_LIMIT: i64 = 50;
pub const LIST_INVOICES_MAX_LIMIT: i64 = 200;

#[derive(Debug, Deserialize)]
pub struct PaymentLinkBody {
    pub email: Option<String>,
    pub callback_url: Option<String>,
}

// ---------------------------------------------------------------------------
// Internal-token middleware (RS-002)
// ---------------------------------------------------------------------------

/// Paths exempt from the internal-token check: liveness probes (the
/// orchestrator does not hold the token) and the Paystack webhook, which is a
/// PUBLIC ingress authenticated by the HMAC signature inside its handler
/// (Paystack cannot know our internal token).
fn is_token_exempt(path: &str) -> bool {
    path == "/healthz" || path == "/metrics" || path == "/webhooks/paystack"
}

/// Pure token check (unit-tested): constant-time compare, empty/missing
/// presented token never matches.
fn internal_token_matches(expected: &str, headers: &HeaderMap) -> bool {
    let presented = headers
        .get("x-internal-token")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default();
    !presented.is_empty()
        && payments_qr::constant_time_eq(expected.trim().as_bytes(), presented.trim().as_bytes())
}

/// Request auth gate (K1+K2): passes for (a) token-exempt probes/webhook,
/// (b) a valid internal token (service callers), (c) a gateway-injected
/// X-Tenant-Slugs header (human OIDC call — the per-route tenant binding
/// then decides), or (d) the dev escape. Everything else is 401.
async fn require_internal_token(
    State(st): State<AppState>,
    req: Request,
    next: Next,
) -> Response {
    if is_token_exempt(req.uri().path()) {
        return next.run(req).await;
    }
    if internal_token_matches(&st.config.internal_token, req.headers())
        || gateway_slugs(req.headers()).is_some()
        || st.config.trust_direct_tenant
    {
        return next.run(req).await;
    }
    (
        StatusCode::UNAUTHORIZED,
        Json(serde_json::json!({
            "error": "unauthorized: valid x-internal-token or gateway-injected X-Tenant-Slugs required"
        })),
    )
        .into_response()
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------
pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/metrics", get(metrics))
        .route("/v1/rate-cards/:tenant_id", put(upsert_rate_card))
        .route("/v1/tenants/:id/plan", put(set_tenant_plan))
        .route("/v1/invoices", get(list_invoices))
        .route("/v1/invoices/generate", post(generate_invoice))
        .route("/v1/invoices/:id", get(get_invoice))
        .route("/v1/invoices/:id/billing-email", patch(set_billing_email))
        .route("/v1/invoices/:id/issue", post(issue_invoice))
        .route("/v1/invoices/:id/void", post(void_invoice))
        .route("/v1/invoices/:id/payment-link", post(payment_link))
        .route("/v1/invoices/:id/qr", get(invoice_qr))
        .route("/webhooks/paystack", post(paystack_webhook))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            require_internal_token,
        ))
        // SPEC-W46 R19: gzip/br response compression (negotiated via
        // Accept-Encoding). Outermost layer so it sees the final response;
        // large list_invoices pages are the main win.
        .layer(tower_http::compression::CompressionLayer::new())
        .with_state(state)
}

/// SPEC-W44 F15-07: dependency-aware liveness. `status: "degraded"` + 503
/// when the Postgres ping (2s budget) fails or usage events have been
/// dead-lettered (`usage_dead_lettered > 0`). The Kafka producer state is
/// REPORTED but does not flip the status: with the producer down events stay
/// durable in billing_outbox (RS-001) and are relayed after recovery.
async fn healthz(State(st): State<AppState>) -> Response {
    use std::sync::atomic::Ordering;
    let pg_ok = match tokio::time::timeout(
        std::time::Duration::from_secs(2),
        sqlx::query("SELECT 1").execute(&st.pool),
    )
    .await
    {
        Ok(Ok(_)) => true,
        Ok(Err(e)) => {
            tracing::warn!(error = %e, "healthz: postgres ping failed");
            false
        }
        Err(_) => {
            tracing::warn!("healthz: postgres ping timed out (2s budget)");
            false
        }
    };
    let dead_lettered = st.usage_dead_lettered.load(Ordering::Relaxed);
    let degraded = !pg_ok || dead_lettered > 0;
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
            "service": "billing-engine",
            "payment_mode": st.config.payment_mode(),
            "ledger_impl": st.config.billing_ledger_impl,
            "checks": {
                "postgres": if pg_ok { "ok" } else { "fail" },
                "kafka_producer": if st.producer.is_some() { "up" } else { "down" },
            },
            "events_published": st.events_published.load(Ordering::Relaxed),
            "events_failed": st.events_failed.load(Ordering::Relaxed),
            "usage_dead_lettered": dead_lettered,
        })),
    )
        .into_response()
}

/// Prometheus text exposition (RS-001): the outbox failure counter is the
/// alerting hook for "InvoicePaid could not reach Kafka" (the event itself
/// stays durable in billing_outbox).
async fn metrics(State(st): State<AppState>) -> Response {
    use std::sync::atomic::Ordering;
    let body = format!(
        "# HELP billing_events_published_total Billing events published to Kafka by the outbox relay.\n\
         # TYPE billing_events_published_total counter\n\
         billing_events_published_total {}\n\
         # HELP billing_events_failed_total Billing event publish failures (retried from the durable outbox).\n\
         # TYPE billing_events_failed_total counter\n\
         billing_events_failed_total {}\n\
         # HELP billing_usage_processed_total Usage events handled by the commands consumer.\n\
         # TYPE billing_usage_processed_total counter\n\
         billing_usage_processed_total {}\n\
         # HELP billing_usage_dead_lettered Usage events dead-lettered after bounded retries.\n\
         # TYPE billing_usage_dead_lettered gauge\n\
         billing_usage_dead_lettered {}\n",
        st.events_published.load(Ordering::Relaxed),
        st.events_failed.load(Ordering::Relaxed),
        st.usage_processed.load(Ordering::Relaxed),
        st.usage_dead_lettered.load(Ordering::Relaxed),
    );
    (
        [(header::CONTENT_TYPE, "text/plain; version=0.0.4; charset=utf-8")],
        body,
    )
        .into_response()
}

/// Light billing-email sanity check (not deliverability proof): one `@`,
/// no whitespace/control chars, sane length. Storage stays unconstrained —
/// this is the API-layer gate.
fn validate_billing_email(email: &str) -> Result<String, ApiError> {
    let e = email.trim();
    let bad = e.is_empty()
        || e.len() > 254
        || e.chars().any(|c| c.is_control() || c.is_whitespace())
        || e.matches('@').count() != 1
        || e.starts_with('@')
        || e.ends_with('@');
    if bad {
        return Err(ApiError::bad_request(
            "billing_email must look like an email address (local@domain)",
        ));
    }
    Ok(e.to_string())
}

// ---------------------------------------------------------------------------
// Rate cards (B2)
// ---------------------------------------------------------------------------
async fn upsert_rate_card(
    State(st): State<AppState>,
    Path(tenant_id): Path<Uuid>,
    headers: HeaderMap,
    Json(body): Json<RateCardBody>,
) -> Result<Json<RateCard>, ApiError> {
    bind_tenant(&st, &headers, tenant_id).await?;
    require_money_role(&st, &headers)?;
    if body.metric.trim().is_empty() {
        return Err(ApiError::bad_request("metric must not be empty"));
    }
    if body.unit_price_cents < 0 || body.included_quota < 0 {
        return Err(ApiError::bad_request(
            "unit_price_cents and included_quota must be >= 0",
        ));
    }
    if !(0..=10000).contains(&body.tax_bps) {
        return Err(ApiError::bad_request("tax_bps must be in 0..=10000"));
    }
    let new_currency = body.currency.trim().to_ascii_uppercase();
    let mut tx = tenant::begin_tenant_tx(&st.pool, tenant_id).await?;
    // SPEC-W43 B-05: a tenant's rate cards must be single-currency, otherwise
    // generate() would rate different metrics in different currencies and
    // stamp the invoice with an arbitrary one. Reject a card whose currency
    // differs from any existing card for this tenant (checked inside the
    // same transaction as the upsert so concurrent writers cannot race past
    // the gate).
    let conflicting: Option<String> = sqlx::query_scalar(
        "SELECT currency FROM rate_cards WHERE tenant_id = $1 AND currency <> $2 LIMIT 1",
    )
    .bind(tenant_id)
    .bind(&new_currency)
    .fetch_optional(&mut *tx)
    .await?;
    if let Some(other) = conflicting {
        return Err(ApiError::new(
            StatusCode::CONFLICT,
            format!(
                "rate card currency {new_currency} conflicts with the tenant's \
                 existing rate card currency {other}; a tenant's rate cards must \
                 be single-currency"
            ),
        ));
    }
    sqlx::query(
        "INSERT INTO rate_cards (tenant_id, metric, unit_price_cents, included_quota, currency, tax_bps) \
         VALUES ($1, $2, $3, $4, $5, $6) \
         ON CONFLICT (tenant_id, metric) DO UPDATE SET \
           unit_price_cents = EXCLUDED.unit_price_cents, \
           included_quota = EXCLUDED.included_quota, \
           currency = EXCLUDED.currency, \
           tax_bps = EXCLUDED.tax_bps",
    )
    .bind(tenant_id)
    .bind(body.metric.trim())
    .bind(body.unit_price_cents)
    .bind(body.included_quota)
    .bind(&new_currency)
    .bind(body.tax_bps as i32)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(Json(RateCard {
        tenant_id,
        metric: body.metric.trim().to_string(),
        unit_price_cents: body.unit_price_cents,
        included_quota: body.included_quota,
        currency: new_currency,
        tax_bps: body.tax_bps,
    }))
}

// ---------------------------------------------------------------------------
// Tenant plan (SPEC-W45 K18: billing's plan source; identity pushes on plan
// change). The rating side reads it in invoices::generate_invoice.
// ---------------------------------------------------------------------------
async fn set_tenant_plan(
    State(st): State<AppState>,
    Path(tenant_id): Path<Uuid>,
    headers: HeaderMap,
    Json(body): Json<PlanBody>,
) -> Result<Json<serde_json::Value>, ApiError> {
    bind_tenant(&st, &headers, tenant_id).await?;
    require_money_role(&st, &headers)?;
    let plan = body.plan.trim().to_ascii_lowercase();
    if plan.is_empty()
        || plan.len() > 64
        || !plan
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
    {
        return Err(ApiError::bad_request(
            "plan must be a non-empty slug (alphanumeric, '-' or '_', <= 64 chars)",
        ));
    }
    let mut tx = tenant::begin_tenant_tx(&st.pool, tenant_id).await?;
    sqlx::query(
        "INSERT INTO tenant_plans (tenant_id, plan, updated_at) VALUES ($1, $2, now()) \
         ON CONFLICT (tenant_id) DO UPDATE SET plan = EXCLUDED.plan, updated_at = now()",
    )
    .bind(tenant_id)
    .bind(&plan)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(Json(serde_json::json!({
        "tenant_id": tenant_id,
        "plan": plan,
    })))
}

// ---------------------------------------------------------------------------
// Invoices (B2)
// ---------------------------------------------------------------------------
async fn generate_invoice(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<GenerateBody>,
) -> Result<(StatusCode, Json<Invoice>), ApiError> {
    bind_tenant(&st, &headers, body.tenant_id).await?;
    require_money_role(&st, &headers)?;
    let billing_email = body
        .billing_email
        .as_deref()
        .map(validate_billing_email)
        .transpose()?;
    let mut tx = tenant::begin_tenant_tx(&st.pool, body.tenant_id).await?;
    let inv = invoices::generate_invoice(&mut tx, body.tenant_id, &body.period, billing_email.as_deref()).await?;
    tx.commit().await?;
    Ok((StatusCode::CREATED, Json(inv)))
}

/// SPEC-W45: PATCH /v1/invoices/:id/billing-email — set (or with `null`,
/// clear) the invoice's billing contact. K1 tenant binding (the invoice's
/// own tenant, via the scoped lookup) + K6 money-role gate.
async fn set_billing_email(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
    Json(body): Json<BillingEmailBody>,
) -> Result<Json<Invoice>, ApiError> {
    let (mut tx, _inv) = begin_scoped_invoice_tx(&st, &headers, id).await?;
    require_money_role(&st, &headers)?;
    let email = body
        .billing_email
        .as_deref()
        .map(validate_billing_email)
        .transpose()?;
    let res = sqlx::query(
        "UPDATE invoices SET billing_email = $1 WHERE id = $2 AND status <> 'void'",
    )
    .bind(&email)
    .bind(id)
    .execute(&mut *tx)
    .await?;
    if res.rows_affected() == 0 {
        return Err(ApiError::new(
            StatusCode::CONFLICT,
            format!("invoice not found or void: {id}"),
        ));
    }
    let updated = invoices::get_invoice(&mut tx, id)
        .await?
        .ok_or_else(|| ApiError::internal("invoice vanished after billing-email update"))?;
    tx.commit().await?;
    Ok(Json(updated))
}

async fn list_invoices(
    State(st): State<AppState>,
    headers: HeaderMap,
    Query(params): Query<ListParams>,
) -> Result<Json<Vec<Invoice>>, ApiError> {
    bind_tenant(&st, &headers, params.tenant_id).await?;
    let status = match &params.status {
        Some(s) => Some(
            InvoiceStatus::parse(s)
                .ok_or_else(|| ApiError::bad_request(format!("unknown status '{s}'")))?,
        ),
        None => None,
    };
    let limit = match params.limit {
        None => LIST_INVOICES_DEFAULT_LIMIT,
        Some(n) if n >= 1 && n <= LIST_INVOICES_MAX_LIMIT => n,
        Some(n) => {
            return Err(ApiError::bad_request(format!(
                "limit must be in 1..={LIST_INVOICES_MAX_LIMIT} (got {n})"
            )))
        }
    };
    let cursor = match params.cursor.as_deref() {
        None => None,
        Some(raw) => Some(
            invoices::InvoiceCursor::decode(raw)
                .ok_or_else(|| ApiError::bad_request("malformed cursor (expected {unix_micros}:{uuid})"))?,
        ),
    };
    let mut tx = tenant::begin_tenant_tx(&st.pool, params.tenant_id).await?;
    let inv = invoices::list_invoices(&mut tx, params.tenant_id, status, limit, cursor).await?;
    tx.commit().await?;
    Ok(Json(inv))
}

async fn get_invoice(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<Invoice>, ApiError> {
    let (tx, inv) = begin_scoped_invoice_tx(&st, &headers, id).await?;
    tx.commit().await?;
    Ok(Json(inv))
}

async fn issue_invoice(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<Invoice>, ApiError> {
    let (mut tx, inv) = begin_scoped_invoice_tx(&st, &headers, id).await?;
    require_money_role(&st, &headers)?;
    let (_prev, updated) = invoices::transition_invoice(&mut tx, id, InvoiceStatus::Issued).await?;
    // Ledger: invoice issued -> DR AR-control / CR revenue (code 200).
    // Zero-amount invoices skip the posting (the ledger rejects 0).
    // SPEC-W43 B-03: the postgres ledger posts INSIDE this transaction, so
    // the ledger entry and the issued transition commit atomically (a crash
    // can no longer strand an issued invoice without its AR/revenue entry);
    // a posting failure now rolls the transition back instead of being a
    // warn-level skip. Only backends that cannot enlist (the dev-only sim)
    // fall back to posting after commit.
    let mut post_after_commit = false;
    if inv.subtotal_cents > 0 {
        let enlisted = st
            .ledger
            .invoice_issued_in_tx(
                &mut tx,
                &inv.tenant_id.to_string(),
                id,
                inv.subtotal_cents as u64,
            )
            .await
            .map_err(|e| ApiError::internal(format!("ledger issued posting failed: {e}")))?;
        post_after_commit = enlisted.is_none();
    }
    tx.commit().await?;
    if post_after_commit {
        if let Err(e) = st
            .ledger
            .invoice_issued(&inv.tenant_id.to_string(), id, inv.subtotal_cents as u64)
            .await
        {
            tracing::warn!(error = %e, invoice_id = %id, "ledger issued posting failed");
        }
    }
    Ok(Json(updated))
}

async fn void_invoice(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<Invoice>, ApiError> {
    let (mut tx, inv) = begin_scoped_invoice_tx(&st, &headers, id).await?;
    require_money_role(&st, &headers)?;
    let (prev, updated) = invoices::transition_invoice(&mut tx, id, InvoiceStatus::Void).await?;
    // SPEC-W43 B-02: voiding an ISSUED/PAST_DUE invoice must unwind its
    // receivable — reversing entry DR revenue / CR AR (deterministic
    // transfer id `billing-void:{invoice_id}`, so retries replay) plus an
    // InvoiceVoided outbox event, both in the SAME transaction as the void
    // transition (B-03/RS-001). Voiding a draft stays free: nothing was ever
    // posted for it, so there is nothing to reverse and no event to emit.
    let reverses = matches!(prev, InvoiceStatus::Issued | InvoiceStatus::PastDue);
    let mut post_after_commit = false;
    if reverses {
        // billingEmail rides along WHEN the invoice carries a billing
        // contact (SPEC-W45: billing-contact notifications downstream).
        let mut data = serde_json::json!({
            "invoiceId": id.to_string(),
            "tenantId": inv.tenant_id.to_string(),
            "period": inv.period,
            "previousStatus": prev.as_str(),
            "subtotalCents": inv.subtotal_cents,
            "currency": inv.currency,
        });
        if let Some(email) = &inv.billing_email {
            data["billingEmail"] = serde_json::Value::String(email.clone());
        }
        let event = crate::models::CloudEvent::new(
            "billing-engine",
            "com.opendesk.billing.InvoiceVoided",
            &id.to_string(),
            &inv.tenant_id.to_string(),
            data,
        );
        let payload = serde_json::to_value(&event)
            .map_err(|e| ApiError::internal(format!("invoice voided event serialize: {e}")))?;
        outbox::enqueue(
            &mut tx,
            &st.config.billing_events_topic,
            &inv.tenant_id.to_string(),
            &payload,
        )
        .await?;
        if inv.subtotal_cents > 0 {
            let enlisted = st
                .ledger
                .invoice_voided_in_tx(
                    &mut tx,
                    &inv.tenant_id.to_string(),
                    id,
                    inv.subtotal_cents as u64,
                )
                .await
                .map_err(|e| ApiError::internal(format!("ledger void reversal failed: {e}")))?;
            post_after_commit = enlisted.is_none();
        }
    }
    tx.commit().await?;
    if post_after_commit {
        // Dev-only sim backend could not enlist in the transaction.
        if let Err(e) = st
            .ledger
            .invoice_voided(&inv.tenant_id.to_string(), id, inv.subtotal_cents as u64)
            .await
        {
            tracing::warn!(error = %e, invoice_id = %id, "ledger void reversal failed");
        }
    }
    if reverses {
        st.outbox_notify.notify_one();
    }
    Ok(Json(updated))
}

// ---------------------------------------------------------------------------
// QR payments (B3)
// ---------------------------------------------------------------------------
async fn payment_link(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
    body: Option<Json<PaymentLinkBody>>,
) -> Result<Json<serde_json::Value>, ApiError> {
    let (tx, inv) = begin_scoped_invoice_tx(&st, &headers, id).await?;
    require_money_role(&st, &headers)?;
    tx.commit().await?; // don't hold a pooled connection across the Paystack HTTP call
    if !matches!(inv.status, InvoiceStatus::Issued | InvoiceStatus::PastDue) {
        return Err(ApiError::new(
            StatusCode::CONFLICT,
            format!(
                "payment link requires an issued/past_due invoice (status: {})",
                inv.status.as_str()
            ),
        ));
    }
    let body = body.map(|Json(b)| b);
    let email = body
        .as_ref()
        .and_then(|b| b.email.clone())
        .unwrap_or_else(|| st.config.paystack_default_email.clone());
    let callback_url = body
        .as_ref()
        .and_then(|b| b.callback_url.clone())
        .unwrap_or_else(|| st.config.paystack_callback_url.clone());
    let reference = id.to_string();

    match st.config.paystack_secret_key.as_deref() {
        Some(secret) => {
            let req = payments_qr::PaystackInitRequest {
                email,
                amount: inv.subtotal_cents,
                reference: reference.clone(),
                callback_url,
                metadata: serde_json::json!({
                    "invoice_id": reference,
                    "tenant_id": inv.tenant_id.to_string(),
                    "period": inv.period,
                }),
            };
            let (authorization_url, paystack_ref) =
                payments_qr::paystack_initialize(&st.http, secret, &req)
                    .await
                    .map_err(|e| ApiError::new(StatusCode::BAD_GATEWAY, e))?;
            let mut tx = tenant::begin_tenant_tx(&st.pool, inv.tenant_id).await?;
            invoices::set_payment_ref(&mut tx, id, &authorization_url).await?;
            tx.commit().await?;
            Ok(Json(serde_json::json!({
                "invoice_id": reference,
                "mode": "paystack",
                "reference": paystack_ref,
                "authorization_url": authorization_url,
            })))
        }
        None => {
            let payload = payments_qr::build_static_payload(
                &st.config.billing_merchant_name,
                &st.config.billing_static_account,
                inv.subtotal_cents,
                &inv.currency,
                &reference,
            );
            let mut tx = tenant::begin_tenant_tx(&st.pool, inv.tenant_id).await?;
            invoices::set_payment_ref(&mut tx, id, &payload).await?;
            tx.commit().await?;
            Ok(Json(serde_json::json!({
                "invoice_id": reference,
                "mode": "static",
                "reference": reference,
                "payload": payload,
            })))
        }
    }
}

async fn invoice_qr(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, ApiError> {
    let (tx, inv) = begin_scoped_invoice_tx(&st, &headers, id).await?;
    tx.commit().await?;
    let payment_ref = inv.payment_ref.clone().ok_or_else(|| {
        ApiError::new(
            StatusCode::NOT_FOUND,
            "no payment link yet; POST /v1/invoices/{id}/payment-link first",
        )
    })?;
    let svg = payments_qr::qr_svg(&payment_ref).map_err(ApiError::internal)?;
    Ok(([(header::CONTENT_TYPE, "image/svg+xml")], svg).into_response())
}

// ---------------------------------------------------------------------------
// Paystack webhook (B3) — public, signature-authenticated.
// ---------------------------------------------------------------------------
async fn paystack_webhook(
    State(st): State<AppState>,
    headers: HeaderMap,
    body: Bytes,
) -> Result<(StatusCode, Json<serde_json::Value>), ApiError> {
    let secret = st.config.paystack_secret_key.as_deref().ok_or_else(|| {
        ApiError::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "webhook disabled: PAYSTACK_SECRET_KEY not configured",
        )
    })?;
    let signature = headers
        .get("x-paystack-signature")
        .and_then(|v| v.to_str().ok())
        .ok_or_else(|| {
            ApiError::new(StatusCode::UNAUTHORIZED, "missing x-paystack-signature")
        })?;
    if !payments_qr::verify_paystack_signature(secret, &body, signature) {
        return Err(ApiError::new(
            StatusCode::UNAUTHORIZED,
            "invalid x-paystack-signature",
        ));
    }

    let payload: serde_json::Value = serde_json::from_slice(&body)
        .map_err(|e| ApiError::bad_request(format!("invalid webhook json: {e}")))?;
    let event = payload
        .get("event")
        .and_then(|v| v.as_str())
        .unwrap_or_default();
    if event != "charge.success" {
        // Acknowledge non-charge events so Paystack stops retrying them.
        return Ok((
            StatusCode::OK,
            Json(serde_json::json!({ "status": "ignored", "event": event })),
        ));
    }
    let reference = payload
        .get("data")
        .and_then(|d| d.get("reference"))
        .and_then(|r| r.as_str())
        .unwrap_or_default();
    let invoice_id = match Uuid::parse_str(reference) {
        Ok(id) => id,
        Err(_) => {
            // Not one of our invoice references; ack to avoid retry storms.
            tracing::warn!(reference, "paystack webhook: unknown reference");
            return Ok((StatusCode::OK, Json(serde_json::json!({ "status": "ignored" }))));
        }
    };

    // Idempotent paid transition: already-paid is a 200 replay (B3).
    // GF6: the webhook has no tenant header — it authenticates via the
    // Paystack HMAC signature instead. The invoice lookup therefore runs on
    // the internal pool (role-gated cross-tenant access); the lookup pins
    // `app.tenant_id` to the invoice's own tenant in the same statement, so
    // the write path stays tenant-scoped even here.
    let mut tx = tenant::begin_internal_tx(&st.internal_pool).await?;
    // R8: the lookup pins `app.tenant_id` to the invoice's own tenant in the
    // SAME statement (set_config + SELECT, one roundtrip).
    let looked_up = match invoices::get_invoice_pin_tenant(&mut tx, invoice_id).await {
        Ok(inv) => inv,
        Err(e) => {
            let _ = tx.rollback().await;
            return Err(ApiError::from(e));
        }
    };
    let Some(inv) = looked_up else {
        let _ = tx.rollback().await;
        // Reference shaped like our invoices but unknown; ack.
        tracing::warn!(invoice_id = %invoice_id, "paystack webhook: invoice not found");
        return Ok((StatusCode::OK, Json(serde_json::json!({ "status": "ignored" }))));
    };

    // SPEC-W43 B-01: a charge against a VOID invoice can never settle it
    // (void is terminal; attempting the transition 409'd and Paystack kept
    // retrying forever). Acknowledge and ignore — the provider stops
    // retrying and nothing is recorded.
    if inv.status == InvoiceStatus::Void {
        tx.commit().await?;
        return Ok((
            StatusCode::OK,
            Json(serde_json::json!({ "status": "ignored", "reason": "invoice_void" })),
        ));
    }

    // SPEC-W43 B-06: a duplicate charge.success for an already-paid invoice
    // stays a 200 replay, but it is no longer silently absorbed — a durable
    // DuplicatePaymentIgnored event (with the provider reference) goes to
    // the outbox in the same ack transaction so reconciliation can see it.
    if inv.status == InvoiceStatus::Paid {
        let event = crate::models::CloudEvent::new(
            "billing-engine",
            "com.opendesk.billing.DuplicatePaymentIgnored",
            &invoice_id.to_string(),
            &inv.tenant_id.to_string(),
            serde_json::json!({
                "invoiceId": invoice_id.to_string(),
                "tenantId": inv.tenant_id.to_string(),
                "paystackReference": reference,
            }),
        );
        let event_payload = serde_json::to_value(&event).map_err(|e| {
            ApiError::internal(format!("duplicate payment event serialize: {e}"))
        })?;
        outbox::enqueue(
            &mut tx,
            &st.config.billing_events_topic,
            &inv.tenant_id.to_string(),
            &event_payload,
        )
        .await?;
        tx.commit().await?;
        st.outbox_notify.notify_one();
        return Ok((StatusCode::OK, Json(serde_json::json!({ "status": "already_paid" }))));
    }

    // SPEC-W43 B-01: the charge must settle EXACTLY this invoice — after
    // the HMAC check, the charged amount and currency must equal the
    // invoice's before any paid transition. A mismatch (underpayment,
    // overpayment, wrong currency, or a payload missing the fields) NEVER
    // settles the invoice: it is recorded as a durable payment_mismatch
    // outbox event and acknowledged with 202 so Paystack stops retrying
    // while the mismatch is investigated.
    let paid_amount = payload
        .get("data")
        .and_then(|d| d.get("amount"))
        .and_then(|a| a.as_i64());
    let paid_currency = payload
        .get("data")
        .and_then(|d| d.get("currency"))
        .and_then(|c| c.as_str())
        .unwrap_or_default();
    if paid_amount != Some(inv.subtotal_cents)
        || !paid_currency.eq_ignore_ascii_case(&inv.currency)
    {
        tracing::warn!(
            invoice_id = %invoice_id,
            reference,
            expected_amount = inv.subtotal_cents,
            expected_currency = %inv.currency,
            received_amount = ?paid_amount,
            received_currency = paid_currency,
            "paystack webhook: amount/currency mismatch; invoice NOT marked paid"
        );
        let event = crate::models::CloudEvent::new(
            "billing-engine",
            "com.opendesk.billing.PaymentMismatch",
            &invoice_id.to_string(),
            &inv.tenant_id.to_string(),
            serde_json::json!({
                "invoiceId": invoice_id.to_string(),
                "tenantId": inv.tenant_id.to_string(),
                "paystackReference": reference,
                "expectedAmountCents": inv.subtotal_cents,
                "expectedCurrency": inv.currency,
                "receivedAmountCents": paid_amount,
                "receivedCurrency": paid_currency,
            }),
        );
        let event_payload = serde_json::to_value(&event)
            .map_err(|e| ApiError::internal(format!("payment mismatch event serialize: {e}")))?;
        outbox::enqueue(
            &mut tx,
            &st.config.billing_events_topic,
            &inv.tenant_id.to_string(),
            &event_payload,
        )
        .await?;
        tx.commit().await?;
        st.outbox_notify.notify_one();
        return Ok((
            StatusCode::ACCEPTED,
            Json(serde_json::json!({ "status": "payment_mismatch" })),
        ));
    }

    match invoices::mark_paid_idempotent(&mut tx, invoice_id).await {
        Ok(None) => {
            // Race: another delivery transitioned the invoice between the
            // lookup and the update. Still a 200 replay.
            tx.commit().await?;
            Ok((StatusCode::OK, Json(serde_json::json!({ "status": "already_paid" }))))
        }
        Ok(Some((_prev, inv))) => {
            // R12: the updated row came back with the UPDATE...RETURNING —
            // no post-transition re-read.
            // RS-001: the InvoicePaid CloudEvent is written to the durable
            // outbox INSIDE the same transaction as the paid transition, so
            // the event can never be silently lost (topic unprovisioned,
            // broker down, ...). The relay republishes with backoff until
            // Kafka accepts it; the paid commit is never rolled back by a
            // publication failure.
            // billingEmail is included WHEN the invoice carries a billing
            // contact (SPEC-W45).
            let mut data = serde_json::json!({
                "invoiceId": invoice_id.to_string(),
                "tenantId": inv.tenant_id.to_string(),
                "period": inv.period,
                "subtotalCents": inv.subtotal_cents,
                "currency": inv.currency,
                "paymentRef": inv.payment_ref,
                "paystackReference": reference,
            });
            if let Some(email) = &inv.billing_email {
                data["billingEmail"] = serde_json::Value::String(email.clone());
            }
            let event = crate::models::CloudEvent::new(
                "billing-engine",
                "com.opendesk.billing.InvoicePaid",
                &invoice_id.to_string(),
                &inv.tenant_id.to_string(),
                data,
            );
            let event_payload = serde_json::to_value(&event)
                .map_err(|e| ApiError::internal(format!("invoice paid event serialize: {e}")))?;
            outbox::enqueue(
                &mut tx,
                &st.config.billing_events_topic,
                &inv.tenant_id.to_string(),
                &event_payload,
            )
            .await?;
            // Ledger: invoice paid -> DR payments-clearing / CR AR (code 202).
            // SPEC-W43 B-03: posted INSIDE this transaction by the postgres
            // ledger so the entry and the paid transition commit atomically;
            // a posting failure rolls the transition back. Only the dev-only
            // sim backend falls back to posting after commit.
            let mut post_after_commit = false;
            if inv.subtotal_cents > 0 {
                let enlisted = st
                    .ledger
                    .invoice_paid_in_tx(
                        &mut tx,
                        &inv.tenant_id.to_string(),
                        invoice_id,
                        inv.subtotal_cents as u64,
                    )
                    .await
                    .map_err(|e| {
                        ApiError::internal(format!("ledger paid posting failed: {e}"))
                    })?;
                post_after_commit = enlisted.is_none();
            }
            tx.commit().await?;
            if post_after_commit {
                if let Err(e) = st
                    .ledger
                    .invoice_paid(&inv.tenant_id.to_string(), invoice_id, inv.subtotal_cents as u64)
                    .await
                {
                    tracing::error!(error = %e, invoice_id = %invoice_id, "ledger paid posting failed");
                }
            }
            // Flush immediately (the poll tick is the backstop).
            st.outbox_notify.notify_one();
            Ok((StatusCode::OK, Json(serde_json::json!({ "status": "paid" }))))
        }
        // NotFound cannot occur here (the row was just loaded above), but
        // keep the ack-on-missing contract for defense in depth.
        Err(BillingError::NotFound(_)) => {
            let _ = tx.rollback().await;
            tracing::warn!(invoice_id = %invoice_id, "paystack webhook: invoice not found");
            Ok((StatusCode::OK, Json(serde_json::json!({ "status": "ignored" }))))
        }
        Err(e) => {
            let _ = tx.rollback().await;
            Err(ApiError::from(e))
        }
    }
}

// ---------------------------------------------------------------------------
// Tests: RS-002 internal-token gate (pure logic; DB-backed handler tests are
// covered by the pgserver-backed gate probes, no embedded Postgres here).
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    fn headers_with(token: Option<&str>) -> HeaderMap {
        let mut h = HeaderMap::new();
        if let Some(t) = token {
            h.insert("x-internal-token", t.parse().unwrap());
        }
        h
    }

    #[test]
    fn internal_token_gate_fail_closed() {
        // Missing header, empty value, and wrong value never match.
        assert!(!internal_token_matches("tok-123", &headers_with(None)));
        assert!(!internal_token_matches("tok-123", &headers_with(Some(""))));
        assert!(!internal_token_matches("tok-123", &headers_with(Some("tok-124"))));
        // Prefix/suffix tricks do not pass the constant-time compare.
        assert!(!internal_token_matches("tok-123", &headers_with(Some("tok-1234"))));
        assert!(!internal_token_matches("tok-123", &headers_with(Some("tok-12"))));
        // Exact match passes (surrounding whitespace tolerated).
        assert!(internal_token_matches("tok-123", &headers_with(Some("tok-123"))));
        assert!(internal_token_matches("tok-123", &headers_with(Some(" tok-123 "))));
    }

    #[test]
    fn billing_email_light_validation() {
        for good in ["a@b.co", "billing+acme@example.com", " x@y.ng "] {
            assert!(validate_billing_email(good).is_ok(), "{good} must pass");
        }
        for bad in ["", "no-at-sign", "a@@b.co", "@b.co", "a@", "a b@c.co", "a\nb@c.co"] {
            assert!(validate_billing_email(bad).is_err(), "{bad:?} must fail");
        }
    }

    #[test]
    fn token_exemptions_are_exactly_health_metrics_and_paystack_webhook() {
        assert!(is_token_exempt("/healthz"));
        assert!(is_token_exempt("/metrics"));
        assert!(is_token_exempt("/webhooks/paystack"));
        for p in [
            "/v1/invoices",
            "/v1/rate-cards/9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20",
            "/webhooks/flutterwave",
            "/healthz2",
            "/webhooks/paystack/extra",
        ] {
            assert!(!is_token_exempt(p), "{p} must require the internal token");
        }
    }
}
