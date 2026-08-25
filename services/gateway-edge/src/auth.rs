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

pub struct JwksValidator {
    http: reqwest::Client,
    jwks_url: String,
    issuer: String,
    audience: Option<String>,
    ttl: Duration,
    keys: RwLock<HashMap<String, DecodingKey>>,
    loaded_at: RwLock<Option<Instant>>,
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

    pub async fn validate(&self, token: &str) -> Result<Claims, AuthError> {
        let header = decode_header(token).map_err(|_| AuthError::MalformedToken)?;
        let kid = header.kid.ok_or(AuthError::MalformedToken)?;

        let stale = self
            .loaded_at
            .read()
            .await
            .map(|t| t.elapsed() > self.ttl)
            .unwrap_or(true);
        if stale || !self.keys.read().await.contains_key(&kid) {
            self.refresh().await?;
        }

        let key = {
            let keys = self.keys.read().await;
            keys.get(&kid).cloned().ok_or(AuthError::MalformedToken)?
        };

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
