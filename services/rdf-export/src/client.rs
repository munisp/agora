//! Source client for graph-service's internal bulk export
//! (services/graph-service/app/routers/internal_export.py:44-116 —
//! READ-ONLY contract reference):
//!
//!   GET {GRAPH_SERVICE_URL}/v1/graph/internal/export/nodes?tenant_id=…&snapshot_date=…
//!   GET {GRAPH_SERVICE_URL}/v1/graph/internal/export/edges?tenant_id=…&snapshot_date=…
//!
//! Contract honored here:
//!   * Auth: X-Internal-Token == graph-service INTERNAL_TOKEN (never JWT).
//!   * tenant_id is MANDATORY; rows are re-verified per line — any row whose
//!     tenant_id differs from the requested tenant ABORTS the run (tenant
//!     isolation is binary; mirror of export.py's per-row re-verify).
//!   * application/x-ndjson, one JSON object per line, deterministic order.
//!     We stream-parse line-by-line (bounded memory — the tenant graph can
//!     be far larger than RAM).
//!   * Response headers X-Row-Count / X-Snapshot-Date are captured; the
//!     effective snapshot_date comes from the server when the caller left
//!     it unpinned (server defaults to today, UTC).
//!
//! Row shapes are the graph_export.py input contract verbatim
//! (export.py build_node_rows / build_edge_rows). serde IGNORES unknown
//! keys on purpose: the export contract has no `name`/phone fields, and any
//! future extra fields must pass through unread (PII discipline — nothing
//! here can project a name literal into RDF).

use futures_util::StreamExt;
use serde::Deserialize;
use thiserror::Error;

/// Node row — graph-service export contract (export.py:279-296). The struct
/// is contract-complete on purpose; `snapshot_date`/`tenant_id` are consumed
/// via the raw-line tenant re-verify and the X-Snapshot-Date header (the
/// effective date is server-authoritative), not via the typed fields.
#[allow(dead_code)] // contract-complete row; not every field is mapped to RDF
#[derive(Debug, Clone, Deserialize)]
pub struct NodeRow {
    pub snapshot_date: String,
    pub tenant_id: String,
    pub label: String,
    pub node_id: String,
    #[serde(default)]
    pub in_degree: Option<i64>,
    #[serde(default)]
    pub out_degree: Option<i64>,
    #[serde(default)]
    pub consent_marketing: Option<bool>,
    #[serde(default)]
    pub quarantine: Option<bool>,
    #[serde(default)]
    pub bookings_total: Option<i64>,
    #[serde(default)]
    pub ltv_cents: Option<i64>,
    #[serde(default)]
    pub no_show_rate: Option<f64>,
    #[serde(default)]
    pub propensity_show: Option<f64>,
    #[serde(default)]
    pub propensity_convert: Option<f64>,
    #[serde(default)]
    pub channel_of_first_touch: Option<String>,
    #[serde(default)]
    pub last_active_at: Option<String>,
}

/// Edge row — graph-service export contract (export.py:341-351).
/// Contract-complete on purpose: `weight`/`edge_at` are parsed but NOT
/// reified into RDF in v1 (mapping.rs documents this), and
/// `snapshot_date`/`tenant_id` are consumed via raw-line verification.
#[allow(dead_code)] // contract-complete row; not every field is mapped to RDF
#[derive(Debug, Clone, Deserialize)]
pub struct EdgeRow {
    pub snapshot_date: String,
    pub tenant_id: String,
    pub edge_type: String,
    pub src_label: String,
    pub src_id: String,
    pub dst_label: String,
    pub dst_id: String,
    #[serde(default)]
    pub weight: Option<f64>,
    #[serde(default)]
    pub edge_at: Option<String>,
}

#[derive(Debug, Error)]
pub enum ExportError {
    #[error("graph-service request failed: {0}")]
    Http(#[from] reqwest::Error),
    #[error("graph-service {kind} export returned {status}: {body}")]
    Upstream {
        kind: &'static str,
        status: u16,
        body: String,
    },
    #[error("invalid NDJSON line {line} in {kind} export: {source}")]
    Ndjson {
        kind: &'static str,
        line: u64,
        source: serde_json::Error,
    },
    #[error("tenant isolation violation: {kind} export row carries tenant_id {found:?}, expected {expected:?} — aborting (fail-closed)")]
    TenantMismatch {
        kind: &'static str,
        expected: String,
        found: String,
    },
}

/// Result metadata for one streamed export endpoint.
#[derive(Debug, Clone, Default)]
pub struct StreamMeta {
    /// X-Row-Count header (server-side count), if present. Informational —
    /// the run counts consumed rows itself; kept for ops cross-checks.
    #[allow(dead_code)]
    pub row_count_header: Option<u64>,
    /// X-Snapshot-Date header — the EFFECTIVE snapshot date.
    pub snapshot_date: Option<String>,
    /// Rows actually consumed (parsed + tenant-verified) by the sink.
    pub rows: u64,
}

/// Incremental NDJSON line splitter. Feeds raw byte chunks, yields complete
/// lines; `finish` flushes a final line that has NO trailing newline (the
/// NDJSON robustness case). Blank lines are skipped (never parsed).
#[derive(Default)]
pub struct LineSplitter {
    buf: Vec<u8>,
}

impl LineSplitter {
    pub fn new() -> Self {
        Self::default()
    }

    /// Push a chunk; returns each completed line (without the newline).
    pub fn push(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        self.buf.extend_from_slice(chunk);
        let mut lines = Vec::new();
        while let Some(pos) = self.buf.iter().position(|b| *b == b'\n') {
            let mut line: Vec<u8> = self.buf.drain(..=pos).collect();
            line.pop(); // \n
            if line.last() == Some(&b'\r') {
                line.pop(); // tolerate CRLF
            }
            if !line.iter().all(|b| b.is_ascii_whitespace()) {
                lines.push(line);
            }
        }
        lines
    }

    /// Flush a final unterminated line (EOF without trailing newline).
    pub fn finish(&mut self) -> Option<Vec<u8>> {
        if self.buf.is_empty() {
            return None;
        }
        let mut line = std::mem::take(&mut self.buf);
        if line.last() == Some(&b'\r') {
            line.pop();
        }
        if line.iter().all(|b| b.is_ascii_whitespace()) {
            return None;
        }
        line.shrink_to_fit();
        Some(line)
    }
}

/// Streaming NDJSON client for the graph-service internal export.
#[derive(Clone)]
pub struct GraphClient {
    http: reqwest::Client,
    base: String,
    token: String,
}

impl GraphClient {
    pub fn new(http: reqwest::Client, base: String, token: String) -> Self {
        Self { http, base, token }
    }

    pub async fn stream_nodes(
        &self,
        tenant_id: &str,
        snapshot_date: Option<&str>,
        sink: &mut (dyn FnMut(NodeRow) -> Result<(), ExportError> + Send),
    ) -> Result<StreamMeta, ExportError> {
        self.stream("nodes", tenant_id, snapshot_date, move |kind, line, raw| {
            let row: NodeRow = serde_json::from_slice(raw)
                .map_err(|source| ExportError::Ndjson { kind, line, source })?;
            sink(row)
        })
        .await
    }

    pub async fn stream_edges(
        &self,
        tenant_id: &str,
        snapshot_date: Option<&str>,
        sink: &mut (dyn FnMut(EdgeRow) -> Result<(), ExportError> + Send),
    ) -> Result<StreamMeta, ExportError> {
        self.stream("edges", tenant_id, snapshot_date, move |kind, line, raw| {
            let row: EdgeRow = serde_json::from_slice(raw)
                .map_err(|source| ExportError::Ndjson { kind, line, source })?;
            sink(row)
        })
        .await
    }

    async fn stream(
        &self,
        kind: &'static str,
        tenant_id: &str,
        snapshot_date: Option<&str>,
        mut handle: impl FnMut(&'static str, u64, &[u8]) -> Result<(), ExportError>,
    ) -> Result<StreamMeta, ExportError> {
        let url = format!("{}/v1/graph/internal/export/{kind}", self.base);
        let mut req = self
            .http
            .get(&url)
            .query(&[("tenant_id", tenant_id)])
            .header("X-Internal-Token", &self.token);
        if let Some(snap) = snapshot_date {
            req = req.query(&[("snapshot_date", snap)]);
        }
        let resp = req.send().await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            let body: String = body.chars().take(512).collect();
            return Err(ExportError::Upstream {
                kind,
                status: status.as_u16(),
                body,
            });
        }
        let mut meta = StreamMeta {
            row_count_header: resp
                .headers()
                .get("x-row-count")
                .and_then(|v| v.to_str().ok())
                .and_then(|v| v.parse::<u64>().ok()),
            snapshot_date: resp
                .headers()
                .get("x-snapshot-date")
                .and_then(|v| v.to_str().ok())
                .map(str::to_string),
            rows: 0,
        };

        let mut splitter = LineSplitter::new();
        let mut line_no: u64 = 0;
        let mut stream = resp.bytes_stream();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk?;
            for line in splitter.push(&chunk) {
                line_no += 1;
                self.verify_line(kind, tenant_id, &line)?;
                handle(kind, line_no, &line)?;
                meta.rows += 1;
            }
        }
        if let Some(line) = splitter.finish() {
            line_no += 1;
            self.verify_line(kind, tenant_id, &line)?;
            handle(kind, line_no, &line)?;
            meta.rows += 1;
        }
        Ok(meta)
    }

    /// Belt-and-braces tenant re-verify on the raw JSON line BEFORE the sink
    /// sees it (export.py re-verifies per row; so do we — a cross-tenant row
    /// must never reach the RDF mapper).
    fn verify_line(
        &self,
        kind: &'static str,
        tenant_id: &str,
        raw: &[u8],
    ) -> Result<(), ExportError> {
        #[derive(Deserialize)]
        struct TenantOnly {
            tenant_id: String,
        }
        let probe: TenantOnly =
            serde_json::from_slice(raw).map_err(|source| ExportError::Ndjson {
                kind,
                line: 0,
                source,
            })?;
        if probe.tenant_id != tenant_id {
            return Err(ExportError::TenantMismatch {
                kind,
                expected: tenant_id.to_string(),
                found: probe.tenant_id,
            });
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splitter_blank_lines_crlf_and_missing_final_newline() {
        let mut s = LineSplitter::new();
        // Chunk boundaries split mid-line; blank lines and CRLF tolerated.
        assert!(s.push(b"{\"a\":1}\n\n").len() == 1);
        assert!(s.push(b"\n{\"b\":").is_empty());
        let lines = s.push(b"2}\r\n   \n{\"c\":3}");
        assert_eq!(lines, vec![b"{\"b\":2}".to_vec()]);
        let tail = s.finish().expect("unterminated final line");
        assert_eq!(tail, b"{\"c\":3}".to_vec());
        assert!(s.finish().is_none());
    }

    #[test]
    fn splitter_empty_stream() {
        let mut s = LineSplitter::new();
        assert!(s.push(b"").is_empty());
        assert!(s.finish().is_none());
    }

    #[test]
    fn node_row_ignores_non_contract_fields() {
        // A row carrying hypothetical PII fields (the export contract NEVER
        // emits these) must parse with the extras dropped on the floor.
        let raw = br#"{"snapshot_date":"2026-01-02","tenant_id":"t1","label":"Person","node_id":"h","name":"SHOULD-NOT-SURFACE","phone":"+234000","in_degree":1}"#;
        let row: NodeRow = serde_json::from_slice(raw).unwrap();
        assert_eq!(row.label, "Person");
        let reserialized = serde_json::to_string(&serde_json::json!({
            "label": row.label, "node_id": row.node_id,
        }))
        .unwrap();
        assert!(!reserialized.contains("SHOULD-NOT-SURFACE"));
    }
}
