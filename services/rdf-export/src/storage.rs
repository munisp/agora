//! MinIO storage: SigV4-signed PUT object, hand-rolled (SPEC-W54 Scope A
//! item 5). This is a line-for-line Rust translation of the PROVEN Go logic
//! in services/notification-worker/internal/activities/s3.go — path-style
//! addressing (http://minio:9000/{bucket}/{key}), `host;x-amz-content-sha256;
//! x-amz-date` signed headers, HMAC-SHA256 key chain.
//!
//! Why hand-rolled (README justifies too): the official AWS SDK / minio Rust
//! crates pull a large async + XML + credential-chain tree for what is here
//! exactly one PUT; `rust-s3` drags hyper and its own signing quirks. The
//! translated s3.go logic is ~90 lines against sha2/hmac/hex + reqwest,
//! which the service already has. Keys are restricted to SigV4-safe
//! characters ([A-Za-z0-9/._-]) — the canonical URI is sent unescaped, which
//! is valid for exactly that set (s3.go:21-22).

use chrono::Utc;
use hmac::{Hmac, Mac};
use sha2::{Digest, Sha256};
use thiserror::Error;

type HmacSha256 = Hmac<Sha256>;

#[derive(Debug, Error)]
pub enum StorageError {
    #[error("invalid s3 key {0:?} (allowed: [A-Za-z0-9/._-], no leading '/', no '..')")]
    InvalidKey(String),
    #[error("s3 put transport error: {0}")]
    Transport(#[from] reqwest::Error),
    #[error("s3 put {bucket}/{key}: status {status}: {body}")]
    Status {
        bucket: String,
        key: String,
        status: u16,
        body: String,
    },
}

#[derive(Clone)]
pub struct MinioClient {
    http: reqwest::Client,
    /// Endpoint WITH scheme, no trailing slash (config normalizes).
    endpoint: String,
    region: String,
    bucket: String,
    access_key: String,
    secret_key: String,
}

pub fn validate_s3_key(key: &str) -> Result<(), StorageError> {
    let ok = !key.is_empty()
        && !key.starts_with('/')
        && !key.contains("..")
        && key
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '/' | '.' | '_' | '-'));
    if ok {
        Ok(())
    } else {
        Err(StorageError::InvalidKey(key.to_string()))
    }
}

fn sha256_hex(b: &[u8]) -> String {
    hex::encode(Sha256::digest(b))
}

fn hmac_sha256(key: &[u8], data: &[u8]) -> Vec<u8> {
    let mut mac = HmacSha256::new_from_slice(key).expect("HMAC accepts any key length");
    mac.update(data);
    mac.finalize().into_bytes().to_vec()
}

/// AWS SigV4 signing key for service s3 (s3.go sigV4Key).
fn sigv4_key(secret: &str, date_stamp: &str, region: &str) -> Vec<u8> {
    let k_date = hmac_sha256(format!("AWS4{secret}").as_bytes(), date_stamp.as_bytes());
    let k_region = hmac_sha256(&k_date, region.as_bytes());
    let k_service = hmac_sha256(&k_region, b"s3");
    hmac_sha256(&k_service, b"aws4_request")
}

impl MinioClient {
    pub fn new(
        http: reqwest::Client,
        endpoint: String,
        region: String,
        bucket: String,
        access_key: String,
        secret_key: String,
    ) -> Self {
        Self {
            http,
            endpoint: endpoint.trim_end_matches('/').to_string(),
            region,
            bucket,
            access_key,
            secret_key,
        }
    }

    /// PUT body to {endpoint}/{bucket}/{key}; returns the s3:// URI.
    pub async fn put_object(
        &self,
        key: &str,
        body: &[u8],
        content_type: &str,
    ) -> Result<String, StorageError> {
        validate_s3_key(key)?;
        let url = format!("{}/{}/{}", self.endpoint, self.bucket, key);
        let host = self
            .endpoint
            .trim_start_matches("http://")
            .trim_start_matches("https://");

        let now = Utc::now();
        let amz_date = now.format("%Y%m%dT%H%M%SZ").to_string();
        let date_stamp = now.format("%Y%m%d").to_string();
        let payload_hash = sha256_hex(body);

        const SIGNED_HEADERS: &str = "host;x-amz-content-sha256;x-amz-date";
        let canonical_request = format!(
            "PUT\n/{}/{}\n\nhost:{}\nx-amz-content-sha256:{}\nx-amz-date:{}\n\n{}\n{}",
            self.bucket, key, host, payload_hash, amz_date, SIGNED_HEADERS, payload_hash
        );
        let scope = format!("{}/{}/s3/aws4_request", date_stamp, self.region);
        let string_to_sign = format!(
            "AWS4-HMAC-SHA256\n{}\n{}\n{}",
            amz_date,
            scope,
            sha256_hex(canonical_request.as_bytes())
        );
        let signature = hex::encode(hmac_sha256(
            &sigv4_key(&self.secret_key, &date_stamp, &self.region),
            string_to_sign.as_bytes(),
        ));
        let authorization = format!(
            "AWS4-HMAC-SHA256 Credential={}/{}, SignedHeaders={}, Signature={}",
            self.access_key, scope, SIGNED_HEADERS, signature
        );

        let resp = self
            .http
            .put(&url)
            .header("Content-Type", content_type)
            .header("X-Amz-Date", &amz_date)
            .header("X-Amz-Content-Sha256", &payload_hash)
            .header("Authorization", authorization)
            .body(body.to_vec())
            .send()
            .await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            let body: String = body.chars().take(512).collect();
            return Err(StorageError::Status {
                bucket: self.bucket.clone(),
                key: key.to_string(),
                status: status.as_u16(),
                body,
            });
        }
        Ok(format!("s3://{}/{}", self.bucket, key))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_validation() {
        assert!(validate_s3_key("extracts/graph_rdf/t1/2026-01-02/graph.ttl").is_ok());
        assert!(validate_s3_key("").is_err());
        assert!(validate_s3_key("/abs").is_err());
        assert!(validate_s3_key("a/../b").is_err());
        assert!(validate_s3_key("a b").is_err());
        assert!(validate_s3_key("a?b").is_err());
    }

    /// SigV4 key-derivation known-answer: AWS documentation's example secret
    /// + dateStamp 20150830, region us-east-1, service s3 (the iam variant of
    /// this chain is AWS's published KAT; s3 value cross-checked against an
    /// independent HMAC-SHA256 implementation).
    #[test]
    fn sigv4_key_derivation_is_stable() {
        let k = sigv4_key(
            "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
            "20150830",
            "us-east-1",
        );
        assert_eq!(
            hex::encode(k),
            "32f78051dcde24c552811d654f4a769112bb834b03975cdd6b1fd7d16248c269"
        );
    }
}
