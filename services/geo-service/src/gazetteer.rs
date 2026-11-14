//! Offline gazetteer (SPEC-W63 Scope A): the `geo_places` reference table,
//! its search/snapshot endpoints, the gazetteer import endpoint, and the
//! boot-time Nigeria seed.
//!
//! Table: migrations/0002_gazetteer.sql — `geo_places(id, tenant_id NULL,
//! kind 'state'|'lga'|'locality', name, code, parent_code, lat/lng NULL,
//! source, updated_at)` with UNIQUE (kind, code) and the SAME NULL-reference
//! RLS variant as geo_boundaries (0001_geo.sql:60-78). Reference rows are
//! world-visible, so reads here run WITHOUT the tenant GUC (the enrich.rs
//! reference-pass idiom); writes run in a transaction with the GUC pinned
//! to the zero uuid (never a real tenant), the import.rs reference-scope
//! idiom.
//!
//! Codes (PINNED slug rule, SPEC-W63 §1): uppercase, spaces/'/' -> '-',
//! strip other punctuation (see [`slug`]). States `NG-<STATE>`, LGAs
//! `NG-<STATE>-<LGA>`, localities importer-assigned.
//!
//! Endpoints:
//!   * GET  /v1/geo/gazetteer?kind=&state=&q=&limit= — search/list reference
//!     places (`q` = case-insensitive substring on name/code; `state` scopes
//!     LGAs/localities to a state, accepting a name or code).
//!   * GET  /v1/geo/gazetteer/snapshot?levels=state,lga — full reference
//!     dump as canonical JSON; ETag = sha256 of the exact body bytes;
//!     If-None-Match match => 304 empty. Built for field-pwa's offline
//!     snapshot sync (W63 Scope C).
//!   * POST /v1/geo/gazetteer/import — CSV / GeoJSON / JSON (multipart file,
//!     {url}, or raw body). Permify manage_bookings gate (src/auth.rs idiom,
//!     same as boundary import). AV-scanned pre-parse via the pinned W63
//!     cross-coder API (`state.av: Option<av::AvClient>` +
//!     `state.av_required: bool`, implemented by Scope B): Infected => 415,
//!     scan error => 503 `av_unavailable` when required, else log + proceed.
//!     No metrics idiom exists in geo-service (checked: no prometheus /
//!     counter infra in the crate), so scan outcomes are structured logs
//!     only (SPEC-W63 §4 fallback). Upsert on (kind, code), one transaction
//!     with a per-row savepoint (import.rs idiom).
//!
//! Boot wiring (main.rs, owned by Scope B this wave):
//!   `gazetteer::apply_gazetteer_migration(&pool).await?;` — fail-closed
//!   (store.rs apply_geo_migration idiom: never serve without the RLS
//!   policy), then `gazetteer::seed_boot(&pool).await?;` — idempotent
//!   INSERT ... ON CONFLICT DO NOTHING from the bundled
//!   seeds/nigeria_gazetteer.csv, behind GEO_GAZETTEER_SEED (default true;
//!   read here directly via std::env because config.rs is Scope B's file —
//!   documented deviation, same SCREAMING_SNAKE + truthy parsing idiom as
//!   config.rs). Seeding failure aborts the boot when enabled: a silently
//!   empty gazetteer would break the field-pwa offline contract.

use axum::body::Bytes;
use axum::extract::{FromRequest, Multipart, Query, State};
use axum::http::{header, HeaderMap, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde::Serialize;
use sqlx::PgPool;

use crate::auth;
use crate::AppState;

/// Gazetteer kinds (CHECK constraint in 0002_gazetteer.sql).
pub const KINDS: [&str; 3] = ["state", "lga", "locality"];
/// Import payload cap: the same 100 MB geo import cap as boundary import
/// (SPEC-W63 G1: stays under the raised 110M clamd stream cap).
pub const MAX_IMPORT_BYTES: usize = crate::import::MAX_IMPORT_BYTES;
/// Row cap for one gazetteer import (localities tier is the design maximum:
/// GeoNames NG-scale, ~50-100k rows — SPEC-W63 G3).
pub const MAX_IMPORT_ROWS: usize = 200_000;
/// errors[] is advisory; cap its length (import.rs idiom).
const MAX_ERROR_ENTRIES: usize = 100;
/// Search result cap (the snapshot endpoint is the full-dump path).
const DEFAULT_SEARCH_LIMIT: i64 = 200;
const MAX_SEARCH_LIMIT: i64 = 2_000;

/// Bundled Nigeria reference seed (derived from
/// scripts/seeds/data/nigeria_lgas.csv — READ-ONLY source): 37 states
/// (incl. FCT) + 774 LGAs, source='nigeria_lgas.csv (public)'.
const SEED_CSV: &str = include_str!("../seeds/nigeria_gazetteer.csv");

fn err_json(status: StatusCode, msg: impl Into<String>) -> (StatusCode, Json<serde_json::Value>) {
    (status, Json(serde_json::json!({ "error": msg.into() })))
}

// ---------------------------------------------------------------------------
// Boot: migration + seed
// ---------------------------------------------------------------------------

/// Apply migrations/0002_gazetteer.sql ONCE at boot (store.rs
/// apply_geo_migration idiom: sqlx::raw_sql, idempotent guards, and a
/// failure ABORTS the boot — never serve without the RLS policy).
pub async fn apply_gazetteer_migration(pool: &PgPool) -> Result<(), String> {
    sqlx::raw_sql(include_str!("../migrations/0002_gazetteer.sql"))
        .execute(pool)
        .await
        .map_err(|e| format!("gazetteer migration (0002_gazetteer.sql) failed: {e}"))?;
    tracing::info!(
        "gazetteer migration applied (0002_gazetteer.sql: geo_places + ENABLE+FORCE RLS, NULL-reference policy)"
    );
    Ok(())
}

/// GEO_GAZETTEER_SEED (default true). Read directly from the environment:
/// config.rs is Scope B's file this wave, so the knob lives here (the
/// SCREAMING_SNAKE + truthy-parse idiom mirrors config.rs::env_bool, with
/// the default flipped to ON). B documents the var in .env.example.
pub fn seed_enabled() -> bool {
    std::env::var("GEO_GAZETTEER_SEED")
        .map(|v| !matches!(v.trim().to_ascii_lowercase().as_str(), "0" | "false" | "no"))
        .unwrap_or(true)
}

/// Boot-time reference seed: idempotent INSERT ... ON CONFLICT (kind, code)
/// DO NOTHING from the bundled CSV (never overwrites operator/imported
/// edits). Fail-closed when enabled (a silently empty gazetteer breaks the
/// field-pwa offline contract). Runs with the RLS GUC pinned to the zero
/// uuid (reference-scope write idiom, src/import.rs).
pub async fn seed_boot(pool: &PgPool) -> Result<(), String> {
    if !seed_enabled() {
        tracing::info!("GEO_GAZETTEER_SEED=false: skipping gazetteer reference seed");
        return Ok(());
    }
    let outcome = parse_csv_records(SEED_CSV.as_bytes(), "")
        .map_err(|e| format!("bundled gazetteer seed is unparseable: {e}"))?;
    let states = outcome.records.iter().filter(|r| r.kind == "state").count();
    let lgas = outcome.records.iter().filter(|r| r.kind == "lga").count();
    if states != 37 || lgas != 774 || outcome.records.len() != 811 || outcome.skipped != 0 {
        return Err(format!(
            "bundled gazetteer seed corrupt: {} states / {} lgas / {} skipped (expected 37/774/0)",
            states, lgas, outcome.skipped
        ));
    }
    let mut tx = crate::store::begin_tenant_tx(pool, uuid::Uuid::nil())
        .await
        .map_err(|e| format!("gazetteer seed tx: {e}"))?;
    let mut inserted = 0u64;
    for rec in &outcome.records {
        let res = sqlx::query(
            "INSERT INTO geo_places (tenant_id, kind, name, code, parent_code, lat, lng, source) \
             VALUES (NULL, $1, $2, $3, $4, $5, $6, $7) \
             ON CONFLICT (kind, code) DO NOTHING",
        )
        .bind(&rec.kind)
        .bind(&rec.name)
        .bind(&rec.code)
        .bind(&rec.parent_code)
        .bind(rec.lat)
        .bind(rec.lng)
        .bind(&rec.source)
        .execute(&mut *tx)
        .await
        .map_err(|e| format!("gazetteer seed row '{}': {e}", rec.code))?;
        inserted += res.rows_affected();
    }
    tx.commit()
        .await
        .map_err(|e| format!("gazetteer seed commit: {e}"))?;
    tracing::info!(
        inserted,
        total = outcome.records.len(),
        "gazetteer reference seed applied (idempotent)"
    );
    Ok(())
}

// ---------------------------------------------------------------------------
// Slug + sha256 (no new crate deps — Cargo.toml is not in this scope)
// ---------------------------------------------------------------------------

/// PINNED gazetteer code slug rule (SPEC-W63 §1): uppercase, spaces and '/'
/// become '-', every other non-alphanumeric (punctuation, apostrophes, ...)
/// is stripped. Applied to names to derive codes and to normalize
/// caller-supplied codes.
pub fn slug(name: &str) -> String {
    name.trim()
        .chars()
        .map(|c| match c {
            ' ' | '/' => '-',
            c => c.to_ascii_uppercase(),
        })
        .filter(|c| c.is_ascii_alphanumeric() || *c == '-')
        .collect()
}

/// Normalize a caller/importer-supplied code for the upsert key
/// (seed codes are already in this shape).
fn normalize_code(code: &str) -> String {
    code.trim().to_ascii_uppercase()
}

const SHA256_K: [u32; 64] = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
];

/// SHA-256 hex digest (FIPS 180-4). Hand-rolled to keep Cargo.toml untouched
/// (the crate has no hash dependency and Cargo.toml is not in this scope);
/// used only for the snapshot ETag content hash. Verified against standard
/// test vectors in the unit tests below.
pub fn sha256_hex(data: &[u8]) -> String {
    let mut h: [u32; 8] = [
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
        0x5be0cd19,
    ];
    let mut msg = data.to_vec();
    let bit_len = (data.len() as u64).wrapping_mul(8);
    msg.push(0x80);
    while msg.len() % 64 != 56 {
        msg.push(0);
    }
    msg.extend_from_slice(&bit_len.to_be_bytes());

    for chunk in msg.chunks_exact(64) {
        let mut w = [0u32; 64];
        for (i, word) in w.iter_mut().take(16).enumerate() {
            *word = u32::from_be_bytes([
                chunk[4 * i],
                chunk[4 * i + 1],
                chunk[4 * i + 2],
                chunk[4 * i + 3],
            ]);
        }
        for i in 16..64 {
            let s0 = w[i - 15].rotate_right(7) ^ w[i - 15].rotate_right(18) ^ (w[i - 15] >> 3);
            let s1 = w[i - 2].rotate_right(17) ^ w[i - 2].rotate_right(19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16]
                .wrapping_add(s0)
                .wrapping_add(w[i - 7])
                .wrapping_add(s1);
        }
        let (mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut hh) =
            (h[0], h[1], h[2], h[3], h[4], h[5], h[6], h[7]);
        for i in 0..64 {
            let s1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
            let ch = (e & f) ^ ((!e) & g);
            let t1 = hh
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(SHA256_K[i])
                .wrapping_add(w[i]);
            let s0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
            let maj = (a & b) ^ (a & c) ^ (b & c);
            let t2 = s0.wrapping_add(maj);
            hh = g;
            g = f;
            f = e;
            e = d.wrapping_add(t1);
            d = c;
            c = b;
            b = a;
            a = t1.wrapping_add(t2);
        }
        h[0] = h[0].wrapping_add(a);
        h[1] = h[1].wrapping_add(b);
        h[2] = h[2].wrapping_add(c);
        h[3] = h[3].wrapping_add(d);
        h[4] = h[4].wrapping_add(e);
        h[5] = h[5].wrapping_add(f);
        h[6] = h[6].wrapping_add(g);
        h[7] = h[7].wrapping_add(hh);
    }

    let mut out = String::with_capacity(64);
    for word in h {
        out.push_str(&format!("{word:08x}"));
    }
    out
}

// ---------------------------------------------------------------------------
// Records + parsing (CSV / GeoJSON / JSON)
// ---------------------------------------------------------------------------

/// One gazetteer row as served by search/snapshot (reference rows only).
#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct Place {
    pub kind: String,
    pub code: String,
    pub name: String,
    pub parent_code: Option<String>,
    pub lat: Option<f64>,
    pub lng: Option<f64>,
    pub source: String,
}

/// One validated place ready for the upsert (import + seed share this).
#[derive(Debug, Clone)]
pub struct PlaceRecord {
    pub kind: String,
    pub code: String,
    pub name: String,
    pub parent_code: Option<String>,
    pub lat: Option<f64>,
    pub lng: Option<f64>,
    pub source: String,
}

#[derive(Debug, Default)]
pub struct ParseOutcome {
    pub records: Vec<PlaceRecord>,
    pub skipped: usize,
    pub errors: Vec<String>,
}

impl ParseOutcome {
    fn push_error(&mut self, msg: String) {
        if self.errors.len() < MAX_ERROR_ENTRIES {
            self.errors.push(msg);
        }
    }
    fn skip(&mut self, msg: String) {
        self.skipped += 1;
        self.push_error(msg);
    }
}

/// Validate + normalize one raw row. `default_source` fills an empty source
/// (the import-level `source` field). Derives the code when empty using the
/// pinned slug rule: state -> NG-<NAME>, lga/locality ->
/// <PARENT_CODE>-<NAME> (parent_code required then).
fn normalize_record(
    kind: &str,
    code: &str,
    name: &str,
    parent_code: Option<&str>,
    lat: Option<f64>,
    lng: Option<f64>,
    source: &str,
    default_source: &str,
) -> Result<PlaceRecord, String> {
    let kind = kind.trim().to_ascii_lowercase();
    if !KINDS.contains(&kind.as_str()) {
        return Err(format!("kind must be one of {KINDS:?}, got '{kind}'"));
    }
    let name = name.trim().to_string();
    if name.is_empty() {
        return Err("name is required".to_string());
    }
    let parent_code = parent_code
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(normalize_code);
    let mut code = normalize_code(code);
    if code.is_empty() {
        code = if kind == "state" {
            format!("NG-{}", slug(&name))
        } else {
            match &parent_code {
                Some(p) => format!("{}-{}", p, slug(&name)),
                None => {
                    return Err(format!(
                        "code is required for a {kind} without parent_code (cannot derive)"
                    ))
                }
            }
        };
    }
    match (lat, lng) {
        (Some(la), Some(lg)) => {
            if !(-90.0..=90.0).contains(&la) || !(-180.0..=180.0).contains(&lg) {
                return Err(format!("lat/lng out of range ({la}, {lg})"));
            }
        }
        (None, None) => {}
        _ => return Err("lat and lng must be given together".to_string()),
    }
    let source = {
        let s = source.trim();
        if s.is_empty() {
            default_source.trim().to_string()
        } else {
            s.to_string()
        }
    };
    Ok(PlaceRecord {
        kind,
        code,
        name,
        parent_code,
        lat,
        lng,
        source,
    })
}

/// Minimal RFC-4180-ish CSV reader (no csv crate in the dependency set):
/// quoted fields with "" escapes, commas and CRLF inside quotes, optional
/// UTF-8 BOM. Returns raw rows INCLUDING the header row.
pub fn parse_csv(bytes: &[u8]) -> Result<Vec<Vec<String>>, String> {
    let text =
        std::str::from_utf8(bytes).map_err(|e| format!("CSV payload must be UTF-8: {e}"))?;
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let mut rows: Vec<Vec<String>> = Vec::new();
    let mut row: Vec<String> = Vec::new();
    let mut field = String::new();
    let mut in_quotes = false;
    let mut at_field_start = true;
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        if in_quotes {
            if c == '"' {
                if chars.peek() == Some(&'"') {
                    chars.next();
                    field.push('"');
                } else {
                    in_quotes = false;
                }
            } else {
                field.push(c);
            }
        } else {
            match c {
                '"' if at_field_start => {
                    in_quotes = true;
                    at_field_start = false;
                }
                ',' => {
                    row.push(std::mem::take(&mut field));
                    at_field_start = true;
                }
                '\n' => {
                    row.push(std::mem::take(&mut field));
                    rows.push(std::mem::take(&mut row));
                    at_field_start = true;
                }
                '\r' if chars.peek() == Some(&'\n') => {
                    chars.next();
                    row.push(std::mem::take(&mut field));
                    rows.push(std::mem::take(&mut row));
                    at_field_start = true;
                }
                _ => {
                    field.push(c);
                    at_field_start = false;
                }
            }
        }
    }
    if in_quotes {
        return Err("unterminated quoted CSV field".to_string());
    }
    if !field.is_empty() || !row.is_empty() {
        row.push(field);
        rows.push(row);
    }
    Ok(rows)
}

/// CSV gazetteer payload: header row with at least `kind` and `name`
/// (`code`, `parent_code`, `lat`, `lng`, `source` optional; unknown columns
/// ignored). `default_source` is the import-level source field.
pub fn parse_csv_records(bytes: &[u8], default_source: &str) -> Result<ParseOutcome, String> {
    let rows = parse_csv(bytes)?;
    let Some(header) = rows.first() else {
        return Err("empty CSV (a header row is required)".to_string());
    };
    let col = |name: &str| {
        header
            .iter()
            .position(|h| h.trim().eq_ignore_ascii_case(name))
    };
    let c_kind = col("kind").ok_or("CSV header must include a 'kind' column")?;
    let c_name = col("name").ok_or("CSV header must include a 'name' column")?;
    let c_code = col("code");
    let c_parent = col("parent_code");
    let c_lat = col("lat");
    let c_lng = col("lng");
    let c_source = col("source");

    let mut out = ParseOutcome::default();
    for (i, row) in rows.iter().skip(1).enumerate() {
        let get = |idx: Option<usize>| -> &str {
            idx.and_then(|j| row.get(j)).map(|s| s.as_str()).unwrap_or("")
        };
        let label = format!("row {}", i + 2);
        let parse_num = |idx: Option<usize>| -> Result<Option<f64>, String> {
            let raw = get(idx).trim();
            if raw.is_empty() {
                return Ok(None);
            }
            raw.parse::<f64>()
                .map(Some)
                .map_err(|e| format!("{label}: invalid number '{raw}': {e}"))
        };
        let rec = match (|| {
            let lat = parse_num(c_lat)?;
            let lng = parse_num(c_lng)?;
            normalize_record(
                get(Some(c_kind)),
                get(c_code),
                get(Some(c_name)),
                Some(get(c_parent)),
                lat,
                lng,
                get(c_source),
                default_source,
            )
        })() {
            Ok(r) => r,
            Err(e) => {
                out.skip(format!("{label}: {e}"));
                continue;
            }
        };
        if out.records.len() >= MAX_IMPORT_ROWS {
            out.skip(format!("{label}: beyond the {MAX_IMPORT_ROWS}-row import cap"));
            continue;
        }
        out.records.push(rec);
    }
    Ok(out)
}

/// JSON gazetteer payload: a bare array of place objects, or
/// `{"places": [...]}`. Fields mirror the CSV columns.
pub fn parse_json_records(
    value: &serde_json::Value,
    default_source: &str,
) -> Result<ParseOutcome, String> {
    let items: &Vec<serde_json::Value> = if let Some(arr) = value.as_array() {
        arr
    } else if let Some(arr) = value.get("places").and_then(|p| p.as_array()) {
        arr
    } else {
        return Err("JSON payload must be an array of places or {\"places\": [...]}".to_string());
    };
    let mut out = ParseOutcome::default();
    for (i, item) in items.iter().enumerate() {
        let label = format!("entry {}", i + 1);
        let Some(obj) = item.as_object() else {
            out.skip(format!("{label}: not an object"));
            continue;
        };
        let s = |k: &str| obj.get(k).and_then(|v| v.as_str()).unwrap_or("");
        let n = |k: &str| -> Result<Option<f64>, String> {
            match obj.get(k) {
                None | Some(serde_json::Value::Null) => Ok(None),
                Some(v) => v
                    .as_f64()
                    .map(Some)
                    .ok_or_else(|| format!("{label}: '{k}' must be a number")),
            }
        };
        let rec = match (|| {
            let lat = n("lat")?;
            let lng = n("lng")?;
            normalize_record(
                s("kind"),
                s("code"),
                s("name"),
                obj.get("parent_code").and_then(|v| v.as_str()),
                lat,
                lng,
                s("source"),
                default_source,
            )
        })() {
            Ok(r) => r,
            Err(e) => {
                out.skip(format!("{label}: {e}"));
                continue;
            }
        };
        if out.records.len() >= MAX_IMPORT_ROWS {
            out.skip(format!("{label}: beyond the {MAX_IMPORT_ROWS}-row import cap"));
            continue;
        }
        out.records.push(rec);
    }
    Ok(out)
}

/// GeoJSON gazetteer payload: a FeatureCollection whose features carry
/// kind/code/name/parent_code/source in `properties`. Point geometries fill
/// lat/lng; other geometry types import WITHOUT coordinates (the gazetteer
/// is a name/code table — polygons belong in geo_boundaries).
pub fn parse_geojson_records(
    value: &serde_json::Value,
    default_source: &str,
) -> Result<ParseOutcome, String> {
    if value.get("type").and_then(|t| t.as_str()) != Some("FeatureCollection") {
        return Err("GeoJSON payload must be a FeatureCollection".to_string());
    }
    let features = value
        .get("features")
        .and_then(|f| f.as_array())
        .ok_or("GeoJSON FeatureCollection must carry a features array")?;
    let mut out = ParseOutcome::default();
    for (i, feat) in features.iter().enumerate() {
        let label = format!("feature {}", i + 1);
        let props = feat
            .get("properties")
            .and_then(|p| p.as_object())
            .cloned()
            .unwrap_or_default();
        let s = |k: &str| props.get(k).and_then(|v| v.as_str()).unwrap_or("");
        // Point => [lng, lat]; anything else => no coordinates.
        let (lat, lng) = match feat.get("geometry") {
            Some(geom)
                if geom.get("type").and_then(|t| t.as_str()) == Some("Point") =>
            {
                match geom.get("coordinates").and_then(|c| c.as_array()) {
                    Some(coords) if coords.len() >= 2 => {
                        match (coords[1].as_f64(), coords[0].as_f64()) {
                            (Some(la), Some(lg)) => (Some(la), Some(lg)),
                            _ => {
                                out.skip(format!("{label}: Point coordinates must be numbers"));
                                continue;
                            }
                        }
                    }
                    _ => {
                        out.skip(format!("{label}: Point geometry without coordinates"));
                        continue;
                    }
                }
            }
            _ => (None, None),
        };
        let rec = match normalize_record(
            s("kind"),
            s("code"),
            s("name"),
            props.get("parent_code").and_then(|v| v.as_str()),
            lat,
            lng,
            s("source"),
            default_source,
        ) {
            Ok(r) => r,
            Err(e) => {
                out.skip(format!("{label}: {e}"));
                continue;
            }
        };
        if out.records.len() >= MAX_IMPORT_ROWS {
            out.skip(format!("{label}: beyond the {MAX_IMPORT_ROWS}-row import cap"));
            continue;
        }
        out.records.push(rec);
    }
    Ok(out)
}

// ---------------------------------------------------------------------------
// GET /v1/geo/gazetteer — search/list
// ---------------------------------------------------------------------------

#[derive(Debug, serde::Deserialize)]
pub struct SearchQuery {
    kind: Option<String>,
    state: Option<String>,
    q: Option<String>,
    limit: Option<i64>,
}

/// Deterministic presentational order: states, then LGAs, then localities.
const KIND_ORDER: &str =
    "CASE kind WHEN 'state' THEN 0 WHEN 'lga' THEN 1 WHEN 'locality' THEN 2 ELSE 3 END";

/// Normalize the `state` filter to a state CODE: accepts a name ("Lagos"),
/// a bare slug ("lagos"), or a full code ("NG-LAGOS").
fn state_filter_code(raw: &str) -> String {
    let t = raw.trim();
    if t.to_ascii_uppercase().starts_with("NG-") {
        t.to_ascii_uppercase()
    } else {
        format!("NG-{}", slug(t))
    }
}

pub async fn search(
    State(state): State<AppState>,
    Query(q): Query<SearchQuery>,
) -> Result<Json<serde_json::Value>, (StatusCode, Json<serde_json::Value>)> {
    let kind = q.kind.as_deref().map(str::trim).map(str::to_ascii_lowercase);
    if let Some(k) = &kind {
        if !KINDS.contains(&k.as_str()) {
            return Err(err_json(
                StatusCode::BAD_REQUEST,
                format!("kind must be one of {KINDS:?}"),
            ));
        }
    }
    let state_code = q
        .state
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(state_filter_code);
    let needle = q
        .q
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string);
    let limit = q
        .limit
        .unwrap_or(DEFAULT_SEARCH_LIMIT)
        .clamp(1, MAX_SEARCH_LIMIT);

    // Reference rows only (world-visible under the NULL-reference policy;
    // no tenant GUC needed — the enrich.rs reference-pass idiom). The state
    // filter matches the state row itself plus LGAs (parent_code = the state
    // code) and localities (parent_code = NG-<STATE>-<LGA>, prefix match).
    let places: Vec<Place> = sqlx::query_as(&format!(
        "SELECT kind, code, name, parent_code, lat, lng, source FROM geo_places \
         WHERE tenant_id IS NULL \
           AND ($1::text IS NULL OR kind = $1) \
           AND ($2::text IS NULL OR code = $2 OR parent_code = $2 \
                OR parent_code LIKE $2 || '-%') \
           AND ($3::text IS NULL OR strpos(lower(name), lower($3)) > 0 \
                OR strpos(lower(code), lower($3)) > 0) \
         ORDER BY {KIND_ORDER}, code \
         LIMIT $4"
    ))
    .bind(kind)
    .bind(state_code)
    .bind(needle)
    .bind(limit)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| {
        err_json(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("gazetteer search query: {e}"),
        )
    })?;

    Ok(Json(serde_json::json!({
        "places": places,
        "count": places.len(),
    })))
}

// ---------------------------------------------------------------------------
// GET /v1/geo/gazetteer/snapshot — full dump + ETag
// ---------------------------------------------------------------------------

#[derive(Debug, serde::Deserialize)]
pub struct SnapshotQuery {
    levels: Option<String>,
}

/// Canonical snapshot body: fixed field order, no whitespace, `null` (not
/// omitted) for absent optional fields — byte-stable for a given row set,
/// which is what makes the sha256 ETag meaningful.
#[derive(Serialize)]
struct SnapshotBody {
    levels: Vec<String>,
    places: Vec<Place>,
}

/// Parse + validate the `levels` query param. Absent/empty = all kinds, in
/// canonical order (independent of the caller's parameter order, so the
/// ETag is stable across client spellings).
fn parse_levels(raw: Option<&str>) -> Result<Vec<String>, (StatusCode, Json<serde_json::Value>)> {
    let mut levels: Vec<String> = match raw {
        Some(r) if !r.trim().is_empty() => r
            .split(',')
            .map(|s| s.trim().to_ascii_lowercase())
            .filter(|s| !s.is_empty())
            .collect(),
        _ => KINDS.iter().map(|s| s.to_string()).collect(),
    };
    for l in &levels {
        if !KINDS.contains(&l.as_str()) {
            return Err(err_json(
                StatusCode::BAD_REQUEST,
                format!("levels must be a comma-separated subset of {KINDS:?}, got '{l}'"),
            ));
        }
    }
    // Canonical order + dedup.
    levels.sort_by_key(|l| KINDS.iter().position(|k| k == l).unwrap_or(usize::MAX));
    levels.dedup();
    Ok(levels)
}

/// RFC 7232 If-None-Match evaluation: exact match on the strong etag,
/// weak-comparison tolerant (W/ prefix), '*' matches any current etag.
fn etag_matches(if_none_match: Option<&HeaderValue>, etag: &str) -> bool {
    let Some(v) = if_none_match.and_then(|v| v.to_str().ok()) else {
        return false;
    };
    v.split(',').any(|candidate| {
        let c = candidate.trim();
        c == "*" || c == etag || c.strip_prefix("W/") == Some(etag)
    })
}

pub async fn snapshot(
    State(state): State<AppState>,
    headers: HeaderMap,
    Query(q): Query<SnapshotQuery>,
) -> Result<Response, (StatusCode, Json<serde_json::Value>)> {
    let levels = parse_levels(q.levels.as_deref())?;
    let places: Vec<Place> = sqlx::query_as(&format!(
        "SELECT kind, code, name, parent_code, lat, lng, source FROM geo_places \
         WHERE tenant_id IS NULL AND kind = ANY($1) \
         ORDER BY {KIND_ORDER}, code"
    ))
    .bind(&levels)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| {
        err_json(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("gazetteer snapshot query: {e}"),
        )
    })?;

    let body = serde_json::to_string(&SnapshotBody { levels, places })
        .map_err(|e| err_json(StatusCode::INTERNAL_SERVER_ERROR, format!("snapshot encode: {e}")))?;
    let etag = format!("\"{}\"", sha256_hex(body.as_bytes()));

    let not_modified = etag_matches(headers.get(header::IF_NONE_MATCH), &etag);
    let mut resp = if not_modified {
        StatusCode::NOT_MODIFIED.into_response()
    } else {
        (StatusCode::OK, body).into_response()
    };
    let is_ok = resp.status() == StatusCode::OK;
    let h = resp.headers_mut();
    h.insert(
        header::ETAG,
        HeaderValue::from_str(&etag).expect("sha256 hex etag is valid header text"),
    );
    // no-cache: clients may store the snapshot, but must revalidate (the
    // ETag round-trip above) before reusing it.
    h.insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
    if is_ok {
        h.insert(header::CONTENT_TYPE, HeaderValue::from_static("application/json"));
    }
    Ok(resp)
}

// ---------------------------------------------------------------------------
// POST /v1/geo/gazetteer/import — CSV / GeoJSON / JSON, AV-scanned, upsert
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum GazetteerFormat {
    Csv,
    GeoJson,
    Json,
}

/// Pinned W63 cross-coder AV mapping (SPEC-W63 "Handler-side mapping"): when
/// `state.av` is Some, scan the raw import bytes BEFORE any parsing;
/// Infected => 415 {error:"infected", signature}; scan error => 503
/// {error:"av_unavailable"} when `state.av_required`, else structured log +
/// proceed. geo-service has no metrics idiom (no counter infra in the
/// crate), so outcomes are structured logs only (SPEC-W63 §4 fallback).
async fn av_scan_import(
    state: &AppState,
    bytes: &[u8],
) -> Result<(), (StatusCode, Json<serde_json::Value>)> {
    let Some(av) = &state.av else {
        return Ok(());
    };
    match av.scan(bytes).await {
        Ok(crate::av::AvVerdict::Clean) => {
            tracing::info!(geo_av_scan = "clean", bytes = bytes.len(), "gazetteer import AV scan");
            Ok(())
        }
        Ok(crate::av::AvVerdict::Infected(signature)) => {
            tracing::warn!(geo_av_scan = "infected", signature = %signature, "gazetteer import rejected by AV scan");
            Err((
                StatusCode::UNSUPPORTED_MEDIA_TYPE,
                Json(serde_json::json!({
                    "error": "infected",
                    "signature": signature,
                })),
            ))
        }
        Err(e) => {
            if state.av_required {
                tracing::error!(geo_av_scan = "error", error = %e, "gazetteer import blocked: AV unavailable (CLAMAV_REQUIRED=true)");
                // Pinned body shape: exactly {"error":"av_unavailable"}.
                Err(err_json(StatusCode::SERVICE_UNAVAILABLE, "av_unavailable"))
            } else {
                tracing::warn!(geo_av_scan = "skipped", error = %e, "AV scan failed; proceeding unscanned (CLAMAV_REQUIRED=false)");
                Ok(())
            }
        }
    }
}

#[derive(Debug, serde::Deserialize)]
struct GazUrlImportBody {
    url: String,
    #[serde(default)]
    source: Option<String>,
}

/// POST /v1/geo/gazetteer/import. Body shapes:
///   * multipart/form-data: `file` (binary, capped) OR `url`, plus optional
///     `format` (csv|geojson|json) and `source` fields;
///   * text/csv, application/geo+json: the raw payload;
///   * application/json: a places payload (array / {"places":[...]} /
///     FeatureCollection) OR {"url": "...", "source": "..."}.
pub async fn import(
    State(state): State<AppState>,
    headers: HeaderMap,
    req: axum::extract::Request,
) -> Result<Json<serde_json::Value>, (StatusCode, Json<serde_json::Value>)> {
    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_ascii_lowercase();

    let (bytes, format, default_source) = if content_type.starts_with("multipart/form-data") {
        let multipart = Multipart::from_request(req, &()).await.map_err(|e| {
            err_json(
                StatusCode::BAD_REQUEST,
                format!("invalid multipart body: {e}"),
            )
        })?;
        read_gaz_multipart(&state, multipart).await?
    } else if content_type.starts_with("text/csv") {
        (read_raw_body(req).await?, GazetteerFormat::Csv, String::new())
    } else if content_type.starts_with("application/geo+json") {
        (
            read_raw_body(req).await?,
            GazetteerFormat::GeoJson,
            String::new(),
        )
    } else if content_type.starts_with("application/json") {
        let raw = read_raw_body(req).await?;
        // {url} JSON body goes through the fetch path (import.rs idiom).
        if let Ok(probe) = serde_json::from_slice::<serde_json::Value>(&raw) {
            if probe.get("url").and_then(|u| u.as_str()).is_some() {
                let body: GazUrlImportBody = serde_json::from_value(probe).map_err(|e| {
                    err_json(StatusCode::BAD_REQUEST, format!("invalid JSON body: {e}"))
                })?;
                let (bytes, fmt) = fetch_url(&state, &body.url).await?;
                (bytes, fmt, body.source.unwrap_or_default())
            } else {
                let fmt = sniff_json_format(&raw)?;
                (raw, fmt, String::new())
            }
        } else {
            return Err(err_json(StatusCode::BAD_REQUEST, "invalid JSON body"));
        }
    } else {
        return Err(err_json(
            StatusCode::UNSUPPORTED_MEDIA_TYPE,
            "unsupported content type: use multipart/form-data, text/csv, \
             application/geo+json, or application/json",
        ));
    };

    // Authz: Permify manage_bookings on organization:{tenant} / user:{sub}
    // (src/auth.rs idiom; reference rows are world-visible, so the import is
    // gated exactly like reference-scope boundary import).
    let gate_tenant = auth::tenant_uuid(&headers).ok_or_else(|| {
        err_json(
            StatusCode::BAD_REQUEST,
            "X-Tenant-Slug (tenant uuid) is required for gazetteer import",
        )
    })?;
    let verdict = auth::authorize_import(
        &state.permify,
        state.config.authz_disabled,
        &headers,
        gate_tenant,
    )
    .await;
    if verdict != auth::Authz::Allow {
        return Err(err_json(verdict.status(), verdict.message()));
    }

    // AV scan of the raw bytes BEFORE any parsing (pinned mapping).
    av_scan_import(&state, &bytes).await?;

    let outcome = match format {
        GazetteerFormat::Csv => parse_csv_records(&bytes, &default_source),
        GazetteerFormat::GeoJson => serde_json::from_slice::<serde_json::Value>(&bytes)
            .map_err(|e| format!("invalid GeoJSON: {e}"))
            .and_then(|v| parse_geojson_records(&v, &default_source)),
        GazetteerFormat::Json => serde_json::from_slice::<serde_json::Value>(&bytes)
            .map_err(|e| format!("invalid JSON: {e}"))
            .and_then(|v| parse_json_records(&v, &default_source)),
    }
    .map_err(|e| err_json(StatusCode::UNPROCESSABLE_ENTITY, e))?;

    if outcome.records.is_empty() {
        return Err(err_json(
            StatusCode::UNPROCESSABLE_ENTITY,
            format!(
                "no importable places ({} skipped: {})",
                outcome.skipped,
                outcome.errors.first().cloned().unwrap_or_default()
            ),
        ));
    }

    let (imported, db_errors) = insert_places(&state.pool, &outcome.records)
        .await
        .map_err(|e| err_json(StatusCode::INTERNAL_SERVER_ERROR, e))?;

    let mut errors = outcome.errors;
    errors.extend(db_errors);
    Ok(Json(serde_json::json!({
        "imported": imported,
        "skipped": outcome.skipped,
        "errors": errors,
    })))
}

/// Read a raw (non-multipart) body with the 100 MB cap. The router's
/// DefaultBodyLimit already enforces the cap for this extractor; map its
/// rejection to a house-style error.
async fn read_raw_body(req: axum::extract::Request) -> Result<Bytes, (StatusCode, Json<serde_json::Value>)> {
    Bytes::from_request(req, &()).await.map_err(|e| {
        err_json(
            StatusCode::BAD_REQUEST,
            format!("invalid body (payload cap is {MAX_IMPORT_BYTES} bytes): {e}"),
        )
    })
}

/// Sniff a .json/unknown-extension payload: a FeatureCollection is GeoJSON,
/// anything else is the places-JSON shape.
fn sniff_json_format(bytes: &[u8]) -> Result<GazetteerFormat, (StatusCode, Json<serde_json::Value>)> {
    let v: serde_json::Value = serde_json::from_slice(bytes)
        .map_err(|e| err_json(StatusCode::UNPROCESSABLE_ENTITY, format!("invalid JSON: {e}")))?;
    if v.get("type").and_then(|t| t.as_str()) == Some("FeatureCollection") {
        Ok(GazetteerFormat::GeoJson)
    } else {
        Ok(GazetteerFormat::Json)
    }
}

/// Detect the gazetteer format from an upload file name (.csv / .geojson /
/// .json sniffed) unless a `format` field pinned it.
fn format_from_name(
    file_name: &str,
    explicit: Option<&str>,
) -> Result<GazetteerFormat, (StatusCode, Json<serde_json::Value>)> {
    if let Some(f) = explicit.map(str::trim).filter(|s| !s.is_empty()) {
        return match f.to_ascii_lowercase().as_str() {
            "csv" => Ok(GazetteerFormat::Csv),
            "geojson" => Ok(GazetteerFormat::GeoJson),
            "json" => Ok(GazetteerFormat::Json),
            other => Err(err_json(
                StatusCode::BAD_REQUEST,
                format!("format must be csv|geojson|json, got '{other}'"),
            )),
        };
    }
    let lower = file_name.to_ascii_lowercase();
    if lower.ends_with(".csv") {
        Ok(GazetteerFormat::Csv)
    } else if lower.ends_with(".geojson") {
        Ok(GazetteerFormat::GeoJson)
    } else if lower.ends_with(".json") {
        Ok(GazetteerFormat::Json)
    } else {
        Err(err_json(
            StatusCode::UNPROCESSABLE_ENTITY,
            format!(
                "unsupported file '{file_name}': expected .csv/.geojson/.json (or pass format=)"
            ),
        ))
    }
}

/// Multipart form: `file` (binary, capped) OR `url`, plus optional `format`
/// and `source` fields.
async fn read_gaz_multipart(
    state: &AppState,
    mut mp: Multipart,
) -> Result<(Bytes, GazetteerFormat, String), (StatusCode, Json<serde_json::Value>)> {
    let mut file: Option<(Bytes, String)> = None;
    let mut url: Option<String> = None;
    let mut format: Option<String> = None;
    let mut source: Option<String> = None;

    while let Some(mut field) = mp.next_field().await.map_err(|e| {
        err_json(
            StatusCode::BAD_REQUEST,
            format!("multipart field error (payload cap is {MAX_IMPORT_BYTES} bytes): {e}"),
        )
    })? {
        match field.name().unwrap_or_default() {
            "file" => {
                let name = field
                    .file_name()
                    .map(|s| s.to_string())
                    .unwrap_or_else(|| "upload.csv".to_string());
                let mut buf = Vec::new();
                while let Some(chunk) = field.chunk().await.map_err(|e| {
                    err_json(StatusCode::BAD_REQUEST, format!("multipart read: {e}"))
                })? {
                    if buf.len() + chunk.len() > MAX_IMPORT_BYTES {
                        return Err(err_json(
                            StatusCode::PAYLOAD_TOO_LARGE,
                            format!("file exceeds the {MAX_IMPORT_BYTES}-byte import cap"),
                        ));
                    }
                    buf.extend_from_slice(&chunk);
                }
                file = Some((Bytes::from(buf), name));
            }
            "url" => url = Some(field.text().await.unwrap_or_default()),
            "format" => format = Some(field.text().await.unwrap_or_default()),
            "source" => source = Some(field.text().await.unwrap_or_default()),
            _ => {}
        }
    }

    let default_source = source.unwrap_or_default();
    if let Some((bytes, name)) = file {
        let fmt = format_from_name(&name, format.as_deref())?;
        let fmt = if fmt == GazetteerFormat::Json {
            // .json files may be places-JSON or GeoJSON — sniff.
            sniff_json_format(&bytes)?
        } else {
            fmt
        };
        return Ok((bytes, fmt, default_source));
    }
    match url {
        Some(u) if !u.trim().is_empty() => {
            let (bytes, fmt) = fetch_url(state, &u).await?;
            let fmt = match format.as_deref().map(str::trim).filter(|s| !s.is_empty()) {
                Some(_) => format_from_name("", format.as_deref())?,
                None => fmt,
            };
            Ok((bytes, fmt, default_source))
        }
        _ => Err(err_json(
            StatusCode::BAD_REQUEST,
            "multipart import needs a 'file' part or a 'url' field",
        )),
    }
}

/// Fetch a `{url}` gazetteer import (http/https only, 100 MB cap, the shared
/// client's 30 s overall timeout — the import.rs fetch_url idiom,
/// duplicated here because import.rs is Scope B's file this wave).
async fn fetch_url(
    state: &AppState,
    url: &str,
) -> Result<(Bytes, GazetteerFormat), (StatusCode, Json<serde_json::Value>)> {
    let url = url.trim();
    if !(url.starts_with("http://") || url.starts_with("https://")) {
        return Err(err_json(
            StatusCode::BAD_REQUEST,
            "url must be http(s) (local paths are not accepted)",
        ));
    }
    let file_name = url
        .split(['?', '#'])
        .next()
        .unwrap_or_default()
        .rsplit('/')
        .next()
        .filter(|s| !s.is_empty())
        .unwrap_or("download.csv")
        .to_string();
    let resp = state
        .http
        .get(url)
        .send()
        .await
        .map_err(|e| err_json(StatusCode::BAD_GATEWAY, format!("fetch url: {e}")))?;
    if !resp.status().is_success() {
        return Err(err_json(
            StatusCode::BAD_GATEWAY,
            format!("fetch url: upstream status {}", resp.status()),
        ));
    }
    if let Some(len) = resp.content_length() {
        if len as usize > MAX_IMPORT_BYTES {
            return Err(err_json(
                StatusCode::PAYLOAD_TOO_LARGE,
                format!("remote file exceeds the {MAX_IMPORT_BYTES}-byte import cap"),
            ));
        }
    }
    use futures_util::StreamExt;
    let mut buf = Vec::new();
    let mut stream = resp.bytes_stream();
    while let Some(chunk) = stream.next().await {
        let chunk =
            chunk.map_err(|e| err_json(StatusCode::BAD_GATEWAY, format!("fetch url: {e}")))?;
        if buf.len() + chunk.len() > MAX_IMPORT_BYTES {
            return Err(err_json(
                StatusCode::PAYLOAD_TOO_LARGE,
                format!("remote file exceeds the {MAX_IMPORT_BYTES}-byte import cap"),
            ));
        }
        buf.extend_from_slice(&chunk);
    }
    let bytes = Bytes::from(buf);
    let fmt = match format_from_name(&file_name, None) {
        Ok(GazetteerFormat::Json) => sniff_json_format(&bytes)?,
        Ok(f) => f,
        Err(_) => sniff_json_format(&bytes).unwrap_or(GazetteerFormat::Csv),
    };
    Ok((bytes, fmt))
}

/// One transaction, per-row savepoint (import.rs idiom): valid rows commit
/// together; a row Postgres rejects is rolled back to its savepoint and
/// reported — the batch survives it. Upsert key: (kind, code). Reference
/// rows only (tenant_id NULL; the GUC pinned to the zero uuid passes the
/// policy's WITH CHECK).
async fn insert_places(pool: &PgPool, records: &[PlaceRecord]) -> Result<(usize, Vec<String>), String> {
    let mut tx = crate::store::begin_tenant_tx(pool, uuid::Uuid::nil())
        .await
        .map_err(|e| format!("begin gazetteer import tx: {e}"))?;
    let mut imported = 0usize;
    let mut errors: Vec<String> = Vec::new();
    for (i, rec) in records.iter().enumerate() {
        sqlx::query("SAVEPOINT geo_gazetteer_row")
            .execute(&mut *tx)
            .await
            .map_err(|e| format!("savepoint: {e}"))?;
        let res = sqlx::query(
            "INSERT INTO geo_places (tenant_id, kind, name, code, parent_code, lat, lng, source) \
             VALUES (NULL, $1, $2, $3, $4, $5, $6, $7) \
             ON CONFLICT (kind, code) DO UPDATE SET \
               name = EXCLUDED.name, parent_code = EXCLUDED.parent_code, \
               lat = EXCLUDED.lat, lng = EXCLUDED.lng, source = EXCLUDED.source, \
               updated_at = now()",
        )
        .bind(&rec.kind)
        .bind(&rec.name)
        .bind(&rec.code)
        .bind(&rec.parent_code)
        .bind(rec.lat)
        .bind(rec.lng)
        .bind(&rec.source)
        .execute(&mut *tx)
        .await;
        match res {
            Ok(_) => {
                sqlx::query("RELEASE SAVEPOINT geo_gazetteer_row")
                    .execute(&mut *tx)
                    .await
                    .map_err(|e| format!("release savepoint: {e}"))?;
                imported += 1;
            }
            Err(e) => {
                sqlx::query("ROLLBACK TO SAVEPOINT geo_gazetteer_row")
                    .execute(&mut *tx)
                    .await
                    .map_err(|e| format!("rollback to savepoint: {e}"))?;
                if errors.len() < MAX_ERROR_ENTRIES {
                    errors.push(format!("row {} (code '{}'): {e}", i + 1, rec.code));
                }
            }
        }
    }
    tx.commit()
        .await
        .map_err(|e| format!("commit gazetteer import tx: {e}"))?;
    Ok((imported, errors))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn slug_pinned_rule() {
        // Uppercase, spaces/'/' -> '-', strip other punctuation.
        assert_eq!(slug("Lagos"), "LAGOS");
        assert_eq!(slug("Akwa Ibom"), "AKWA-IBOM");
        assert_eq!(slug("Ado-Odo/Ota"), "ADO-ODO-OTA");
        assert_eq!(slug("Jama'are"), "JAMAARE");
        assert_eq!(slug("Ogba/Egbema/Ndoni"), "OGBA-EGBEMA-NDONI");
        assert_eq!(slug("  FCT  "), "FCT");
    }

    #[test]
    fn sha256_standard_vectors() {
        assert_eq!(
            sha256_hex(b""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        assert_eq!(
            sha256_hex(b"abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        // Multi-block (>55 bytes) vector.
        assert_eq!(
            sha256_hex(b"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"),
            "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"
        );
    }

    #[test]
    fn csv_parser_quotes_crlf_bom() {
        let rows = parse_csv(b"\xef\xbb\xbfkind,name,note\r\nstate,Lagos,\"say \"\"hi\"\"\"\r\nlga,\"Ikeja, Main\",x\n")
            .expect("parse");
        assert_eq!(rows.len(), 3);
        assert_eq!(rows[0], vec!["kind", "name", "note"]);
        assert_eq!(rows[1], vec!["state", "Lagos", "say \"hi\""]);
        assert_eq!(rows[2], vec!["lga", "Ikeja, Main", "x"]);
        assert!(parse_csv(b"a,b\n\"unclosed,x").is_err());
    }

    #[test]
    fn bundled_seed_shape() {
        // SPEC-W63 §3 + verification: 37 states (incl FCT) + 774 LGAs, codes
        // match the pinned slug rule, parent linkage intact.
        let out = parse_csv_records(SEED_CSV.as_bytes(), "").expect("seed parses");
        assert_eq!(out.skipped, 0, "{:?}", out.errors);
        assert_eq!(out.records.len(), 811);
        let states: Vec<_> = out.records.iter().filter(|r| r.kind == "state").collect();
        let lgas: Vec<_> = out.records.iter().filter(|r| r.kind == "lga").collect();
        assert_eq!(states.len(), 37);
        assert_eq!(lgas.len(), 774);
        assert!(states.iter().any(|s| s.code == "NG-FCT"));
        let state_codes: std::collections::HashSet<_> =
            states.iter().map(|s| s.code.as_str()).collect();
        let mut seen = std::collections::HashSet::new();
        for r in &out.records {
            assert!(seen.insert(r.code.clone()), "dup code {}", r.code);
            assert!(r.source.contains("nigeria_lgas.csv"));
            match r.kind.as_str() {
                "state" => {
                    assert_eq!(r.code, format!("NG-{}", slug(&r.name)));
                    assert!(r.parent_code.is_none());
                }
                "lga" => {
                    let parent = r.parent_code.as_deref().expect("lga parent");
                    assert!(state_codes.contains(parent), "{parent}");
                    assert_eq!(r.code, format!("{}-{}", parent, slug(&r.name)));
                }
                other => panic!("unexpected kind {other}"),
            }
        }
    }

    #[test]
    fn normalize_record_derives_codes() {
        let r = normalize_record("state", "", "Lagos", None, None, None, "src", "")
            .expect("state");
        assert_eq!(r.code, "NG-LAGOS");
        let r = normalize_record("lga", "", "Ikeja", Some("NG-LAGOS"), None, None, "", "imp")
            .expect("lga");
        assert_eq!(r.code, "NG-LAGOS-IKEJA");
        assert_eq!(r.source, "imp");
        // Codes normalize to uppercase (upsert-key stability).
        let r = normalize_record("lga", "ng-lagos-ikeja", "Ikeja", None, None, None, "", "")
            .expect("lga");
        assert_eq!(r.code, "NG-LAGOS-IKEJA");
        // No derivation possible for a parent-less locality.
        assert!(normalize_record("locality", "", "Somewhere", None, None, None, "", "").is_err());
        // Kind validation + coordinate validation.
        assert!(normalize_record("ward", "", "X", None, None, None, "", "").is_err());
        assert!(normalize_record("state", "", "X", None, Some(95.0), Some(0.0), "", "").is_err());
        assert!(normalize_record("state", "", "X", None, Some(9.0), None, "", "").is_err());
    }

    #[test]
    fn parse_json_and_geojson_records() {
        let v = serde_json::json!([
            {"kind": "state", "name": "Lagos"},
            {"kind": "lga", "name": "Ikeja", "parent_code": "NG-LAGOS", "lat": 6.6, "lng": 3.35},
            {"kind": "ward", "name": "bogus"}
        ]);
        let out = parse_json_records(&v, "test").expect("parse");
        assert_eq!(out.records.len(), 2);
        assert_eq!(out.skipped, 1);
        assert_eq!(out.records[0].code, "NG-LAGOS");
        assert_eq!(out.records[1].lat, Some(6.6));

        let gj = serde_json::json!({
            "type": "FeatureCollection",
            "features": [
                {"type": "Feature", "properties": {"kind": "locality", "name": "Yaba",
                    "parent_code": "NG-LAGOS-LAGOS-MAINLAND", "code": "NG-LAGOS-LAGOS-MAINLAND-YABA"},
                 "geometry": {"type": "Point", "coordinates": [3.38, 6.51]}},
                {"type": "Feature", "properties": {"kind": "locality", "name": "NoCoords",
                    "parent_code": "NG-LAGOS-LAGOS-MAINLAND"},
                 "geometry": null}
            ]
        });
        let out = parse_geojson_records(&gj, "test").expect("parse");
        assert_eq!(out.records.len(), 2);
        assert_eq!(out.records[0].lat, Some(6.51));
        assert_eq!(out.records[0].lng, Some(3.38));
        assert!(out.records[1].lat.is_none());
        // {"places": [...]} wrapper is accepted.
        let wrapped = serde_json::json!({"places": [{"kind": "state", "name": "Oyo"}]});
        assert_eq!(parse_json_records(&wrapped, "").unwrap().records.len(), 1);
    }

    #[test]
    fn levels_parse_canonical() {
        assert_eq!(parse_levels(None).unwrap(), vec!["state", "lga", "locality"]);
        // Order-independent + deduped (ETag stability across spellings).
        assert_eq!(
            parse_levels(Some("lga, state,lga")).unwrap(),
            vec!["state", "lga"]
        );
        assert!(parse_levels(Some("state,ward")).is_err());
    }

    #[test]
    fn etag_if_none_match_semantics() {
        let strong = HeaderValue::from_static("\"abc123\"");
        assert!(etag_matches(Some(&strong), "\"abc123\""));
        let weak = HeaderValue::from_static("W/\"abc123\"");
        assert!(etag_matches(Some(&weak), "\"abc123\""));
        let star = HeaderValue::from_static("*");
        assert!(etag_matches(Some(&star), "\"abc123\""));
        let other = HeaderValue::from_static("\"different\"");
        assert!(!etag_matches(Some(&other), "\"abc123\""));
        assert!(!etag_matches(None, "\"abc123\""));
        let list = HeaderValue::from_static("\"x\", \"abc123\"");
        assert!(etag_matches(Some(&list), "\"abc123\""));
    }

    #[test]
    fn state_filter_normalization() {
        assert_eq!(state_filter_code("Lagos"), "NG-LAGOS");
        assert_eq!(state_filter_code("akwa ibom"), "NG-AKWA-IBOM");
        assert_eq!(state_filter_code("NG-LAGOS"), "NG-LAGOS");
        assert_eq!(state_filter_code("ng-fct"), "NG-FCT");
    }

    #[test]
    fn seed_enabled_default_and_override() {
        std::env::remove_var("GEO_GAZETTEER_SEED");
        assert!(seed_enabled());
        std::env::set_var("GEO_GAZETTEER_SEED", "false");
        assert!(!seed_enabled());
        std::env::set_var("GEO_GAZETTEER_SEED", "0");
        assert!(!seed_enabled());
        std::env::set_var("GEO_GAZETTEER_SEED", "true");
        assert!(seed_enabled());
        std::env::remove_var("GEO_GAZETTEER_SEED");
    }
}
