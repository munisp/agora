//! SPEC-W100 C1/C2 integration tests: DFSP-bound payout execution —
//! innovation #11 phase 2 (marketplace-bound rail dispatch + durable
//! provenance + K20 approve continuity).
//!
//! Idiom: route_smoke.rs `#[path]`-mirror (the crate is binary-only) — the
//! REAL `routes.rs` route table boots behind a real TCP listener, backed by
//! the sim ledger + in-memory stores, with PAYOUT_DFSPS pointed at IN-TEST
//! axum FSPIOP execution stubs (/quotes + /transfers COMMITTED, per-stub
//! request counters). No external rail is ever contacted.
//!
//! Coverage (SPEC-W100 §Verification gate 2): chosen-DFSP execution with
//! the other DFSP seeing ZERO requests; unknown-DFSP 422 with NO ledger
//! reservation; legacy unbound payout unchanged (null provenance); bound
//! DFSP down -> failed + hold voided; K20 park -> approve executes on the
//! RECORDED DFSP; approve after the DFSP left the config -> honest 502
//! (funds stay reserved, never silently rerouted); reconciliation in-flight
//! exposes the dfsp_fsp_id binding.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use serde::Serialize;
use tracing::warn;

#[path = "../src/auth.rs"]
mod auth;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/consumer.rs"]
mod consumer;
#[path = "../src/dapr.rs"]
mod dapr;
#[path = "../src/event_failures.rs"]
mod event_failures;
#[path = "../src/events.rs"]
mod events;
#[path = "../src/flutterwave.rs"]
mod flutterwave;
#[path = "../src/ledger/mod.rs"]
mod ledger;
#[path = "../src/marketplace.rs"]
mod marketplace;
#[path = "../src/mojaloop.rs"]
mod mojaloop;
#[path = "../src/payouts.rs"]
mod payouts;
#[path = "../src/registry.rs"]
mod registry;
#[path = "../src/routes.rs"]
mod routes;
#[path = "../src/tenant.rs"]
mod tenant;
#[path = "../src/transfers.rs"]
mod transfers;


// Traits in scope for store/ledger/registry seeding assertions.
use ledger::LedgerClient as _;
use payouts::PayoutAttemptStore as _;
use registry::Registry as _;

/// Verbatim mirror of `src/main.rs::http_client` (referenced as
/// `crate::http_client` by mojaloop.rs / flutterwave.rs).
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

/// Verbatim mirror of `src/main.rs::AppState` (the router's state type).
#[derive(Clone)]
pub struct AppState {
    pub ledger: Arc<dyn ledger::LedgerClient>,
    pub outbox: dapr::DaprOutbox,
    pub mojaloop: mojaloop::MojaloopAdapter,
    pub marketplace: marketplace::Marketplace,
    pub flutterwave: flutterwave::FlutterwaveAdapter,
    pub config: Arc<config::Config>,
    pub dlq: Arc<dyn consumer::DlqSink>,
    pub auth: auth::AuthConfig,
    pub payout_attempts: Arc<dyn payouts::PayoutAttemptStore>,
    pub registry: Arc<dyn registry::Registry>,
    pub transfer_attempts: Arc<dyn transfers::TransferAttemptStore>,
    pub event_failures: Arc<dyn event_failures::EventFailureStore>,
    pub ensured_tenants: Arc<std::sync::Mutex<std::collections::HashSet<String>>>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
    pub commands_dead_lettered: Arc<AtomicU64>,
    pub commands_processed: Arc<AtomicU64>,
    pub payouts_attempted: Arc<AtomicU64>,
    pub payouts_committed: Arc<AtomicU64>,
    pub payouts_failed: Arc<AtomicU64>,
    pub payouts_unknown: Arc<AtomicU64>,
}

/// Verbatim mirror of `src/main.rs::impl AppState` (called by the handlers).
impl AppState {
    /// Mirror of `src/main.rs::ensure_accounts` (SPEC-W46 R3 per-process
    /// account-ensure cache).
    pub async fn ensure_accounts(&self, tenant_id: &str) -> Result<(), ledger::LedgerError> {
        {
            let ensured = self
                .ensured_tenants
                .lock()
                .unwrap_or_else(|p| p.into_inner());
            if ensured.contains(tenant_id) {
                return Ok(());
            }
        }
        self.ledger.create_accounts(tenant_id).await?;
        self.ensured_tenants
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .insert(tenant_id.to_string());
        Ok(())
    }

    /// Mirror of `src/main.rs::publish_event` (SPEC-W46 R5 fire-and-forget
    /// spawn after the durable write; the event is fully built first).
    pub async fn publish_event<T: Serialize + Send + Sync + 'static>(
        &self,
        type_name: &str,
        subject: &str,
        tenant_id: &str,
        data: T,
    ) {
        let event = events::CloudEvent::new(
            "payments-service",
            &format!("com.opendesk.payments.{type_name}"),
            subject,
            tenant_id,
            data,
        );
        let outbox = self.outbox.clone();
        let events_published = self.events_published.clone();
        let events_failed = self.events_failed.clone();
        tokio::spawn(async move {
            match outbox.publish(&event).await {
                Ok(()) => {
                    events_published.fetch_add(1, Ordering::Relaxed);
                }
                Err(e) => {
                    events_failed.fetch_add(1, Ordering::Relaxed);
                    warn!(
                        error = %e,
                        type_ = %event.type_,
                        "dapr pubsub publish failed (best-effort outbox)"
                    );
                }
            }
        });
    }
    /// Mirror of `src/main.rs::publish_event_reliable` (SPEC-W49 A1:
    /// retry-with-backoff + durable payment_event_failures record).
    pub async fn publish_event_reliable<T: Serialize + Send + Sync + 'static>(
        &self,
        type_name: &str,
        tx_ref: &str,
        tenant_id: &str,
        data: T,
    ) {
        let event = events::CloudEvent::new(
            "payments-service",
            &format!("com.opendesk.payments.{type_name}"),
            tx_ref,
            tenant_id,
            data,
        );
        let outbox = self.outbox.clone();
        let failures = self.event_failures.clone();
        let events_published = self.events_published.clone();
        let events_failed = self.events_failed.clone();
        let tx_ref = tx_ref.to_string();
        let tenant_id = tenant_id.to_string();
        tokio::spawn(async move {
            let mut last_err: Option<reqwest::Error> = None;
            for attempt in 1..=event_failures::PUBLISH_ATTEMPTS {
                if attempt > 1 {
                    let backoff = std::time::Duration::from_millis(
                        event_failures::RETRY_BACKOFF_MS[(attempt - 2) as usize],
                    );
                    tokio::time::sleep(backoff).await;
                }
                match outbox.publish(&event).await {
                    Ok(()) => {
                        events_published.fetch_add(1, Ordering::Relaxed);
                        return;
                    }
                    Err(e) => {
                        warn!(
                            attempt,
                            error = %e,
                            type_ = %event.type_,
                            tx_ref = %tx_ref,
                            "dapr pubsub publish failed; retrying"
                        );
                        last_err = Some(e);
                    }
                }
            }
            events_failed.fetch_add(1, Ordering::Relaxed);
            let err_text = last_err
                .map(|e| e.to_string())
                .unwrap_or_else(|| "publish failed".to_string());
            let failure = event_failures::PaymentEventFailure {
                id: uuid::Uuid::new_v4(),
                tx_ref: tx_ref.clone(),
                tenant_id,
                payload: serde_json::to_value(&event)
                    .unwrap_or_else(|_| serde_json::json!({})),
                error: err_text.clone(),
                created_at: chrono::Utc::now(),
            };
            if let Err(e) = failures.record(&failure).await {
                tracing::error!(
                    error = %e,
                    publish_error = %err_text,
                    tx_ref = %tx_ref,
                    "payment_event_failures record failed; capture event is ONLY in this log line"
                );
            } else {
                tracing::error!(
                    tx_ref = %tx_ref,
                    publish_error = %err_text,
                    "capture event publish exhausted retries; recorded to payment_event_failures \
                     (replay via POST /internal/events/replay)"
                );
            }
        });
    }
}

// ---------------------------------------------------------------------------
// Test scaffolding
// ---------------------------------------------------------------------------

/// In-test FSPIOP EXECUTION stub DFSP (axum): /quotes echoes the requested
/// terms, /transfers answers COMMITTED. Per-stub request counters + the
/// FSPIOP-Destination headers seen, so a test can prove WHICH DFSP executed
/// (and that every other DFSP saw zero requests).
#[derive(Clone, Default)]
struct ExecStub {
    quotes: Arc<AtomicU64>,
    transfers: Arc<AtomicU64>,
    dests: Arc<std::sync::Mutex<Vec<String>>>,
}

async fn exec_quotes(
    axum::extract::State(st): axum::extract::State<ExecStub>,
    headers: axum::http::HeaderMap,
    axum::Json(body): axum::Json<serde_json::Value>,
) -> axum::Json<serde_json::Value> {
    st.quotes.fetch_add(1, Ordering::Relaxed);
    if let Some(d) = headers.get("FSPIOP-Destination").and_then(|v| v.to_str().ok()) {
        st.dests.lock().unwrap().push(d.to_string());
    }
    let amount = body["amount"]["amount"].as_str().unwrap_or("0.00").to_string();
    let currency = body["amount"]["currency"].as_str().unwrap_or("NGN").to_string();
    axum::Json(serde_json::json!({
        "transferAmount": {"currency": currency, "amount": amount},
        "expiration": "2099-01-01T00:00:00.000Z",
        "ilpPacket": "AYICgQAAAAAAAAPoFGF0ZXN0",
        "condition": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
        "payeeFspFee": {"currency": currency, "amount": "0.00"},
        "payeeFspCommission": {"currency": currency, "amount": "0.00"},
    }))
}

async fn exec_transfers(
    axum::extract::State(st): axum::extract::State<ExecStub>,
    headers: axum::http::HeaderMap,
    axum::Json(_body): axum::Json<serde_json::Value>,
) -> axum::Json<serde_json::Value> {
    st.transfers.fetch_add(1, Ordering::Relaxed);
    if let Some(d) = headers.get("FSPIOP-Destination").and_then(|v| v.to_str().ok()) {
        st.dests.lock().unwrap().push(d.to_string());
    }
    axum::Json(serde_json::json!({
        "transferState": "COMMITTED",
        "completedTimestamp": "2026-11-20T00:00:00Z",
    }))
}

async fn spawn_exec(st: &ExecStub) -> String {
    let app = axum::Router::new()
        .route("/quotes", axum::routing::post(exec_quotes))
        .route("/transfers", axum::routing::post(exec_transfers))
        .with_state(st.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    format!("http://{addr}")
}

struct Handles {
    base: String,
    registry: Arc<registry::MemRegistry>,
    payout_attempts: Arc<payouts::MemPayoutAttemptStore>,
    ledger: Arc<ledger::sim::SimLedgerClient>,
}

fn test_config(payout_approval_threshold_cents: u64) -> config::Config {
    config::Config {
        port: 0,
        ledger_impl: "sim".to_string(),
        tb_addresses: String::new(),
        tb_cluster_id: 0,
        kafka_brokers: "127.0.0.1:1".to_string(),
        kafka_group_id: "quote-marketplace".to_string(),
        kafka_commands_topic: "opendesk.payments.commands".to_string(),
        kafka_consumer_enabled: false,
        dlq_topic: "opendesk.dlq".to_string(),
        dapr_host: "127.0.0.1".to_string(),
        dapr_http_port: 1, // unreachable; the outbox is best-effort
        dapr_pubsub: "pubsub".to_string(),
        events_topic: "opendesk.payments.events".to_string(),
        mojaloop_endpoint: "http://127.0.0.1:1".to_string(),
        mojaloop_allow_sim: false,
        platform_fee_bps: 0,
        internal_token: None,
        trust_direct_tenant: true,
        database_url: None,
        internal_database_url: None,
        payout_reconciler_interval_secs: 30,
        money_roles: vec!["owner".to_string(), "admin".to_string()],
        payout_approval_threshold_cents,
        payout_dfsps: Vec::new(),
        payout_quote_timeout_ms: 4000,
    }
}

/// Boot the real router with the given DFSPs and auth posture.
async fn spawn_full(
    dfsps: Vec<marketplace::DfspConfig>,
    mojaloop_endpoint: &str,
    payout_approval_threshold_cents: u64,
    internal_token: Option<&str>,
    trust_direct: bool,
) -> Handles {
    let registry = Arc::new(registry::MemRegistry::default());
    let attempts = Arc::new(payouts::MemPayoutAttemptStore::default());
    let ledger = Arc::new(ledger::sim::SimLedgerClient::new(0));
    let state = AppState {
        ledger: ledger.clone(),
        outbox: dapr::DaprOutbox::new(
            "http://127.0.0.1:1".to_string(),
            "pubsub".to_string(),
            "opendesk.payments.events".to_string(),
        ),
        mojaloop: mojaloop::MojaloopAdapter::new(mojaloop_endpoint.to_string()),
        marketplace: marketplace::Marketplace::new(dfsps, 4000),
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(test_config(payout_approval_threshold_cents)),
        dlq: Arc::new(consumer::UnavailableDlqSink),
        auth: auth::AuthConfig::new(
            internal_token.map(|s| s.to_string()),
            trust_direct,
            vec!["owner".to_string(), "admin".to_string()],
        ),
        payout_attempts: attempts.clone(),
        registry: registry.clone(),
        transfer_attempts: Arc::new(transfers::MemTransferAttemptStore::default()),
        event_failures: Arc::new(event_failures::MemEventFailureStore::default()),
        ensured_tenants: Arc::new(std::sync::Mutex::new(std::collections::HashSet::new())),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        commands_dead_lettered: Arc::new(AtomicU64::new(0)),
        commands_processed: Arc::new(AtomicU64::new(0)),
        payouts_attempted: Arc::new(AtomicU64::new(0)),
        payouts_committed: Arc::new(AtomicU64::new(0)),
        payouts_failed: Arc::new(AtomicU64::new(0)),
        payouts_unknown: Arc::new(AtomicU64::new(0)),
    };
    let app = routes::router(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    Handles {
        base: format!("http://{addr}"),
        registry,
        payout_attempts: attempts,
        ledger,
    }
}

/// Register a beneficiary owned by `tenant`; returns its id.
async fn add_beneficiary(h: &Handles, tenant: &str) -> uuid::Uuid {
    let id = uuid::Uuid::new_v4();
    h.registry
        .create_beneficiary(&registry::Beneficiary {
            id,
            tenant_id: tenant.to_string(),
            label: "Main till".to_string(),
            party_id_info: serde_json::json!({
                "partyIdType": "MSISDN",
                "partyIdentifier": "+2348030000000"
            }),
            created_by: "test".to_string(),
            created_at: chrono::Utc::now(),
            disabled_at: None,
        })
        .await
        .unwrap();
    id
}
// ---------------------------------------------------------------------------
// SPEC-W100 tests
// ---------------------------------------------------------------------------

/// Fund the tenant revenue account (hold + capture) so payout_hold has
/// credits to reserve against (SimLedgerClient enforces debits <= credits).
async fn fund_revenue(h: &Handles, tenant: &str, cents: u64) {
    h.ledger.create_accounts(tenant).await.unwrap();
    let dep = uuid::Uuid::new_v4();
    h.ledger.hold_deposit(tenant, dep, cents).await.unwrap();
    h.ledger
        .capture(tenant, dep, uuid::Uuid::new_v4(), None)
        .await
        .unwrap();
}

async fn post_payout(
    base: &str,
    tenant: &str,
    cents: u64,
    beneficiary: uuid::Uuid,
    key: &str,
    dfsp: Option<&str>,
) -> reqwest::Response {
    let mut body = serde_json::json!({
        "tenant_id": tenant,
        "amount_cents": cents,
        "currency": "NGN",
        "beneficiary_id": beneficiary,
        "idempotency_key": key,
    });
    if let Some(d) = dfsp {
        body["dfsp_fsp_id"] = serde_json::json!(d);
    }
    http_client()
        .post(format!("{base}/v1/payouts"))
        .json(&body)
        .send()
        .await
        .unwrap()
}

/// C1: a bound payout executes on its chosen DFSP's endpoint with
/// FSPIOP-Destination = fsp id; the other configured DFSP sees ZERO
/// requests; the binding is recorded durably on the attempt row.
#[tokio::test]
async fn chosen_dfsp_executes_on_its_endpoint_only() {
    let cheap = ExecStub::default();
    let pricey = ExecStub::default();
    let cheap_url = spawn_exec(&cheap).await;
    let pricey_url = spawn_exec(&pricey).await;
    let h = spawn_full(
        vec![
            marketplace::DfspConfig { fsp_id: "dfsp-z-cheap".to_string(), endpoint: cheap_url },
            marketplace::DfspConfig { fsp_id: "dfsp-a-pricey".to_string(), endpoint: pricey_url },
        ],
        "http://127.0.0.1:1",
        0,
        None,
        true,
    )
    .await;
    fund_revenue(&h, "t1", 10_000).await;
    let ben = add_beneficiary(&h, "t1").await;

    let resp = post_payout(&h.base, "t1", 1_500, ben, "w100-chosen", Some("dfsp-z-cheap")).await;
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::CREATED, "{body}");
    assert_eq!(body["mojaloop"]["state"], "COMMITTED");

    // The chosen DFSP quoted + transferred; the other saw nothing at all.
    assert_eq!(cheap.quotes.load(Ordering::Relaxed), 1);
    assert_eq!(cheap.transfers.load(Ordering::Relaxed), 1);
    assert_eq!(pricey.quotes.load(Ordering::Relaxed), 0, "unselected DFSP must see ZERO requests");
    assert_eq!(pricey.transfers.load(Ordering::Relaxed), 0);
    assert!(cheap
        .dests
        .lock()
        .unwrap()
        .iter()
        .all(|d| d == "dfsp-z-cheap"));

    // C2: durable provenance on the attempt row.
    let pid = body["payout_id"].as_str().unwrap();
    let attempt = h
        .payout_attempts
        .get_scoped(pid, "t1")
        .await
        .unwrap()
        .expect("attempt row recorded");
    assert_eq!(attempt.dfsp_fsp_id.as_deref(), Some("dfsp-z-cheap"));
    assert_eq!(attempt.state, payouts::AttemptState::Committed);
}

/// C1: an unknown dfsp_fsp_id is rejected 422 BEFORE any ledger hold — no
/// reservation leaks, no attempt row is recorded.
#[tokio::test]
async fn unknown_dfsp_422_leaks_no_reservation() {
    let good = ExecStub::default();
    let good_url = spawn_exec(&good).await;
    let h = spawn_full(
        vec![marketplace::DfspConfig { fsp_id: "dfsp-good".to_string(), endpoint: good_url }],
        "http://127.0.0.1:1",
        0,
        None,
        true,
    )
    .await;
    fund_revenue(&h, "t1", 10_000).await;
    let ben = add_beneficiary(&h, "t1").await;

    let resp = post_payout(&h.base, "t1", 1_500, ben, "w100-unknown", Some("dfsp-nope")).await;
    let status = resp.status();
    let body = resp.text().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::UNPROCESSABLE_ENTITY, "{body}");
    assert!(body.contains("unknown dfsp_fsp_id"), "{body}");

    // No rail contact, no attempt row, no reservation.
    assert_eq!(good.quotes.load(Ordering::Relaxed), 0);
    assert_eq!(good.transfers.load(Ordering::Relaxed), 0);
    assert!(
        h.payout_attempts.summary_scoped("t1").await.unwrap().is_empty(),
        "rejected payout must not record an attempt"
    );
    let bal = h.ledger.balance("t1").await.unwrap();
    let pending: i128 = bal
        .accounts
        .iter()
        .filter(|a| a.account.starts_with("tenant:t1:"))
        .map(|a| a.pending_net)
        .sum();
    assert_eq!(pending, 0, "422 must leak NO ledger reservation");
}

/// C1: an unbound payout (no dfsp_fsp_id) takes the legacy
/// MOJALOOP_ENDPOINT rail with dest "payeefsp", unchanged, and records null
/// provenance.
#[tokio::test]
async fn legacy_unbound_payout_committed_null_provenance() {
    let legacy = ExecStub::default();
    let legacy_url = spawn_exec(&legacy).await;
    let other = ExecStub::default();
    let other_url = spawn_exec(&other).await;
    let h = spawn_full(
        vec![marketplace::DfspConfig { fsp_id: "dfsp-other".to_string(), endpoint: other_url }],
        &legacy_url,
        0,
        None,
        true,
    )
    .await;
    fund_revenue(&h, "t1", 10_000).await;
    let ben = add_beneficiary(&h, "t1").await;

    let resp = post_payout(&h.base, "t1", 1_500, ben, "w100-legacy", None).await;
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::CREATED, "{body}");
    assert_eq!(body["mojaloop"]["state"], "COMMITTED");

    assert_eq!(legacy.transfers.load(Ordering::Relaxed), 1);
    assert_eq!(other.quotes.load(Ordering::Relaxed), 0);
    assert!(legacy
        .dests
        .lock()
        .unwrap()
        .iter()
        .all(|d| d == "payeefsp"));

    let pid = body["payout_id"].as_str().unwrap();
    let attempt = h
        .payout_attempts
        .get_scoped(pid, "t1")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(attempt.dfsp_fsp_id, None, "legacy rail records null provenance");
}

/// C1: chosen DFSP down -> honest failure, the pending hold is voided (no
/// funds stranded), the attempt is recorded Failed with the binding kept.
#[tokio::test]
async fn bound_dfsp_down_fails_and_voids_hold() {
    let h = spawn_full(
        vec![marketplace::DfspConfig {
            fsp_id: "dfsp-dead".to_string(),
            endpoint: "http://127.0.0.1:1".to_string(), // nothing listens
        }],
        "http://127.0.0.1:1",
        0,
        None,
        true,
    )
    .await;
    fund_revenue(&h, "t1", 10_000).await;
    let ben = add_beneficiary(&h, "t1").await;

    let resp = post_payout(&h.base, "t1", 1_500, ben, "w100-down", Some("dfsp-dead")).await;
    let status = resp.status();
    let body = resp.text().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::BAD_GATEWAY, "{body}");
    assert!(body.contains("payout rail rejected"), "{body}");

    let summary = h.payout_attempts.summary_scoped("t1").await.unwrap();
    assert_eq!(summary.len(), 1);
    assert_eq!(summary[0].state, payouts::AttemptState::Failed);
    let bal = h.ledger.balance("t1").await.unwrap();
    let pending: i128 = bal
        .accounts
        .iter()
        .filter(|a| a.account.starts_with("tenant:t1:"))
        .map(|a| a.pending_net)
        .sum();
    assert_eq!(pending, 0, "failed payout must void the reservation");
}

/// C2/K20: a bound payout parked above the approval threshold executes on
/// the RECORDED DFSP when approved (the rail is not called at park time).
#[tokio::test]
async fn k20_approve_executes_on_recorded_dfsp() {
    let chosen = ExecStub::default();
    let chosen_url = spawn_exec(&chosen).await;
    let other = ExecStub::default();
    let other_url = spawn_exec(&other).await;
    let h = spawn_full(
        vec![
            marketplace::DfspConfig { fsp_id: "dfsp-z-cheap".to_string(), endpoint: chosen_url },
            marketplace::DfspConfig { fsp_id: "dfsp-a-pricey".to_string(), endpoint: other_url },
        ],
        "http://127.0.0.1:1",
        1_000, // threshold: 1_500 > 1_000 parks the payout
        None,
        true,
    )
    .await;
    fund_revenue(&h, "t1", 10_000).await;
    let ben = add_beneficiary(&h, "t1").await;

    let resp = post_payout(&h.base, "t1", 1_500, ben, "w100-k20", Some("dfsp-z-cheap")).await;
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::ACCEPTED, "{body}");
    assert_eq!(body["status"], "pending_approval");
    assert_eq!(chosen.quotes.load(Ordering::Relaxed), 0, "parked: rail NOT called");

    let pid = body["payout_id"].as_str().unwrap().to_string();
    let resp = http_client()
        .post(format!("{}/v1/payouts/{pid}/approve", h.base))
        .header("x-user-roles", "owner")
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::CREATED, "{body}");
    assert_eq!(body["mojaloop"]["state"], "COMMITTED");

    // Executed on the RECORDED DFSP only.
    assert_eq!(chosen.quotes.load(Ordering::Relaxed), 1);
    assert_eq!(chosen.transfers.load(Ordering::Relaxed), 1);
    assert_eq!(other.quotes.load(Ordering::Relaxed), 0);
    assert_eq!(other.transfers.load(Ordering::Relaxed), 0);

    let attempt = h
        .payout_attempts
        .get_scoped(&pid, "t1")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(attempt.state, payouts::AttemptState::Committed);
    assert_eq!(attempt.dfsp_fsp_id.as_deref(), Some("dfsp-z-cheap"));
}

/// C2/K20: if the recorded DFSP has left PAYOUT_DFSPS by approval time, the
/// approve fails HONESTLY (502), the payout stays parked with its funds
/// reserved — never silently rerouted to the legacy rail.
#[tokio::test]
async fn k20_approve_after_dfsp_removed_is_honest_502() {
    let chosen = ExecStub::default();
    let chosen_url = spawn_exec(&chosen).await;
    let legacy = ExecStub::default();
    let legacy_url = spawn_exec(&legacy).await;
    let h = spawn_full(
        vec![marketplace::DfspConfig { fsp_id: "dfsp-z-cheap".to_string(), endpoint: chosen_url }],
        &legacy_url,
        1_000,
        None,
        true,
    )
    .await;
    fund_revenue(&h, "t1", 10_000).await;
    let ben = add_beneficiary(&h, "t1").await;

    let resp = post_payout(&h.base, "t1", 1_500, ben, "w100-k20-removed", Some("dfsp-z-cheap")).await;
    assert_eq!(resp.status(), reqwest::StatusCode::ACCEPTED);
    let pid = resp.json::<serde_json::Value>().await.unwrap()["payout_id"]
        .as_str()
        .unwrap()
        .to_string();

    // Rebuild the service over the SAME stores with the DFSP removed from
    // the config (PAYOUT_DFSPS edited between park and approve).
    let state = AppState {
        ledger: h.ledger.clone(),
        outbox: dapr::DaprOutbox::new(
            "http://127.0.0.1:1".to_string(),
            "pubsub".to_string(),
            "opendesk.payments.events".to_string(),
        ),
        mojaloop: mojaloop::MojaloopAdapter::new(legacy_url),
        marketplace: marketplace::Marketplace::new(Vec::new(), 4000), // dfsp removed
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(test_config(1_000)),
        dlq: Arc::new(consumer::UnavailableDlqSink),
        auth: auth::AuthConfig::new(None, true, vec!["owner".to_string(), "admin".to_string()]),
        payout_attempts: h.payout_attempts.clone(),
        registry: h.registry.clone(),
        transfer_attempts: Arc::new(transfers::MemTransferAttemptStore::default()),
        event_failures: Arc::new(event_failures::MemEventFailureStore::default()),
        ensured_tenants: Arc::new(std::sync::Mutex::new(std::collections::HashSet::new())),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        commands_dead_lettered: Arc::new(AtomicU64::new(0)),
        commands_processed: Arc::new(AtomicU64::new(0)),
        payouts_attempted: Arc::new(AtomicU64::new(0)),
        payouts_committed: Arc::new(AtomicU64::new(0)),
        payouts_failed: Arc::new(AtomicU64::new(0)),
        payouts_unknown: Arc::new(AtomicU64::new(0)),
    };
    let app = routes::router(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base2 = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    let resp = http_client()
        .post(format!("{base2}/v1/payouts/{pid}/approve"))
        .header("x-user-roles", "owner")
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::BAD_GATEWAY, "{body}");
    assert!(body.contains("no longer in PAYOUT_DFSPS"), "{body}");

    // Never rerouted: neither the removed DFSP nor the legacy rail was hit.
    assert_eq!(chosen.quotes.load(Ordering::Relaxed), 0);
    assert_eq!(legacy.quotes.load(Ordering::Relaxed), 0);
    assert_eq!(legacy.transfers.load(Ordering::Relaxed), 0);

    // The payout stays parked with its reservation intact.
    let attempt = h
        .payout_attempts
        .get_scoped(&pid, "t1")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(attempt.state, payouts::AttemptState::PendingApproval);
    let bal = h.ledger.balance("t1").await.unwrap();
    let pending: i128 = bal
        .accounts
        .iter()
        .filter(|a| a.account.starts_with("tenant:t1:"))
        .map(|a| a.pending_net)
        .sum();
    assert_ne!(pending, 0, "funds remain RESERVED — never silently moved");
}

/// C2: the reconciliation report's in-flight section exposes the recorded
/// dfsp_fsp_id binding.
#[tokio::test]
async fn recon_in_flight_exposes_dfsp_binding() {
    let chosen = ExecStub::default();
    let chosen_url = spawn_exec(&chosen).await;
    let h = spawn_full(
        vec![marketplace::DfspConfig { fsp_id: "dfsp-z-cheap".to_string(), endpoint: chosen_url }],
        "http://127.0.0.1:1",
        1_000,
        None,
        true,
    )
    .await;
    fund_revenue(&h, "t1", 10_000).await;
    let ben = add_beneficiary(&h, "t1").await;

    let resp = post_payout(&h.base, "t1", 1_500, ben, "w100-recon", Some("dfsp-z-cheap")).await;
    assert_eq!(resp.status(), reqwest::StatusCode::ACCEPTED, "parked in-flight");
    // An unbound in-flight payout parks alongside (null binding).
    let resp = post_payout(&h.base, "t1", 1_200, ben, "w100-recon-legacy", None).await;
    assert_eq!(resp.status(), reqwest::StatusCode::ACCEPTED);

    let resp = http_client()
        .get(format!("{}/v1/reconciliation/report?tenant_id=t1", h.base))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(status, reqwest::StatusCode::OK, "{body}");
    let in_flight = body["payout_attempts"]["in_flight"].as_array().unwrap();
    assert_eq!(in_flight.len(), 2, "{body}");
    let bound = in_flight
        .iter()
        .find(|a| a["amount_cents"] == 1_500)
        .expect("bound attempt in flight");
    assert_eq!(bound["dfsp_fsp_id"], "dfsp-z-cheap");
    let unbound = in_flight
        .iter()
        .find(|a| a["amount_cents"] == 1_200)
        .expect("legacy attempt in flight");
    assert_eq!(unbound["dfsp_fsp_id"], serde_json::Value::Null);
}
