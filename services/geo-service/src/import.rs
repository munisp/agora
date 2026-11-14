//! Boundary import (SPEC-W52 Scope A item 2): POST /v1/geo/boundaries/import.
//!
//! Accepts a multipart file upload (`.geojson` / `.json` / `.gpkg` /
//! `.zip` shapefile) OR a `{url}` (multipart field or JSON body) pointing at
//! the same formats. Files are parsed by **terrio** (pure-Rust vector I/O;
//! zip archives are read in place through terrio's /vsizip/ vfs — no shell
//! outs, no C deps). Layers whose CRS is not EPSG:4326 are reprojected with
//! terrio's CRS engine (`Layer::reproject_from_to_epsg`); a CRS-less layer is
//! treated as EPSG:4326 (RFC 7946 default); a WKT-only CRS without an EPSG
//! code is rejected 422 (documented limitation — official Nigerian survey
//! sources ship EPSG-coded .prj, e.g. EPSG:32631-33).
//!
//! Only Polygon/MultiPolygon geometries are importable (the column is
//! geography(MultiPolygon,4326)); everything else is counted in `skipped`
//! with an advisory `errors[]` entry. Insert is ONE transaction with a
//! per-row savepoint: valid rows commit together, a row PostGIS rejects
//! (invalid ring, non-polygon after ST_MakeValid, ...) is rolled back to its
//! savepoint and reported in `errors[]` — the batch survives it. Upsert key
//! is the (scope-tenant, kind, code) unique index. Caps: 50k features,
//! 100 MB upload/download.

use axum::body::Bytes;
use axum::extract::{FromRequest, Multipart, State};
use axum::http::{HeaderMap, StatusCode};
use axum::Json;
use sqlx::{PgPool, Postgres, Transaction};

use crate::auth;
use crate::AppState;

/// Hard caps (SPEC-W52): 50k features / 100 MB payload.
pub const MAX_FEATURES: usize = 50_000;
pub const MAX_IMPORT_BYTES: usize = 100 * 1024 * 1024;
/// errors[] is advisory; cap its length so a pathological file cannot
/// produce a 50k-entry error response.
const MAX_ERROR_ENTRIES: usize = 100;

const KINDS: [&str; 4] = ["state", "lga", "ward", "custom"];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scope {
    /// Platform reference data (tenant_id NULL, world-visible).
    Reference,
    /// Tenant-private custom boundaries.
    Tenant(uuid::Uuid),
}

#[derive(Debug)]
pub struct ImportParams {
    pub kind: String,
    pub name_field: String,
    pub code_field: String,
    pub source: String,
    pub scope: Scope,
}

/// One validated, WGS84 boundary ready for the upsert.
#[derive(Debug)]
pub struct BoundaryRecord {
    pub name: String,
    pub code: String,
    /// ISO WKB (terrio `Geometry::to_wkb`), EPSG:4326 coordinates.
    pub wkb: Vec<u8>,
}

#[derive(Debug, Default)]
pub struct IngestOutcome {
    pub records: Vec<BoundaryRecord>,
    pub skipped: usize,
    pub errors: Vec<String>,
}

impl IngestOutcome {
    fn push_error(&mut self, msg: String) {
        if self.errors.len() < MAX_ERROR_ENTRIES {
            self.errors.push(msg);
        }
    }
}

fn err_json(status: StatusCode, msg: impl Into<String>) -> (StatusCode, Json<serde_json::Value>) {
    (status, Json(serde_json::json!({ "error": msg.into() })))
}

/// POST /v1/geo/boundaries/import — multipart or JSON entry point.
pub async fn handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    req: axum::extract::Request,
) -> Result<Json<serde_json::Value>, (StatusCode, Json<serde_json::Value>)> {
    // Tenant context: X-Tenant-Slug carries the tenant uuid on this platform
    // (the Permify organization id / RLS GUC — src/auth.rs header docs).
    let tenant_hdr = auth::tenant_slug(&headers);

    let content_type = headers
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_string();

    let (bytes, file_name, params) = if content_type.starts_with("multipart/form-data") {
        let multipart = Multipart::from_request(req, &()).await.map_err(|e| {
            err_json(
                StatusCode::BAD_REQUEST,
                format!("invalid multipart body: {e}"),
            )
        })?;
        read_multipart(&state, multipart, tenant_hdr.as_deref()).await?
    } else {
        let Json(body): Json<UrlImportBody> = Json::from_request(req, &())
            .await
            .map_err(|e| err_json(StatusCode::BAD_REQUEST, format!("invalid JSON body: {e}")))?;
        let params = params_from_fields(
            body.kind,
            body.name_field,
            body.code_field,
            body.source,
            body.scope,
            tenant_hdr.as_deref(),
        )?;
        let (bytes, file_name) = fetch_url(&state, &body.url).await?;
        (bytes, file_name, params)
    };

    // Authz: Permify manage_bookings on organization:{tenant} / user:{sub}.
    // Reference-scope imports require the SAME perm (they write world-visible
    // rows); the organization for the check is the request tenant.
    let gate_tenant = match params.scope {
        Scope::Tenant(t) => t,
        Scope::Reference => tenant_hdr
            .as_deref()
            .and_then(|s| s.parse::<uuid::Uuid>().ok())
            .ok_or_else(|| {
                err_json(
                    StatusCode::BAD_REQUEST,
                    "X-Tenant-Slug (tenant uuid) is required for boundary import",
                )
            })?,
    };
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

    // SPEC-W63 section 4 AV hook: scan the raw import bytes BEFORE the
    // terrio parse (pinned handler-side mapping; Infected => 415
    // {error:"infected",signature}, Err => 503 {error:"av_unavailable"}
    // when CLAMAV_REQUIRED, else loud log + proceed — av.py semantics:
    // a scan ERROR is never clean). No metrics idiom exists in
    // geo-service, so the outcome is a structured `geo_av_scan` log line
    // (outcome=clean|infected|error|skipped).
    match &state.av {
        Some(av) => match av.scan(&bytes).await {
            Ok(crate::av::AvVerdict::Clean) => {
                tracing::info!(outcome = "clean", bytes = bytes.len(), file = %file_name, "geo_av_scan");
            }
            Ok(crate::av::AvVerdict::Infected(signature)) => {
                tracing::warn!(outcome = "infected", file = %file_name, "geo_av_scan");
                return Err((
                    StatusCode::UNSUPPORTED_MEDIA_TYPE,
                    Json(serde_json::json!({ "error": "infected", "signature": signature })),
                ));
            }
            Err(e) if state.av_required => {
                tracing::error!(outcome = "error", error = %e, av_required = true, "geo_av_scan");
                return Err(err_json(StatusCode::SERVICE_UNAVAILABLE, "av_unavailable"));
            }
            Err(e) => {
                tracing::warn!(outcome = "error", error = %e, av_required = false,
                    "geo_av_scan: scan failed; CLAMAV_REQUIRED=false — proceeding UNSCANNED (dev posture)");
            }
        },
        None => {
            tracing::warn!(outcome = "skipped", reason = "av_disabled", file = %file_name, "geo_av_scan");
        }
    }

    // Parse + reproject + validate (terrio is sync CPU work — keep it off
    // the async executor threads).
    let name_field = params.name_field.clone();
    let code_field = params.code_field.clone();
    let outcome = tokio::task::spawn_blocking(move || {
        ingest_bytes(&file_name, &bytes, &name_field, &code_field)
    })
    .await
    .map_err(|e| {
        err_json(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("ingest task: {e}"),
        )
    })?
    .map_err(|e| err_json(StatusCode::UNPROCESSABLE_ENTITY, e))?;

    let (imported, db_errors) = insert_records(&state.pool, &params, outcome.records)
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

#[derive(Debug, serde::Deserialize)]
struct UrlImportBody {
    url: String,
    kind: Option<String>,
    name_field: Option<String>,
    code_field: Option<String>,
    source: Option<String>,
    scope: Option<String>,
}

fn params_from_fields(
    kind: Option<String>,
    name_field: Option<String>,
    code_field: Option<String>,
    source: Option<String>,
    scope: Option<String>,
    tenant_hdr: Option<&str>,
) -> Result<ImportParams, (StatusCode, Json<serde_json::Value>)> {
    let kind = kind.unwrap_or_default().trim().to_ascii_lowercase();
    if !KINDS.contains(&kind.as_str()) {
        return Err(err_json(
            StatusCode::BAD_REQUEST,
            format!("kind must be one of {KINDS:?}"),
        ));
    }
    let name_field = name_field.unwrap_or_default().trim().to_string();
    let code_field = code_field.unwrap_or_default().trim().to_string();
    if name_field.is_empty() || code_field.is_empty() {
        return Err(err_json(
            StatusCode::BAD_REQUEST,
            "name_field and code_field are required",
        ));
    }
    let scope = match scope
        .unwrap_or_else(|| "reference".to_string())
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "reference" => Scope::Reference,
        "tenant" => {
            let t = tenant_hdr
                .and_then(|s| s.parse::<uuid::Uuid>().ok())
                .ok_or_else(|| {
                    err_json(
                        StatusCode::BAD_REQUEST,
                        "scope=tenant requires X-Tenant-Slug to carry the tenant uuid",
                    )
                })?;
            Scope::Tenant(t)
        }
        other => {
            return Err(err_json(
                StatusCode::BAD_REQUEST,
                format!("scope must be reference|tenant, got '{other}'"),
            ))
        }
    };
    Ok(ImportParams {
        kind,
        name_field,
        code_field,
        source: source.unwrap_or_default().trim().to_string(),
        scope,
    })
}

/// Multipart form: `file` (binary, capped) OR `url`, plus the parameter
/// fields. A form with neither file nor url is a 400.
async fn read_multipart(
    state: &AppState,
    mut mp: Multipart,
    tenant_hdr: Option<&str>,
) -> Result<(Bytes, String, ImportParams), (StatusCode, Json<serde_json::Value>)> {
    let mut file: Option<(Bytes, String)> = None;
    let mut url: Option<String> = None;
    let mut kind = None;
    let mut name_field = None;
    let mut code_field = None;
    let mut source = None;
    let mut scope = None;

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
                    .unwrap_or_else(|| "upload.geojson".to_string());
                // Stream the field with an explicit cap (axum's
                // DefaultBodyLimit does not apply to multipart fields).
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
            "kind" => kind = Some(field.text().await.unwrap_or_default()),
            "name_field" => name_field = Some(field.text().await.unwrap_or_default()),
            "code_field" => code_field = Some(field.text().await.unwrap_or_default()),
            "source" => source = Some(field.text().await.unwrap_or_default()),
            "scope" => scope = Some(field.text().await.unwrap_or_default()),
            _ => {}
        }
    }

    let params = params_from_fields(kind, name_field, code_field, source, scope, tenant_hdr)?;
    if let Some((bytes, name)) = file {
        return Ok((bytes, name, params));
    }
    match url {
        Some(u) if !u.trim().is_empty() => {
            let (bytes, name) = fetch_url(state, &u).await?;
            Ok((bytes, name, params))
        }
        _ => Err(err_json(
            StatusCode::BAD_REQUEST,
            "multipart import needs a 'file' part or a 'url' field",
        )),
    }
}

/// Fetch a `{url}` import (http/https only, 100 MB cap, the shared client's
/// 30 s overall timeout — payments RS-006 idiom).
async fn fetch_url(
    state: &AppState,
    url: &str,
) -> Result<(Bytes, String), (StatusCode, Json<serde_json::Value>)> {
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
        .unwrap_or("download.geojson")
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
    Ok((Bytes::from(buf), file_name))
}

fn check_extension(file_name: &str) -> Result<(), String> {
    let lower = file_name.to_ascii_lowercase();
    for ext in [".geojson", ".json", ".gpkg", ".zip"] {
        if lower.ends_with(ext) {
            return Ok(());
        }
    }
    Err(format!(
        "unsupported file '{file_name}': expected .geojson/.json/.gpkg/.zip (shapefile)"
    ))
}

/// terrio ingest: bytes -> validated EPSG:4326 boundary records. Sync (runs
/// in spawn_blocking); no network — `{url}` bytes arrive here already
/// fetched, so the import code path is identical for uploads and URLs.
pub fn ingest_bytes(
    file_name: &str,
    bytes: &[u8],
    name_field: &str,
    code_field: &str,
) -> Result<IngestOutcome, String> {
    check_extension(file_name)?;
    // terrio opens paths (local/vsi); stage the payload in a temp dir under
    // the real file name so extension-based format detection sees it.
    // tempfile cleans up on drop.
    let dir = tempfile::tempdir().map_err(|e| format!("staging tempdir: {e}"))?;
    let staged = dir.path().join(file_name);
    std::fs::write(&staged, bytes).map_err(|e| format!("staging write: {e}"))?;
    let staged = staged.to_string_lossy().to_string();

    // Zip shapefiles: open the vector member through terrio's /vsizip/ vfs.
    let open_path = if staged.to_ascii_lowercase().ends_with(".zip") {
        zip_member_path(&staged)?
    } else {
        staged.clone()
    };

    let ds = terrio::open_vector(&open_path, &terrio::VectorOpenOptions::default())
        .map_err(|e| format!("terrio open '{file_name}': {e}"))?;

    let mut out = IngestOutcome::default();
    let mut seen: usize = 0;
    for layer in &ds.layers {
        seen += layer.features.len();
        if seen > MAX_FEATURES {
            out.push_error(format!(
                "feature cap exceeded ({MAX_FEATURES}); trailing features were not read"
            ));
            break;
        }
        let layer = reproject_if_needed(layer)?;
        for feature in &layer.features {
            match record_from_feature(feature, &layer, name_field, code_field) {
                Ok(rec) => out.records.push(rec),
                Err(skip) => {
                    out.skipped += 1;
                    out.push_error(skip);
                }
            }
        }
    }
    if out.records.is_empty() && out.skipped == 0 {
        return Err(format!("'{file_name}' contains no features"));
    }
    Ok(out)
}

/// Find the vector member of an uploaded zip through terrio's vfs (the
/// shapefile member preferred — its .dbf/.shx siblings stay reachable inside
/// the archive, GDAL-style).
fn zip_member_path(zip_path: &str) -> Result<String, String> {
    let root = format!("/vsizip/{zip_path}");
    // Recursive: shapefiles are commonly zipped under a subdirectory, and
    // entry names are relative to the archive root.
    let opts = terrio::vsi::ListOptions {
        recursive: true,
        depth: Some(8),
        absolute: false,
        details: false,
    };
    let entries = terrio::vsi::list(&root, &opts).map_err(|e| format!("list zip archive: {e}"))?;
    let mut candidate: Option<String> = None;
    for entry in &entries {
        let name = entry.name.to_ascii_lowercase();
        if name.ends_with(".shp") {
            return Ok(format!("{root}/{}", entry.name));
        }
        if candidate.is_none()
            && [".geojson", ".json", ".gpkg"]
                .iter()
                .any(|ext| name.ends_with(ext))
        {
            candidate = Some(entry.name.clone());
        }
    }
    let member = candidate
        .ok_or_else(|| "zip archive holds no .shp/.geojson/.json/.gpkg member".to_string())?;
    Ok(format!("{root}/{member}"))
}

/// Reproject a layer to EPSG:4326 when its declared CRS differs (terrio CRS
/// engine). CRS-less layers are already 4326 by convention (RFC 7946);
/// WKT-only CRS metadata without an EPSG code is a documented rejection.
pub fn reproject_if_needed(layer: &terrio::Layer) -> Result<terrio::Layer, String> {
    match layer.crs_epsg() {
        None => {
            if layer.crs_wkt().is_some() {
                Err(format!(
                    "layer '{}' declares a CRS without an EPSG code (WKT-only); \
                     re-save it with an EPSG-coded CRS (e.g. EPSG:32631) or EPSG:4326",
                    layer.name
                ))
            } else {
                Ok(layer.clone())
            }
        }
        Some(4326) => Ok(layer.clone()),
        Some(src) => layer
            .reproject_from_to_epsg(src, 4326)
            .map_err(|e| format!("reproject EPSG:{src} -> EPSG:4326: {e}")),
    }
}

/// Validate one feature into a boundary record: polygon geometry + the two
/// attribute fields. Anything missing is a skip (counted + advisory error),
/// never a batch abort.
fn record_from_feature(
    feature: &terrio::Feature,
    layer: &terrio::Layer,
    name_field: &str,
    code_field: &str,
) -> Result<BoundaryRecord, String> {
    let geom = match &feature.geometry {
        Some(g) => g,
        None => return Err(format!("feature {}: no geometry", feature.fid)),
    };
    let wkb = match geom {
        terrio::Geometry::Polygon { .. } | terrio::Geometry::MultiPolygon(_) => geom.to_wkb(),
        other => {
            return Err(format!(
                "feature {}: geometry {:?} is not a polygon (only Polygon/MultiPolygon importable)",
                feature.fid,
                other.geom_type()
            ))
        }
    };
    let name = attr_text(feature, layer, name_field).ok_or_else(|| {
        format!(
            "feature {}: missing/empty '{name_field}' attribute",
            feature.fid
        )
    })?;
    let code = attr_text(feature, layer, code_field).ok_or_else(|| {
        format!(
            "feature {}: missing/empty '{code_field}' attribute",
            feature.fid
        )
    })?;
    Ok(BoundaryRecord { name, code, wkb })
}

fn attr_text(feature: &terrio::Feature, layer: &terrio::Layer, field: &str) -> Option<String> {
    let v = feature.get(&layer.schema, field).ok()?;
    let s = match v {
        terrio::FieldValue::Text(s) => s.clone(),
        terrio::FieldValue::Integer(i) => i.to_string(),
        terrio::FieldValue::Float(f) => f.to_string(),
        terrio::FieldValue::Boolean(b) => b.to_string(),
        terrio::FieldValue::Date(d) | terrio::FieldValue::DateTime(d) => d.clone(),
        _ => return None,
    };
    let s = s.trim().to_string();
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

/// One transaction, per-row savepoint: valid rows commit together; a row
/// PostGIS rejects is rolled back to its savepoint and reported — the batch
/// survives it. Geometry is MakeValid'd (ring-order/repair), forced 2D
/// (geography is 2D-only), ST_Multi-promoted and cast to
/// geography(MultiPolygon,4326) — all parameterized ($n), never interpolated.
/// Returns (imported, per-row db errors).
async fn insert_records(
    pool: &PgPool,
    params: &ImportParams,
    records: Vec<BoundaryRecord>,
) -> Result<(usize, Vec<String>), String> {
    let scope_tenant = match params.scope {
        Scope::Tenant(t) => t,
        // The zero uuid is never a real tenant: with the GUC pinned to it,
        // reference (tenant_id NULL) rows pass the policy's WITH CHECK and no
        // tenant row can match.
        Scope::Reference => uuid::Uuid::nil(),
    };
    let mut tx: Transaction<'_, Postgres> = crate::store::begin_tenant_tx(pool, scope_tenant)
        .await
        .map_err(|e| format!("begin import tx: {e}"))?;
    let tenant_db: Option<uuid::Uuid> = match params.scope {
        Scope::Tenant(t) => Some(t),
        Scope::Reference => None,
    };
    let mut imported = 0usize;
    let mut errors: Vec<String> = Vec::new();
    for (i, rec) in records.iter().enumerate() {
        sqlx::query("SAVEPOINT geo_import_row")
            .execute(&mut *tx)
            .await
            .map_err(|e| format!("savepoint: {e}"))?;
        let res = sqlx::query(
            "INSERT INTO geo_boundaries (tenant_id, kind, name, code, geom, source) \
             VALUES ($1, $2, $3, $4, \
                     ST_Multi(ST_Force2D(ST_MakeValid(ST_GeomFromWKB($5, 4326))))::geography, $6) \
             ON CONFLICT (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), kind, code) \
             DO UPDATE SET name = EXCLUDED.name, geom = EXCLUDED.geom, source = EXCLUDED.source",
        )
        .bind(tenant_db)
        .bind(&params.kind)
        .bind(&rec.name)
        .bind(&rec.code)
        .bind(&rec.wkb)
        .bind(&params.source)
        .execute(&mut *tx)
        .await;
        match res {
            Ok(_) => {
                sqlx::query("RELEASE SAVEPOINT geo_import_row")
                    .execute(&mut *tx)
                    .await
                    .map_err(|e| format!("release savepoint: {e}"))?;
                imported += 1;
            }
            Err(e) => {
                sqlx::query("ROLLBACK TO SAVEPOINT geo_import_row")
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
        .map_err(|e| format!("commit import tx: {e}"))?;
    Ok((imported, errors))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// TEST-ONLY fixture parse: the hand-written tiny GeoJSON
    /// (tests/fixtures/boundaries_4326.geojson — synthetic, NOT real
    /// boundaries) yields 3 polygon records; the attribute-less polygon and
    /// the Point feature are skipped with advisory errors.
    #[test]
    fn ingest_geojson_fixture_counts_records_and_skips() {
        let bytes = include_bytes!("../tests/fixtures/boundaries_4326.geojson");
        let out =
            ingest_bytes("boundaries_4326.geojson", bytes, "name", "code").expect("fixture parses");
        assert_eq!(out.records.len(), 3, "records: {:?}", out.records.len());
        assert_eq!(out.skipped, 2, "errors: {:?}", out.errors);
        let codes: Vec<&str> = out.records.iter().map(|r| r.code.as_str()).collect();
        assert_eq!(codes, vec!["TS-A", "TL-A", "TW-A"]);
        assert!(out.records.iter().all(|r| !r.wkb.is_empty()));
    }

    /// TEST-ONLY zip path: the same fixture zipped (boundaries_zip.zip) is
    /// opened through terrio's /vsizip/ vfs member resolution.
    #[test]
    fn ingest_zip_member_through_vsi() {
        let bytes = include_bytes!("../tests/fixtures/boundaries_zip.zip");
        let out = ingest_bytes("boundaries_zip.zip", bytes, "name", "code").expect("zip parses");
        assert_eq!(out.records.len(), 3);
        assert_eq!(out.skipped, 2);
    }

    #[test]
    fn unsupported_extension_is_rejected() {
        let err = ingest_bytes("boundaries.csv", b"x", "name", "code").expect_err("csv rejected");
        assert!(err.contains("unsupported file"), "{err}");
    }

    /// A synthetic EPSG:32631 (UTM 31N) layer built with terrio's API:
    /// reproject_if_needed must run terrio's CRS engine and produce
    /// EPSG:4326 coordinates near Lagos (easting ~499-501 km around the 3E
    /// central meridian -> lng ~2.98-3.02; northing ~718-720 km -> lat
    /// ~6.48-6.51).
    #[test]
    fn reproject_epsg_32631_to_4326() {
        let mut layer = terrio::Layer::new("test_only_utm_wards")
            .with_geom_type(terrio::GeometryType::Polygon)
            .with_epsg(32631);
        layer.add_field(terrio::FieldDef::new("name", terrio::FieldType::Text));
        layer.add_field(terrio::FieldDef::new("code", terrio::FieldType::Text));
        // terrio Rings are stored WITHOUT the closing duplicate point.
        let ring = vec![
            terrio::vector::Coord::xy(499_000.0, 718_000.0),
            terrio::vector::Coord::xy(501_000.0, 718_000.0),
            terrio::vector::Coord::xy(501_000.0, 720_000.0),
            terrio::vector::Coord::xy(499_000.0, 720_000.0),
        ];
        let mut f = terrio::Feature::with_geometry(1, terrio::Geometry::polygon(ring, vec![]), 2);
        f.set(
            &layer.schema,
            "name",
            terrio::FieldValue::Text("UTM Ward".to_string()),
        )
        .expect("set name");
        f.set(
            &layer.schema,
            "code",
            terrio::FieldValue::Text("UW-1".to_string()),
        )
        .expect("set code");
        layer.push(f);

        let out = reproject_if_needed(&layer).expect("reproject 32631 -> 4326");
        assert_eq!(out.crs_epsg(), Some(4326));
        let geom = out.features[0].geometry.as_ref().expect("geometry");
        for c in geom.all_coords() {
            assert!(
                (2.9..=3.1).contains(&c.x) && (6.4..=6.6).contains(&c.y),
                "reprojected coord not near Lagos: ({}, {})",
                c.x,
                c.y
            );
        }
        // ... and the reprojected layer still yields a boundary record.
        let rec = record_from_feature(&out.features[0], &out, "name", "code").expect("record");
        assert_eq!(rec.code, "UW-1");
        assert!(!rec.wkb.is_empty());
    }

    /// A CRS-less layer is RFC 7946 EPSG:4326 by convention: no-op.
    #[test]
    fn crs_less_layer_is_treated_as_4326() {
        let mut layer = terrio::Layer::new("test_only_plain");
        layer.push(terrio::Feature::with_geometry(
            1,
            terrio::Geometry::polygon(
                vec![
                    terrio::vector::Coord::xy(3.0, 6.0),
                    terrio::vector::Coord::xy(3.1, 6.0),
                    terrio::vector::Coord::xy(3.1, 6.1),
                    terrio::vector::Coord::xy(3.0, 6.1),
                ],
                vec![],
            ),
            0,
        ));
        let out = reproject_if_needed(&layer).expect("no-op reproject");
        assert!(out.crs_epsg().is_none());
    }
}
