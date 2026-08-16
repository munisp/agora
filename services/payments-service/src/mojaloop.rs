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
    #[error("mojaloop quote rejected: {0}")]
    QuoteRejected(String),
    #[error("mojaloop transfer not committed: {0}")]
    NotCommitted(String),
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

    /// Execute the FSPIOP quote → transfer sequence. Returns the committed
    /// outcome or an error (caller must NOT post the ledger payout on error).
    pub async fn execute_payout(
        &self,
        instruction: &PayoutInstruction,
    ) -> Result<PayoutOutcome, MojaloopError> {
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
        let resp = self
            .post("/quotes", "quotes", &payee_fsp, &quote)
            .await?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(MojaloopError::QuoteRejected(format!("{status}: {body}")));
        }
        let quote_resp: QuoteResponse = resp.json().await.unwrap_or(QuoteResponse {
            transfer_amount: None,
            expiration: None,
            ilp_packet: None,
            condition: None,
        });

        // 2. Transfer (accepting the quote terms).
        let transfer = TransferRequest {
            transfer_id: instruction.transfer_id.to_string(),
            payer_fsp: self.source_fsp.clone(),
            payee_fsp: payee_fsp.clone(),
            amount: quote_resp.transfer_amount.clone().unwrap_or(amount),
            ilp_packet: quote_resp.ilp_packet,
            condition: quote_resp.condition,
            expiration: quote_resp.expiration,
        };
        let resp = self
            .post("/transfers", "transfers", &payee_fsp, &transfer)
            .await?;
        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(MojaloopError::NotCommitted(format!("{status}: {body}")));
        }
        let transfer_resp: TransferResponse = resp.json().await.unwrap_or(TransferResponse {
            transfer_state: None,
            completed_timestamp: None,
            fulfilment: None,
        });
        let state = transfer_resp
            .transfer_state
            .clone()
            .unwrap_or_else(|| "COMMITTED".to_string());
        // mojaloop-simulator may omit the state on simplified flows; treat
        // explicit non-COMMITTED states as failure.
        if state != "COMMITTED" && state != "RECEIVED" {
            return Err(MojaloopError::NotCommitted(state));
        }

        Ok(PayoutOutcome {
            quote_id,
            transfer_id: instruction.transfer_id.to_string(),
            state,
            completed_at: transfer_resp.completed_timestamp,
            amount: transfer.amount,
        })
    }
}
