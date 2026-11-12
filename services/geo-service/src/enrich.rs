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
