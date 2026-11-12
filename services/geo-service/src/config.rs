//! geo-service configuration (SPEC-W52 Scope A). SCREAMING_SNAKE env vars,
//! `${VAR:-default}`-wired in docker-compose.yml and documented in
//! .env.example (the house env idiom — do not repeat the geo-var drift).
//!
//! Fail-closed posture:
//!   * GEO_DATABASE_URL is REQUIRED (the service has no non-DB mode: every
//!     endpoint but /healthz reads or writes PostGIS) — unset = no boot.
//!   * GEO_TILES_PATH unset is NOT a boot error: tiles/style endpoints then
//!     answer fail-closed 503 with a plain error (real disabled-config
//!     semantics, src/tiles.rs).
//!   * PERMIFY_URL unset disables authz checks (dev escape, boot WARN —
//!     billing-engine idiom); AUTHZ_DISABLED=true bypasses them explicitly.

use std::net::SocketAddr;

#[derive(Debug, Clone)]
pub struct Config {
    /// Booking PostGIS DSN (GEO_DATABASE_URL). Required.
    pub database_url: String,
    /// Listen address (GEO_BIND, default 0.0.0.0:7015).
    pub bind: SocketAddr,
    /// PMTiles archive path (GEO_TILES_PATH). None = tiles disabled (503).
    pub tiles_path: Option<String>,
    /// Permify base URL (PERMIFY_URL). None = checks disabled (dev escape).
    pub permify_url: Option<String>,
    /// AUTHZ_DISABLED=true: explicit dev bypass of the Permify gate.
    pub authz_disabled: bool,
}

fn env_str(key: &str) -> Option<String> {
    std::env::var(key)
        .ok()
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
}

fn env_bool(key: &str) -> bool {
    std::env::var(key)
        .map(|v| matches!(v.trim().to_ascii_lowercase().as_str(), "1" | "true" | "yes"))
        .unwrap_or(false)
}

impl Config {
    pub fn from_env() -> Result<Self, String> {
        let database_url = env_str("GEO_DATABASE_URL").ok_or_else(|| {
            "GEO_DATABASE_URL is required (booking PostGIS DSN, e.g. \
             postgres://app_geo_login:***@postgres:5432/booking); geo-service has no \
             non-DB mode"
                .to_string()
        })?;
        let bind = env_str("GEO_BIND")
            .unwrap_or_else(|| "0.0.0.0:7015".to_string())
            .parse::<SocketAddr>()
            .map_err(|e| format!("GEO_BIND must be host:port (e.g. 0.0.0.0:7015): {e}"))?;
        Ok(Self {
            database_url,
            bind,
            tiles_path: env_str("GEO_TILES_PATH"),
            permify_url: env_str("PERMIFY_URL"),
            authz_disabled: env_bool("AUTHZ_DISABLED"),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn geo_database_url_is_required_fail_closed() {
        std::env::remove_var("GEO_DATABASE_URL");
        let err = Config::from_env().expect_err("unset DSN must refuse to boot");
        assert!(err.contains("GEO_DATABASE_URL"), "{err}");
    }

    #[test]
    fn defaults_parse() {
        std::env::set_var("GEO_DATABASE_URL", "postgres://u:p@localhost:5432/booking");
        std::env::remove_var("GEO_BIND");
        std::env::remove_var("GEO_TILES_PATH");
        std::env::remove_var("PERMIFY_URL");
        std::env::remove_var("AUTHZ_DISABLED");
        let cfg = Config::from_env().expect("config");
        assert_eq!(cfg.bind.port(), 7015);
        assert!(cfg.tiles_path.is_none());
        assert!(!cfg.authz_disabled);
        std::env::remove_var("GEO_DATABASE_URL");
    }
}
