//! SPEC-W99 C1/C2 integration tests: DFSP quote marketplace + ledger
//! reconciliation report (innovation #11 MVP).
//!
//! Idiom: route_smoke.rs `#[path]`-mirror (the crate is binary-only) — the
//! REAL `routes.rs` route table boots behind a real TCP listener, backed by
//! the sim ledger + in-memory stores, with the marketplace pointed at
//! IN-TEST axum FSPIOP stub DFSPs (flutterwave.rs stub-server idiom). No
//! external rail is ever contacted.
//!
//! Coverage (SPEC-W99 §Verification gate 2): ranked comparison, partial
//! failure, all-down 502, zero-DFSP 503, currency/amount 400s, uniform-422
//! beneficiary oracle guard, echo-mismatch -> errors[], unparseable-fee ->
//! errors[], per-DFSP timeout, auth posture (401/403/dev-escape), recon
//! rollups + in-flight + checks + cross-tenant emptiness, smoke (no
//! match-layer 404).

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

/// In-test FSPIOP stub DFSP (axum). Canned quote terms per stub instance.
#[derive(Clone)]
struct StubDfsp {
    fee: Option<&'static str>,
    commission: Option<&'static str>,
    echo_wrong_amount: bool,
    bad_fee: bool,
    delay_ms: u64,
}

async fn stub_quotes(
    axum::extract::State(cfg): axum::extract::State<StubDfsp>,
    axum::Json(body): axum::Json<serde_json::Value>,
) -> axum::Json<serde_json::Value> {
    if cfg.delay_ms > 0 {
        tokio::time::sleep(std::time::Duration::from_millis(cfg.delay_ms)).await;
    }
    let amount = body["amount"]["amount"].as_str().unwrap_or("0.00").to_string();
    let currency = body["amount"]["currency"].as_str().unwrap_or("NGN").to_string();
    let echoed = if cfg.echo_wrong_amount {
        "9999999.99".to_string()
    } else {
        amount
    };
    let mut resp = serde_json::json!({
        "transferAmount": {"currency": currency, "amount": echoed},
        "expiration": "2099-01-01T00:00:00.000Z",
        "ilpPacket": "AYICgQAAAAAAAAPoFGF0ZXN0",
        "condition": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    });
    if cfg.bad_fee {
        resp["payeeFspFee"] = serde_json::json!({"currency": "NGN", "amount": "lots"});
        resp["payeeFspCommission"] = serde_json::json!({"currency": "NGN", "amount": "1.00"});
    } else if let (Some(f), Some(c)) = (cfg.fee, cfg.commission) {
        resp["payeeFspFee"] = serde_json::json!({"currency": currency, "amount": f});
        resp["payeeFspCommission"] = serde_json::json!({"currency": currency, "amount": c});
    }
    axum::Json(resp)
}

/// Boot an FSPIOP stub DFSP on an ephemeral port; returns its base URL.
async fn spawn_dfsp(cfg: StubDfsp) -> String {
    let app = axum::Router::new()
        .route("/quotes", axum::routing::post(stub_quotes))
        .with_state(cfg);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    format!("http://{addr}")
}

/// Handles into the spawned server state.
struct Handles {
    base: String,
    registry: Arc<registry::MemRegistry>,
    payout_attempts: Arc<payouts::MemPayoutAttemptStore>,
    ledger: Arc<ledger::sim::SimLedgerClient>,
}

fn test_config() -> config::Config {
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
        payout_approval_threshold_cents: 0,
        payout_dfsps: Vec::new(),
        payout_quote_timeout_ms: 4000,
    }
}

/// Boot the real router with the given DFSPs and auth posture.
async fn spawn_full(
    dfsps: Vec<marketplace::DfspConfig>,
    quote_timeout_ms: u64,
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
        mojaloop: mojaloop::MojaloopAdapter::new("http://127.0.0.1:1".to_string()),
        marketplace: marketplace::Marketplace::new(dfsps, quote_timeout_ms),
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(test_config()),
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

async fn spawn_market(dfsps: Vec<marketplace::DfspConfig>) -> Handles {
    spawn_full(dfsps, 4000, None, true).await
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

fn quote_body(tenant: &str, amount: u64, ccy: &str, beneficiary: Option<uuid::Uuid>) -> serde_json::Value {
    serde_json::json!({
        "tenant_id": tenant,
        "amount_cents": amount,
        "currency": ccy,
        "beneficiary_id": beneficiary,
    })
}

fn attempt(id: &str, tenant: &str, cents: u64, state: payouts::AttemptState, secs_ago: i64) -> payouts::PayoutAttempt {
    let ts = chrono::Utc::now() - chrono::Duration::seconds(secs_ago);
    payouts::PayoutAttempt {
        payout_id: id.to_string(),
        tenant_id: tenant.to_string(),
        amount_cents: cents,
        currency: "NGN".to_string(),
        payee: serde_json::Value::Null,
        state,
        detail: None,
        created_at: ts,
        updated_at: ts,
    }
}

// ---------------------------------------------------------------------------
// C1: quote marketplace
// ---------------------------------------------------------------------------

#[tokio::test]
async fn ranked_comparison_prefers_lower_fees() {
    let cheap = spawn_dfsp(StubDfsp {
        fee: Some("10.00"),
        commission: Some("5.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let pricey = spawn_dfsp(StubDfsp {
        fee: Some("20.00"),
        commission: Some("5.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let h = spawn_market(vec![
        marketplace::DfspConfig { fsp_id: "dfsp-z-cheap".to_string(), endpoint: cheap },
        marketplace::DfspConfig { fsp_id: "dfsp-a-pricey".to_string(), endpoint: pricey },
    ])
    .await;
    let ben = add_beneficiary(&h, "t1").await;
    let resp = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .json(&quote_body("t1", 100_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200, "{}", resp.text().await.unwrap());
    let v: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(v["errors"].as_array().unwrap().len(), 0);
    assert_eq!(v["best_fsp"], "dfsp-z-cheap");
    assert_eq!(
        v["ranked"].as_array().unwrap(),
        &vec![serde_json::json!("dfsp-z-cheap"), serde_json::json!("dfsp-a-pricey")]
    );
    let quotes = v["quotes"].as_array().unwrap();
    assert_eq!(quotes.len(), 2);
    // Quotes presented in deterministic fsp_id order (a-pricey first) —
    // deliberately NOT the rank order, so the ranking assertions above can
    // never pass by presentation-order accident.
    assert_eq!(quotes[0]["fsp_id"], "dfsp-a-pricey");
    assert_eq!(quotes[0]["receive_amount_cents"], 97_500);
    assert_eq!(quotes[1]["fsp_id"], "dfsp-z-cheap");
    assert_eq!(quotes[1]["fees_disclosed"], true);
    // 1000.00 - 10.00 - 5.00 = 985.00 -> 98500 minor units.
    assert_eq!(quotes[1]["receive_amount_cents"], 98_500);
    assert_eq!(quotes[1]["payee_fsp_fee"]["amount"], "10.00");
    assert_eq!(quotes[1]["payee_fsp_commission"]["amount"], "5.00");
    assert_eq!(v["beneficiary_id"], ben.to_string());
    assert!(v["generated_at"].as_str().unwrap().contains('T'));
}

#[tokio::test]
async fn partial_failure_is_200_with_honest_errors() {
    let good = spawn_dfsp(StubDfsp {
        fee: Some("10.00"),
        commission: Some("5.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let h = spawn_market(vec![
        marketplace::DfspConfig { fsp_id: "dfsp-dead".to_string(), endpoint: "http://127.0.0.1:1".to_string() },
        marketplace::DfspConfig { fsp_id: "dfsp-good".to_string(), endpoint: good },
    ])
    .await;
    let ben = add_beneficiary(&h, "t1").await;
    let resp = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .json(&quote_body("t1", 50_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let v: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(v["quotes"].as_array().unwrap().len(), 1);
    let errors = v["errors"].as_array().unwrap();
    assert_eq!(errors.len(), 1);
    assert_eq!(errors[0]["fsp_id"], "dfsp-dead");
    assert!(errors[0]["error"].as_str().unwrap().contains("quote request failed"));
}

#[tokio::test]
async fn all_dfsps_down_is_502() {
    let h = spawn_market(vec![
        marketplace::DfspConfig { fsp_id: "dfsp-a".to_string(), endpoint: "http://127.0.0.1:1".to_string() },
        marketplace::DfspConfig { fsp_id: "dfsp-b".to_string(), endpoint: "http://127.0.0.1:2".to_string() },
    ])
    .await;
    let ben = add_beneficiary(&h, "t1").await;
    let resp = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .json(&quote_body("t1", 50_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 502);
    let v: serde_json::Value = resp.json().await.unwrap();
    let msg = v["error"].as_str().unwrap();
    assert!(msg.contains("dfsp-a"), "{msg}");
    assert!(msg.contains("dfsp-b"), "{msg}");
}

#[tokio::test]
async fn zero_dfsps_configured_is_503() {
    let h = spawn_market(Vec::new()).await;
    let ben = add_beneficiary(&h, "t1").await;
    let resp = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .json(&quote_body("t1", 50_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 503);
    let v: serde_json::Value = resp.json().await.unwrap();
    assert!(v["error"].as_str().unwrap().contains("PAYOUT_DFSPS"));
}

#[tokio::test]
async fn currency_and_amount_validation() {
    let good = spawn_dfsp(StubDfsp {
        fee: Some("10.00"),
        commission: Some("5.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let h = spawn_market(vec![marketplace::DfspConfig {
        fsp_id: "dfsp-good".to_string(),
        endpoint: good,
    }])
    .await;
    let ben = add_beneficiary(&h, "t1").await;
    let post = |body: serde_json::Value| {
        let base = h.base.clone();
        async move {
            http_client()
                .post(format!("{base}/v1/payouts/quotes"))
                .json(&body)
                .send()
                .await
                .unwrap()
        }
    };
    // lowercase / 4-letter / digit-containing / amount 0 -> 400.
    assert_eq!(post(quote_body("t1", 100, "ngn", Some(ben))).await.status(), 400);
    assert_eq!(post(quote_body("t1", 100, "NGNX", Some(ben))).await.status(), 400);
    assert_eq!(post(quote_body("t1", 100, "N1N", Some(ben))).await.status(), 400);
    assert_eq!(post(quote_body("t1", 0, "NGN", Some(ben))).await.status(), 400);
    // Cross-border read-only quoting IS allowed (USD): execution stays NGN-only.
    let resp = post(quote_body("t1", 100_000, "USD", Some(ben))).await;
    assert_eq!(resp.status(), 200, "{}", resp.text().await.unwrap());
}

#[tokio::test]
async fn beneficiary_oracle_guard_uniform_422() {
    let good = spawn_dfsp(StubDfsp {
        fee: Some("10.00"),
        commission: Some("5.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let h = spawn_market(vec![marketplace::DfspConfig {
        fsp_id: "dfsp-good".to_string(),
        endpoint: good,
    }])
    .await;
    let ben = add_beneficiary(&h, "t1").await;
    let foreign = add_beneficiary(&h, "t2").await;
    let post = |body: serde_json::Value| {
        let base = h.base.clone();
        async move {
            http_client()
                .post(format!("{base}/v1/payouts/quotes"))
                .json(&body)
                .send()
                .await
                .unwrap()
        }
    };
    // Missing beneficiary_id, unknown id, foreign-tenant id: uniform 422.
    assert_eq!(post(quote_body("t1", 100, "NGN", None)).await.status(), 422);
    assert_eq!(
        post(quote_body("t1", 100, "NGN", Some(uuid::Uuid::new_v4()))).await.status(),
        422
    );
    assert_eq!(post(quote_body("t1", 100, "NGN", Some(foreign))).await.status(), 422);
    // Disabled beneficiary: uniform 422 too.
    h.registry.disable_beneficiary(ben, "t1").await.unwrap();
    assert_eq!(post(quote_body("t1", 100, "NGN", Some(ben))).await.status(), 422);
}

#[tokio::test]
async fn echo_mismatch_and_bad_fee_go_to_errors() {
    let mismatched = spawn_dfsp(StubDfsp {
        fee: Some("1.00"),
        commission: Some("1.00"),
        echo_wrong_amount: true,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let badfee = spawn_dfsp(StubDfsp {
        fee: None,
        commission: None,
        echo_wrong_amount: false,
        bad_fee: true,
        delay_ms: 0,
    })
    .await;
    let good = spawn_dfsp(StubDfsp {
        fee: Some("1.00"),
        commission: Some("1.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let h = spawn_market(vec![
        marketplace::DfspConfig { fsp_id: "dfsp-badfee".to_string(), endpoint: badfee },
        marketplace::DfspConfig { fsp_id: "dfsp-good".to_string(), endpoint: good },
        marketplace::DfspConfig { fsp_id: "dfsp-mismatch".to_string(), endpoint: mismatched },
    ])
    .await;
    let ben = add_beneficiary(&h, "t1").await;
    let resp = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .json(&quote_body("t1", 10_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let v: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(v["quotes"].as_array().unwrap().len(), 1);
    assert_eq!(v["quotes"][0]["fsp_id"], "dfsp-good");
    let errors = v["errors"].as_array().unwrap();
    assert_eq!(errors.len(), 2);
    assert_eq!(errors[0]["fsp_id"], "dfsp-badfee");
    assert!(errors[0]["error"].as_str().unwrap().contains("unparseable payeeFspFee"));
    assert_eq!(errors[1]["fsp_id"], "dfsp-mismatch");
    assert!(errors[1]["error"].as_str().unwrap().contains("echo mismatch"));
}

#[tokio::test]
async fn slow_dfsp_times_out_into_errors() {
    let slow = spawn_dfsp(StubDfsp {
        fee: Some("1.00"),
        commission: Some("1.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 500,
    })
    .await;
    let good = spawn_dfsp(StubDfsp {
        fee: Some("1.00"),
        commission: Some("1.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let h = spawn_full(
        vec![
            marketplace::DfspConfig { fsp_id: "dfsp-good".to_string(), endpoint: good },
            marketplace::DfspConfig { fsp_id: "dfsp-slow".to_string(), endpoint: slow },
        ],
        50, // 50ms quote timeout: the 500ms stub must time out
        None,
        true,
    )
    .await;
    let ben = add_beneficiary(&h, "t1").await;
    let resp = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .json(&quote_body("t1", 10_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let v: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(v["quotes"].as_array().unwrap().len(), 1);
    let errors = v["errors"].as_array().unwrap();
    assert_eq!(errors.len(), 1);
    assert_eq!(errors[0]["fsp_id"], "dfsp-slow");
    assert!(errors[0]["error"].as_str().unwrap().contains("timed out"));
}

#[tokio::test]
async fn auth_posture_401_403_and_internal_token() {
    let good = spawn_dfsp(StubDfsp {
        fee: Some("1.00"),
        commission: Some("1.00"),
        echo_wrong_amount: false,
        bad_fee: false,
        delay_ms: 0,
    })
    .await;
    let dfsps = || {
        vec![marketplace::DfspConfig {
            fsp_id: "dfsp-good".to_string(),
            endpoint: good.clone(),
        }]
    };
    // Internal token configured, no dev escape: no headers -> 401; the token
    // itself -> 200 (internal-token callers are money-role exempt).
    let h = spawn_full(dfsps(), 4000, Some("s3cret"), false).await;
    let ben = add_beneficiary(&h, "t1").await;
    let anon = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .json(&quote_body("t1", 10_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(anon.status(), 401);
    let authed = http_client()
        .post(format!("{}/v1/payouts/quotes", h.base))
        .header("x-internal-token", "s3cret")
        .json(&quote_body("t1", 10_000, "NGN", Some(ben)))
        .send()
        .await
        .unwrap();
    assert_eq!(authed.status(), 200, "{}", authed.text().await.unwrap());
    // Dev escape on but a non-money role presented -> 403.
    let h2 = spawn_full(dfsps(), 4000, None, true).await;
    let ben2 = add_beneficiary(&h2, "t1").await;
    let member = http_client()
        .post(format!("{}/v1/payouts/quotes", h2.base))
        .header("x-user-roles", "member")
        .json(&quote_body("t1", 10_000, "NGN", Some(ben2)))
        .send()
        .await
        .unwrap();
    assert_eq!(member.status(), 403);
}

// ---------------------------------------------------------------------------
// C2: reconciliation report
// ---------------------------------------------------------------------------

#[tokio::test]
async fn recon_report_rollups_in_flight_checks_and_scoping() {
    let h = spawn_market(Vec::new()).await;
    // t1: 2 unknown (1000 older, 2000 newer), 1 committed, 1 pending_approval.
    for a in [
        attempt("p-u1", "t1", 1000, payouts::AttemptState::Unknown, 300),
        attempt("p-u2", "t1", 2000, payouts::AttemptState::Unknown, 200),
        attempt("p-c1", "t1", 500, payouts::AttemptState::Committed, 100),
        attempt("p-p1", "t1", 700, payouts::AttemptState::PendingApproval, 50),
        // t2 noise: must never leak into t1's report.
        attempt("p-t2", "t2", 999, payouts::AttemptState::Unknown, 10),
    ] {
        h.payout_attempts.record(&a).await.unwrap();
    }
    // Ledger: fund revenue via a deposit hold + capture (the ledger
    // enforces debits<=credits on the revenue liability account), then
    // reserve exactly the in-flight sum (3700) as a payout hold.
    h.ledger.create_accounts("t1").await.unwrap();
    let dep = uuid::Uuid::new_v4();
    h.ledger.hold_deposit("t1", dep, 10_000).await.unwrap();
    h.ledger
        .capture("t1", dep, uuid::Uuid::new_v4(), None)
        .await
        .unwrap();
    h.ledger
        .payout_hold("t1", uuid::Uuid::new_v4(), 3700)
        .await
        .unwrap();

    let resp = http_client()
        .get(format!("{}/v1/reconciliation/report?tenant_id=t1", h.base))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200, "{}", resp.text().await.unwrap());
    let v: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(v["tenant_id"], "t1");
    let by_state = v["payout_attempts"]["by_state"].as_array().unwrap();
    let rollup = |s: &str| -> Option<(u64, u128)> {
        by_state.iter().find(|r| r["state"] == s).map(|r| {
            (
                r["count"].as_u64().unwrap(),
                r["amount_cents"].as_u64().unwrap() as u128,
            )
        })
    };
    assert_eq!(rollup("unknown"), Some((2, 3000)));
    assert_eq!(rollup("committed"), Some((1, 500)));
    assert_eq!(rollup("pending_approval"), Some((1, 700)));
    assert_eq!(v["payout_attempts"]["reserved_cents"], 3700);
    // In-flight: unknown + pending_approval only, oldest first.
    let in_flight = v["payout_attempts"]["in_flight"].as_array().unwrap();
    let ids: Vec<&str> = in_flight.iter().map(|r| r["payout_id"].as_str().unwrap()).collect();
    assert_eq!(ids, vec!["p-u1", "p-u2", "p-p1"]);
    // Checks: unknown warn + pending info + reserved-within-pending info.
    let checks = v["checks"].as_array().unwrap();
    let check = |id: &str| checks.iter().find(|c| c["id"] == id).cloned().unwrap();
    assert_eq!(check("unknown_attempts")["severity"], "warn");
    assert_eq!(check("pending_approvals")["severity"], "info");
    let rwp = check("reserved_within_pending");
    assert_eq!(rwp["severity"], "info");
    assert!(rwp["message"].as_str().unwrap().contains("lending"));
    // Ledger snapshot carried verbatim.
    let accounts = v["ledger"]["accounts"].as_array().unwrap();
    assert!(accounts.iter().any(|a| a["account"] == "tenant:t1:revenue"));

    // Cross-tenant: t2 sees only its own row; t1's rows are invisible.
    let resp2 = http_client()
        .get(format!("{}/v1/reconciliation/report?tenant_id=t2", h.base))
        .send()
        .await
        .unwrap();
    assert_eq!(resp2.status(), 200);
    let v2: serde_json::Value = resp2.json().await.unwrap();
    let bs2 = v2["payout_attempts"]["by_state"].as_array().unwrap();
    assert_eq!(bs2.len(), 1);
    assert_eq!(bs2[0]["state"], "unknown");
    assert_eq!(bs2[0]["count"], 1);
    assert_eq!(bs2[0]["amount_cents"], 999);
    assert_eq!(v2["payout_attempts"]["in_flight"].as_array().unwrap().len(), 1);
}

#[tokio::test]
async fn recon_report_warns_when_reserved_exceeds_pending() {
    let h = spawn_market(Vec::new()).await;
    h.payout_attempts
        .record(&attempt("p-big", "t9", 5000, payouts::AttemptState::Unknown, 10))
        .await
        .unwrap();
    // No ledger hold: reserved (5000) > pending (0) -> WARN.
    h.ledger.create_accounts("t9").await.unwrap();
    let resp = http_client()
        .get(format!("{}/v1/reconciliation/report?tenant_id=t9", h.base))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), 200);
    let v: serde_json::Value = resp.json().await.unwrap();
    let checks = v["checks"].as_array().unwrap();
    let rwp = checks.iter().find(|c| c["id"] == "reserved_within_pending").unwrap();
    assert_eq!(rwp["severity"], "warn");
    assert!(rwp["message"].as_str().unwrap().contains("investigate"));
}
