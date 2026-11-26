//! Point-in-polygon enrichment (SPEC-W52 Scope A item 2):
//! GET /v1/geo/enrich?lat=&lng= — the reverse-geocode hook Scope B
//! (booking-service) calls to auto-fill civic/fieldcapture lga/ward.
//!
//! Query order is REFERENCE FIRST, then tenant custom: the reference pass
//! (tenant_id IS NULL rows — world-visible under the RLS policy, so no
//! tenant GUC is needed) covers state/lga/ward; the tenant pass runs inside
//! a tenant-scoped transaction (GUC set, src/store.rs) and adds that
//! tenant's custom rows. Tenant hits never overwrite a reference
//! state/lga/ward value; they only fill slots the reference pass left empty
//! and append to `boundaries`.
//!
//! NO-HIT BEHAVIOR (documented, chosen over 404): the endpoint answers
//! 200 with `{"state":null,"lga":null,"ward":null,"boundaries":[]}` when no
//! boundary covers the point — a point outside every known boundary is a
//! normal answer, not an error, and Scope B's fail-open client treats 4xx as
//! "skip enrichment" anyway.
//!
//! All geometry is parameterized ($1/$2 via ST_MakePoint) — never
//! interpolated.

use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::Json;
use serde::Serialize;

use crate::auth;
use crate::AppState;

#[derive(Debug, serde::Deserialize)]
pub struct EnrichQuery {
    lat: Option<f64>,
    lng: Option<f64>,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
struct BoundaryHit {
    kind: String,
    name: String,
    code: String,
}

#[derive(Debug, Serialize)]
pub struct EnrichResponse {
    state: Option<String>,
    lga: Option<String>,
    ward: Option<String>,
    boundaries: Vec<BoundaryHit>,
}

fn err_json(status: StatusCode, msg: impl Into<String>) -> (StatusCode, Json<serde_json::Value>) {
    (status, Json(serde_json::json!({ "error": msg.into() })))
}

/// Deterministic hit order for a mixed-kind covering set.
const KIND_ORDER: &str = "CASE kind WHEN 'state' THEN 0 WHEN 'lga' THEN 1 \
                          WHEN 'ward' THEN 2 ELSE 3 END";

pub async fn handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Query(q): Query<EnrichQuery>,
) -> Result<Json<EnrichResponse>, (StatusCode, Json<serde_json::Value>)> {
    let (lat, lng) = match (q.lat, q.lng) {
        (Some(lat), Some(lng)) => (lat, lng),
        _ => {
            return Err(err_json(
                StatusCode::BAD_REQUEST,
                "lat and lng query parameters are required",
            ))
        }
    };
    if !(-90.0..=90.0).contains(&lat) || !(-180.0..=180.0).contains(&lng) {
        return Err(err_json(
            StatusCode::BAD_REQUEST,
            "lat must be in [-90,90] and lng in [-180,180]",
        ));
    }

    // Pass 1 — platform reference boundaries (no tenant GUC: the
    // NULL-reference policy rows are world-visible by design).
    let reference: Vec<BoundaryHit> = sqlx::query_as(&format!(
        "SELECT kind, name, code FROM geo_boundaries \
         WHERE tenant_id IS NULL \
           AND ST_Covers(geom, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography) \
         ORDER BY {KIND_ORDER}, name"
    ))
    .bind(lng)
    .bind(lat)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| {
        err_json(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("enrich reference query: {e}"),
        )
    })?;

    // Pass 2 — tenant custom boundaries, RLS-scoped via the tenant GUC. A
    // missing/non-uuid X-Tenant-Slug means "no tenant context": reference
    // data only (documented — Scope B forwards whatever tenant header it
    // has and degrades gracefully).
    let tenant_hits: Vec<BoundaryHit> = match auth::tenant_uuid(&headers) {
        Some(tenant) => {
            let mut tx = crate::store::begin_tenant_tx(&state.pool, tenant)
                .await
                .map_err(|e| {
                    err_json(StatusCode::INTERNAL_SERVER_ERROR, format!("tenant tx: {e}"))
                })?;
            let rows: Vec<BoundaryHit> = sqlx::query_as(&format!(
                "SELECT kind, name, code FROM geo_boundaries \
                 WHERE tenant_id IS NOT NULL \
                   AND ST_Covers(geom, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography) \
                 ORDER BY {KIND_ORDER}, name"
            ))
            .bind(lng)
            .bind(lat)
            .fetch_all(&mut *tx)
            .await
            .map_err(|e| {
                err_json(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    format!("enrich tenant query: {e}"),
                )
            })?;
            tx.commit().await.map_err(|e| {
                err_json(StatusCode::INTERNAL_SERVER_ERROR, format!("tenant tx: {e}"))
            })?;
            rows
        }
        None => Vec::new(),
    };

    // Reference first, tenant fills only slots reference left empty.
    let pick = |kind: &str| -> Option<String> {
        reference
            .iter()
            .chain(tenant_hits.iter())
            .find(|h| h.kind == kind)
            .map(|h| h.name.clone())
    };
    let resp = EnrichResponse {
        state: pick("state"),
        lga: pick("lga"),
        ward: pick("ward"),
        boundaries: reference.into_iter().chain(tenant_hits).collect(),
    };
    Ok(Json(resp))
}

// ---------------------------------------------------------------------------
// GET /v1/geo/boundaries?kind=lga&codes=NG-LAGOS-IKEJA,NG-FCT-ABUJA-MUNICIPAL
// (2026-11-21): REFERENCE boundary geometry by code, as a GeoJSON
// FeatureCollection. analytics-pipeline uses it to put LGA polygons on the
// Acquisition cost map. Reference rows only (tenant_id IS NULL), which are
// world-visible platform data like the gazetteer snapshot, so no tenant
// gate. Codes are bound as a text[] parameter, never interpolated.
// ---------------------------------------------------------------------------

/// Upper bound on codes per request (Nigeria has 774 LGAs).
pub const MAX_BOUNDARY_CODES: usize = 1000;

#[derive(Debug, serde::Deserialize)]
pub struct BoundariesQuery {
    kind: Option<String>,
    codes: Option<String>,
}

/// Validate and split the query: kind must be a known boundary kind; codes
/// are comma separated, trimmed, upper-cased, deduplicated, non-empty.
pub fn parse_boundaries_query(
    kind: Option<&str>,
    codes: Option<&str>,
) -> Result<(String, Vec<String>), String> {
    let kind = kind.unwrap_or("lga").trim().to_ascii_lowercase();
    if !["state", "lga", "ward", "custom"].contains(&kind.as_str()) {
        return Err(format!("unknown kind {kind:?}"));
    }
    let mut out: Vec<String> = Vec::new();
    for c in codes.unwrap_or_default().split(',') {
        let c = c.trim().to_ascii_uppercase();
        if !c.is_empty() && !out.contains(&c) {
            out.push(c);
        }
    }
    if out.is_empty() {
        return Err("codes is required (comma separated)".into());
    }
    if out.len() > MAX_BOUNDARY_CODES {
        return Err(format!("at most {MAX_BOUNDARY_CODES} codes per request"));
    }
    Ok((kind, out))
}

#[derive(Debug, sqlx::FromRow)]
struct BoundaryGeom {
    kind: String,
    name: String,
    code: String,
    geojson: String,
}

pub async fn boundaries(
    State(state): State<AppState>,
    Query(q): Query<BoundariesQuery>,
) -> Result<Json<serde_json::Value>, (StatusCode, Json<serde_json::Value>)> {
    let (kind, codes) = parse_boundaries_query(q.kind.as_deref(), q.codes.as_deref())
        .map_err(|e| err_json(StatusCode::BAD_REQUEST, e))?;
    // 5 decimal places (~1 m) keeps the payload small for a dashboard map.
    let rows: Vec<BoundaryGeom> = sqlx::query_as(
        "SELECT kind, name, code, ST_AsGeoJSON(geom::geometry, 5) AS geojson \
         FROM geo_boundaries WHERE tenant_id IS NULL AND kind = $1 AND code = ANY($2) \
         ORDER BY code",
    )
    .bind(&kind)
    .bind(&codes)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| err_json(StatusCode::INTERNAL_SERVER_ERROR, format!("boundaries query: {e}")))?;
    let mut features = Vec::with_capacity(rows.len());
    for r in rows {
        let geometry: serde_json::Value = serde_json::from_str(&r.geojson).map_err(|e| {
            err_json(StatusCode::INTERNAL_SERVER_ERROR, format!("boundary geojson: {e}"))
        })?;
        features.push(serde_json::json!({
            "type": "Feature",
            "properties": { "kind": r.kind, "name": r.name, "code": r.code },
            "geometry": geometry,
        }));
    }
    Ok(Json(serde_json::json!({ "type": "FeatureCollection", "features": features })))
}

#[cfg(test)]
mod boundaries_query_tests {
    use super::*;

    #[test]
    fn defaults_to_lga_and_normalises_codes() {
        let (k, c) = parse_boundaries_query(None, Some(" ng-lagos-ikeja ,NG-LAGOS-IKEJA,,NG-FCT-ABUJA-MUNICIPAL")).unwrap();
        assert_eq!(k, "lga");
        assert_eq!(c, vec!["NG-LAGOS-IKEJA", "NG-FCT-ABUJA-MUNICIPAL"]);
    }

    #[test]
    fn rejects_unknown_kind_and_missing_codes() {
        assert!(parse_boundaries_query(Some("country"), Some("NG")).is_err());
        assert!(parse_boundaries_query(Some("lga"), None).is_err());
        assert!(parse_boundaries_query(Some("lga"), Some(" , ")).is_err());
    }

    #[test]
    fn caps_code_count() {
        let many: Vec<String> = (0..=MAX_BOUNDARY_CODES).map(|i| format!("C{i}")).collect();
        assert!(parse_boundaries_query(None, Some(&many.join(","))).is_err());
    }
}
