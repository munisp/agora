/**
 * Typed mirrors of the booking-service BFF contracts (base path
 * /api/bookings via the APISIX gateway). Field names match the Go JSON
 * tags exactly:
 *
 *  - bookings:  services/booking-service/internal/store/bookings.go (Booking)
 *  - leads:     internal/store/leads.go (Lead) + httpapi/leads.go requests
 *  - referrals: internal/store/referrals.go (Referral) + httpapi/referrals.go
 *  - payouts:   internal/referrals (Payout) + httpapi/payouts.go
 *  - incidents: internal/store/incidents.go (Incident) + httpapi/incidents.go
 *  - devices:   SPEC-W16 cross-agent contract §1 (Agent B implements
 *               POST/DELETE /v1/devices — coded to the contract, not the
 *               in-flight Go code).
 *  - field capture: SPEC-W16 contract §4 / Agent B POST /v1/field/capture.
 *
 * Money convention (SPEC-W14 §2): every *_ngn amount is an integer in KOBO.
 */

// ---------------------------------------------------------------------------
// Bookings (today dashboard)
// ---------------------------------------------------------------------------

/** store.Booking — GET /v1/bookings → {bookings: Booking[]}. */
export interface Booking {
  id: string;
  tenant_id: string;
  offering_id: string;
  team_member_id: string;
  contact_id: string;
  starts_at: string; // RFC3339
  ends_at: string;
  status: string; // pending|confirmed|cancelled|completed|no_show
  source: string; // web|voice|api
  idempotency_key: string;
  created_at: string;
  updated_at: string;
}

// ---------------------------------------------------------------------------
// Leads + CAC (SPEC-W13)
// ---------------------------------------------------------------------------

export type LeadStatus = "new" | "contacted" | "qualified" | "converted" | "lost";

export type LeadChannel =
  | "voice"
  | "whatsapp"
  | "telegram"
  | "web"
  | "sms"
  | "webhook"
  | "ussd"
  | "qr"
  | "promo"
  | "field";

/** store.Lead — GET /v1/leads → {leads: Lead[]}. */
export interface Lead {
  lead_id: string;
  tenant_id: string;
  phone_e164: string;
  channel_of_first_touch: string;
  campaign_id: string | null;
  promo_code: string | null;
  utm?: Record<string, unknown>;
  lga_id: number | null;
  status: string;
  consent_id: string | null;
  dedupe_key: string;
  created_at: string;
  updated_at: string;
}

/** httpapi.createLeadRequest — POST /v1/leads → 201|200 {lead, created}. */
export interface CreateLeadRequest {
  phone_e164: string;
  channel: string; // channel_of_first_touch
  promo_code?: string;
  utm?: Record<string, unknown>;
  ref?: string; // QR slug
  campaign_id?: string;
  lga_id?: number;
  consent_id?: string;
}

export interface CreateLeadResponse {
  lead: Lead;
  /** false = dedupe hit, existing first-touch lead returned (HTTP 200). */
  created: boolean;
}

// ---------------------------------------------------------------------------
// Field capture queue (SPEC-W16 §4 / Agent B POST /v1/field/capture)
// ---------------------------------------------------------------------------

export interface GpsPoint {
  lat: number;
  lng: number;
  accuracy: number | null;
}

/**
 * One offline-queue item. Idempotent on client_id — the server dedupes
 * "field_capture:{client_id}" (contract §4), so retries are safe.
 */
export interface FieldCaptureItem {
  client_id: string; // uuid generated on-device at capture time
  kind: "lead_capture" | "checkin";
  payload: Record<string, unknown>;
  captured_at: string; // RFC3339
  gps: GpsPoint | null;
}

/**
 * ASSUMPTION: Agent B's POST /v1/field/capture body envelope. SPEC-W16 §4
 * defines the item shape; the batch wrapper ({items: [...]}) mirrors the
 * field PWA flush and is marked ASSUMPTION until Agent B's
 * docs/field-capture.md lands — adjust here only.
 */
export interface FieldCaptureRequest {
  items: FieldCaptureItem[];
}

// ---------------------------------------------------------------------------
// Referrals + commissions (SPEC-W14)
// ---------------------------------------------------------------------------

export type ReferralStatus =
  | "pending"
  | "verified"
  | "converted"
  | "paid"
  | "rejected";

/** store.Referral — GET /v1/referrals → {referrals: Referral[]}. */
export interface Referral {
  referral_id: string;
  tenant_id?: string;
  referrer_type: "contact" | "agent" | "staff" | string;
  referrer_id: string;
  referee_phone: string;
  campaign_id?: string | null;
  status: ReferralStatus | string;
  bounty_rule_id?: string | null;
  created_at?: string;
  verified_at?: string | null;
  paid_at?: string | null;
}

/** httpapi.createReferralRequest — POST /v1/referrals → 201|200 {referral, created}. */
export interface CreateReferralRequest {
  referrer_type: string; // contact | agent | staff
  referrer_id: string;
  referee_phone: string;
  campaign_id?: string;
  bounty_rule_id?: string;
}

export interface CreateReferralResponse {
  referral: Referral;
  created: boolean;
}

/** referrals.Payout — GET /v1/payouts?status=&limit= → {payouts: Payout[]}. */
export interface Payout {
  payout_id: string;
  tenant_id?: string;
  beneficiary_id: string;
  amount_ngn: number; // integer kobo
  status: "queued" | "processing" | "paid" | "failed" | string;
  provider?: "paystack" | "flutterwave" | string | null;
  provider_ref?: string | null;
  failure_reason?: string | null;
  created_at?: string;
  paid_at?: string | null;
}

// ---------------------------------------------------------------------------
// Incidents (SPEC-W11 Part B)
// ---------------------------------------------------------------------------

export type IncidentSeverity = "critical" | "high" | "medium" | "low";
export type IncidentStatus = "new" | "dispatched" | "acknowledged" | "closed";

/** store.Incident — GET /v1/incidents?status=&from=&to= → {incidents: [...]}. */
export interface Incident {
  id: string;
  tenant_id: string;
  reference_number: string;
  incident_type: string;
  severity: IncidentSeverity | string;
  payload: unknown; // json.RawMessage — arbitrary provider payload
  status: IncidentStatus | string;
  created_at: string;
  dispatched_at?: string;
}

/** store.IncidentDelivery — POST /v1/incidents/{id}/dispatch → {deliveries: [...]}. */
export interface IncidentDelivery {
  id: string;
  incident_id: string;
  endpoint_url: string;
  status: string; // pending | retrying | delivered | dlq
  attempts: number;
  last_status_code?: number;
  last_error: string;
  created_at: string;
  delivered_at?: string;
}

// ---------------------------------------------------------------------------
// Devices / push registration (SPEC-W16 §1)
// ---------------------------------------------------------------------------

/** POST /v1/devices body — called after push permission grant. */
export interface RegisterDeviceRequest {
  token: string;
  platform: "android" | "ios" | "web";
  app: "admin" | "field";
}
