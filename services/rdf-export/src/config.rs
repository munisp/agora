//! rdf-export configuration (SPEC-W54 Scope A). SCREAMING_SNAKE env vars,
//! `${VAR:-default}`-wired in docker-compose.yml and documented in
//! .env.example (the house env idiom).
//!
//! Fail-closed posture:
//!   * GRAPH_SERVICE_URL is REQUIRED — the service has no non-graph mode
//!     (every run reads graph-service's internal NDJSON export).
//!   * GRAPH_INTERNAL_TOKEN is REQUIRED — it is the outbound
//!     X-Internal-Token for /v1/graph/internal/export/* (must equal
//!     graph-service's INTERNAL_TOKEN; graph overlay, S1-F6-01).
//!   * MINIO_ROOT_USER / MINIO_ROOT_PASSWORD are REQUIRED — exports have no
//!     non-storage mode (the whole point is landing graph.ttl/graph.jsonld).
//!   * RDF_EXPORT_INTERNAL_TOKEN unset is NOT a boot error: the server
//!     boots (healthz/metrics answer) but /v1/rdf-export/* then answers
//!     fail-closed 503 (real disabled-config semantics, geo-service
//!     GEO_TILES_PATH idiom). A WRONG token is always 401.

use std::net::SocketAddr;

#[derive(Debug, Clone)]
pub struct Config {
    /// Listen address (RDF_EXPORT_BIND, default 0.0.0.0:7021).
    pub bind: SocketAddr,
    /// graph-service base URL (GRAPH_SERVICE_URL). Required.
    pub graph_service_url: String,
    /// Outbound X-Internal-Token for graph-service internal export
    /// (GRAPH_INTERNAL_TOKEN). Required.
    pub graph_internal_token: String,
    /// Inbound X-Internal-Token guard for /v1/rdf-export/*
    /// (RDF_EXPORT_INTERNAL_TOKEN). None = disabled-config 503 posture.
    pub internal_token: Option<String>,
    /// MinIO endpoint WITH scheme (MINIO_ENDPOINT, default
    /// http://minio:9000; a bare host:port gets http:// prepended).
    pub minio_endpoint: String,
    /// MinIO bucket for RDF extracts (RDF_MINIO_BUCKET, default `lake`).
    pub minio_bucket: String,
    /// SigV4 region (MINIO_REGION, default us-east-1 — MinIO accepts any).
    pub minio_region: String,
    /// SigV4 credentials (MINIO_ROOT_USER / MINIO_ROOT_PASSWORD). Required.
    pub minio_access_key: String,
    pub minio_secret_key: String,
    /// Whole-run budget in seconds (RDF_EXPORT_TIMEOUT_S, default 300).
    /// The export is a synchronous batch call; generous by design.
    pub export_timeout_s: u64,
}

fn env_str(key: &str) -> Option<String> {
    std::env::var(key)
        .ok()
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
}

impl Config {
    pub fn from_env() -> Result<Self, String> {
        let graph_service_url = env_str("GRAPH_SERVICE_URL").ok_or_else(|| {
            "GRAPH_SERVICE_URL is required (e.g. http://graph-service:7014); \
             rdf-export has no non-graph mode"
                .to_string()
        })?;
        let graph_internal_token = env_str("GRAPH_INTERNAL_TOKEN").ok_or_else(|| {
            "GRAPH_INTERNAL_TOKEN is required (must equal graph-service's \
             INTERNAL_TOKEN — see infra/docker-compose.graph.yml)"
                .to_string()
        })?;
        let minio_access_key = env_str("MINIO_ROOT_USER").ok_or_else(|| {
            "MINIO_ROOT_USER is required (SigV4 access key for the lake bucket)".to_string()
        })?;
        let minio_secret_key = env_str("MINIO_ROOT_PASSWORD").ok_or_else(|| {
            "MINIO_ROOT_PASSWORD is required (SigV4 secret key for the lake bucket)".to_string()
        })?;
        let bind = env_str("RDF_EXPORT_BIND")
            .unwrap_or_else(|| "0.0.0.0:7021".to_string())
            .parse::<SocketAddr>()
            .map_err(|e| format!("RDF_EXPORT_BIND must be host:port (e.g. 0.0.0.0:7021): {e}"))?;
        let mut minio_endpoint =
            env_str("MINIO_ENDPOINT").unwrap_or_else(|| "http://minio:9000".to_string());
        if !minio_endpoint.starts_with("http://") && !minio_endpoint.starts_with("https://") {
            minio_endpoint = format!("http://{minio_endpoint}");
        }
        let export_timeout_s = env_str("RDF_EXPORT_TIMEOUT_S")
            .map(|v| {
                v.parse::<u64>()
                    .map_err(|e| format!("RDF_EXPORT_TIMEOUT_S must be seconds: {e}"))
            })
            .transpose()?
            .unwrap_or(300);
        Ok(Self {
            bind,
            graph_service_url: graph_service_url.trim_end_matches('/').to_string(),
            graph_internal_token,
            internal_token: env_str("RDF_EXPORT_INTERNAL_TOKEN"),
            minio_endpoint: minio_endpoint.trim_end_matches('/').to_string(),
            minio_bucket: env_str("RDF_MINIO_BUCKET").unwrap_or_else(|| "lake".to_string()),
            minio_region: env_str("MINIO_REGION").unwrap_or_else(|| "us-east-1".to_string()),
            minio_access_key,
            minio_secret_key,
            export_timeout_s,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const REQUIRED: [(&str, &str); 4] = [
        ("GRAPH_SERVICE_URL", "http://graph-service:7014"),
        ("GRAPH_INTERNAL_TOKEN", "t"),
        ("MINIO_ROOT_USER", "minioadmin"),
        ("MINIO_ROOT_PASSWORD", "minioadmin"),
    ];

    fn clear() {
        for (k, _) in REQUIRED {
            std::env::remove_var(k);
        }
        for k in [
            "RDF_EXPORT_BIND",
            "RDF_EXPORT_INTERNAL_TOKEN",
            "MINIO_ENDPOINT",
            "RDF_MINIO_BUCKET",
            "MINIO_REGION",
            "RDF_EXPORT_TIMEOUT_S",
        ] {
            std::env::remove_var(k);
        }
    }

    /// Single test fn: process env is shared, so the clear/set assertions
    /// must run sequentially (parallel cargo tests would race on the vars).
    #[test]
    fn fail_closed_and_defaults() {
        clear();
        // Nothing set -> first missing var (GRAPH_SERVICE_URL) refuses boot.
        let err = Config::from_env().expect_err("unset config must refuse to boot");
        assert!(err.contains("GRAPH_SERVICE_URL"), "{err}");

        for (k, v) in REQUIRED {
            std::env::set_var(k, v);
        }
        std::env::set_var("MINIO_ENDPOINT", "minio:9000"); // bare host:port
        let cfg = Config::from_env().expect("config");
        assert_eq!(cfg.bind.port(), 7021);
        assert_eq!(cfg.minio_endpoint, "http://minio:9000");
        assert_eq!(cfg.minio_bucket, "lake");
        assert_eq!(cfg.minio_region, "us-east-1");
        assert_eq!(cfg.export_timeout_s, 300);
        // Unset inbound token = disabled-config posture (routes answer 503).
        assert!(cfg.internal_token.is_none());
        clear();
    }
}
