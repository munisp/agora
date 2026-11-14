//! ClamAV zINSTREAM client (SPEC-W63 section 4 — Rust port of
//! services/ocr-service/app/av.py, the SPEC-W59 client).
//!
//! Same "a ~60-line client is cheaper than a dependency" idiom as the
//! Python original: tokio TCP only, no clamd crate. The clamd sidecar
//! (docker-compose `clamav`, :3310, internal network only) speaks:
//! `zINSTREAM\0`, then chunks framed as 4-byte big-endian length + payload
//! (2 MiB frames), terminated by a zero-length chunk. The reply is one
//! status line TERMINATED BY NUL for z-commands (e.g. `stream: OK\0`);
//! `\r\n`-terminated shapes are tolerated too.
//!
//! Fail posture (SPEC-W59 P3, unchanged): an ERROR reply — including
//! `INSTREAM size limit exceeded` — is a scan ERROR (Err), NEVER clean.
//! The handler layer maps Infected(sig) -> 415 {error:"infected",
//! signature} and Err -> 503 {error:"av_unavailable"} when
//! CLAMAV_REQUIRED=true (structured geo_av_scan log + proceed otherwise).

use std::io;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

/// zINSTREAM frame size (2 MiB — matches av.py and clamd expectations).
const CHUNK: usize = 2 * 1024 * 1024;
/// The reply is a single short status line; bound the read so a
/// misbehaving daemon cannot stream unbounded data into memory.
const MAX_REPLY_BYTES: usize = 8 * 1024;

/// Scan verdict (pinned SPEC-W63 cross-coder API — Coder A codes against
/// this exact shape).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AvVerdict {
    Clean,
    /// Malware signature matched (signature NAME only — never bytes).
    Infected(String),
}

/// One TCP connection per scan; clamd handles concurrency server-side
/// (av.py idiom). Cheap to clone (host String + port + timeout).
#[derive(Debug, Clone)]
pub struct AvClient {
    host: String,
    port: u16,
    timeout: Duration,
}

impl AvClient {
    pub fn new(host: String, port: u16, timeout: std::time::Duration) -> Self {
        Self {
            host,
            port,
            timeout,
        }
    }

    /// Scan `bytes`; the whole exchange (connect + stream + reply) is
    /// bounded by the configured timeout.
    pub async fn scan(&self, bytes: &[u8]) -> Result<AvVerdict, std::io::Error> {
        match tokio::time::timeout(self.timeout, self.scan_inner(bytes)).await {
            Ok(res) => res,
            Err(_) => Err(io::Error::new(
                io::ErrorKind::TimedOut,
                format!("clamd scan exceeded the {:?} timeout", self.timeout),
            )),
        }
    }

    async fn scan_inner(&self, bytes: &[u8]) -> Result<AvVerdict, io::Error> {
        let mut stream = TcpStream::connect((self.host.as_str(), self.port)).await?;
        stream.write_all(b"zINSTREAM\0").await?;
        for chunk in bytes.chunks(CHUNK) {
            stream
                .write_all(&(chunk.len() as u32).to_be_bytes())
                .await?;
            stream.write_all(chunk).await?;
        }
        // Zero-length chunk terminates the stream.
        stream.write_all(&0u32.to_be_bytes()).await?;
        stream.flush().await?;

        // Reply: one status line, terminated by NUL (z-command replies) or
        // newline (older shapes), bounded by MAX_REPLY_BYTES.
        let mut reply: Vec<u8> = Vec::new();
        let mut byte = [0u8; 1];
        loop {
            let n = stream.read(&mut byte).await?;
            if n == 0 {
                break;
            }
            reply.push(byte[0]);
            if byte[0] == b'\0' || byte[0] == b'\n' || reply.len() >= MAX_REPLY_BYTES {
                break;
            }
        }
        if reply.is_empty() {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "clamd closed the connection without a reply",
            ));
        }
        parse_reply(&reply)
    }
}

/// Reply mapping (identical to av.py `_parse_reply`): only `stream: OK` is
/// clean; `<sig> FOUND` is Infected(sig); EVERYTHING else — including
/// `INSTREAM size limit exceeded ERROR` — is a scan error (Err), never
/// silently clean.
///
/// Real clamd terminates z-command replies with NUL (`stream: OK\0`); the
/// trim MUST strip trailing NUL bytes (and \r\n) or every real-clamd reply
/// is misread as a scan error and imports fail-closed 503.
fn parse_reply(reply: &[u8]) -> Result<AvVerdict, io::Error> {
    let lossy = String::from_utf8_lossy(reply);
    let text = lossy.trim_matches(|c: char| c == '\0' || c.is_whitespace());
    let body = text
        .strip_prefix("stream:")
        .map(str::trim)
        .ok_or_else(|| invalid(format!("unexpected clamd reply: {}", truncate(text))))?;
    if body == "OK" {
        return Ok(AvVerdict::Clean);
    }
    if let Some(sig) = body.strip_suffix(" FOUND") {
        let sig = sig.trim();
        if !sig.is_empty() {
            return Ok(AvVerdict::Infected(sig.to_string()));
        }
    }
    Err(invalid(format!("clamd scan error: {}", truncate(text))))
}

fn invalid(msg: String) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, msg)
}

/// 200-char bound on echoed daemon text (log-size + injection hygiene).
fn truncate(s: &str) -> &str {
    match s.char_indices().nth(200) {
        Some((i, _)) => &s[..i],
        None => s,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_ok_is_clean_nul_and_newline_terminated() {
        // Real clamd zINSTREAM reply shape is NUL-terminated.
        assert_eq!(parse_reply(b"stream: OK\0").unwrap(), AvVerdict::Clean);
        assert_eq!(parse_reply(b"stream: OK\n").unwrap(), AvVerdict::Clean);
        assert_eq!(parse_reply(b"stream: OK\r\n").unwrap(), AvVerdict::Clean);
        assert_eq!(parse_reply(b"stream: OK").unwrap(), AvVerdict::Clean);
    }

    #[test]
    fn parse_found_is_infected_signature_only() {
        let v = parse_reply(b"stream: Eicar-Signature FOUND\0").unwrap();
        assert_eq!(v, AvVerdict::Infected("Eicar-Signature".to_string()));
        let v = parse_reply(b"stream: Eicar-Signature FOUND\n").unwrap();
        assert_eq!(v, AvVerdict::Infected("Eicar-Signature".to_string()));
    }

    #[test]
    fn parse_size_limit_error_is_never_clean() {
        for reply in [
            &b"INSTREAM size limit exceeded ERROR\0"[..],
            b"INSTREAM size limit exceeded ERROR\n",
        ] {
            let err = parse_reply(reply).expect_err("size-limit ERROR is a scan error, never clean");
            assert_eq!(err.kind(), io::ErrorKind::InvalidData);
            assert!(err.to_string().contains("size limit exceeded"), "{err}");
        }
    }

    #[test]
    fn parse_garbage_is_error() {
        assert!(parse_reply(b"bogus\0").is_err());
        assert!(parse_reply(b"stream:  FOUND\0").is_err());
        assert!(parse_reply(b"stream: UNKNOWN COMMAND\0").is_err());
        assert!(parse_reply(b"stream: OKish\0").is_err());
    }

    #[test]
    fn truncate_bounds_long_replies() {
        let long = "x".repeat(500);
        assert_eq!(truncate(&long).len(), 200);
        assert_eq!(truncate("short"), "short");
    }
}
