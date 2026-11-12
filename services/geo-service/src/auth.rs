//! AuthN/Z for geo-service (SPEC-W52 Scope A item 3).
//!
//! Permify authorization client — the billing-engine src/permify.rs pattern
//! (itself the booking-service Go client's HTTP API v1 REST replication; the
//! SPEC pointer to "payments-service" is stale — payments has no Permify
//! client, billing's is the platform's Rust copy):
//!
//!   POST {PERMIFY_URL}/v1/tenants/{tenant_id}/permissions/check
//!   {"metadata": {"snap_token": "", "schema_version": "", "depth": 20},
//!    "entity":   {"type": "organization", "id": tenant_id},
//!    "permission": "manage_bookings",
//!    "subject":  {"type": "user", "id": <JWT sub via X-User-Id>}}
//!   -> 200 {"can": "RESULT_ALLOWED" | "RESULT_DENIED"}
//!
//! Boundary IMPORT (both scopes — reference-scope imports write world-visible
//! rows, so they require the SAME manage_bookings perm) is gated on
//! `manage_bookings` against organization:{tenant_id} with the tenant taken
//! from the X-Tenant-Slug header (house tenant-context idiom; on this
//! platform the gateway/Permify organization id is the tenant UUID, so the
//! header value must be the tenant uuid — a non-uuid value 400s on
//! tenant-scoped operations and is treated as "no tenant context" on reads).
//!
//! Fail-CLOSED contract: a transport error, a non-2xx Permify status, or an
//! undecodable body is an Err (-> 502, never a silent allow); any `can` other
//! than RESULT_ALLOWED is a denial (-> 403). AUTHZ_DISABLED=true is the
//! explicit dev bypass (booking-service idiom); PERMIFY_URL configured empty
//! also disables the gate (boot WARNs) — never set either in production.

use std::time::Duration;

use axum::http::HeaderMap;

/// The Permify permission required to import boundaries (any scope).
pub const PERM_MANAGE_BOOKINGS: &str = "manage_bookings";

/// House tenant-context header (booking-service tenantMiddleware idiom).
pub const TENANT_HEADER: &str = "x-tenant-slug";
/// Gateway-injected JWT `sub` (APISIX serverless-pre-function K1). Never
/// trust a caller-sent copy: the gateway global rule strips and re-injects
/// it from the verified token.
pub const USER_ID_HEADER: &str = "x-user-id";

/// Permify HTTP API v1 check client. Cheap to clone into shared state.
#[derive(Clone)]
pub struct PermifyClient {
    http: reqwest::Client,
    /// None = checks disabled (PERMIFY_URL configured empty; dev escape).
    base_url: Option<String>,
}

impl PermifyClient {
    /// Tight timeouts (5s connect / 10s overall): an authz check sits on the
    /// request hot path and must fail fast (fail-closed 502) instead of
    /// parking the request.
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
        // tenant_id is a validated uuid at every call site (URL-path safe);
        // user_id is header-derived but only ever placed in the JSON body.
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

/// Request tenant context: the X-Tenant-Slug header value. Raw (unvalidated)
/// here; callers that need it as a uuid parse it and 400 on failure.
pub fn tenant_slug(headers: &HeaderMap) -> Option<String> {
    headers
        .get(TENANT_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

/// Request tenant as a uuid, when the header carries one. Reads (enrich)
/// treat a missing/non-uuid header as "no tenant context" (reference data
/// only); writes (tenant-scope import) 400 instead.
pub fn tenant_uuid(headers: &HeaderMap) -> Option<uuid::Uuid> {
    tenant_slug(headers).and_then(|s| s.parse::<uuid::Uuid>().ok())
}

/// Caller identity: the gateway-injected JWT sub.
pub fn user_id(headers: &HeaderMap) -> Option<String> {
    headers
        .get(USER_ID_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

/// Gate outcome for the import route.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Authz {
    Allow,
    /// 401 — no caller identity (X-User-Id absent) while checks are live.
    Unauthenticated,
    /// 403 — Permify denial.
    Forbidden,
    /// 502 — Permify unreachable/undecodable (fail-closed, never an allow).
    Unavailable,
}

impl Authz {
    pub fn status(self) -> axum::http::StatusCode {
        match self {
            Self::Allow => axum::http::StatusCode::OK,
            Self::Unauthenticated => axum::http::StatusCode::UNAUTHORIZED,
            Self::Forbidden => axum::http::StatusCode::FORBIDDEN,
            Self::Unavailable => axum::http::StatusCode::BAD_GATEWAY,
        }
    }

    pub fn message(self) -> &'static str {
        match self {
            Self::Allow => "allowed",
            Self::Unauthenticated => {
                "unauthorized: caller identity required (gateway-injected X-User-Id)"
            }
            Self::Forbidden => "forbidden: manage_bookings on the tenant organization is required",
            Self::Unavailable => "bad gateway: authorization check failed (fail-closed)",
        }
    }
}

/// The import gate: Permify manage_bookings on organization:{tenant} /
/// user:{sub}. Reference-scope imports require the same perm (they write
/// world-visible rows). Bypassed when AUTHZ_DISABLED=true or the client is
/// unconfigured (PERMIFY_URL empty — dev escape, both WARNed at boot).
pub async fn authorize_import(
    permify: &PermifyClient,
    authz_disabled: bool,
    headers: &HeaderMap,
    tenant_id: uuid::Uuid,
) -> Authz {
    if authz_disabled || !permify.configured() {
        return Authz::Allow;
    }
    let Some(user) = user_id(headers) else {
        return Authz::Unauthenticated;
    };
    match permify
        .check(&tenant_id.to_string(), &user, PERM_MANAGE_BOOKINGS)
        .await
    {
        Ok(true) => Authz::Allow,
        Ok(false) => Authz::Forbidden,
        Err(e) => {
            tracing::warn!(error = %e, "permify check failed (fail-closed 502)");
            Authz::Unavailable
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Local Permify stub (billing-engine permify.rs test idiom): answers
    /// the canned `can` result (or a 500), capturing the last request for
    /// assertions. Loopback only — no external network.
    async fn spawn_stub(
        can: &'static str,
        fail: bool,
    ) -> (
        String,
        std::sync::Arc<std::sync::Mutex<Option<serde_json::Value>>>,
    ) {
        use axum::extract::{Request, State};
        use axum::routing::post;
        use axum::Json;

        type Capture = std::sync::Arc<std::sync::Mutex<Option<serde_json::Value>>>;
        #[derive(Clone)]
        struct Mock {
            can: &'static str,
            fail: bool,
            last: Capture,
        }
        async fn check(
            State(m): State<Mock>,
            req: Request,
        ) -> (axum::http::StatusCode, Json<serde_json::Value>) {
            let body = axum::body::to_bytes(req.into_body(), 1 << 20)
                .await
                .unwrap_or_default();
            let json: serde_json::Value = serde_json::from_slice(&body).unwrap_or_default();
            *m.last.lock().unwrap() = Some(json);
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
            .route("/v1/tenants/:tenant/permissions/check", post(check))
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

    fn headers(pairs: &[(&str, &str)]) -> HeaderMap {
        let mut h = HeaderMap::new();
        for (k, v) in pairs {
            h.insert(
                axum::http::header::HeaderName::from_bytes(k.as_bytes()).unwrap(),
                axum::http::HeaderValue::from_str(v).unwrap(),
            );
        }
        h
    }

    #[tokio::test]
    async fn import_gate_allow_deny_outage_and_request_shape() {
        let tenant = uuid::Uuid::new_v4();
        let (base, last) = spawn_stub("RESULT_ALLOWED", false).await;
        let client = PermifyClient::new(Some(base));
        let h = headers(&[("x-user-id", "user-1")]);
        assert_eq!(
            authorize_import(&client, false, &h, tenant).await,
            Authz::Allow
        );
        let body = last.lock().unwrap().clone().expect("stub hit");
        assert_eq!(
            body["entity"],
            serde_json::json!({"type": "organization", "id": tenant.to_string()})
        );
        assert_eq!(body["permission"], serde_json::json!(PERM_MANAGE_BOOKINGS));
        assert_eq!(
            body["subject"],
            serde_json::json!({"type": "user", "id": "user-1"})
        );

        let (base, _) = spawn_stub("RESULT_DENIED", false).await;
        let client = PermifyClient::new(Some(base));
        assert_eq!(
            authorize_import(&client, false, &h, tenant).await,
            Authz::Forbidden
        );

        // Fail-closed: a Permify 5xx is 502, never a silent allow.
        let (base, _) = spawn_stub("RESULT_ALLOWED", true).await;
        let client = PermifyClient::new(Some(base));
        assert_eq!(
            authorize_import(&client, false, &h, tenant).await,
            Authz::Unavailable
        );
    }

    #[tokio::test]
    async fn import_gate_dev_bypasses_and_missing_user() {
        let tenant = uuid::Uuid::new_v4();
        // AUTHZ_DISABLED=true bypasses.
        let client = PermifyClient::new(Some("http://127.0.0.1:1".to_string()));
        assert_eq!(
            authorize_import(&client, true, &HeaderMap::new(), tenant).await,
            Authz::Allow
        );
        // Unconfigured client (PERMIFY_URL empty) bypasses (dev escape).
        let client = PermifyClient::new(None);
        assert!(!client.configured());
        assert_eq!(
            authorize_import(&client, false, &HeaderMap::new(), tenant).await,
            Authz::Allow
        );
        // Live checks without caller identity: 401.
        let (base, _) = spawn_stub("RESULT_ALLOWED", false).await;
        let client = PermifyClient::new(Some(base));
        assert_eq!(
            authorize_import(&client, false, &HeaderMap::new(), tenant).await,
            Authz::Unauthenticated
        );
    }
}
