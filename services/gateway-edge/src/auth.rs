//! JWT validation against the Keycloak `opendesk` realm JWKS (SPEC §8).
//!
//! RS256 tokens are verified with keys fetched from the realm certs endpoint
//! and cached with a TTL (refreshed early on unknown `kid`). Tenant
//! authorization uses the `tenant_slugs` claim populated by the Keycloak
//! group-membership attribute mapper.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use jsonwebtoken::{decode, decode_header, jwk, Algorithm, DecodingKey, Validation};
use serde::Deserialize;
use thiserror::Error;
use tokio::sync::RwLock;
use tracing::warn;

#[derive(Debug, Error)]
pub enum AuthError {
    #[error("missing bearer token")]
    MissingToken,
    #[error("malformed token")]
    MalformedToken,
    #[error("jwks fetch failed: {0}")]
    JwksFetch(String),
    #[error("token validation failed: {0}")]
    Validation(String),
    #[error("token is not authorized for tenant {0}")]
    Forbidden(String),
}

#[derive(Debug, Clone, Deserialize)]
pub struct Claims {
    #[serde(default)]
    pub sub: Option<String>,
    /// Populated by the Keycloak group attribute mapper (SPEC §8).
    #[serde(default)]
    pub tenant_slugs: Vec<String>,
    #[serde(default)]
    pub exp: Option<u64>,
}

/// Returns true when the token claims grant access to `tenant_slug`.
pub fn authorize_tenant(claims: &Claims, tenant_slug: &str) -> bool {
    claims.tenant_slugs.iter().any(|s| s == tenant_slug)
}

#[derive(Clone)]
pub enum Authenticator {
    /// Dev mode (`EDGE_AUTH_DISABLED=true`): every request is allowed.
    Disabled,
    Jwks(Arc<JwksValidator>),
}

impl Authenticator {
    pub async fn authenticate(&self, token: Option<&str>, tenant: &str) -> Result<Claims, AuthError> {
        match self {
            Authenticator::Disabled => Ok(Claims {
                sub: Some("dev".to_string()),
                tenant_slugs: vec![tenant.to_string()],
                exp: None,
            }),
            Authenticator::Jwks(v) => {
                let token = token.ok_or(AuthError::MissingToken)?;
                let token = token.strip_prefix("Bearer ").unwrap_or(token);
                let claims = v.validate(token).await?;
                if !authorize_tenant(&claims, tenant) {
                    return Err(AuthError::Forbidden(tenant.to_string()));
                }
                Ok(claims)
            }
        }
    }
}

/// SPEC-W46 R16: a token carrying an unknown `kid` is negative-cached for
/// this long — previously EVERY such token triggered a full JWKS HTTP fetch
/// (an unauthenticated DoS amplifier: 1 request = 1 upstream fetch).
const UNKNOWN_KID_TTL: Duration = Duration::from_secs(60);
/// Bound on the negative cache (kid strings are attacker-controlled).
const UNKNOWN_KID_CACHE_MAX: usize = 1024;

pub struct JwksValidator {
    http: reqwest::Client,
    jwks_url: String,
    issuer: String,
    audience: Option<String>,
    ttl: Duration,
    keys: RwLock<HashMap<String, DecodingKey>>,
    loaded_at: RwLock<Option<Instant>>,
    /// R16 single-flight: at most one JWKS refresh in flight; concurrent
    /// validations that all miss the cache queue on this mutex and reuse the
    /// winner's fetch (double-checked after acquisition).
    refresh_lock: tokio::sync::Mutex<()>,
    /// R16 negative cache: kid -> when it was confirmed absent from a FRESH
    /// key set. Bounded (UNKNOWN_KID_CACHE_MAX, expired/oldest evicted).
    unknown_kids: RwLock<HashMap<String, Instant>>,
}

impl JwksValidator {
    pub fn new(jwks_url: String, issuer: String, audience: Option<String>, ttl: Duration) -> Self {
        Self {
            // RS-006 idiom (payments-service): explicit timeouts so a hung
            // Keycloak can never park token validation forever — 5s connect,
            // 10s total (tighter than the 30s rail budget: JWKS is tiny and
            // on the request hot path).
            http: reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(5))
                .timeout(Duration::from_secs(10))
                .build()
                .expect("reqwest client with static timeout configuration must build"),
            jwks_url,
            issuer,
            audience,
            ttl,
            keys: RwLock::new(HashMap::new()),
            loaded_at: RwLock::new(None),
            refresh_lock: tokio::sync::Mutex::new(()),
            unknown_kids: RwLock::new(HashMap::new()),
        }
    }

    async fn refresh(&self) -> Result<(), AuthError> {
        let set: jwk::JwkSet = self
            .http
            .get(&self.jwks_url)
            .send()
            .await
            .map_err(|e| AuthError::JwksFetch(e.to_string()))?
            .error_for_status()
            .map_err(|e| AuthError::JwksFetch(e.to_string()))?
            .json()
            .await
            .map_err(|e| AuthError::JwksFetch(e.to_string()))?;
        let mut map = HashMap::new();
        for key in &set.keys {
            if let jwk::AlgorithmParameters::RSA(rsa) = &key.algorithm {
                if let Some(kid) = key.common.key_id.clone() {
                    match DecodingKey::from_rsa_components(&rsa.n, &rsa.e) {
                        Ok(k) => {
                            map.insert(kid, k);
                        }
                        Err(e) => warn!(error = %e, kid = %kid, "skipping unusable jwk"),
                    }
                }
            }
        }
        if map.is_empty() {
            return Err(AuthError::JwksFetch("no RSA keys in jwks".to_string()));
        }
        *self.keys.write().await = map;
        *self.loaded_at.write().await = Some(Instant::now());
        Ok(())
    }

    /// True while the kid sits in the (fresh) negative cache.
    async fn kid_recently_unknown(&self, kid: &str) -> bool {
        self.unknown_kids
            .read()
            .await
            .get(kid)
            .map(|t| t.elapsed() < UNKNOWN_KID_TTL)
            .unwrap_or(false)
    }

    /// Record a confirmed-unknown kid, bounded: expired entries are evicted
    /// first, then the oldest if still full.
    async fn mark_kid_unknown(&self, kid: &str) {
        let mut cache = self.unknown_kids.write().await;
        if cache.len() >= UNKNOWN_KID_CACHE_MAX {
            cache.retain(|_, t| t.elapsed() < UNKNOWN_KID_TTL);
            while cache.len() >= UNKNOWN_KID_CACHE_MAX {
                if let Some(oldest) = cache
                    .iter()
                    .max_by_key(|(_, t)| t.elapsed())
                    .map(|(k, _)| k.clone())
                {
                    cache.remove(&oldest);
                } else {
                    break;
                }
            }
        }
        cache.insert(kid.to_string(), Instant::now());
    }

    pub async fn validate(&self, token: &str) -> Result<Claims, AuthError> {
        let header = decode_header(token).map_err(|_| AuthError::MalformedToken)?;
        let kid = header.kid.ok_or(AuthError::MalformedToken)?;

        let key = {
            let keys = self.keys.read().await;
            keys.get(&kid).cloned()
        };
        let key = match key {
            Some(k) => Some(k),
            None => {
                // R16: unknown kid — serve the negative cache instead of a
                // full JWKS fetch per request.
                if self.kid_recently_unknown(&kid).await {
                    return Err(AuthError::MalformedToken);
                }
                None
            }
        };
        let stale = self
            .loaded_at
            .read()
            .await
            .map(|t| t.elapsed() > self.ttl)
            .unwrap_or(true);

        let key = if stale || key.is_none() {
            // R16 single-flight: exactly one refresh runs; everyone else
            // waits and then re-checks the winner's fresh key set.
            let _permit = self.refresh_lock.lock().await;
            // Double-checked under the lock: only refresh when the set the
            // lock winner left behind is still stale/missing.
            let fresh = self
                .loaded_at
                .read()
                .await
                .map(|t| t.elapsed() <= self.ttl)
                .unwrap_or(false);
            if !fresh {
                self.refresh().await?;
            }
            let key = self.keys.read().await.get(&kid).cloned();
            if key.is_none() {
                // Confirmed absent from a fresh set: negative-cache it.
                self.mark_kid_unknown(&kid).await;
            }
            key
        } else {
            key
        };
        let key = key.ok_or(AuthError::MalformedToken)?;

        let mut validation = Validation::new(Algorithm::RS256);
        validation.set_issuer(&[self.issuer.clone()]);
        if let Some(aud) = &self.audience {
            validation.set_audience(&[aud.clone()]);
        } else {
            validation.validate_aud = false;
        }
        let data = decode::<Claims>(token, &key, &validation)
            .map_err(|e| AuthError::Validation(e.to_string()))?;
        Ok(data.claims)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tenant_authorization_matches_slug() {
        let claims = Claims {
            sub: Some("u1".into()),
            tenant_slugs: vec!["acme".into(), "globex".into()],
            exp: None,
        };
        assert!(authorize_tenant(&claims, "acme"));
        assert!(!authorize_tenant(&claims, "initech"));
    }

    /// RS-006: the JWKS validator must construct (timeouts are statically
    /// configured; a builder failure would panic here).
    #[test]
    fn jwks_validator_constructs_with_timeouts() {
        let v = JwksValidator::new(
            "http://keycloak:8080/certs".into(),
            "http://keycloak:8080/realms/opendesk".into(),
            Some("opendesk".into()),
            Duration::from_secs(60),
        );
        assert_eq!(v.audience.as_deref(), Some("opendesk"));
    }

    /// R16: the unknown-kid negative cache is bounded and fresh entries hit.
    #[tokio::test]
    async fn unknown_kid_negative_cache_is_bounded_and_fresh() {
        let v = JwksValidator::new(
            "http://keycloak:8080/certs".into(),
            "http://keycloak:8080/realms/opendesk".into(),
            None,
            Duration::from_secs(60),
        );
        assert!(!v.kid_recently_unknown("kid-x").await);
        v.mark_kid_unknown("kid-x").await;
        assert!(v.kid_recently_unknown("kid-x").await);
        // Fill far past the bound: the map never exceeds UNKNOWN_KID_CACHE_MAX.
        for i in 0..(UNKNOWN_KID_CACHE_MAX * 2) {
            v.mark_kid_unknown(&format!("kid-{i}")).await;
        }
        assert!(v.unknown_kids.read().await.len() <= UNKNOWN_KID_CACHE_MAX);
    }

    #[test]
    fn empty_claims_authorize_nothing() {
        let claims = Claims {
            sub: None,
            tenant_slugs: vec![],
            exp: None,
        };
        assert!(!authorize_tenant(&claims, "acme"));
    }
}
