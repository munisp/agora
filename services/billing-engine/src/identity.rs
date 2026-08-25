//! K1 slug -> uuid tenant resolution against identity-service (SPEC-W44 F1).
//!
//! Billing tenants are uuid-keyed, but the gateway's K1 claim
//! (`X-Tenant-Slugs`) carries Keycloak SLUGS, so a human call whose tenant
//! param is a uuid can never string-match the claim. This module closes that
//! gap: each claimed slug is resolved to its tenant uuid via
//! identity-service and the binding succeeds when any resolved id equals the
//! requested tenant uuid.
//!
//! Contract coded against (identity-service/internal/httpapi/server.go
//! `getTenant` + auth.go `validInternalToken`):
//! - `GET {IDENTITY_BASE_URL}/v1/tenants/{slug}`
//! - header `X-Internal-Token: <IDENTITY_INTERNAL_TOKEN>` authenticates the
//!   service caller (constant-time compare on the identity side);
//! - 200 JSON body carries `"id"` (tenant uuid) and `"slug"`;
//! - 404 = unknown slug (resolves to None); anything else (network error,
//!   timeout, 401/403/5xx, undecodable body) is an error.
//! This is the same caller contract booking-service uses
//! (booking-service/internal/bookingops/resolver.go, direct-HTTP mode), with
//! one deliberate difference: NO STALE SERVING. Booking serves an expired
//! cache entry when identity is down because it optimizes availability; this
//! resolver feeds an AUTHORIZATION decision, so identity errors fail CLOSED
//! (the caller maps them to 503) — an outage must never widen access.

use std::collections::HashMap;
use std::fmt;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use uuid::Uuid;

/// Slug charset accepted by identity-service (`^[a-z0-9][a-z0-9-]{1,62}$`).
/// Claim values are JWT-derived but still caller-controlled input, so
/// anything outside this shape is refused BEFORE it can reach a URL path
/// (e.g. `../internal/...` traversal) — an invalid slug simply does not
/// resolve.
fn is_valid_slug(slug: &str) -> bool {
    let b = slug.as_bytes();
    !b.is_empty()
        && b.len() <= 63
        && b[0].is_ascii_alphanumeric()
        && b.iter().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == b'-')
}

#[derive(Debug)]
pub enum ResolveError {
    /// IDENTITY_BASE_URL configured empty: resolution is disabled.
    Disabled,
    /// Identity unreachable, non-200/404 status, or undecodable body.
    Request(String),
}

impl fmt::Display for ResolveError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ResolveError::Disabled => write!(
                f,
                "identity resolution disabled (IDENTITY_BASE_URL empty)"
            ),
            ResolveError::Request(m) => write!(f, "identity resolution failed: {m}"),
        }
    }
}

struct CacheEntry {
    id: Uuid,
    fetched_at: Instant,
}

/// Tenant slug -> uuid resolver with a small positive-only TTL cache.
/// Cheap to clone into shared state; the cache is interior-mutable.
pub struct SlugResolver {
    http: reqwest::Client,
    base_url: Option<String>,
    internal_token: Option<String>,
    ttl: Duration,
    cache: Mutex<HashMap<String, CacheEntry>>,
}

impl SlugResolver {
    pub fn new(
        http: reqwest::Client,
        base_url: &str,
        internal_token: Option<String>,
        ttl: Duration,
    ) -> Self {
        let base_url = base_url.trim().trim_end_matches('/');
        Self {
            http,
            base_url: (!base_url.is_empty()).then(|| base_url.to_string()),
            internal_token: internal_token.filter(|t| !t.trim().is_empty()),
            ttl: if ttl.is_zero() {
                Duration::from_secs(60)
            } else {
                ttl
            },
            cache: Mutex::new(HashMap::new()),
        }
    }

    /// Whether identity resolution is configured at all. When false the
    /// uuid binding fails closed (403) without any network call.
    pub fn configured(&self) -> bool {
        self.base_url.is_some()
    }

    /// Resolve one slug to its tenant uuid. `Ok(None)` = the slug is
    /// syntactically valid but unknown to identity (404) — NOT an error;
    /// the caller simply continues with the next claimed slug. `Err` =
    /// resolution could not be performed; the caller MUST fail closed.
    pub async fn resolve(&self, slug: &str) -> Result<Option<Uuid>, ResolveError> {
        if !is_valid_slug(slug) {
            tracing::warn!(slug, "refusing to resolve malformed tenant slug");
            return Ok(None);
        }
        let base = self.base_url.as_ref().ok_or(ResolveError::Disabled)?;
        if let Some(hit) = self
            .cache
            .lock()
            .expect("slug cache poisoned")
            .get(slug)
            .filter(|e| e.fetched_at.elapsed() < self.ttl)
            .map(|e| e.id)
        {
            return Ok(Some(hit));
        }

        let url = format!("{base}/v1/tenants/{slug}");
        let mut req = self
            .http
            .get(&url)
            .timeout(Duration::from_secs(3));
        if let Some(token) = &self.internal_token {
            req = req.header("x-internal-token", token);
        }
        let resp = req
            .send()
            .await
            .map_err(|e| ResolveError::Request(format!("GET {url}: {e}")))?;
        let status = resp.status();
        if status == reqwest::StatusCode::NOT_FOUND {
            return Ok(None);
        }
        if !status.is_success() {
            return Err(ResolveError::Request(format!(
                "GET {url}: status {status}"
            )));
        }
        let body: serde_json::Value = resp
            .json()
            .await
            .map_err(|e| ResolveError::Request(format!("GET {url}: decode: {e}")))?;
        let id = body
            .get("id")
            .and_then(|v| v.as_str())
            .and_then(|s| Uuid::parse_str(s).ok())
            .ok_or_else(|| {
                ResolveError::Request(format!("GET {url}: body has no uuid `id` field"))
            })?;
        self.cache
            .lock()
            .expect("slug cache poisoned")
            .insert(slug.to_string(), CacheEntry { id, fetched_at: Instant::now() });
        Ok(Some(id))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn slug_charset_matches_identity_contract() {
        for ok in ["acme", "acme-prod", "a1", "twin-abc123"] {
            assert!(is_valid_slug(ok), "{ok}");
        }
        for bad in [
            "",
            "-acme",
            "Acme",
            "../internal/tenants",
            "a/b",
            "a b",
            "a?x=1",
            &"a".repeat(64),
        ] {
            assert!(!is_valid_slug(bad), "{bad:?}");
        }
    }

    #[tokio::test]
    async fn malformed_slug_resolves_none_without_network() {
        // base_url points at a dead port: a malformed slug must NEVER dial.
        let r = SlugResolver::new(
            reqwest::Client::new(),
            "http://127.0.0.1:1",
            None,
            Duration::from_secs(60),
        );
        assert!(r.configured());
        assert_eq!(r.resolve("../etc").await.unwrap(), None);
    }

    #[tokio::test]
    async fn disabled_resolution_is_an_explicit_error() {
        let r = SlugResolver::new(reqwest::Client::new(), "", None, Duration::from_secs(60));
        assert!(!r.configured());
        assert!(matches!(
            r.resolve("acme").await,
            Err(ResolveError::Disabled)
        ));
    }

    #[tokio::test]
    async fn unreachable_identity_fails_closed() {
        let r = SlugResolver::new(
            reqwest::Client::new(),
            "http://127.0.0.1:1",
            Some("tok".to_string()),
            Duration::from_secs(60),
        );
        assert!(matches!(
            r.resolve("acme").await,
            Err(ResolveError::Request(_))
        ));
    }
}
