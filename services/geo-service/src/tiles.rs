//! Self-hosted tile + style serving (SPEC-W52 Scope A item 2):
//! GET /v1/geo/tiles/{z}/{x}/{y}.mvt and GET /v1/geo/style.json, served from
//! the PMTiles archive at GEO_TILES_PATH via terrio's tiles reader
//! (`tiles::TileArchive` — PMTiles are read in place: header + the tiles
//! needed only).
//!
//! FAIL-CLOSED DISABLED CONFIG (real code, not a mock): when GEO_TILES_PATH
//! is unset (or the archive cannot be opened), both endpoints answer
//! 503 with a plain-text error — provisioning the archive is an operator
//! action (`terrio convert ... .pmtiles` -> GEO_TILES_PATH, see
//! docs/geospatial-terrio.md), and a map client must see an explicit
//! failure, never synthetic tiles.
//!
//! The archive must hold VECTOR (MVT) tiles for the .mvt endpoint; a raster
//! archive answers 404 with an explanatory message (the endpoint contract is
//! vector tiles only). style.json is generated from the archive's real
//! metadata (vector_layers, bounds, zoom range) — the `tiles` URL is
//! RELATIVE ("tiles/{z}/{x}/{y}.mvt") so the same style works whether it was
//! served via /api/geo/style.json or /api/geo/v1/geo/style.json (MapLibre
//! resolves it against the style URL).

use axum::extract::{Path, State};
use axum::http::{header, StatusCode};
use axum::response::{IntoResponse, Response};

use crate::AppState;

fn unavailable(msg: impl Into<String>) -> Response {
    (
        StatusCode::SERVICE_UNAVAILABLE,
        [(header::CONTENT_TYPE, "text/plain; charset=utf-8")],
        msg.into(),
    )
        .into_response()
}

fn not_found(msg: impl Into<String>) -> Response {
    (
        StatusCode::NOT_FOUND,
        [(header::CONTENT_TYPE, "text/plain; charset=utf-8")],
        msg.into(),
    )
        .into_response()
}

/// Open the configured archive (sync terrio I/O — runs in spawn_blocking).
/// Err(503 text) when tiles are disabled or the archive is unreadable.
async fn open_archive(state: &AppState) -> Result<terrio::tiles::TileArchive, Response> {
    let Some(path) = state.config.tiles_path.clone() else {
        return Err(unavailable(
            "geo tiles not configured: GEO_TILES_PATH is unset (provision a PMTiles archive — see docs/geospatial-terrio.md)",
        ));
    };
    tokio::task::spawn_blocking(move || terrio::tiles::TileArchive::open(&path))
        .await
        .map_err(|e| unavailable(format!("geo tiles unavailable: open task: {e}")))?
        .map_err(|e| unavailable(format!("geo tiles unavailable: cannot open archive: {e}")))
}

/// The last segment carries the `.mvt` suffix (`{z}/{x}/{y}.mvt`) — axum
/// path params span whole segments, so the suffix is validated here: a
/// non-.mvt request is a 404, not a tile.
pub async fn tile(
    State(state): State<AppState>,
    Path((z, x, y_mvt)): Path<(u8, u32, String)>,
) -> Response {
    let Some(y) = y_mvt
        .strip_suffix(".mvt")
        .and_then(|v| v.parse::<u32>().ok())
    else {
        return not_found(format!(
            "'{y_mvt}' is not a vector tile address (expected {{y}}.mvt)"
        ));
    };
    let mut archive = match open_archive(&state).await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let result =
        tokio::task::spawn_blocking(move || (archive.format(), archive.tile(z, x, y))).await;
    let (format, tile) = match result {
        Ok((f, Ok(t))) => (f, t),
        Ok((_, Err(e))) => return unavailable(format!("geo tiles unavailable: tile read: {e}")),
        Err(e) => return unavailable(format!("geo tiles unavailable: tile task: {e}")),
    };
    if format != terrio::tiles::TileFormat::Mvt {
        return not_found(format!(
            "the configured archive holds {} tiles; this endpoint serves vector (.mvt) tiles only",
            format.name()
        ));
    }
    match tile {
        Some(bytes) => (
            StatusCode::OK,
            [
                (
                    header::CONTENT_TYPE,
                    "application/vnd.mapbox-vector-tile".to_string(),
                ),
                // Tiles are immutable per archive generation; let the
                // gateway/clients cache briefly.
                (header::CACHE_CONTROL, "public, max-age=300".to_string()),
            ],
            bytes,
        )
            .into_response(),
        None => not_found(format!("no tile z{z}/x{x}/y{y} in the configured archive")),
    }
}

/// MapLibre style JSON generated from the archive's real metadata. One
/// vector source ("geo") + a background layer + a fill/line pair per
/// declared vector layer. No glyphs are referenced (no glyph server this
/// wave), so symbol/text layers are intentionally absent.
pub async fn style(State(state): State<AppState>) -> Response {
    let archive = match open_archive(&state).await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let (metadata, bounds, (minzoom, maxzoom), format) = (
        archive.metadata(),
        archive.bounds(),
        archive.zoom_range(),
        archive.format(),
    );
    if format != terrio::tiles::TileFormat::Mvt {
        return not_found(format!(
            "the configured archive holds {} tiles; style.json is generated for vector (mvt) archives only",
            format.name()
        ));
    }
    let meta = |key: &str| -> Option<&str> {
        metadata
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.as_str())
    };
    let name = meta("name").unwrap_or("geo-service self-hosted tiles");
    let vector_layer_ids: Vec<String> = meta("vector_layers")
        .and_then(|v| serde_json::from_str::<serde_json::Value>(v).ok())
        .and_then(|v| v.as_array().cloned())
        .unwrap_or_default()
        .iter()
        .filter_map(|l| l.get("id").and_then(|id| id.as_str()).map(str::to_string))
        .collect();

    let mut source = serde_json::json!({
        "type": "vector",
        // Relative: resolves against the style URL, so both the /api/geo/*
        // gateway alias and the direct service path work unchanged.
        "tiles": ["tiles/{z}/{x}/{y}.mvt"],
        "minzoom": minzoom,
        "maxzoom": maxzoom,
    });
    if let Some(b) = bounds {
        source["bounds"] = serde_json::json!(b);
    }

    let mut layers = vec![serde_json::json!({
        "id": "background",
        "type": "background",
        "paint": {"background-color": "#f8f9fa"}
    })];
    for id in &vector_layer_ids {
        layers.push(serde_json::json!({
            "id": format!("{id}-fill"),
            "type": "fill",
            "source": "geo",
            "source-layer": id,
            "filter": ["==", ["geometry-type"], "Polygon"],
            "paint": {"fill-color": "#9dbcd4", "fill-opacity": 0.5}
        }));
        layers.push(serde_json::json!({
            "id": format!("{id}-line"),
            "type": "line",
            "source": "geo",
            "source-layer": id,
            "paint": {"line-color": "#4a6fa5", "line-width": 1}
        }));
    }

    let style = serde_json::json!({
        "version": 8,
        "name": name,
        "sources": {"geo": source},
        "layers": layers,
    });
    (
        StatusCode::OK,
        [(header::CONTENT_TYPE, "application/json; charset=utf-8")],
        serde_json::to_string_pretty(&style).unwrap_or_else(|_| "{}".to_string()),
    )
        .into_response()
}
