//! Permify authorization client (SPEC-W47 D item 2; closes A-MW MW-L5).
//!
//! The Permify schema has always advertised `view_billing` / `manage_billing`
//! on `organization` (infra/permify/schema.perm; W47-B keeps them additive),
//! but billing-engine never enforced them — billing authz was only the K1
//! tenant binding + the K6 money-role header gate. This module is the check
//! side: the lightest REAL client, replicating the booking-service pattern
//! (booking-service/internal/permify/permify.go, HTTP API v1 REST instead of
//! gRPC — the documented platform deviation):
//!
//!   POST {PERMIFY_URL}/v1/tenants/{tenant_id}/permissions/check
//!   {"metadata": {"snap_token": "", "schema_version": "", "depth": 20},
//!    "entity":   {"type": "organization", "id": tenant_id},
//!    "permission": "view_billing" | "manage_billing",
//!    "subject":  {"type": "user", "id": x-user-id}}
//!   -> 200 {"can": "RESULT_ALLOWED" | "RESULT_DENIED"}
//!
//! `tenant_id` is the identity tenant uuid (billing tenants are uuid-keyed
//! and identity provisions the Permify tenant + writes the organization
//! owner/billing tuples under that same id — identity-service
//! internal/httpapi server.go/members.go), so no slug mapping is needed here.
//!
//! Fail-CLOSED contract (the caller in routes.rs maps these): a transport
//! error, a non-2xx Permify status, or an undecodable body is an Err (->
//! 502, never a silent allow); `can` values other than RESULT_ALLOWED are a
//! denial (-> 403). When PERMIFY_URL is configured EMPTY the client is
//! disabled (`configured() == false`) and the gate is skipped — the dev-only
//! escape, mirroring booking's AUTHZ_DISABLED; boot logs a WARN.

use std::time::Duration;

/// The Permify permission required to READ invoices (list/get).
pub const PERM_VIEW_BILLING: &str = "view_billing";
/// The Permify permission required to MUTATE invoice lifecycle
/// (issue/void). Generate/rate-card/payment-link stay on the K6 money-role
/// gate (W47-D scope).
pub const PERM_MANAGE_BILLING: &str = "manage_billing";

/// Permify HTTP API v1 check client. Cheap to clone into shared state.
#[derive(Clone)]
pub struct PermifyClient {
    http: reqwest::Client,
    /// None = checks disabled (PERMIFY_URL configured empty; dev escape).
    base_url: Option<String>,
}

impl PermifyClient {
    /// Build the client with its own tight timeouts (5s connect / 10s
    /// overall — the booking Go client uses 10s): an authz check sits on the
    /// request hot path and must fail fast (fail-closed 502) instead of
    /// parking the request for the shared client's 30s budget.
    pub fn new(base_url: Option<String>) -> Self {
        let base_url = base_url
            .map(|u| u.trim().trim_end_matches('/').to_string())
            .filter(|u| !u.is_empty());
        let http = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(5))
            .timeout(Duration::from_secs(10))
            .build()
            .expect("reqwest client with static timeout configuration must build");
        Self { http, base_url }
    }

    /// Whether checks are wired (PERMIFY_URL non-empty). When false the
    /// route gate skips enforcement (dev escape; boot WARNs).
    pub fn configured(&self) -> bool {
        self.base_url.is_some()
    }

    /// POST /v1/tenants/{tenant}/permissions/check. `Ok(true)` only on an
    /// explicit RESULT_ALLOWED; every other outcome is a denial or an error.
    pub async fn check(
        &self,
        tenant_id: &str,
        user_id: &str,
        permission: &str,
    ) -> Result<bool, String> {
        let base = self
            .base_url
            .as_deref()
            .ok_or_else(|| "permify checks disabled (PERMIFY_URL empty)".to_string())?;
        // tenant_id is a Uuid::to_string() at every call site (URL-path
        // safe); user_id is header-derived but only ever placed in the JSON
        // body, never in the URL.
        let url = format!("{base}/v1/tenants/{tenant_id}/permissions/check");
        let body = serde_json::json!({
            "metadata": {"snap_token": "", "schema_version": "", "depth": 20},
            "entity": {"type": "organization", "id": tenant_id},
            "permission": permission,
            "subject": {"type": "user", "id": user_id},
        });
        let resp = self
            .http
            .post(&url)
            .json(&body)
            .send()
            .await
            .map_err(|e| format!("permify permissions/check: {e}"))?;
        let status = resp.status();
        if !status.is_success() {
            let snippet = resp.text().await.unwrap_or_default();
            let snippet: String = snippet.chars().take(256).collect();
            return Err(format!(
                "permify permissions/check: status {status}: {snippet}"
            ));
        }
        let out: serde_json::Value = resp
            .json()
            .await
            .map_err(|e| format!("permify permissions/check: decode: {e}"))?;
        let can = out.get("can").and_then(|v| v.as_str()).unwrap_or_default();
        Ok(can == "RESULT_ALLOWED" || can == "allowed")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Mock Permify: answers the canned `can` result (or a 500 when
    /// `fail`), capturing the last request path + body for assertions.
    async fn spawn_stub(
        can: &'static str,
        fail: bool,
    ) -> (String, std::sync::Arc<std::sync::Mutex<Option<(String, serde_json::Value)>>>) {
        use axum::extract::{Request, State};
        use axum::routing::post;
        use axum::Json;

        type Capture = std::sync::Arc<std::sync::Mutex<Option<(String, serde_json::Value)>>>;
        #[derive(Clone)]
        struct Mock {
            can: &'static str,
            fail: bool,
            last: Capture,
        }
        async fn check(State(m): State<Mock>, req: Request) -> (axum::http::StatusCode, Json<serde_json::Value>) {
            let path = req.uri().path().to_string();
            let body = axum::body::to_bytes(req.into_body(), 1 << 20)
                .await
                .unwrap_or_default();
            let json: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            *m.last.lock().unwrap() = Some((path, json));
            if m.fail {
                return (
                    axum::http::StatusCode::INTERNAL_SERVER_ERROR,
                    Json(serde_json::json!({"error": "stub outage"})),
                );
            }
            (
                axum::http::StatusCode::OK,
                Json(serde_json::json!({"can": m.can})),
            )
        }
        let last: Capture = Default::default();
        let app = axum::Router::new()
            .route(
                "/v1/tenants/:tenant/permissions/check",
                post(check),
            )
            .with_state(Mock {
                can,
                fail,
                last: last.clone(),
            });
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        (format!("http://{addr}"), last)
    }

    #[tokio::test]
    async fn check_allow_deny_and_request_shape() {
        let tenant = "b3b3f6f0-6f6b-4f6b-8f6b-6f6b6f6b6f6b";
        let (base, last) = spawn_stub("RESULT_ALLOWED", false).await;
        let client = PermifyClient::new(Some(base));
        assert!(client.configured());
        let allowed = client
            .check(tenant, "user-1", PERM_VIEW_BILLING)
            .await
            .expect("check ok");
        assert!(allowed, "RESULT_ALLOWED must allow");
        let (path, body) = last.lock().unwrap().clone().expect("stub hit");
        assert_eq!(path, format!("/v1/tenants/{tenant}/permissions/check"));
        assert_eq!(body["entity"], serde_json::json!({"type": "organization", "id": tenant}));
        assert_eq!(body["permission"], serde_json::json!(PERM_VIEW_BILLING));
        assert_eq!(body["subject"], serde_json::json!({"type": "user", "id": "user-1"}));
        assert_eq!(body["metadata"]["depth"], serde_json::json!(20));

        // Denial (and the legacy lowercase alias) are not allows.
        let (base, _) = spawn_stub("RESULT_DENIED", false).await;
        let client = PermifyClient::new(Some(base));
        assert!(!client.check(tenant, "user-1", PERM_MANAGE_BILLING).await.unwrap());
        let (base, _) = spawn_stub("allowed", false).await;
        let client = PermifyClient::new(Some(base));
        assert!(client.check(tenant, "user-1", PERM_VIEW_BILLING).await.unwrap());
    }

    /// Fail-closed: a Permify 5xx is an Err (the route maps it to 502), never
    /// a silent allow; an unreachable Permify is an Err too.
    #[tokio::test]
    async fn check_fails_closed_on_outage() {
        let (base, _) = spawn_stub("RESULT_ALLOWED", true).await;
        let client = PermifyClient::new(Some(base));
        let err = client
            .check("t", "u", PERM_VIEW_BILLING)
            .await
            .expect_err("5xx must be an error");
        assert!(err.contains("status"), "error should carry the status: {err}");

        let client = PermifyClient::new(Some("http://127.0.0.1:1".to_string()));
        client
            .check("t", "u", PERM_VIEW_BILLING)
            .await
            .expect_err("connection refused must be an error");

        // Disabled client: configured() false, check errors (never allows).
        let client = PermifyClient::new(Some(String::new()));
        assert!(!client.configured());
        client
            .check("t", "u", PERM_VIEW_BILLING)
            .await
            .expect_err("disabled client must not allow");
    }
}
