//! AuthN/Z for the money routes (SPEC-W43 P-09, contract C1).
//!
//! Two independent mechanisms:
//!
//! 1. **Internal token** (`X-Internal-Token` header vs `PAYMENTS_INTERNAL_TOKEN`,
//!    constant-time compare). Authenticates service-to-service callers
//!    (Temporal workers, other services). `/activities/*` and
//!    `/v1/internal/*` REQUIRE it.
//! 2. **Gateway tenant binding** (C1): APISIX validates the JWT and injects
//!    `X-Tenant-Slugs` (comma-joined `tenant_slugs` claim). When the header is
//!    present, a request body's / path's `tenant_id` must appear in it exactly,
//!    else 403.
//! 3. **Money-role gate** (SPEC-W44 K6, closes S1-F7-01): money-MUTATION
//!    endpoints additionally require `X-User-Roles` (gateway-injected from the
//!    verified JWT) to intersect `MONEY_ROLES` (default "owner,admin") —
//!    tenant membership alone never authorizes moving money. Fail-closed:
//!    no roles header = no roles. Valid internal-token callers (K2) and the
//!    dev escape are exempt.
//!
//! Fail-closed posture: with no token configured AND no gateway header AND
//! the dev escape off, money routes answer 503 (they cannot be served
//! safely). `OPENDESK_TRUST_DIRECT_TENANT=1` is the documented dev escape
//! (standalone, no gateway) — OFF by default and never set in compose.

use axum::http::{HeaderMap, StatusCode};

use crate::flutterwave::constant_time_eq;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthRejection {
    /// 401 — authentication required / wrong internal token.
    Unauthorized,
    /// 403 — authenticated, but the tenant is not bound to the caller.
    Forbidden,
    /// 403 — authenticated + tenant-bound, but the caller lacks a money role
    /// (SPEC-W44 K6: X-User-Roles ∩ MONEY_ROLES required on money mutations).
    RoleForbidden,
    /// 503 — fail-closed: no token configured, no gateway header, no dev
    /// escape; the route cannot be served safely.
    Unavailable,
}

impl AuthRejection {
    pub fn status(self) -> StatusCode {
        match self {
            Self::Unauthorized => StatusCode::UNAUTHORIZED,
            Self::Forbidden | Self::RoleForbidden => StatusCode::FORBIDDEN,
            Self::Unavailable => StatusCode::SERVICE_UNAVAILABLE,
        }
    }

    pub fn message(self) -> &'static str {
        match self {
            Self::Unauthorized => "unauthorized: valid X-Internal-Token required",
            Self::Forbidden => "forbidden: tenant_id is not bound to the caller",
            Self::RoleForbidden => {
                "forbidden: a money role is required for this operation \
                 (X-User-Roles intersect MONEY_ROLES, default owner/admin)"
            }
            Self::Unavailable => {
                "service unavailable: payments auth is not configured \
                 (PAYMENTS_INTERNAL_TOKEN unset and gateway header absent)"
            }
        }
    }
}

#[derive(Debug, Clone, Default)]
pub struct AuthConfig {
    pub internal_token: Option<String>,
    pub trust_direct_tenant: bool,
    /// SPEC-W44 K6: roles allowed to perform money mutations (from the
    /// MONEY_ROLES env, default "owner,admin"; lowercase, trimmed).
    pub money_roles: Vec<String>,
}

pub const INTERNAL_TOKEN_HEADER: &str = "x-internal-token";
pub const TENANT_SLUGS_HEADER: &str = "x-tenant-slugs";
/// K1: gateway-injected from the verified JWT (`sub`); never trust a
/// caller-sent copy (APISIX strips and re-injects).
pub const USER_ID_HEADER: &str = "x-user-id";
/// K1/K6: gateway-injected csv of `realm_access.roles` (empty string when the
/// token carries no roles — fail-closed by construction).
pub const USER_ROLES_HEADER: &str = "x-user-roles";

impl AuthConfig {
    pub fn new(
        internal_token: Option<String>,
        trust_direct_tenant: bool,
        money_roles: Vec<String>,
    ) -> Self {
        Self {
            internal_token: internal_token.filter(|t| !t.trim().is_empty()),
            trust_direct_tenant,
            money_roles,
        }
    }

    fn presented_token<'h>(&self, headers: &'h HeaderMap) -> Option<&'h str> {
        headers
            .get(INTERNAL_TOKEN_HEADER)
            .and_then(|v| v.to_str().ok())
            .map(str::trim)
            .filter(|s| !s.is_empty())
    }

    fn token_matches(&self, presented: &str) -> bool {
        match &self.internal_token {
            Some(expected) => constant_time_eq(expected.as_bytes(), presented.as_bytes()),
            None => false,
        }
    }

    fn tenant_slugs<'h>(&self, headers: &'h HeaderMap) -> Option<&'h str> {
        headers
            .get(TENANT_SLUGS_HEADER)
            .and_then(|v| v.to_str().ok())
            .map(str::trim)
            .filter(|s| !s.is_empty())
    }

    /// Authorize a tenant-scoped money route. Order:
    ///   1. A presented internal token must be valid (wrong token => 401);
    ///      a valid internal token fully authenticates the call (C1:
    ///      "explicitly authenticated internal token").
    ///   2. Gateway-driven request (X-Tenant-Slugs present): the tenant must
    ///      be listed exactly, else 403.
    ///   3. Dev escape (OPENDESK_TRUST_DIRECT_TENANT=1): allow.
    ///   4. Token configured but not presented => 401; no token configured at
    ///      all => 503 fail-closed.
    pub fn authorize_tenant(
        &self,
        headers: &HeaderMap,
        tenant_id: &str,
    ) -> Result<(), AuthRejection> {
        if let Some(presented) = self.presented_token(headers) {
            if self.internal_token.is_none() {
                // Unsolicited token with none configured proves nothing; fall
                // through to the gateway/dev posture.
            } else if self.token_matches(presented) {
                return Ok(());
            } else {
                return Err(AuthRejection::Unauthorized);
            }
        }
        if let Some(slugs) = self.tenant_slugs(headers) {
            let bound = slugs.split(',').any(|s| s.trim() == tenant_id);
            return if bound {
                Ok(())
            } else {
                Err(AuthRejection::Forbidden)
            };
        }
        if self.trust_direct_tenant {
            return Ok(());
        }
        if self.internal_token.is_some() {
            return Err(AuthRejection::Unauthorized);
        }
        Err(AuthRejection::Unavailable)
    }

    /// Internal-only routes (`/activities/*`, `/v1/internal/*`): require the
    /// internal token. With no token configured the route fails closed (503)
    /// unless the dev escape is on.
    pub fn require_internal(&self, headers: &HeaderMap) -> Result<(), AuthRejection> {
        match (&self.internal_token, self.presented_token(headers)) {
            (Some(_), Some(p)) if self.token_matches(p) => Ok(()),
            (Some(_), _) => Err(AuthRejection::Unauthorized),
            (None, _) if self.trust_direct_tenant => Ok(()),
            (None, _) => Err(AuthRejection::Unavailable),
        }
    }

    /// Gateway-injected `X-User-Id` (JWT sub), for deposit provenance (K7).
    pub fn user_id<'h>(&self, headers: &'h HeaderMap) -> Option<&'h str> {
        headers
            .get(USER_ID_HEADER)
            .and_then(|v| v.to_str().ok())
            .map(str::trim)
            .filter(|s| !s.is_empty())
    }

    /// Gateway-injected `X-User-Roles` (csv of realm roles, K1). An absent or
    /// empty header means NO roles (fail-closed, K6).
    pub fn user_roles(&self, headers: &HeaderMap) -> Vec<String> {
        headers
            .get(USER_ROLES_HEADER)
            .and_then(|v| v.to_str().ok())
            .map(|s| {
                s.split(',')
                    .map(|t| t.trim().to_ascii_lowercase())
                    .filter(|t| !t.is_empty())
                    .collect()
            })
            .unwrap_or_default()
    }

    /// SPEC-W44 K6 money-role gate for money-MUTATION endpoints
    /// (`/v1/deposits`, capture, refunds, no-show fee, payouts,
    /// beneficiaries). Order:
    ///   1. A presented internal token must be valid (wrong => 401); a valid
    ///      internal token fully authenticates the call (service caller, K2).
    ///   2. Gateway-driven request: `X-User-Roles` must intersect
    ///      `money_roles`; the header absent/empty means NO roles => 403.
    ///   3. Dev escape (OPENDESK_TRUST_DIRECT_TENANT=1) with no roles header
    ///      at all: allow (standalone dev only; never set in compose).
    /// Call AFTER `authorize_tenant` so a tenant mismatch is still a 403.
    pub fn require_money_role(&self, headers: &HeaderMap) -> Result<(), AuthRejection> {
        if let Some(presented) = self.presented_token(headers) {
            if self.internal_token.is_some() {
                if self.token_matches(presented) {
                    return Ok(());
                }
                return Err(AuthRejection::Unauthorized);
            }
        }
        let roles = self.user_roles(headers);
        if roles
            .iter()
            .any(|r| self.money_roles.iter().any(|m| m == r))
        {
            return Ok(());
        }
        let roles_header_present = headers.contains_key(USER_ROLES_HEADER);
        if self.trust_direct_tenant && !roles_header_present {
            return Ok(());
        }
        Err(AuthRejection::RoleForbidden)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    fn headers_with(pairs: &[(&str, &str)]) -> HeaderMap {
        let mut h = HeaderMap::new();
        for (k, v) in pairs {
            h.insert(
                axum::http::header::HeaderName::from_bytes(k.as_bytes()).unwrap(),
                HeaderValue::from_str(v).unwrap(),
            );
        }
        h
    }

    fn auth(token: Option<&str>, dev: bool) -> AuthConfig {
        AuthConfig::new(
            token.map(|s| s.to_string()),
            dev,
            vec!["owner".to_string(), "admin".to_string()],
        )
    }

    // ---------------- authorize_tenant matrix (P-09 / C1) ----------------

    #[test]
    fn valid_internal_token_authorizes_any_tenant() {
        let a = auth(Some("s3cret"), false);
        let h = headers_with(&[("x-internal-token", "s3cret")]);
        assert_eq!(a.authorize_tenant(&h, "tenant-a"), Ok(()));
    }

    #[test]
    fn wrong_internal_token_is_401() {
        let a = auth(Some("s3cret"), false);
        let h = headers_with(&[("x-internal-token", "wrong")]);
        assert_eq!(
            a.authorize_tenant(&h, "tenant-a"),
            Err(AuthRejection::Unauthorized)
        );
        // ... even when a gateway header would otherwise match.
        let h = headers_with(&[
            ("x-internal-token", "wrong"),
            ("x-tenant-slugs", "tenant-a"),
        ]);
        assert_eq!(
            a.authorize_tenant(&h, "tenant-a"),
            Err(AuthRejection::Unauthorized)
        );
    }

    #[test]
    fn gateway_header_binds_matching_tenant() {
        let a = auth(Some("s3cret"), false);
        let h = headers_with(&[("x-tenant-slugs", "tenant-a, tenant-b")]);
        assert_eq!(a.authorize_tenant(&h, "tenant-b"), Ok(()));
    }

    #[test]
    fn gateway_header_rejects_unlisted_tenant_403() {
        let a = auth(Some("s3cret"), false);
        let h = headers_with(&[("x-tenant-slugs", "tenant-a")]);
        assert_eq!(
            a.authorize_tenant(&h, "tenant-b"),
            Err(AuthRejection::Forbidden)
        );
        // Substring games do not bind.
        let h = headers_with(&[("x-tenant-slugs", "tenant-a")]);
        assert_eq!(
            a.authorize_tenant(&h, "tenant-"),
            Err(AuthRejection::Forbidden)
        );
    }

    #[test]
    fn gateway_binding_enforced_even_without_configured_token() {
        let a = auth(None, false);
        let h = headers_with(&[("x-tenant-slugs", "tenant-a")]);
        assert_eq!(a.authorize_tenant(&h, "tenant-a"), Ok(()));
        assert_eq!(
            a.authorize_tenant(&h, "tenant-b"),
            Err(AuthRejection::Forbidden)
        );
    }

    #[test]
    fn no_credentials_with_token_configured_is_401() {
        let a = auth(Some("s3cret"), false);
        assert_eq!(
            a.authorize_tenant(&HeaderMap::new(), "tenant-a"),
            Err(AuthRejection::Unauthorized)
        );
    }

    #[test]
    fn no_token_no_gateway_no_dev_escape_is_503_fail_closed() {
        let a = auth(None, false);
        assert_eq!(
            a.authorize_tenant(&HeaderMap::new(), "tenant-a"),
            Err(AuthRejection::Unavailable)
        );
    }

    #[test]
    fn dev_escape_allows_direct_tenant() {
        let a = auth(None, true);
        assert_eq!(a.authorize_tenant(&HeaderMap::new(), "tenant-a"), Ok(()));
        // A gateway header, when present, is still authoritative.
        let h = headers_with(&[("x-tenant-slugs", "tenant-a")]);
        assert_eq!(
            a.authorize_tenant(&h, "tenant-b"),
            Err(AuthRejection::Forbidden)
        );
    }

    // ---------------- require_internal matrix ----------------

    #[test]
    fn require_internal_accepts_valid_token() {
        let a = auth(Some("s3cret"), false);
        let h = headers_with(&[("x-internal-token", "s3cret")]);
        assert_eq!(a.require_internal(&h), Ok(()));
    }

    #[test]
    fn require_internal_rejects_missing_or_wrong_token_401() {
        let a = auth(Some("s3cret"), false);
        assert_eq!(
            a.require_internal(&HeaderMap::new()),
            Err(AuthRejection::Unauthorized)
        );
        let h = headers_with(&[("x-internal-token", "nope")]);
        assert_eq!(
            a.require_internal(&h),
            Err(AuthRejection::Unauthorized)
        );
    }

    #[test]
    fn require_internal_unset_token_is_503_fail_closed() {
        let a = auth(None, false);
        // Even a presented token cannot help when none is configured.
        let h = headers_with(&[("x-internal-token", "anything")]);
        assert_eq!(
            a.require_internal(&h),
            Err(AuthRejection::Unavailable)
        );
    }

    #[test]
    fn require_internal_dev_escape_allows() {
        let a = auth(None, true);
        assert_eq!(a.require_internal(&HeaderMap::new()), Ok(()));
    }

    // ---------------- require_money_role matrix (SPEC-W44 K6) ----------------

    #[test]
    fn money_role_passes_with_owner_or_admin() {
        let a = auth(Some("s3cret"), false);
        let h = headers_with(&[("x-user-roles", "finance,owner")]);
        assert_eq!(a.require_money_role(&h), Ok(()));
        let h = headers_with(&[("x-user-roles", "Admin")]); // case-insensitive
        assert_eq!(a.require_money_role(&h), Ok(()));
    }

    #[test]
    fn money_role_member_without_role_is_403() {
        let a = auth(Some("s3cret"), false);
        // Tenant member WITHOUT a money role: the S1-F7-01 exploit class.
        let h = headers_with(&[
            ("x-tenant-slugs", "tenant-a"),
            ("x-user-roles", "member"),
        ]);
        assert_eq!(
            a.require_money_role(&h),
            Err(AuthRejection::RoleForbidden)
        );
        // Gateway injects an EMPTY roles header when the token has no roles:
        // still fail-closed.
        let h = headers_with(&[("x-tenant-slugs", "tenant-a"), ("x-user-roles", "")]);
        assert_eq!(
            a.require_money_role(&h),
            Err(AuthRejection::RoleForbidden)
        );
        // Header absent entirely: no header = no roles.
        let h = headers_with(&[("x-tenant-slugs", "tenant-a")]);
        assert_eq!(
            a.require_money_role(&h),
            Err(AuthRejection::RoleForbidden)
        );
    }

    #[test]
    fn money_role_valid_internal_token_bypasses() {
        // Service callers (K2) authenticate with the internal token and are
        // not role-gated; a wrong token stays 401.
        let a = auth(Some("s3cret"), false);
        let h = headers_with(&[("x-internal-token", "s3cret")]);
        assert_eq!(a.require_money_role(&h), Ok(()));
        let h = headers_with(&[("x-internal-token", "wrong"), ("x-user-roles", "owner")]);
        assert_eq!(
            a.require_money_role(&h),
            Err(AuthRejection::Unauthorized),
            "a wrong internal token must not fall through to the role check"
        );
    }

    #[test]
    fn money_role_dev_escape_allows_headerless_direct_calls_only() {
        let a = auth(None, true);
        assert_eq!(a.require_money_role(&HeaderMap::new()), Ok(()));
        // A caller-SUPPLIED roles header without a money role is still 403
        // even under the dev escape (the escape covers headerless standalone
        // calls, not role spoofing).
        let h = headers_with(&[("x-user-roles", "member")]);
        assert_eq!(
            a.require_money_role(&h),
            Err(AuthRejection::RoleForbidden)
        );
    }

    #[test]
    fn user_id_reads_gateway_header() {
        let a = auth(None, false);
        let h = headers_with(&[("x-user-id", "user-123")]);
        assert_eq!(a.user_id(&h), Some("user-123"));
        assert_eq!(a.user_id(&HeaderMap::new()), None);
    }
}
