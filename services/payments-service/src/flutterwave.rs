//! Flutterwave card/checkout rail (SPEC-W12 §6): `POST /v1/payments/flutterwave/initialize`
//! plus the `POST /webhooks/flutterwave` callback, mirroring the proven
//! Paystack module shape (billing-engine `payments_qr.rs` + `paystack_webhook`)
//! on this service's own idioms:
//!
//! * client construction: `reqwest::Client` adapter struct with a thiserror
//!   error enum (MojaloopAdapter idiom);
//! * ledger: initialize posts a deposit hold (code 100), the webhook captures
//!   it (code 101) — the payments-service equivalent of the Paystack
//!   initialize -> mark-paid flow (NOTE: the billing-engine ledger codes
//!   200/201/202 belong to that service's invoice ledger; this service's
//!   LedgerClient models the same flow with its deposit hold/capture codes);
//! * outbox: best-effort `AppState::publish_event` (ADR-0007 note), events
//!   `com.opendesk.payments.FlutterwavePayment{Initialized,Captured}`;
//! * webhook auth: `verif-hash` header == configured `FLUTTERWAVE_SECRET_HASH`
//!   compared in CONSTANT TIME (`constant_time_eq` mirrors the Paystack
//!   signature compare; Flutterwave signs webhooks with a shared static
//!   secret, not an HMAC, so no body HMAC is involved).
//!
//! Config is read from env here (not config.rs) to keep this module additive:
//! `FLUTTERWAVE_SECRET_KEY` (API bearer), `FLUTTERWAVE_SECRET_HASH` (webhook
//! verif-hash), `FLUTTERWAVE_BASE_URL` (default https://api.flutterwave.com/v3),
//! `FLUTTERWAVE_REDIRECT_URL` (optional checkout redirect).
//!
//! ASSUMPTION (no live keys in this wave): the v3 `POST /payments` request /
//! response shapes below follow the public Flutterwave standard-checkout
//! contract ({tx_ref, amount, currency, redirect_url, customer, meta} ->
//! {status:"success", data:{link}}) and the webhook sends
//! {event:"charge.completed", data:{status:"successful", tx_ref, amount,
//! currency, id, meta}}. Marked per SPEC-W12 Agent A's ASSUMPTION convention.

use axum::{
    extract::State,
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::post,
    Json, Router,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

use crate::ledger::{transfer_id_from_key, LedgerError};
use crate::AppState;

// ---------------------------------------------------------------------------
// Constant-time compare (mirror of billing-engine payments_qr.rs).
// ---------------------------------------------------------------------------

/// Length-independent XOR fold: length mismatch fails fast without an
/// early-exit on content; equal-length inputs compare in constant time.
pub fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

/// Verify a Flutterwave `verif-hash` header against the configured secret
/// hash (SPEC-W12 §6). Both sides are trimmed; the compare is constant-time.
pub fn verify_verif_hash(configured_secret_hash: &str, presented: &str) -> bool {
    constant_time_eq(
        configured_secret_hash.trim().as_bytes(),
        presented.trim().as_bytes(),
    )
}

// ---------------------------------------------------------------------------
// Flutterwave v3 client (MojaloopAdapter idiom).
// ---------------------------------------------------------------------------

#[derive(Debug, Error)]
pub enum FlutterwaveError {
    #[error("flutterwave HTTP error: {0}")]
    Http(#[from] reqwest::Error),
    #[error("flutterwave initialize rejected: {0}")]
    Rejected(String),
}

#[derive(Debug, Clone)]
pub struct FlutterwaveAdapter {
    http: reqwest::Client,
    base_url: String,
    /// v3 secret key (Authorization: Bearer). Empty = initialize disabled.
    secret_key: String,
    /// Shared webhook secret compared against the `verif-hash` header.
    /// None = webhook disabled (503, mirrors the Paystack path when
    /// PAYSTACK_SECRET_KEY is unset).
    secret_hash: Option<String>,
    redirect_url: Option<String>,
}

impl FlutterwaveAdapter {
    /// Build from environment (SPEC-W12 §8 naming).
    pub fn from_env() -> Self {
        Self {
            // RS-006: shared client with 5s connect / 30s overall timeouts.
            http: crate::http_client(),
            base_url: std::env::var("FLUTTERWAVE_BASE_URL")
                .unwrap_or_else(|_| "https://api.flutterwave.com/v3".to_string()),
            secret_key: std::env::var("FLUTTERWAVE_SECRET_KEY").unwrap_or_default(),
            secret_hash: std::env::var("FLUTTERWAVE_SECRET_HASH")
                .ok()
                .filter(|s| !s.is_empty()),
            redirect_url: std::env::var("FLUTTERWAVE_REDIRECT_URL")
                .ok()
                .filter(|s| !s.is_empty()),
        }
    }

    fn verif_hash(&self) -> Option<&str> {
        self.secret_hash.as_deref()
    }

    /// ASSUMPTION-shaped v3 standard-checkout initialize:
    /// POST {base}/payments -> {status:"success", data:{link}}.
    /// Returns the checkout link on success.
    pub async fn initialize(&self, req: &FwInitializeRequest) -> Result<String, FlutterwaveError> {
        let resp = self
            .http
            .post(format!("{}/payments", self.base_url))
            .bearer_auth(&self.secret_key)
            .json(req)
            .send()
            .await?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(FlutterwaveError::Rejected(format!("HTTP {status}: {body}")));
        }
        let parsed: FwInitializeResponse = resp
            .json()
            .await
            .map_err(FlutterwaveError::Http)?;
        if parsed.status != "success" {
            return Err(FlutterwaveError::Rejected(parsed.message));
        }
        parsed
            .data
            .map(|d| d.link)
            .ok_or_else(|| FlutterwaveError::Rejected("missing data.link".to_string()))
    }
}

/// ASSUMPTION: v3 standard checkout create-payment body.
#[derive(Debug, Serialize)]
pub struct FwInitializeRequest {
    pub tx_ref: String,
    /// Major units as a decimal string ("1250.00") — v3 accepts both numbers
    /// and numeric strings; we send a string so minor-unit conversion is
    /// exact (no float rounding, billing-engine integer-math rule).
    pub amount: String,
    pub currency: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub redirect_url: Option<String>,
    pub customer: FwCustomer,
    pub meta: FwMeta,
}

#[derive(Debug, Serialize)]
pub struct FwCustomer {
    pub email: String,
}

/// Round-trips through the webhook payload (`data.meta`) so the callback can
/// find the tenant + hold without a local lookup table.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FwMeta {
    pub tenant_id: String,
    /// Round-trips to Flutterwave and back for reconciliation; the webhook
    /// path currently only needs `tenant_id`.
    #[allow(dead_code)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub booking_id: Option<String>,
}

#[derive(Debug, Deserialize)]
struct FwInitializeResponse {
    status: String,
    #[serde(default)]
    message: String,
    /// Absent or null on error envelopes.
    #[serde(default)]
    data: Option<FwInitializeData>,
}

#[derive(Debug, Deserialize)]
struct FwInitializeData {
    link: String,
}

/// Minor units -> exact major-unit decimal string (mojaloop.rs idiom).
fn minor_to_decimal(amount_cents: u64) -> String {
    format!("{}.{:02}", amount_cents / 100, amount_cents % 100)
}

/// `fw-{uuid}` hold references: deterministic, grep-able, and collision-free
/// against Paystack-style invoice references.
fn make_tx_ref(hold_id: Uuid) -> String {
    format!("fw-{hold_id}")
}

fn parse_tx_ref(tx_ref: &str) -> Option<Uuid> {
    tx_ref.strip_prefix("fw-").and_then(|s| Uuid::parse_str(s).ok())
}

// ---------------------------------------------------------------------------
// Router (registered additively from main.rs).
// ---------------------------------------------------------------------------

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/v1/payments/flutterwave/initialize", post(initialize))
        .route("/webhooks/flutterwave", post(webhook))
        .with_state(state)
}

fn err_json(status: StatusCode, msg: impl Into<String>) -> Response {
    (status, Json(serde_json::json!({ "error": msg.into() }))).into_response()
}

/// Ledger error mapping (mirrors routes.rs From<LedgerError> for ApiError).
fn map_ledger_error(e: LedgerError) -> Response {
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
    err_json(status, e.to_string())
}

// ---------------------------------------------------------------------------
// POST /v1/payments/flutterwave/initialize
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
pub struct InitializeBody {
    pub tenant_id: String,
    pub amount_cents: u64,
    pub currency: String,
    pub email: String,
    pub booking_id: Option<String>,
    pub idempotency_key: Option<String>,
    pub redirect_url: Option<String>,
}

async fn initialize(State(st): State<AppState>, Json(body): Json<InitializeBody>) -> Response {
    if body.amount_cents == 0 {
        return err_json(StatusCode::BAD_REQUEST, "amount_cents must be > 0");
    }
    if body.currency.is_empty() {
        return err_json(StatusCode::BAD_REQUEST, "currency is required");
    }
    if body.email.is_empty() {
        return err_json(StatusCode::BAD_REQUEST, "email is required (flutterwave customer)");
    }
    if st.flutterwave.secret_key.is_empty() {
        return err_json(
            StatusCode::SERVICE_UNAVAILABLE,
            "flutterwave disabled: FLUTTERWAVE_SECRET_KEY not configured",
        );
    }

    // Deterministic hold id => initialize retries are idempotent end-to-end
    // (transfer_id_from_key idiom from routes.rs hold_deposit).
    let key = body.idempotency_key.clone().or_else(|| {
        body.booking_id
            .as_ref()
            .map(|b| format!("fw-hold:{b}"))
    });
    let hold_id = transfer_id_from_key(key.as_deref());
    let tx_ref = make_tx_ref(hold_id);

    // 1. Ledger hold first (code 100): the webhook capture is then a pure,
    //    idempotent ledger op. Rail-first ordering used by payouts does not
    //    apply here — the "rail" commit IS the webhook, not the initialize.
    let hold = match st
        .ledger
        .hold_deposit(&body.tenant_id, hold_id, body.amount_cents)
        .await
    {
        Ok(t) => t,
        Err(e) => return map_ledger_error(e),
    };

    // 2. Create the checkout link on the rail (ASSUMPTION-shaped v3 API).
    let req = FwInitializeRequest {
        tx_ref: tx_ref.clone(),
        amount: minor_to_decimal(body.amount_cents),
        currency: body.currency.clone(),
        redirect_url: body
            .redirect_url
            .clone()
            .or_else(|| st.flutterwave.redirect_url.clone()),
        customer: FwCustomer {
            email: body.email.clone(),
        },
        meta: FwMeta {
            tenant_id: body.tenant_id.clone(),
            booking_id: body.booking_id.clone(),
        },
    };
    let link = match st.flutterwave.initialize(&req).await {
        Ok(link) => link,
        Err(e) => {
            // The hold stays pending and can be voided via /v1/refunds;
            // retrying initialize with the same idempotency key replays it.
            return err_json(
                StatusCode::BAD_GATEWAY,
                format!("flutterwave initialize failed: {e}"),
            );
        }
    };

    st.publish_event(
        "FlutterwavePaymentInitialized",
        &tx_ref,
        &body.tenant_id,
        serde_json::json!({
            "txRef": tx_ref,
            "depositId": hold.id_string(),
            "bookingId": body.booking_id,
            "amountCents": body.amount_cents,
            "currency": body.currency,
            "ledgerRef": hold.id_string(),
        }),
    )
    .await;

    (
        StatusCode::CREATED,
        Json(serde_json::json!({
            "tx_ref": tx_ref,
            "link": link,
            "deposit_id": hold.id_string(),
        })),
    )
        .into_response()
}

// ---------------------------------------------------------------------------
// POST /webhooks/flutterwave (public, verif-hash authenticated)
// ---------------------------------------------------------------------------

async fn webhook(
    State(st): State<AppState>,
    headers: HeaderMap,
    body: axum::body::Bytes,
) -> Response {
    let secret_hash = match st.flutterwave.verif_hash() {
        Some(h) => h,
        None => {
            return err_json(
                StatusCode::SERVICE_UNAVAILABLE,
                "webhook disabled: FLUTTERWAVE_SECRET_HASH not configured",
            )
        }
    };
    let presented = headers
        .get("verif-hash")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default();
    if presented.is_empty() {
        return err_json(StatusCode::UNAUTHORIZED, "missing verif-hash");
    }
    if !verify_verif_hash(secret_hash, presented) {
        return err_json(StatusCode::UNAUTHORIZED, "invalid verif-hash");
    }

    let payload: serde_json::Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(e) => return err_json(StatusCode::BAD_REQUEST, format!("invalid webhook json: {e}")),
    };
    let event = payload
        .get("event")
        .and_then(|v| v.as_str())
        .unwrap_or_default();
    if event != "charge.completed" {
        // Acknowledge non-charge events so Flutterwave stops retrying them
        // (paystack_webhook idiom).
        return Json(serde_json::json!({ "status": "ignored", "event": event })).into_response();
    }
    let data = payload.get("data").cloned().unwrap_or(serde_json::json!({}));
    let charge_status = data.get("status").and_then(|v| v.as_str()).unwrap_or_default();
    if charge_status != "successful" {
        return Json(serde_json::json!({ "status": "ignored", "charge_status": charge_status }))
            .into_response();
    }
    let tx_ref = data.get("tx_ref").and_then(|v| v.as_str()).unwrap_or_default();
    let hold_id = match parse_tx_ref(tx_ref) {
        Some(id) => id,
        None => {
            // Not one of our references; ack to avoid retry storms.
            tracing::warn!(tx_ref, "flutterwave webhook: unknown tx_ref");
            return Json(serde_json::json!({ "status": "ignored" })).into_response();
        }
    };
    let tenant_id = data
        .get("meta")
        .and_then(|m| m.get("tenant_id"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string());
    let tenant_id = match tenant_id {
        Some(t) if !t.is_empty() => t,
        _ => {
            tracing::warn!(tx_ref, "flutterwave webhook: missing meta.tenant_id");
            return Json(serde_json::json!({ "status": "ignored" })).into_response();
        }
    };

    // Capture the full hold (code 101). Deterministic capture id => webhook
    // replays are idempotent; an already-captured hold is a 200 replay, not
    // an error (paystack_webhook "already_paid" idiom).
    let capture_id = Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("fw-capture:{tx_ref}").as_bytes(),
    );
    let result = match st.ledger.capture(&tenant_id, hold_id, capture_id, None).await {
        Ok(r) => r,
        Err(LedgerError::AlreadyResolved(_)) => {
            return Json(serde_json::json!({ "status": "already_captured" })).into_response();
        }
        Err(LedgerError::TransferNotFound(_)) => {
            // Reference shaped like ours but unknown to the ledger; ack.
            tracing::warn!(tx_ref, "flutterwave webhook: hold not found");
            return Json(serde_json::json!({ "status": "ignored" })).into_response();
        }
        Err(e) => return map_ledger_error(e),
    };

    // Sanity: the charged amount should equal the captured hold. We never
    // block the capture on a mismatch (money already moved on the rail) —
    // alert via logs for reconciliation instead.
    if let Some(charged) = data.get("amount").and_then(|v| v.as_f64()) {
        let charged_cents = (charged * 100.0).round() as u64;
        if charged_cents != result.post.amount {
            tracing::warn!(
                tx_ref,
                charged_cents,
                captured_cents = result.post.amount,
                "flutterwave webhook: charged/captured amount mismatch"
            );
        }
    }

    st.publish_event(
        "FlutterwavePaymentCaptured",
        tx_ref,
        &tenant_id,
        serde_json::json!({
            "txRef": tx_ref,
            "depositId": hold_id.to_string(),
            "flutterwaveId": data.get("id"),
            "postedAmountCents": result.post.amount,
            "revenueCents": result.revenue.amount,
            "platformFeeCents": result.platform_fee.as_ref().map(|t| t.amount),
            "currency": data.get("currency"),
            "ledgerRef": result.post.id_string(),
        }),
    )
    .await;

    Json(serde_json::json!({ "status": "captured" })).into_response()
}

// ---------------------------------------------------------------------------
// Unit tests: constant-time compare, tx_ref round-trip, request/response
// shapes. No live calls (no keys in this wave).
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn constant_time_eq_behaviour() {
        assert!(constant_time_eq(b"abc", b"abc"));
        assert!(!constant_time_eq(b"abc", b"abd"));
        assert!(!constant_time_eq(b"abc", b"ab"));
        assert!(constant_time_eq(b"", b""));
    }

    #[test]
    fn verif_hash_compare() {
        assert!(verify_verif_hash("s3cret-hash", "s3cret-hash"));
        assert!(verify_verif_hash("  s3cret-hash ", "s3cret-hash"));
        assert!(!verify_verif_hash("s3cret-hash", "s3cret-hasi"));
        assert!(!verify_verif_hash("s3cret-hash", ""));
        assert!(!verify_verif_hash("s3cret-hash", "s3cret-hash-plus"));
    }

    #[test]
    fn tx_ref_round_trip() {
        let id = Uuid::new_v4();
        let r = make_tx_ref(id);
        assert!(r.starts_with("fw-"));
        assert_eq!(parse_tx_ref(&r), Some(id));
        assert_eq!(parse_tx_ref("9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20"), None);
        assert_eq!(parse_tx_ref(""), None);
        assert_eq!(parse_tx_ref("fw-not-a-uuid"), None);
    }

    #[test]
    fn minor_units_render_major_decimal() {
        assert_eq!(minor_to_decimal(125_000), "1250.00");
        assert_eq!(minor_to_decimal(5), "0.05");
        assert_eq!(minor_to_decimal(99), "0.99");
        assert_eq!(minor_to_decimal(0), "0.00");
    }

    #[test]
    fn initialize_request_serializes_v3_shape() {
        let req = FwInitializeRequest {
            tx_ref: "fw-9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20".to_string(),
            amount: minor_to_decimal(125_000),
            currency: "NGN".to_string(),
            redirect_url: Some("https://example.com/cb".to_string()),
            customer: FwCustomer {
                email: "a@b.c".to_string(),
            },
            meta: FwMeta {
                tenant_id: "t1".to_string(),
                booking_id: None,
            },
        };
        let v = serde_json::to_value(&req).unwrap();
        assert_eq!(v["tx_ref"], "fw-9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20");
        assert_eq!(v["amount"], "1250.00");
        assert_eq!(v["currency"], "NGN");
        assert_eq!(v["customer"]["email"], "a@b.c");
        assert_eq!(v["meta"]["tenant_id"], "t1");
        // booking_id skipped when None.
        assert!(v["meta"].get("booking_id").is_none());
    }

    #[test]
    fn initialize_response_decodes_v3_shape() {
        let body = br#"{"status":"success","message":"Hosted Link","data":{"link":"https://checkout.flutterwave.com/abc"}}"#;
        let parsed: FwInitializeResponse = serde_json::from_slice(body).unwrap();
        assert_eq!(parsed.status, "success");
        assert_eq!(
            parsed.data.map(|d| d.link).as_deref(),
            Some("https://checkout.flutterwave.com/abc")
        );
        // Rejection envelope decodes too (status != "success" handled by caller).
        let rej: FwInitializeResponse =
            serde_json::from_slice(br#"{"status":"error","message":"Invalid key","data":null}"#)
                .unwrap();
        assert_eq!(rej.status, "error");
        assert!(rej.data.is_none());
    }

    #[test]
    fn webhook_payload_fields_parse() {
        // ASSUMPTION-shaped charge.completed payload.
        let body = br#"{"event":"charge.completed","data":{"id":123,"status":"successful","tx_ref":"fw-9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20","amount":1250.00,"currency":"NGN","meta":{"tenant_id":"t1"}}}"#;
        let v: serde_json::Value = serde_json::from_slice(body).unwrap();
        assert_eq!(v["event"], "charge.completed");
        let data = &v["data"];
        assert_eq!(data["status"], "successful");
        let id = parse_tx_ref(data["tx_ref"].as_str().unwrap()).unwrap();
        assert_eq!(id.to_string(), "9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20");
        assert_eq!(data["meta"]["tenant_id"], "t1");
        assert_eq!(data["amount"].as_f64().unwrap(), 1250.0);
    }

    // ------------------------------------------------------------------
    // SIM-004 handler-level regression tests: the webhook must fail closed
    // (401, no state change) on a missing/invalid verif-hash, 503 when no
    // hash is configured at all, and replays must not double-capture.
    // ------------------------------------------------------------------

    use crate::config::Config;
    use crate::consumer::UnavailableDlqSink;
    use crate::dapr::DaprOutbox;
    use crate::ledger::sim::SimLedgerClient;
    use crate::mojaloop::MojaloopAdapter;
    use std::sync::atomic::AtomicU64;
    use std::sync::Arc;

    fn test_state(secret_hash: Option<&str>) -> AppState {
        let cfg = Config {
            port: 0,
            ledger_impl: "sim".to_string(),
            tb_addresses: String::new(),
            tb_cluster_id: 0,
            kafka_brokers: String::new(),
            kafka_group_id: "test".to_string(),
            kafka_commands_topic: "opendesk.payments.commands".to_string(),
            kafka_consumer_enabled: false,
            dlq_topic: "opendesk.dlq".to_string(),
            dapr_host: "127.0.0.1".to_string(),
            dapr_http_port: 1, // unreachable; outbox is best-effort
            dapr_pubsub: "pubsub".to_string(),
            events_topic: "opendesk.payments.events".to_string(),
            mojaloop_endpoint: "http://127.0.0.1:1".to_string(),
            mojaloop_allow_sim: false,
            platform_fee_bps: 0,
        };
        AppState {
            ledger: Arc::new(SimLedgerClient::new(0)),
            outbox: DaprOutbox::new(
                "http://127.0.0.1:1".to_string(),
                "pubsub".to_string(),
                "opendesk.payments.events".to_string(),
            ),
            mojaloop: MojaloopAdapter::new("http://127.0.0.1:1".to_string()),
            flutterwave: FlutterwaveAdapter {
                http: crate::http_client(),
                base_url: "http://127.0.0.1:1".to_string(),
                secret_key: "sk_test".to_string(),
                secret_hash: secret_hash.map(|s| s.to_string()),
                redirect_url: None,
            },
            config: Arc::new(cfg),
            dlq: Arc::new(UnavailableDlqSink),
            events_published: Arc::new(AtomicU64::new(0)),
            events_failed: Arc::new(AtomicU64::new(0)),
            commands_dead_lettered: Arc::new(AtomicU64::new(0)),
        }
    }

    fn webhook_body(hold_id: Uuid) -> axum::body::Bytes {
        serde_json::to_vec(&serde_json::json!({
            "event": "charge.completed",
            "data": {
                "id": 123,
                "status": "successful",
                "tx_ref": make_tx_ref(hold_id),
                "amount": 12.00,
                "currency": "NGN",
                "meta": {"tenant_id": "t-1"}
            }
        }))
        .unwrap()
        .into()
    }

    async fn seed_hold(st: &AppState, hold_id: Uuid, amount_cents: u64) {
        st.ledger
            .hold_deposit("t-1", hold_id, amount_cents)
            .await
            .expect("seed hold");
    }

    #[tokio::test]
    async fn webhook_missing_verif_hash_is_401_and_no_state_change() {
        let st = test_state(Some("s3cret"));
        let hold_id = Uuid::new_v4();
        seed_hold(&st, hold_id, 1_200).await;
        let resp = webhook(
            State(st.clone()),
            HeaderMap::new(),
            webhook_body(hold_id),
        )
        .await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
        // Hold must still be pending (no capture applied).
        let bal = st.ledger.balance("t-1").await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account.ends_with(":revenue"))
            .map(|a| a.posted_net)
            .unwrap_or(0);
        assert_eq!(revenue, 0, "no capture may be applied without a valid signature");
    }

    #[tokio::test]
    async fn webhook_invalid_verif_hash_is_401_and_no_state_change() {
        let st = test_state(Some("s3cret"));
        let hold_id = Uuid::new_v4();
        seed_hold(&st, hold_id, 1_200).await;
        let mut headers = HeaderMap::new();
        headers.insert("verif-hash", "wrong-hash".parse().unwrap());
        let resp = webhook(State(st.clone()), headers, webhook_body(hold_id)).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
        let bal = st.ledger.balance("t-1").await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account.ends_with(":revenue"))
            .map(|a| a.posted_net)
            .unwrap_or(0);
        assert_eq!(revenue, 0, "invalid signature must not move money");
    }

    #[tokio::test]
    async fn webhook_without_configured_hash_is_503_fail_closed() {
        let st = test_state(None);
        let hold_id = Uuid::new_v4();
        seed_hold(&st, hold_id, 1_200).await;
        let mut headers = HeaderMap::new();
        headers.insert("verif-hash", "anything".parse().unwrap());
        let resp = webhook(State(st), headers, webhook_body(hold_id)).await;
        assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
    }

    #[tokio::test]
    async fn webhook_valid_hash_captures_once_and_replay_is_idempotent() {
        let st = test_state(Some("s3cret"));
        let hold_id = Uuid::new_v4();
        seed_hold(&st, hold_id, 1_200).await;
        let mut headers = HeaderMap::new();
        headers.insert("verif-hash", "s3cret".parse().unwrap());

        let resp = webhook(State(st.clone()), headers.clone(), webhook_body(hold_id)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        // Exact duplicate delivery: must not double-capture.
        let resp = webhook(State(st.clone()), headers, webhook_body(hold_id)).await;
        assert_eq!(resp.status(), StatusCode::OK);

        let bal = st.ledger.balance("t-1").await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account.ends_with(":revenue"))
            .map(|a| a.posted_net)
            .unwrap_or(0);
        assert_eq!(revenue, 1_200, "duplicate webhook must not double-apply");
    }
}
