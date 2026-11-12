//! X-Internal-Token guard (house idiom: constant-time compare, FAIL-CLOSED,
//! JWTs never accepted — RECON §4 "Internal APIs"). Mesh-internal only:
//! rdf-export has NO APISIX route (kyc-service precedent), so this token is
//! the whole authN story for /v1/rdf-export/*.
//!
//! Fail-closed semantics:
//!   * token UNSET  -> 503 (disabled-config; geo-service GEO_TILES_PATH idiom)
//!   * header missing/wrong -> 401
//!   * match -> proceed

use axum::http::HeaderMap;

pub const INTERNAL_TOKEN_HEADER: &str = "x-internal-token";

/// Length leaks are acceptable (same posture as Go subtle.ConstantTimeCompare);
/// content comparison never short-circuits.
pub fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Auth {
    Ok,
    /// Token unset server-side -> 503.
    Unconfigured,
    /// Header missing or mismatched -> 401.
    Unauthenticated,
}

pub fn check_internal_token(expected: Option<&str>, headers: &HeaderMap) -> Auth {
    match expected {
        None => Auth::Unconfigured,
        Some(expected) => {
            let presented = headers
                .get(INTERNAL_TOKEN_HEADER)
                .and_then(|v| v.to_str().ok());
            match presented {
                Some(got) if constant_time_eq(got.as_bytes(), expected.as_bytes()) => Auth::Ok,
                _ => Auth::Unauthenticated,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    #[test]
    fn constant_time_compare() {
        assert!(constant_time_eq(b"abc", b"abc"));
        assert!(!constant_time_eq(b"abc", b"abd"));
        assert!(!constant_time_eq(b"abc", b"ab"));
        assert!(!constant_time_eq(b"", b"x"));
        assert!(constant_time_eq(b"", b""));
    }

    #[test]
    fn fail_closed_matrix() {
        let mut h = HeaderMap::new();
        // Unset server token -> Unconfigured regardless of headers.
        assert_eq!(check_internal_token(None, &h), Auth::Unconfigured);
        h.insert(INTERNAL_TOKEN_HEADER, HeaderValue::from_static("t"));
        assert_eq!(check_internal_token(None, &h), Auth::Unconfigured);
        // Set token, missing header -> Unauthenticated.
        assert_eq!(
            check_internal_token(Some("t"), &HeaderMap::new()),
            Auth::Unauthenticated
        );
        // Wrong -> Unauthenticated; right -> Ok.
        h.insert(INTERNAL_TOKEN_HEADER, HeaderValue::from_static("nope"));
        assert_eq!(check_internal_token(Some("t"), &h), Auth::Unauthenticated);
        h.insert(INTERNAL_TOKEN_HEADER, HeaderValue::from_static("t"));
        assert_eq!(check_internal_token(Some("t"), &h), Auth::Ok);
    }
}
