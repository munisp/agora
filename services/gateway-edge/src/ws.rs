//! WebSocket endpoints (SPEC §12: `/ws/*` routed here via APISIX).
//!
//! - `GET /ws?tenant={slug}` — live booking events for the tenant
//!   (Kafka `opendesk.booking.events`).
//! - `GET /ws/transcripts?tenant={slug}` — live transcript tail
//!   (Fluvio `opendesk.transcripts-raw`).
//! - `GET /ws/intel?tenant={slug}` — live enriched turns
//!   (sentiment/intent/entities; Kafka `opendesk.conversation.enriched`).
//!
//! Auth token transport: PREFERRED is the `Sec-WebSocket-Protocol: bearer.<jwt>`
//! header (tokens in URLs leak into access logs / browser history). The
//! legacy `?token={jwt}` query parameter is still accepted for compatibility
//! and logs a deprecation warning once per process.
//!
//! Backpressure: drop-slow policy — consumers lagging past the channel
//! capacity get a `{"type":"lagged","dropped":n}` notice and the drop is
//! counted in `/metrics`.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        Query, State,
    },
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
};
use serde::Deserialize;
use tokio::sync::broadcast;
use tracing::{debug, warn};

use crate::auth::AuthError;
use crate::bus;
use crate::metrics;
use crate::AppState;

#[derive(Debug, Deserialize)]
pub struct WsQuery {
    pub tenant: String,
    pub token: Option<String>,
}

/// Log the query-param deprecation warning at most once per process.
static QUERY_TOKEN_DEPRECATION_LOGGED: AtomicBool = AtomicBool::new(false);

/// Bearer token carried in the `Sec-WebSocket-Protocol` header as
/// `bearer.<jwt>` (possibly among a comma-separated protocol list). Returns
/// the JWT plus the exact protocol token to echo back in the upgrade
/// response (RFC 6455: the server must select one of the offered
/// subprotocols or the client aborts the connection).
pub(crate) fn bearer_protocol_token(headers: &HeaderMap) -> Option<(String, String)> {
    let raw = headers.get("sec-websocket-protocol")?.to_str().ok()?;
    raw.split(',').map(str::trim).find_map(|proto| {
        proto
            .strip_prefix("bearer.")
            .filter(|t| !t.is_empty())
            .map(|t| (t.to_string(), proto.to_string()))
    })
}

/// Resolve the auth token for a websocket upgrade: the
/// `Sec-WebSocket-Protocol: bearer.<jwt>` header is PREFERRED; the
/// `?token=` query parameter is kept for compatibility and logs a
/// deprecation warning once per process. Returns the token (if any) and the
/// subprotocol the server must select on upgrade (if the header was used).
pub(crate) fn resolve_ws_token(
    headers: &HeaderMap,
    query_token: Option<&str>,
) -> (Option<String>, Option<String>) {
    if let Some((token, proto)) = bearer_protocol_token(headers) {
        return (Some(token), Some(proto));
    }
    if let Some(t) = query_token.filter(|t| !t.is_empty()) {
        if !QUERY_TOKEN_DEPRECATION_LOGGED.swap(true, Ordering::Relaxed) {
            warn!(
                "ws auth via ?token= query parameter is DEPRECATED (tokens in URLs                  leak into logs); use the Sec-WebSocket-Protocol: bearer.<jwt> header"
            );
        }
        return (Some(t.to_string()), None);
    }
    (None, None)
}

async fn authorize(state: &AppState, tenant: &str, token: Option<&str>) -> Result<(), Response> {
    match state
        .auth
        .authenticate(token, tenant)
        .await
    {
        Ok(_) => Ok(()),
        Err(e) => {
            metrics::inc(&metrics::AUTH_FAILURES);
            let status = match &e {
                AuthError::MissingToken | AuthError::MalformedToken => StatusCode::UNAUTHORIZED,
                AuthError::Validation(_) => StatusCode::UNAUTHORIZED,
                AuthError::Forbidden(_) => StatusCode::FORBIDDEN,
                AuthError::JwksFetch(_) => StatusCode::BAD_GATEWAY,
            };
            warn!(error = %e, tenant = %tenant, "ws auth failed");
            Err((
                status,
                axum::Json(serde_json::json!({ "error": e.to_string() })),
            )
                .into_response())
        }
    }
}

macro_rules! ws_handler {
    ($name:ident, $channel:expr, $log:literal) => {
        pub async fn $name(
            State(state): State<AppState>,
            Query(query): Query<WsQuery>,
            headers: HeaderMap,
            ws: WebSocketUpgrade,
        ) -> Response {
            let (token, proto) = resolve_ws_token(&headers, query.token.as_deref());
            if let Err(resp) = authorize(&state, &query.tenant, token.as_deref()).await {
                return resp;
            }
            let channel = $channel(&query.tenant);
            let rx = state.bus.subscribe(&channel).await;
            debug!(tenant = %query.tenant, $log);
            // RFC 6455: echo the offered bearer subprotocol so the client
            // completes the handshake.
            let ws = match proto {
                Some(p) => ws.protocols([p]),
                None => ws,
            };
            ws.on_upgrade(move |socket| handle_socket(socket, rx))
        }
    };
}

ws_handler!(ws_booking_events, bus::booking_channel, "booking events subscriber connected");
ws_handler!(ws_transcripts, bus::transcripts_channel, "transcript tail subscriber connected");
ws_handler!(ws_intel, bus::intel_channel, "intel subscriber connected");

async fn handle_socket(mut socket: WebSocket, mut rx: broadcast::Receiver<Arc<str>>) {
    metrics::inc(&metrics::WS_CONNECTIONS_ACTIVE);
    loop {
        tokio::select! {
            event = rx.recv() => {
                match event {
                    Ok(payload) => {
                        if socket.send(Message::Text(payload.to_string())).await.is_err() {
                            break;
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(n)) => {
                        // Drop-slow policy: report the drop, keep the connection.
                        metrics::add(&metrics::EVENTS_DROPPED_SLOW_CONSUMER, n);
                        let notice = serde_json::json!({
                            "type": "lagged",
                            "dropped": n,
                        })
                        .to_string();
                        if socket.send(Message::Text(notice)).await.is_err() {
                            break;
                        }
                    }
                    Err(broadcast::error::RecvError::Closed) => break,
                }
            }
            incoming = socket.recv() => {
                match incoming {
                    Some(Ok(Message::Close(_))) | None => break,
                    Some(Ok(Message::Ping(p))) => {
                        if socket.send(Message::Pong(p)).await.is_err() {
                            break;
                        }
                    }
                    Some(Ok(_)) => {}
                    Some(Err(_)) => break,
                }
            }
        }
    }
    metrics::WS_CONNECTIONS_ACTIVE.fetch_sub(1, std::sync::atomic::Ordering::Relaxed);
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    fn headers_with_protocol(value: &str) -> HeaderMap {
        let mut h = HeaderMap::new();
        h.insert("sec-websocket-protocol", HeaderValue::from_str(value).unwrap());
        h
    }

    #[test]
    fn bearer_protocol_token_parses_jwt() {
        let h = headers_with_protocol("bearer.eyJhbGciOiJIUzI1NiJ9.payload.sig");
        let (token, proto) = bearer_protocol_token(&h).expect("token from header");
        assert_eq!(token, "eyJhbGciOiJIUzI1NiJ9.payload.sig");
        assert_eq!(proto, "bearer.eyJhbGciOiJIUzI1NiJ9.payload.sig");
    }

    #[test]
    fn bearer_protocol_token_skips_non_bearer_entries() {
        let h = headers_with_protocol("graphql-ws, bearer.abc123 , chat");
        assert_eq!(
            bearer_protocol_token(&h).map(|(t, _)| t),
            Some("abc123".to_string())
        );
    }

    #[test]
    fn bearer_protocol_token_absent_or_empty() {
        assert!(bearer_protocol_token(&HeaderMap::new()).is_none());
        let h = headers_with_protocol("bearer.");
        assert!(bearer_protocol_token(&h).is_none(), "empty jwt rejected");
    }

    #[test]
    fn header_token_preferred_over_query_param() {
        let h = headers_with_protocol("bearer.header-jwt");
        let (token, proto) = resolve_ws_token(&h, Some("query-jwt"));
        assert_eq!(token.as_deref(), Some("header-jwt"));
        assert!(proto.is_some());
    }

    #[test]
    fn query_param_still_accepted_without_protocol_echo() {
        let (token, proto) = resolve_ws_token(&HeaderMap::new(), Some("query-jwt"));
        assert_eq!(token.as_deref(), Some("query-jwt"));
        assert!(proto.is_none(), "no protocol to echo for query auth");
    }

    #[test]
    fn no_token_anywhere() {
        assert_eq!(resolve_ws_token(&HeaderMap::new(), None), (None, None));
        assert_eq!(resolve_ws_token(&HeaderMap::new(), Some("")), (None, None));
    }
}
