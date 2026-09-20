//! Shared data model: CloudEvents envelope, usage records, invoices (SPEC-W7 B).

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ---------------------------------------------------------------------------
// CloudEvents 1.0 (SPEC §4) — same envelope shape as payments-service.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize)]
pub struct CloudEvent<T: Serialize> {
    pub specversion: &'static str,
    pub id: String,
    pub source: String,
    #[serde(rename = "type")]
    pub type_: String,
    pub subject: String,
    pub time: String,
    /// CloudEvents extension attribute carrying the tenant (SPEC §4).
    pub tenantid: String,
    pub data: T,
}

impl<T: Serialize> CloudEvent<T> {
    pub fn new(source: &str, type_: &str, subject: &str, tenant_id: &str, data: T) -> Self {
        Self {
            specversion: "1.0",
            id: Uuid::new_v4().to_string(),
            source: source.to_string(),
            type_: type_.to_string(),
            subject: subject.to_string(),
            time: Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true),
            tenantid: tenant_id.to_string(),
            data,
        }
    }
}

/// Inbound envelope for `opendesk.usage.events` (B1). Only the fields the
/// metering path needs are decoded.
#[derive(Debug, Clone, Deserialize)]
pub struct RawCloudEvent {
    pub id: String,
    #[serde(rename = "type")]
    #[allow(dead_code)]
    pub type_: String,
    #[serde(default)]
    pub data: serde_json::Value,
}

/// data payload of com.opendesk.usage.UsageRecord
/// (see services/booking-service/internal/bookingops/usage.go).
#[derive(Debug, Clone, Deserialize)]
pub struct UsageRecordData {
    pub tenant_id: Uuid,
    pub metric: String,
    pub value: i64,
    pub ts: DateTime<Utc>,
    #[serde(default)]
    pub meta: serde_json::Value,
}

// ---------------------------------------------------------------------------
// Invoices (B2)
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum InvoiceStatus {
    Draft,
    Issued,
    Paid,
    Void,
    PastDue,
}

impl InvoiceStatus {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Draft => "draft",
            Self::Issued => "issued",
            Self::Paid => "paid",
            Self::Void => "void",
            Self::PastDue => "past_due",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "draft" => Some(Self::Draft),
            "issued" => Some(Self::Issued),
            "paid" => Some(Self::Paid),
            "void" => Some(Self::Void),
            "past_due" => Some(Self::PastDue),
            _ => None,
        }
    }

    /// Legal state-machine transitions (SPEC-W7 B2/B3):
    /// draft -> issued | void; issued -> paid | past_due | void;
    /// past_due -> paid | void. paid and void are terminal.
    pub fn can_transition_to(&self, next: InvoiceStatus) -> bool {
        use InvoiceStatus::*;
        matches!(
            (self, next),
            (Draft, Issued)
                | (Draft, Void)
                | (Issued, Paid)
                | (Issued, PastDue)
                | (Issued, Void)
                | (PastDue, Paid)
                | (PastDue, Void)
        )
    }
}

/// One rated metric line on an invoice.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct LineItem {
    pub metric: String,
    /// Total metered quantity in the period.
    pub quantity: i64,
    /// Free quota absorbed by the plan/rate card.
    pub included_quota: i64,
    /// max(0, quantity - included_quota).
    pub billable: i64,
    pub unit_price_cents: i64,
    /// billable * unit_price_cents.
    pub amount_cents: i64,
    /// VAT-ready tax basis points (0..=10000), stamped from the rate card at
    /// generation (SPEC-W45 K18). Serde default 0 so legacy stored line-item
    /// JSON (pre-0006) keeps decoding.
    #[serde(default)]
    pub tax_bps: i64,
}

#[derive(Debug, Clone, Serialize)]
pub struct Invoice {
    pub id: Uuid,
    pub tenant_id: Uuid,
    pub period: String,
    pub status: InvoiceStatus,
    pub subtotal_cents: i64,
    pub currency: String,
    pub line_items: Vec<LineItem>,
    /// Tenant billing contact (SPEC-W45 K12-side billing field); set at
    /// generate time / via PATCH, carried into paid/voided event payloads.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub billing_email: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub payment_ref: Option<String>,
    pub created_at: DateTime<Utc>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub issued_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub paid_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize)]
pub struct RateCard {
    pub tenant_id: Uuid,
    pub metric: String,
    pub unit_price_cents: i64,
    pub included_quota: i64,
    pub currency: String,
    /// VAT-ready tax basis points (0..=10000, default 0).
    pub tax_bps: i64,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn invoice_state_machine_allows_spec_transitions() {
        use InvoiceStatus::*;
        // Happy path.
        assert!(Draft.can_transition_to(Issued));
        assert!(Issued.can_transition_to(Paid));
        // Dunning then payment.
        assert!(Issued.can_transition_to(PastDue));
        assert!(PastDue.can_transition_to(Paid));
        // Void from any pre-payment state.
        assert!(Draft.can_transition_to(Void));
        assert!(Issued.can_transition_to(Void));
        assert!(PastDue.can_transition_to(Void));
    }

    #[test]
    fn invoice_state_machine_rejects_illegal_transitions() {
        use InvoiceStatus::*;
        // Terminal states.
        for next in [Draft, Issued, Paid, Void, PastDue] {
            assert!(!Paid.can_transition_to(next), "paid is terminal");
            assert!(!Void.can_transition_to(next), "void is terminal");
        }
        // No skipping / rewinding.
        assert!(!Draft.can_transition_to(Paid));
        assert!(!Draft.can_transition_to(PastDue));
        assert!(!Issued.can_transition_to(Draft));
        assert!(!PastDue.can_transition_to(Issued));
        assert!(!PastDue.can_transition_to(Draft));
        // No self-transitions (idempotent "paid" is handled separately as
        // a 200 replay, not as a state transition).
        for s in [Draft, Issued, Paid, Void, PastDue] {
            assert!(!s.can_transition_to(s));
        }
    }

    #[test]
    fn line_item_tax_bps_defaults_to_zero_for_legacy_stored_json() {
        // Legacy (pre-0006) line-item JSON has no tax_bps key; it must keep
        // decoding with the 0 default (SPEC-W45 K18 VAT-ready foundation).
        let legacy = serde_json::json!({
            "metric": "booking",
            "quantity": 1200,
            "included_quota": 1000,
            "billable": 200,
            "unit_price_cents": 50,
            "amount_cents": 10_000,
        });
        let li: LineItem = serde_json::from_value(legacy).expect("legacy line item decodes");
        assert_eq!(li.tax_bps, 0);
        assert_eq!(li.amount_cents, 10_000);
        // A stamped tax survives the round trip.
        let stamped = LineItem { tax_bps: 750, ..li };
        let round: LineItem =
            serde_json::from_value(serde_json::to_value(&stamped).unwrap()).unwrap();
        assert_eq!(round.tax_bps, 750);
    }

    #[test]
    fn status_roundtrips_through_str() {
        for s in [
            InvoiceStatus::Draft,
            InvoiceStatus::Issued,
            InvoiceStatus::Paid,
            InvoiceStatus::Void,
            InvoiceStatus::PastDue,
        ] {
            assert_eq!(InvoiceStatus::parse(s.as_str()), Some(s));
        }
        assert_eq!(InvoiceStatus::parse("overdue"), None);
    }
}
