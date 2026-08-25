//! Mojaloop adapter (SPEC §9): FSPIOP-style `POST /quotes` then `POST /transfers`
//! against the mojaloop-simulator (`MOJALOOP_ENDPOINT`, default
//! `http://mojaloop:8444`) for cross-border payout of tenant earnings.

use chrono::Utc;
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

#[derive(Debug, Error)]
pub enum MojaloopError {
    #[error("mojaloop HTTP error: {0}")]
    Http(#[from] reqwest::Error),
}

#[derive(Debug, Clone)]
pub struct MojaloopAdapter {
    http: reqwest::Client,
    endpoint: String,
    /// FSPIOP-Source FSP id for this platform.
    source_fsp: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PartyIdInfo {
    pub party_id_type: String,
    pub party_identifier: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Party {
    pub party_id_info: PartyIdInfo,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Money {
    pub currency: String,
    pub amount: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct TransactionType {
    scenario: &'static str,
    initiator: &'static str,
    initiator_type: &'static str,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct QuoteRequest {
    quote_id: String,
    transaction_id: String,
    payer: Party,
    payee: Party,
    amount_type: &'static str,
    amount: Money,
    transaction_type: TransactionType,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct QuoteResponse {
    transfer_amount: Option<Money>,
    expiration: Option<String>,
    ilp_packet: Option<String>,
    condition: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct TransferRequest {
    transfer_id: String,
    payer_fsp: String,
    payee_fsp: String,
    amount: Money,
    ilp_packet: Option<String>,
    condition: Option<String>,
    expiration: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct TransferResponse {
    transfer_state: Option<String>,
    completed_timestamp: Option<String>,
    /// FSPIOP fulfilment (ILP); decoded for forward compatibility with real
    /// switch responses, not yet used in the payout decision.
    #[allow(dead_code)]
    fulfilment: Option<String>,
}

/// SPEC-W43 P-01 (contract C3): tri-state outcome of a rail payout attempt.
/// ONLY an explicit `COMMITTED` from a well-formed response counts as
/// committed; decode failures, missing/ambiguous states and transport errors
/// after the transfer was sent are UNKNOWN (reconciler territory), and
/// explicit rejections are FAILED.
#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "snake_case", tag = "rail_outcome")]
pub enum PayoutRailOutcome {
    Committed(PayoutOutcome),
    Failed(String),
    Unknown(String),
}

/// Result of re-querying the rail for a previously-unknown transfer
/// (reconciler sweep).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RailQueryState {
    Committed,
    Failed(String),
    Unknown(String),
}

/// Pure classification of an FSPIOP `transferState` (P-01): only explicit
/// COMMITTED commits; explicit ABORTED fails; RECEIVED / missing / anything
/// else is UNKNOWN — never defaulted to COMMITTED.
pub fn classify_transfer_state(state: Option<&str>) -> RailQueryState {
    match state {
        Some("COMMITTED") => RailQueryState::Committed,
        Some("ABORTED") => RailQueryState::Failed("transferState=ABORTED".to_string()),
        Some(other) => RailQueryState::Unknown(format!("transferState={other}")),
        None => RailQueryState::Unknown("transferState missing from response".to_string()),
    }
}

/// Exact decimal-string -> minor units ("1250.00" -> 125000). None on
/// anything unparseable or with more than 2 fraction digits.
fn decimal_to_minor(s: &str) -> Option<u64> {
    let s = s.trim();
    let (major, frac) = match s.split_once('.') {
        Some((m, f)) => (m, f),
        None => (s, ""),
    };
    if frac.len() > 2 {
        return None;
    }
    if major.is_empty() {
        return None;
    }
    let frac_digits = frac.len();
    let major: u64 = major.parse().ok()?;
    let frac: u64 = if frac.is_empty() { 0 } else { frac.parse().ok()? };
    let frac = if frac_digits == 1 { frac * 10 } else { frac };
    Some(major.checked_mul(100)?.checked_add(frac)?)
}

/// P-08: the quote's echoed transfer amount must match the requested
/// amount/currency exactly (integer minor-unit comparison).
pub fn quote_echo_matches(requested: &Money, echoed: &Money) -> bool {
    if requested.currency != echoed.currency {
        return false;
    }
    match (
        decimal_to_minor(&requested.amount),
        decimal_to_minor(&echoed.amount),
    ) {
        (Some(a), Some(b)) => a == b,
        _ => false,
    }
}

/// Outcome of a successful (committed) payout on the Mojaloop rail.
#[derive(Debug, Clone, Serialize)]
pub struct PayoutOutcome {
    pub quote_id: String,
    pub transfer_id: String,
    pub state: String,
    pub completed_at: Option<String>,
    pub amount: Money,
}

/// Payout instruction passed from the REST layer.
#[derive(Debug, Clone)]
pub struct PayoutInstruction {
    /// Deterministic id (derived from the caller's idempotency key) so retries
    /// of the same payout are idempotent on the rail.
    pub transfer_id: Uuid,
    pub amount_cents: u64,
    pub currency: String,
    pub payee: PartyIdInfo,
    pub payer: PartyIdInfo,
}

fn minor_to_decimal(amount_cents: u64) -> String {
    format!("{}.{:02}", amount_cents / 100, amount_cents % 100)
}

impl MojaloopAdapter {
    pub fn new(endpoint: String) -> Self {
        Self {
            // RS-006: shared client with 5s connect / 30s overall timeouts.
            http: crate::http_client(),
            endpoint,
            source_fsp: "opendesk".to_string(),
        }
    }

    fn fspiop_date() -> String {
        // IMF-fixdate (HTTP date), required by FSPIOP.
        Utc::now().format("%a, %d %b %Y %H:%M:%S GMT").to_string()
    }

    async fn post<T: Serialize>(
        &self,
        path: &str,
        resource: &str,
        dest_fsp: &str,
        body: &T,
    ) -> Result<reqwest::Response, MojaloopError> {
        let url = format!("{}{}", self.endpoint, path);
        let content_type = format!("application/vnd.interoperability.{resource}+json;version=1.0");
        let resp = self
            .http
            .post(url)
            .header("content-type", content_type.as_str())
            .header("accept", content_type.as_str())
            .header("date", Self::fspiop_date())
            .header("fspiop-source", self.source_fsp.as_str())
            .header("fspiop-destination", dest_fsp)
            .json(body)
            .send()
            .await?;
        Ok(resp)
    }

    /// Execute the FSPIOP quote → transfer sequence (SPEC-W43 P-01, C3).
    ///
    /// Tri-state result: only an explicit `COMMITTED` from a well-formed
    /// transfer response yields [`PayoutRailOutcome::Committed`]. Quote-stage
    /// failures are `Failed` (no transfer was sent, so the rail cannot have
    /// moved money). Anything ambiguous AFTER the transfer was sent —
    /// transport error, non-success status, undecodable body, missing state,
    /// `RECEIVED`, any unrecognized state — is `Unknown` and must be recorded
    /// for the reconciler. NEVER defaults to COMMITTED.
    pub async fn execute_payout(
        &self,
        instruction: &PayoutInstruction,
    ) -> PayoutRailOutcome {
        let amount = Money {
            currency: instruction.currency.clone(),
            amount: minor_to_decimal(instruction.amount_cents),
        };
        let quote_id = Uuid::new_v4().to_string();
        let payee_fsp = "payeefsp".to_string(); // mojaloop-simulator default payee FSP

        // 1. Quote.
        let quote = QuoteRequest {
            quote_id: quote_id.clone(),
            transaction_id: instruction.transfer_id.to_string(),
            payer: Party {
                party_id_info: instruction.payer.clone(),
            },
            payee: Party {
                party_id_info: instruction.payee.clone(),
            },
            amount_type: "SEND",
            amount: amount.clone(),
            transaction_type: TransactionType {
                scenario: "TRANSFER",
                initiator: "PAYER",
                initiator_type: "BUSINESS",
            },
        };
        let resp = match self.post("/quotes", "quotes", &payee_fsp, &quote).await {
            Ok(r) => r,
            // Quote-stage transport failure: no transfer was sent yet.
            Err(e) => return PayoutRailOutcome::Failed(format!("quote request failed: {e}")),
        };
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return PayoutRailOutcome::Failed(format!("quote rejected: {status}: {body}"));
        }
        let quote_resp: QuoteResponse = match resp.json().await {
            Ok(q) => q,
            Err(e) => {
                return PayoutRailOutcome::Failed(format!("unparseable quote response: {e}"))
            }
        };
        // P-08: verify the quote echo (amount + currency) before accepting
        // the quote terms — a rail that echoes different terms is rejected.
        if let Some(echo) = &quote_resp.transfer_amount {
            if !quote_echo_matches(&amount, echo) {
                return PayoutRailOutcome::Failed(format!(
                    "quote echo mismatch: requested {} {}, rail echoed {} {}",
                    amount.amount, amount.currency, echo.amount, echo.currency
                ));
            }
        }

        // 2. Transfer (accepting the verified quote terms).
        let transfer = TransferRequest {
            transfer_id: instruction.transfer_id.to_string(),
            payer_fsp: self.source_fsp.clone(),
            payee_fsp: payee_fsp.clone(),
            amount: quote_resp.transfer_amount.clone().unwrap_or(amount),
            ilp_packet: quote_resp.ilp_packet,
            condition: quote_resp.condition,
            expiration: quote_resp.expiration,
        };
        let resp = match self.post("/transfers", "transfers", &payee_fsp, &transfer).await {
            Ok(r) => r,
            // The transfer may have reached the rail: UNKNOWN, reconcile.
            Err(e) => {
                return PayoutRailOutcome::Unknown(format!(
                    "transfer request failed after it may have been sent: {e}"
                ))
            }
        };
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            // A non-success status after sending is ambiguous (proxy errors,
            // rail error callbacks after acceptance): UNKNOWN, reconcile.
            return PayoutRailOutcome::Unknown(format!("transfer returned {status}: {body}"));
        }
        let transfer_resp: TransferResponse = match resp.json().await {
            Ok(t) => t,
            // Decode failure = unknown (C3), never committed.
            Err(e) => {
                return PayoutRailOutcome::Unknown(format!(
                    "unparseable transfer response: {e}"
                ))
            }
        };
        match classify_transfer_state(transfer_resp.transfer_state.as_deref()) {
            RailQueryState::Committed => PayoutRailOutcome::Committed(PayoutOutcome {
                quote_id,
                transfer_id: instruction.transfer_id.to_string(),
                state: "COMMITTED".to_string(),
                completed_at: transfer_resp.completed_timestamp,
                amount: transfer.amount,
            }),
            RailQueryState::Failed(reason) => PayoutRailOutcome::Failed(reason),
            RailQueryState::Unknown(reason) => PayoutRailOutcome::Unknown(reason),
        }
    }

    /// Re-query the rail for a transfer's current state (reconciler sweep
    /// for UNKNOWN payout attempts, C3). Conservative: only an explicit
    /// COMMITTED resolves as committed; 404 means the rail never accepted the
    /// transfer (failed); everything else stays unknown.
    pub async fn query_transfer_state(&self, transfer_id: Uuid) -> RailQueryState {
        let url = format!("{}/transfers/{}", self.endpoint, transfer_id);
        let content_type = "application/vnd.interoperability.transfers+json;version=1.0";
        let resp = self
            .http
            .get(url)
            .header("accept", content_type)
            .header("date", Self::fspiop_date())
            .header("fspiop-source", self.source_fsp.as_str())
            .send()
            .await;
        let resp = match resp {
            Ok(r) => r,
            Err(e) => return RailQueryState::Unknown(format!("query failed: {e}")),
        };
        if resp.status() == reqwest::StatusCode::NOT_FOUND {
            return RailQueryState::Failed("transfer unknown to the rail (404)".to_string());
        }
        if !resp.status().is_success() {
            return RailQueryState::Unknown(format!("query returned {}", resp.status()));
        }
        match resp.json::<TransferResponse>().await {
            Ok(t) => classify_transfer_state(t.transfer_state.as_deref()),
            Err(e) => RailQueryState::Unknown(format!("unparseable query response: {e}")),
        }
    }
}

// ---------------------------------------------------------------------------
// Unit tests (SPEC-W43 P-01/P-08): transfer-state classification, quote-echo
// verification, decimal conversion. Pure functions — no HTTP needed.
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_explicit_committed_commits() {
        assert_eq!(
            classify_transfer_state(Some("COMMITTED")),
            RailQueryState::Committed
        );
        // RECEIVED is pending/unknown, NOT committed.
        assert!(matches!(
            classify_transfer_state(Some("RECEIVED")),
            RailQueryState::Unknown(_)
        ));
        // Missing state is unknown — never defaulted to COMMITTED.
        assert!(matches!(
            classify_transfer_state(None),
            RailQueryState::Unknown(_)
        ));
        // Unrecognized states are unknown (reconciler keeps sweeping).
        assert!(matches!(
            classify_transfer_state(Some("garbage")),
            RailQueryState::Unknown(_)
        ));
        // Explicit ABORTED is a failure.
        assert!(matches!(
            classify_transfer_state(Some("ABORTED")),
            RailQueryState::Failed(_)
        ));
    }

    #[test]
    fn decimal_to_minor_exact() {
        assert_eq!(decimal_to_minor("1250.00"), Some(125_000));
        assert_eq!(decimal_to_minor("0.05"), Some(5));
        assert_eq!(decimal_to_minor("0.5"), Some(50));
        assert_eq!(decimal_to_minor("12"), Some(1_200));
        assert_eq!(decimal_to_minor("1.234"), None, "more than 2 dp rejected");
        assert_eq!(decimal_to_minor(""), None);
        assert_eq!(decimal_to_minor("abc"), None);
        assert_eq!(decimal_to_minor(".50"), None);
    }

    #[test]
    fn quote_echo_must_match_amount_and_currency() {
        let req = Money {
            currency: "NGN".to_string(),
            amount: minor_to_decimal(125_000),
        };
        let ok = Money {
            currency: "NGN".to_string(),
            amount: "1250.0".to_string(), // equal value, different formatting
        };
        assert!(quote_echo_matches(&req, &ok));
        let wrong_amount = Money {
            currency: "NGN".to_string(),
            amount: "1250.01".to_string(),
        };
        assert!(!quote_echo_matches(&req, &wrong_amount));
        let wrong_ccy = Money {
            currency: "USD".to_string(),
            amount: "1250.00".to_string(),
        };
        assert!(!quote_echo_matches(&req, &wrong_ccy));
        let junk = Money {
            currency: "NGN".to_string(),
            amount: "lots".to_string(),
        };
        assert!(!quote_echo_matches(&req, &junk));
    }

    #[test]
    fn minor_units_render_major_decimal() {
        assert_eq!(minor_to_decimal(125_000), "1250.00");
        assert_eq!(minor_to_decimal(5), "0.05");
        assert_eq!(minor_to_decimal(0), "0.00");
    }
}
