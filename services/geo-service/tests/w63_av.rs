//! SPEC-W63 section 4: AvClient (src/av.rs) protocol tests against a FAKE
//! clamd (tokio TCP listener) — no database, no real clamd, no network
//! beyond 127.0.0.1. Mirrors the W59 python socket-fixture coverage:
//! clean reply, FOUND reply (EICAR), "INSTREAM size limit exceeded" error,
//! silent daemon, hung daemon (timeout), unreachable host, plus multi-
//! chunk (2 MiB frame) reassembly. Follows the w52_http.rs layout idiom
//! (src modules compiled into the test crate via #[path]).
//!
//! IMPORTANT: the fake clamd sends REAL clamd-style replies — z-command
//! replies are NUL-terminated (`stream: OK\0`), NOT newline-terminated.
//! A newline-only fixture masks the NUL-trim requirement in parse_reply
//! (verifier catch); do not weaken this back to `\n`.

#[path = "../src/av.rs"]
mod av;

use std::time::Duration;

use av::{AvClient, AvVerdict};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// The EICAR test string — the canonical "this is a virus" fixture every
/// clamd ships a signature for (harmless by construction).
const EICAR: &[u8] =
    b"X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*";

#[derive(Clone, Copy)]
enum Mode {
    Clean,
    Found,
    SizeLimit,
    Silent,
    Hang,
}

/// Spawn a fake clamd: accepts ONE connection, reads and validates the
/// zINSTREAM framing (zINSTREAM\0 + BE-length chunks + zero terminator),
/// then replies per `mode` with a NUL-terminated status line, exactly
/// like real clamd z-commands (Silent: close without a reply; Hang:
/// never reply). Returns the bound port.
async fn spawn_fake_clamd(mode: Mode) -> u16 {
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind fake clamd");
    let port = listener.local_addr().expect("local addr").port();
    tokio::spawn(async move {
        let (mut sock, _) = listener.accept().await.expect("accept");
        let mut cmd = vec![0u8; b"zINSTREAM\0".len()];
        sock.read_exact(&mut cmd).await.expect("read command");
        assert_eq!(&cmd, b"zINSTREAM\0", "client must speak zINSTREAM");
        let mut total: usize = 0;
        loop {
            let mut len_buf = [0u8; 4];
            sock.read_exact(&mut len_buf).await.expect("read chunk len");
            let len = u32::from_be_bytes(len_buf) as usize;
            if len == 0 {
                break;
            }
            // zINSTREAM frames are capped at 2 MiB by the client.
            assert!(len <= 2 * 1024 * 1024, "frame over 2 MiB: {len}");
            let mut buf = vec![0u8; len];
            sock.read_exact(&mut buf).await.expect("read chunk");
            total += len;
        }
        let _ = total;
        match mode {
            // Real clamd z-command replies are NUL-terminated.
            Mode::Clean => sock.write_all(b"stream: OK\0").await.expect("reply"),
            Mode::Found => sock
                .write_all(b"stream: Eicar-Signature FOUND\0")
                .await
                .expect("reply"),
            Mode::SizeLimit => sock
                .write_all(b"INSTREAM size limit exceeded ERROR\0")
                .await
                .expect("reply"),
            Mode::Silent => { /* drop without a reply */ }
            Mode::Hang => {
                tokio::time::sleep(Duration::from_secs(30)).await;
            }
        }
    });
    port
}

fn client(port: u16) -> AvClient {
    AvClient::new("127.0.0.1".to_string(), port, Duration::from_secs(5))
}

#[tokio::test]
async fn clean_reply_is_clean() {
    let port = spawn_fake_clamd(Mode::Clean).await;
    let v = client(port)
        .scan(b"perfectly innocent geojson")
        .await
        .expect("scan");
    assert_eq!(v, AvVerdict::Clean);
}

#[tokio::test]
async fn eicar_reply_is_infected_with_signature() {
    let port = spawn_fake_clamd(Mode::Found).await;
    let v = client(port).scan(EICAR).await.expect("scan");
    assert_eq!(v, AvVerdict::Infected("Eicar-Signature".to_string()));
}

#[tokio::test]
async fn size_limit_exceeded_is_error_never_clean() {
    let port = spawn_fake_clamd(Mode::SizeLimit).await;
    let err = client(port)
        .scan(&vec![0u8; 128])
        .await
        .expect_err("INSTREAM size limit exceeded must be Err, never clean");
    assert_eq!(err.kind(), std::io::ErrorKind::InvalidData);
    assert!(err.to_string().contains("size limit exceeded"), "{err}");
}

#[tokio::test]
async fn silent_daemon_is_unexpected_eof() {
    let port = spawn_fake_clamd(Mode::Silent).await;
    let err = client(port).scan(b"x").await.expect_err("no reply -> Err");
    assert_eq!(err.kind(), std::io::ErrorKind::UnexpectedEof);
}

#[tokio::test]
async fn hung_daemon_times_out() {
    let port = spawn_fake_clamd(Mode::Hang).await;
    let c = AvClient::new("127.0.0.1".to_string(), port, Duration::from_millis(200));
    let err = c.scan(b"x").await.expect_err("hung daemon -> timeout Err");
    assert_eq!(err.kind(), std::io::ErrorKind::TimedOut);
}

#[tokio::test]
async fn unreachable_host_is_error() {
    // Bind then drop the listener: the port is closed, connect refuses.
    let port = {
        let l = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        l.local_addr().expect("addr").port()
    };
    let err = client(port).scan(b"x").await.expect_err("unreachable -> Err");
    assert_eq!(err.kind(), std::io::ErrorKind::ConnectionRefused);
}

#[tokio::test]
async fn multi_chunk_stream_reassembles() {
    // > 2 MiB payload => at least two zINSTREAM frames; the fake validates
    // the framing (per-frame read_exact) and answers clean.
    let port = spawn_fake_clamd(Mode::Clean).await;
    let payload = vec![7u8; (2 * 1024 * 1024) + 1234];
    let v = client(port).scan(&payload).await.expect("scan");
    assert_eq!(v, AvVerdict::Clean);
}

#[tokio::test]
async fn empty_payload_scans_clean() {
    let port = spawn_fake_clamd(Mode::Clean).await;
    let v = client(port).scan(b"").await.expect("scan");
    assert_eq!(v, AvVerdict::Clean);
}
