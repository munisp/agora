//! SPEC-W99 C1: Mojaloop DFSP quote marketplace (innovation #11 MVP).
//!
//! READ-ONLY quote comparison across multiple DFSPs (development
//! "multi-region" posture: every configured DFSP is queried for the same
//! payout instruction and the caller sees the real per-DFSP FSPIOP quote
//! terms side by side). No ledger writes, no transfer sends — only
//! `POST /quotes` calls. Money movement stays on the single configured
//! MOJALOOP_ENDPOINT rail (`/v1/payouts`, P-13 NGN-only) — this module
//! changes NO execution behavior.
//!
//! DFSPs are configured via `PAYOUT_DFSPS` (`fspId=endpoint` pairs). Zero
//! configured DFSPs => the route fails closed with 503 (never a fabricated
//! quote). Per-DFSP failures are isolated: one unreachable DFSP never
//! sinks the comparison (partial results + honest `errors[]`), but when
//! EVERY DFSP fails the route answers 502.

use serde::Serialize;
use uuid::Uuid;

use crate::mojaloop::{
    decimal_to_minor, MojaloopAdapter, Money, PartyIdInfo, PayoutInstruction,
};

/// One configured destination DFSP (a quoting peer).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DfspConfig {
    /// FSPIOP-Destination header value (e.g. "mojaloop", "dfsp2").
    pub fsp_id: String,
    /// Absolute http(s) base URL of the DFSP's FSPIOP endpoint.
    pub endpoint: String,
}

/// Parse `PAYOUT_DFSPS` (`fspId=endpoint,fspId2=endpoint2`). Invalid entries
/// are skipped with a warning (a malformed entry must never take the whole
/// marketplace down at boot); duplicate fsp ids are rejected the same way.
/// Returns the validated list — possibly empty (the route then 503s).
pub fn parse_dfsps(raw: &str) -> Vec<DfspConfig> {
    let mut out: Vec<DfspConfig> = Vec::new();
    for entry in raw.split(',').map(str::trim).filter(|e| !e.is_empty()) {
        let (fsp_id, endpoint) = match entry.split_once('=') {
            Some((a, b)) => (a.trim(), b.trim()),
            None => {
                tracing::warn!(entry, "PAYOUT_DFSPS entry without '=' ignored");
                continue;
            }
        };
        let id_ok = !fsp_id.is_empty()
            && fsp_id.len() <= 32
            && fsp_id
                .chars()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-');
        if !id_ok {
            tracing::warn!(fsp_id, "PAYOUT_DFSPS invalid fsp id ignored");
            continue;
        }
        if !(endpoint.starts_with("http://") || endpoint.starts_with("https://")) {
            tracing::warn!(endpoint, "PAYOUT_DFSPS non-http(s) endpoint ignored");
            continue;
        }
        if out.iter().any(|d| d.fsp_id == fsp_id) {
            tracing::warn!(fsp_id, "PAYOUT_DFSPS duplicate fsp id ignored");
            continue;
        }
        out.push(DfspConfig {
            fsp_id: fsp_id.to_string(),
            endpoint: endpoint.trim_end_matches('/').to_string(),
        });
    }
    out
}

/// Successful per-DFSP quote, with fees surfaced verbatim (C1 transparency).
#[derive(Debug, Clone, Serialize)]
pub struct DfspQuote {
    pub fsp_id: String,
    pub quote_id: String,
    pub transfer_amount: Money,
    pub payee_fsp_fee: Option<Money>,
    pub payee_fsp_commission: Option<Money>,
    /// transfer - fee - commission in minor units, computed ONLY when the
    /// DFSP disclosed both fee fields and they parse exactly. None = fees
    /// undisclosed; the UI says so instead of inventing a number.
    pub receive_amount_cents: Option<u64>,
    pub fees_disclosed: bool,
    pub expiration: Option<String>,
}

/// Per-DFSP failure, reported honestly (never silently dropped).
#[derive(Debug, Clone, Serialize)]
pub struct DfspError {
    pub fsp_id: String,
    pub error: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct QuoteComparison {
    pub quotes: Vec<DfspQuote>,
    pub errors: Vec<DfspError>,
    /// fsp ids in rank order (cheapest receive amount first).
    pub ranked: Vec<String>,
    pub best_fsp: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Marketplace {
    dfsps: Vec<DfspConfig>,
    timeout: std::time::Duration,
}

/// Rank comparator (C1): receive amount DESC (undisclosed fees fall back to
/// the transfer amount), then fees-disclosed first, then fsp_id ASC for
/// determinism. Kept as a standalone fn so a redirection proof can break it.
pub fn rank_quotes(quotes: &[DfspQuote]) -> Vec<String> {
    let mut ranked: Vec<&DfspQuote> = quotes.iter().collect();
    ranked.sort_by(|a, b| {
        let recv_a = a
            .receive_amount_cents
            .unwrap_or_else(|| decimal_to_minor(&a.transfer_amount.amount).unwrap_or(0));
        let recv_b = b
            .receive_amount_cents
            .unwrap_or_else(|| decimal_to_minor(&b.transfer_amount.amount).unwrap_or(0));
        recv_b
            .cmp(&recv_a)
            .then(b.fees_disclosed.cmp(&a.fees_disclosed))
            .then(a.fsp_id.cmp(&b.fsp_id))
    });
    ranked.iter().map(|q| q.fsp_id.clone()).collect()
}

impl Marketplace {
    pub fn new(dfsps: Vec<DfspConfig>, quote_timeout_ms: u64) -> Self {
        Self {
            dfsps,
            timeout: std::time::Duration::from_millis(quote_timeout_ms.max(1)),
        }
    }

    pub fn dfsp_count(&self) -> usize {
        self.dfsps.len()
    }

    /// SPEC-W100 C1: endpoint of a configured DFSP by fsp id (execution
    /// binding); None when the id is not configured.
    pub fn dfsp_endpoint(&self, fsp_id: &str) -> Option<String> {
        self.dfsps
            .iter()
            .find(|d| d.fsp_id == fsp_id)
            .map(|d| d.endpoint.clone())
    }

    /// Fan the quote out to every configured DFSP in parallel; per-DFSP
    /// timeout and error isolation. Deterministic output order (config
    /// order), ranking via [`rank_quotes`].
    pub async fn quote_compare(
        &self,
        tenant_id: &str,
        amount_cents: u64,
        currency: &str,
        payee: &PartyIdInfo,
    ) -> QuoteComparison {
        let instruction = PayoutInstruction {
            transfer_id: Uuid::new_v4(),
            amount_cents,
            currency: currency.to_string(),
            payee: payee.clone(),
            payer: PartyIdInfo {
                party_id_type: "ALIAS".to_string(),
                party_identifier: format!("tenant:{tenant_id}"),
            },
        };
        let mut handles = Vec::with_capacity(self.dfsps.len());
        for dfsp in &self.dfsps {
            let dfsp = dfsp.clone();
            let instruction = instruction.clone();
            let timeout = self.timeout;
            handles.push(tokio::spawn(async move {
                let result = tokio::time::timeout(timeout, async {
                    // One adapter per DFSP endpoint: request_quote posts to
                    // the adapter's own endpoint.
                    let per_dfsp = MojaloopAdapter::new(dfsp.endpoint.clone());
                    per_dfsp.request_quote(&instruction, &dfsp.fsp_id).await
                })
                .await;
                (dfsp, result)
            }));
        }
        let mut quotes = Vec::new();
        let mut errors = Vec::new();
        for h in handles {
            let (dfsp, result) = match h.await {
                Ok(pair) => pair,
                Err(e) => {
                    // JoinError (panic/cancel) — should not happen; report it.
                    errors.push(DfspError {
                        fsp_id: "unknown".to_string(),
                        error: format!("quote task join error: {e}"),
                    });
                    continue;
                }
            };
            match result {
                Ok(Ok(vq)) => {
                    let fee = vq
                        .payee_fsp_fee
                        .as_ref()
                        .and_then(|m| decimal_to_minor(&m.amount));
                    let commission = vq
                        .payee_fsp_commission
                        .as_ref()
                        .and_then(|m| decimal_to_minor(&m.amount));
                    let transfer = decimal_to_minor(&vq.transfer_amount.amount);
                    let (receive, disclosed) = match (transfer, fee, commission) {
                        (Some(t), Some(f), Some(c)) => (Some(t.saturating_sub(f + c)), true),
                        _ => (None, false),
                    };
                    quotes.push(DfspQuote {
                        fsp_id: dfsp.fsp_id,
                        quote_id: vq.quote_id,
                        transfer_amount: vq.transfer_amount,
                        payee_fsp_fee: vq.payee_fsp_fee,
                        payee_fsp_commission: vq.payee_fsp_commission,
                        receive_amount_cents: receive,
                        fees_disclosed: disclosed,
                        expiration: vq.expiration,
                    });
                }
                Ok(Err(e)) => errors.push(DfspError {
                    fsp_id: dfsp.fsp_id,
                    error: e,
                }),
                Err(_) => errors.push(DfspError {
                    fsp_id: dfsp.fsp_id,
                    error: format!("quote timed out after {}ms", self.timeout.as_millis()),
                }),
            }
        }
        // Deterministic presentation order: fsp_id asc within both lists.
        quotes.sort_by(|a, b| a.fsp_id.cmp(&b.fsp_id));
        errors.sort_by(|a, b| a.fsp_id.cmp(&b.fsp_id));
        let ranked = rank_quotes(&quotes);
        let best_fsp = ranked.first().cloned();
        QuoteComparison {
            quotes,
            errors,
            ranked,
            best_fsp,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn q(fsp: &str, transfer: u64, fee: Option<u64>, commission: Option<u64>) -> DfspQuote {
        let money = |cents: u64| Money {
            currency: "NGN".to_string(),
            amount: format!("{}.{:02}", cents / 100, cents % 100),
        };
        let (receive, disclosed) = match (fee, commission) {
            (Some(f), Some(c)) => (Some(transfer - f - c), true),
            _ => (None, false),
        };
        DfspQuote {
            fsp_id: fsp.to_string(),
            quote_id: "q".to_string(),
            transfer_amount: money(transfer),
            payee_fsp_fee: fee.map(money),
            payee_fsp_commission: commission.map(money),
            receive_amount_cents: receive,
            fees_disclosed: disclosed,
            expiration: None,
        }
    }

    #[test]
    fn parse_dfsps_validates_and_dedupes() {
        let d = parse_dfsps("mojaloop=http://mojaloop:8444,dfsp2=https://dfsp2.example.com/");
        assert_eq!(d.len(), 2);
        assert_eq!(d[0].fsp_id, "mojaloop");
        assert_eq!(d[1].endpoint, "https://dfsp2.example.com"); // trailing slash stripped
        // dup id, bad id, missing '=', non-http endpoint all ignored.
        let d = parse_dfsps(
            "mojaloop=http://a:1,mojaloop=http://b:2,BADUPPER=http://c:3,\
             no-equals-sign,dfsp4=ftp://x,dfsp5=http://d:5",
        );
        assert_eq!(d.len(), 2);
        assert_eq!(d[0].fsp_id, "mojaloop");
        assert_eq!(d[1].fsp_id, "dfsp5");
        assert!(parse_dfsps("").is_empty());
        assert!(parse_dfsps("  , ,").is_empty());
    }

    #[test]
    fn ranking_prefers_highest_receive_then_disclosure_then_id() {
        // dfsp-a: transfer 1000.00, fee 10.00, commission 5.00 -> receive 985.00
        // dfsp-b: transfer 1000.00, fee 20.00, commission 5.00 -> receive 975.00
        // dfsp-c: transfer 1000.00, fees undisclosed -> fallback 100000, but
        //         disclosure tiebreak puts disclosed quotes first at equal recv.
        let quotes = vec![
            q("dfsp-b", 100_000, Some(2_000), Some(500)),
            q("dfsp-c", 100_000, None, None),
            q("dfsp-a", 100_000, Some(1_000), Some(500)),
        ];
        let ranked = rank_quotes(&quotes);
        // dfsp-c ranks first on raw transfer (fallback 100000 > 98500), but
        // between dfsp-a (98500) and dfsp-b (97500) the better receive wins.
        assert_eq!(ranked, vec!["dfsp-c", "dfsp-a", "dfsp-b"]);
        // Equal receive: disclosed beats undisclosed.
        let quotes = vec![q("dfsp-c", 98_500, None, None), q("dfsp-a", 100_000, Some(1_000), Some(500))];
        assert_eq!(rank_quotes(&quotes), vec!["dfsp-a", "dfsp-c"]);
        // Full tie: fsp_id asc for determinism.
        let quotes = vec![q("dfsp-b", 100_000, Some(1_000), Some(500)), q("dfsp-a", 100_000, Some(1_000), Some(500))];
        assert_eq!(rank_quotes(&quotes), vec!["dfsp-a", "dfsp-b"]);
    }
}
