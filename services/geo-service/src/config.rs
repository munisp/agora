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


/// SPEC-W63 section 4: ClamAV zINSTREAM scan settings for import bytes —
/// the SAME four CLAMAV_* envs and semantics as ocr-service (.env.example
/// shared block; do not drift). Standalone resolver (NOT a Config field)
/// so the Config struct literal — mirrored verbatim in the test crates —
/// is untouched. CLAMAV_HOST unset/empty = scanning DISABLED (boot WARN;
/// dev sandbox only, NEVER production). Malformed values are fail-closed
/// boot errors (same posture as GEO_DATABASE_URL).
#[derive(Debug, Clone)]
pub struct ClamavConfig {
    /// clamd host (compose sidecar DNS name `clamav`).
    pub host: String,
    /// clamd TCP port (CLAMAV_PORT, default 3310).
    pub port: u16,
    /// Connect+stream+reply budget in seconds (CLAMAV_TIMEOUT_S, default 30).
    pub timeout_s: u64,
    /// CLAMAV_REQUIRED (default TRUE = fail-closed 503 av_unavailable when
    /// clamd is unreachable; false = loud structured log + proceed).
    pub required: bool,
}

impl ClamavConfig {
    /// None when CLAMAV_HOST is unset/empty (scanning disabled).
    pub fn from_env() -> Result<Option<Self>, String> {
        let host = match env_str("CLAMAV_HOST") {
            Some(h) => h,
            None => return Ok(None),
        };
        let port = match env_str("CLAMAV_PORT") {
            Some(p) => p
                .parse::<u16>()
                .map_err(|e| format!("CLAMAV_PORT must be a TCP port (e.g. 3310): {e}"))?,
            None => 3310,
        };
        let timeout_s = match env_str("CLAMAV_TIMEOUT_S") {
            Some(t) => t
                .parse::<u64>()
                .map_err(|e| format!("CLAMAV_TIMEOUT_S must be whole seconds (e.g. 30): {e}"))?,
            None => 30,
        };
        // REQUIRED defaults TRUE (fail-closed); only an explicit false/0/no
        // opts out; garbage is a boot error, never a silent fail-open.
        let required = match env_str("CLAMAV_REQUIRED") {
            None => true,
            Some(v) => match v.to_ascii_lowercase().as_str() {
                "1" | "true" | "yes" => true,
                "0" | "false" | "no" => false,
                other => {
                    return Err(format!(
                        "CLAMAV_REQUIRED must be true/false, got '{other}'"
                    ))
                }
            },
        };
        Ok(Some(Self {
            host,
            port,
            timeout_s,
            required,
        }))
    }
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


#[cfg(test)]
mod clamav_tests {
    use super::*;

    fn clear() {
        for k in [
            "CLAMAV_HOST",
            "CLAMAV_PORT",
            "CLAMAV_TIMEOUT_S",
            "CLAMAV_REQUIRED",
        ] {
            std::env::remove_var(k);
        }
    }

    #[test]
    fn host_unset_disables_scanning() {
        clear();
        assert!(ClamavConfig::from_env().expect("env").is_none());
    }

    #[test]
    fn defaults_when_only_host_set() {
        clear();
        std::env::set_var("CLAMAV_HOST", "clamav");
        let c = ClamavConfig::from_env().expect("env").expect("enabled");
        assert_eq!(c.host, "clamav");
        assert_eq!(c.port, 3310);
        assert_eq!(c.timeout_s, 30);
        assert!(c.required, "CLAMAV_REQUIRED defaults true (fail-closed)");
        clear();
    }

    #[test]
    fn explicit_overrides_parse() {
        clear();
        std::env::set_var("CLAMAV_HOST", "clamd.internal");
        std::env::set_var("CLAMAV_PORT", "3399");
        std::env::set_var("CLAMAV_TIMEOUT_S", "45");
        std::env::set_var("CLAMAV_REQUIRED", "false");
        let c = ClamavConfig::from_env().expect("env").expect("enabled");
        assert_eq!(c.host, "clamd.internal");
        assert_eq!(c.port, 3399);
        assert_eq!(c.timeout_s, 45);
        assert!(!c.required);
        clear();
    }

    #[test]
    fn invalid_port_is_a_boot_error() {
        clear();
        std::env::set_var("CLAMAV_HOST", "clamav");
        std::env::set_var("CLAMAV_PORT", "not-a-port");
        let err = ClamavConfig::from_env().expect_err("invalid port must refuse to boot");
        assert!(err.contains("CLAMAV_PORT"), "{err}");
        clear();
    }

    #[test]
    fn garbage_required_is_a_boot_error_never_silent_fail_open() {
        clear();
        std::env::set_var("CLAMAV_HOST", "clamav");
        std::env::set_var("CLAMAV_REQUIRED", "maybe");
        let err = ClamavConfig::from_env().expect_err("garbage bool must refuse to boot");
        assert!(err.contains("CLAMAV_REQUIRED"), "{err}");
        clear();
    }
}
