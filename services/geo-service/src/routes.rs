//! Route table (SPEC-W52 Scope A). Canonical paths carry the /v1/geo prefix;
//! the /style.json and /tiles/* aliases exist because the APISIX `api-geo`
//! route strips /api/geo, and admin-web's default NEXT_PUBLIC_GEO_STYLE_URL
//! is /api/geo/style.json (Scope C contract) — both gateway shapes must land
//! on a real handler.
//!
//! There is deliberately NO /internal/* surface on this service (nothing to
//! block at the gateway beyond the generic deny rules).

use axum::routing::{get, post};
use axum::{Json, Router};

use crate::{enrich, import, tiles, AppState};

async fn healthz() -> Json<serde_json::Value> {
    Json(serde_json::json!({"status": "ok"}))
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/v1/geo/boundaries/import", post(import::handler))
        .route("/v1/geo/enrich", get(enrich::handler))
        .route("/v1/geo/tiles/:z/:x/:y_mvt", get(tiles::tile))
        .route("/v1/geo/style.json", get(tiles::style))
        // Aliases for the /api/geo/style.json gateway shape (Scope C's
        // NEXT_PUBLIC_GEO_STYLE_URL default).
        .route("/style.json", get(tiles::style))
        .route("/tiles/:z/:x/:y_mvt", get(tiles::tile))
        // The JSON {url} import body goes through axum's Json extractor,
        // which honors this limit; multipart fields are capped explicitly in
        // import::read_multipart (DefaultBodyLimit does not apply there).
        .layer(axum::extract::DefaultBodyLimit::max(
            import::MAX_IMPORT_BYTES,
        ))
        .with_state(state)
}
