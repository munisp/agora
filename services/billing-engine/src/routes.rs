//! REST API (SPEC-W7 B2/B3): rate cards, invoicing, payment links/QR, and the
//! public Paystack webhook.
//!
//! Auth contract (B2): every /v1/* route requires the `X-Tenant-ID` header to
//! match the target tenant (service-to-service via APISIX). Mutating
//! generate/void additionally require the gateway-injected `x-user-roles`
//! header to contain the Keycloak realm role `owner` or `admin` (403
//! otherwise). `/webhooks/paystack` is exempt: it authenticates via the
//! Paystack HMAC signature instead.

use axum::{
    body::Bytes,
    extract::{Path, Query, State},
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post, put},
    Json, Router,
};
use serde::Deserialize;
use uuid::Uuid;

use sqlx::{Postgres, Transaction};

use crate::invoices::{self, BillingError};
use crate::models::{Invoice, InvoiceStatus, RateCard};
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
            BillingError::Db(_) => ApiError::internal(e.to_string()),
        }
    }
}

impl From<sqlx::Error> for ApiError {
    fn from(e: sqlx::Error) -> Self {
        ApiError::internal(e.to_string())
    }
}

// ---------------------------------------------------------------------------
// Auth helpers (B2)
// ---------------------------------------------------------------------------

/// `X-Tenant-ID` must be present and equal the route's tenant.
fn require_tenant(headers: &HeaderMap, tenant_id: Uuid) -> Result<(), ApiError> {
    let parsed = tenant_from_header(headers)?;
    if parsed != tenant_id {
        return Err(ApiError::forbidden("X-Tenant-ID does not match tenant"));
    }
    Ok(())
}

/// Parse the request's tenant context (`X-Tenant-ID`, gateway-validated; the
/// gateway strips client-spoofed copies since W34 GF4). This tenant is what
/// the RLS GUC is set to for the request's transactions (GF6).
fn tenant_from_header(headers: &HeaderMap) -> Result<Uuid, ApiError> {
    let hdr = headers
        .get("x-tenant-id")
        .and_then(|v| v.to_str().ok())
        .ok_or_else(|| ApiError::forbidden("missing X-Tenant-ID header"))?;
    Uuid::parse_str(hdr.trim())
        .map_err(|_| ApiError::forbidden("malformed X-Tenant-ID header"))
}

/// `x-user-roles` (gateway-injected, comma/space separated realm roles) must
/// contain owner or admin for generate/void.
fn require_owner_or_admin(headers: &HeaderMap) -> Result<(), ApiError> {
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
    if roles.iter().any(|r| r == "owner" || r == "admin") {
        Ok(())
    } else {
        Err(ApiError::forbidden(
            "realm role owner or admin required for this operation",
        ))
    }
}

/// Begin a tenant-scoped transaction (GF6: `app.tenant_id` GUC from the
/// gateway-validated header) and load the invoice inside it. Under RLS a
/// cross-tenant id is simply invisible (404); when RLS is bypassed (dev
/// superuser pool) the explicit tenant check below still forbids it (403) —
/// the header check is kept as a second layer, not the only one.
async fn begin_scoped_invoice_tx(
    st: &AppState,
    headers: &HeaderMap,
    id: Uuid,
) -> Result<(Transaction<'static, Postgres>, Invoice), ApiError> {
    let tenant_id = tenant_from_header(headers)?;
    let mut tx = tenant::begin_tenant_tx(&st.pool, tenant_id).await?;
    let inv = invoices::get_invoice(&mut tx, id)
        .await?
        .ok_or_else(|| ApiError::new(StatusCode::NOT_FOUND, format!("invoice not found: {id}")))?;
    if inv.tenant_id != tenant_id {
        return Err(ApiError::forbidden("X-Tenant-ID does not match tenant"));
    }
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
}

fn default_currency() -> String {
    "USD".to_string()
}

#[derive(Debug, Deserialize)]
pub struct GenerateBody {
    pub tenant_id: Uuid,
    pub period: String,
}

#[derive(Debug, Deserialize)]
pub struct ListParams {
    pub tenant_id: Uuid,
    pub status: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct PaymentLinkBody {
    pub email: Option<String>,
    pub callback_url: Option<String>,
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------
pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/v1/rate-cards/{tenant_id}", put(upsert_rate_card))
        .route("/v1/invoices", get(list_invoices))
        .route("/v1/invoices/generate", post(generate_invoice))
        .route("/v1/invoices/{id}", get(get_invoice))
        .route("/v1/invoices/{id}/issue", post(issue_invoice))
        .route("/v1/invoices/{id}/void", post(void_invoice))
        .route("/v1/invoices/{id}/payment-link", post(payment_link))
        .route("/v1/invoices/{id}/qr", get(invoice_qr))
        .route("/webhooks/paystack", post(paystack_webhook))
        .with_state(state)
}

async fn healthz(State(st): State<AppState>) -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "status": "ok",
        "service": "billing-engine",
        "payment_mode": st.config.payment_mode(),
        "events_published": st.events_published.load(std::sync::atomic::Ordering::Relaxed),
        "events_failed": st.events_failed.load(std::sync::atomic::Ordering::Relaxed),
    }))
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
    require_tenant(&headers, tenant_id)?;
    if body.metric.trim().is_empty() {
        return Err(ApiError::bad_request("metric must not be empty"));
    }
    if body.unit_price_cents < 0 || body.included_quota < 0 {
        return Err(ApiError::bad_request(
            "unit_price_cents and included_quota must be >= 0",
        ));
    }
    let mut tx = tenant::begin_tenant_tx(&st.pool, tenant_id).await?;
    sqlx::query(
        "INSERT INTO rate_cards (tenant_id, metric, unit_price_cents, included_quota, currency) \
         VALUES ($1, $2, $3, $4, $5) \
         ON CONFLICT (tenant_id, metric) DO UPDATE SET \
           unit_price_cents = EXCLUDED.unit_price_cents, \
           included_quota = EXCLUDED.included_quota, \
           currency = EXCLUDED.currency",
    )
    .bind(tenant_id)
    .bind(body.metric.trim())
    .bind(body.unit_price_cents)
    .bind(body.included_quota)
    .bind(body.currency.trim().to_ascii_uppercase())
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(Json(RateCard {
        tenant_id,
        metric: body.metric.trim().to_string(),
        unit_price_cents: body.unit_price_cents,
        included_quota: body.included_quota,
        currency: body.currency.trim().to_ascii_uppercase(),
    }))
}

// ---------------------------------------------------------------------------
// Invoices (B2)
// ---------------------------------------------------------------------------
async fn generate_invoice(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<GenerateBody>,
) -> Result<(StatusCode, Json<Invoice>), ApiError> {
    require_tenant(&headers, body.tenant_id)?;
    require_owner_or_admin(&headers)?;
    let mut tx = tenant::begin_tenant_tx(&st.pool, body.tenant_id).await?;
    let inv = invoices::generate_invoice(&mut tx, body.tenant_id, &body.period).await?;
    tx.commit().await?;
    Ok((StatusCode::CREATED, Json(inv)))
}

async fn list_invoices(
    State(st): State<AppState>,
    headers: HeaderMap,
    Query(params): Query<ListParams>,
) -> Result<Json<Vec<Invoice>>, ApiError> {
    require_tenant(&headers, params.tenant_id)?;
    let status = match &params.status {
        Some(s) => Some(
            InvoiceStatus::parse(s)
                .ok_or_else(|| ApiError::bad_request(format!("unknown status '{s}'")))?,
        ),
        None => None,
    };
    let mut tx = tenant::begin_tenant_tx(&st.pool, params.tenant_id).await?;
    let inv = invoices::list_invoices(&mut tx, params.tenant_id, status).await?;
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
    invoices::transition_invoice(&mut tx, id, InvoiceStatus::Issued).await?;
    let updated = invoices::get_invoice(&mut tx, id)
        .await?
        .ok_or_else(|| ApiError::internal("invoice vanished after issue"))?;
    tx.commit().await?;
    // Ledger: invoice issued -> DR AR-control / CR revenue (code 200).
    // Zero-amount invoices skip the posting (the ledger rejects 0). Posted
    // after the DB commit, exactly like before GF6 (failures are warn-level,
    // not rolled back).
    if inv.subtotal_cents > 0 {
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
    let (mut tx, _inv) = begin_scoped_invoice_tx(&st, &headers, id).await?;
    require_owner_or_admin(&headers)?;
    invoices::transition_invoice(&mut tx, id, InvoiceStatus::Void).await?;
    let updated = invoices::get_invoice(&mut tx, id)
        .await?
        .ok_or_else(|| ApiError::internal("invoice vanished after void"))?;
    tx.commit().await?;
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
) -> Result<Json<serde_json::Value>, ApiError> {
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
        return Ok(Json(serde_json::json!({ "status": "ignored", "event": event })));
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
            return Ok(Json(serde_json::json!({ "status": "ignored" })));
        }
    };

    // Idempotent paid transition: already-paid is a 200 replay (B3).
    // GF6: the webhook has no tenant header — it authenticates via the
    // Paystack HMAC signature instead. The invoice lookup therefore runs on
    // the internal pool (role-gated cross-tenant access); once the row is
    // found, `app.tenant_id` is pinned to the invoice's own tenant for the
    // transition, so the write path stays tenant-scoped even here.
    let mut tx = tenant::begin_internal_tx(&st.internal_pool).await?;
    let looked_up = match invoices::get_invoice(&mut tx, invoice_id).await {
        Ok(inv) => inv,
        Err(e) => {
            let _ = tx.rollback().await;
            return Err(ApiError::from(BillingError::from(e)));
        }
    };
    let Some(inv) = looked_up else {
        let _ = tx.rollback().await;
        // Reference shaped like our invoices but unknown; ack.
        tracing::warn!(invoice_id = %invoice_id, "paystack webhook: invoice not found");
        return Ok(Json(serde_json::json!({ "status": "ignored" })));
    };
    tenant::set_tenant_guc(&mut tx, inv.tenant_id).await?;
    match invoices::mark_paid_idempotent(&mut tx, invoice_id).await {
        Ok(None) => {
            tx.commit().await?;
            Ok(Json(serde_json::json!({ "status": "already_paid" })))
        }
        Ok(Some(_prev)) => {
            let inv = invoices::get_invoice(&mut tx, invoice_id)
                .await?
                .ok_or_else(|| ApiError::internal("invoice vanished after payment"))?;
            tx.commit().await?;
            // Ledger: invoice paid -> DR payments-clearing / CR AR (code 202).
            if inv.subtotal_cents > 0 {
                if let Err(e) = st
                    .ledger
                    .invoice_paid(&inv.tenant_id.to_string(), invoice_id, inv.subtotal_cents as u64)
                    .await
                {
                    tracing::warn!(error = %e, invoice_id = %invoice_id, "ledger paid posting failed");
                }
            }
            st.publish_event(
                "InvoicePaid",
                &invoice_id.to_string(),
                &inv.tenant_id.to_string(),
                serde_json::json!({
                    "invoiceId": invoice_id.to_string(),
                    "tenantId": inv.tenant_id.to_string(),
                    "period": inv.period,
                    "subtotalCents": inv.subtotal_cents,
                    "currency": inv.currency,
                    "paymentRef": inv.payment_ref,
                    "paystackReference": reference,
                }),
            )
            .await;
            Ok(Json(serde_json::json!({ "status": "paid" })))
        }
        // NotFound cannot occur here (the row was just loaded above), but
        // keep the ack-on-missing contract for defense in depth.
        Err(BillingError::NotFound(_)) => {
            let _ = tx.rollback().await;
            tracing::warn!(invoice_id = %invoice_id, "paystack webhook: invoice not found");
            Ok(Json(serde_json::json!({ "status": "ignored" })))
        }
        Err(e) => {
            let _ = tx.rollback().await;
            Err(ApiError::from(e))
        }
    }
}
