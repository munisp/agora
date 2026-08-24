/**
 * Domain models mirrored from the platform services (see SPEC.md §7).
 * Field names match the Postgres schemas / JSON contracts emitted by the
 * Go and Python services behind the APISIX gateway.
 */

// ---------- identity-service ----------

export interface Tenant {
  id: string;
  slug: string;
  name: string;
  timezone: string;
  currency: string;
  locale: string;
  /** Per-tenant vocabulary overrides, e.g. { "booking": "reservation" }. */
  terminology: Record<string, string>;
  plan: "free" | "pro" | "business" | string;
  /** Industry pack id, e.g. "salon" | "clinic" | "consultancy" | "support-desk". */
  industry?: string;
  /** Resolved industry pack summary (absent on older identity-service builds). */
  pack?: IndustryPackSummary | null;
  created_at: string;
}

/** Booking policy carried by an industry pack (SPEC-CRM §C). */
export interface BookingPolicy {
  depositPercent?: number;
  noShowFeeCents?: number;
  phoneConfirmation?: boolean;
  intakeRequired?: boolean;
  cancellationWindowHours?: number;
}

/** Dashboard copy provided by an industry pack. */
export interface DashboardLabels {
  bookingSingular?: string;
  bookingPlural?: string;
  customerTerm?: string;
}

/** Resolved pack summary embedded in GET /v1/tenants/{slug}. */
export interface IndustryPackSummary {
  displayName?: string;
  terminology?: Record<string, string>;
  bookingPolicy?: BookingPolicy;
  dashboardLabels?: DashboardLabels;
}

export interface PublicSite {
  tenant_slug: string;
  site_slug: string;
  published: boolean;
  theme: SiteTheme;
  business_name: string;
  tagline?: string;
  timezone: string;
  currency: string;
  locale: string;
}

export interface SiteTheme {
  /** hex colour, e.g. "#8a6d4b" (legacy snake_case keys kept for compatibility) */
  accent?: string;
  logo_url?: string;
  hero_blurb?: string;
  /** Wave-3 theme editor contract (PUT /v1/site theme jsonb, camelCase). */
  primaryColor?: string;
  logoUrl?: string;
  heroTitle?: string;
  heroSubtitle?: string;
  /** layout template id, e.g. "classic" | "modern" | "compact" */
  template?: string;
  /**
   * White-label branding (SPEC-W7 Part C): display name shown on the public
   * site header/footer instead of the tenant business name when set.
   */
  brandName?: string;
  /** legacy snake_case companion of brandName for older readers */
  brand_name?: string;
  /**
   * White-label custom domain the tenant intends to serve the public site
   * from (informational note — the actual mapping lives at the gateway/DNS).
   */
  customDomain?: string;
}

// ---------- booking-service ----------

export type BookingStatus =
  | "pending"
  | "confirmed"
  | "rescheduled"
  | "cancelled"
  | "completed"
  | "no_show";

export type BookingSource = "voice" | "chat" | "web" | "admin" | string;

export interface Offering {
  id: string;
  tenant_id: string;
  name: string;
  description: string;
  duration_min: number;
  buffer_min: number;
  price_cents: number;
  currency: string;
  capacity: number;
  bookable: boolean;
  created_at: string;
}

export interface TeamMember {
  id: string;
  tenant_id: string;
  name: string;
  email: string;
  role: string;
  active: boolean;
}

export interface AvailabilityRule {
  id: string;
  tenant_id: string;
  team_member_id: string;
  /** 0 = Sunday .. 6 = Saturday */
  weekday: number;
  /** minutes after midnight, local tenant time */
  start_min: number;
  end_min: number;
  effective_from?: string | null;
  effective_to?: string | null;
}

export interface Contact {
  id: string;
  tenant_id: string;
  name: string;
  phone: string;
  email: string;
  notes: string;
}

export interface Booking {
  id: string;
  tenant_id: string;
  offering_id: string;
  team_member_id: string | null;
  contact_id: string;
  starts_at: string;
  ends_at: string;
  status: BookingStatus;
  source: BookingSource;
  idempotency_key: string;
  created_at: string;
  updated_at: string;
  /** Joined/enriched fields the service may include in list responses. */
  offering_name?: string;
  contact_name?: string;
  contact_phone?: string;
  team_member_name?: string;
}

export interface AvailabilitySlot {
  starts_at: string;
  ends_at: string;
  team_member_id?: string | null;
  remaining_capacity?: number;
}

// ---------- knowledge-service ----------

export interface KnowledgeDocument {
  id: string;
  tenant_id: string;
  title: string;
  body: string;
  source_url?: string | null;
  created_at: string;
}

export interface KnowledgeSearchHit {
  document_id: string;
  chunk_id?: string;
  title: string;
  snippet: string;
  score: number;
}

/**
 * Self-improving KB draft (knowledge-service, innovation 4): created when a
 * search scores below the suggestion threshold and the query looks like a
 * question. Approve creates a real document; reject deletes the suggestion.
 */
export interface KbSuggestion {
  id: string;
  tenant_id: string;
  /** the customer question that fell below the score threshold */
  question: string;
  suggested_title?: string | null;
  suggested_answer?: string | null;
  /** best RRF score seen for the question (for context) */
  score?: number | null;
  status: "pending" | "approved" | "rejected" | string;
  created_at: string;
}

/** Text-to-SQL analytics result (knowledge-service POST /v1/analytics/query). */
export interface AnalyticsQueryResult {
  sql: string;
  columns: string[];
  /** rows as positional arrays aligned with `columns` */
  rows: unknown[][];
  truncated?: boolean;
  explanation?: string;
}

// ---------- analytics-pipeline (revenue intelligence) ----------

/** Pricing recommendation (analytics-pipeline GET /v1/recommendations). */
export interface PricingRecommendation {
  tenant_id: string;
  offering_id: string;
  offering_name?: string;
  peak_hour_multiplier: number;
  suggested_deposit_pct: number;
  no_show_risk_band?: string;
  generated_at?: string;
}

// ---------- payments-service ----------

export interface AccountBalance {
  tenant_id: string;
  /** pending deposit holds */
  deposits_cents: number;
  /** captured, pay-out-able revenue */
  revenue_cents: number;
  /** lifetime paid out */
  paid_out_cents: number;
  currency: string;
}

export interface LedgerEntry {
  id: string;
  tenant_id: string;
  /** TigerBeetle transfer code: 100 hold, 101 capture, 102 refund, 103 no-show fee, 104 payout */
  code: number;
  amount_cents: number;
  currency: string;
  booking_id?: string | null;
  memo?: string;
  created_at: string;
}

export interface Payout {
  id: string;
  tenant_id: string;
  amount_cents: number;
  currency: string;
  status: "requested" | "settled" | "failed" | string;
  /** Mojaloop transfer reference when settled cross-border. */
  rail_ref?: string | null;
  created_at: string;
}

/**
 * Registered payout beneficiary (W44/K7 payee registry).
 * GET /api/payments/v1/beneficiaries → Beneficiary[]; payouts reference the
 * beneficiary by id — raw per-call payee details are rejected by the service.
 */
export interface Beneficiary {
  id: string;
  label: string;
  /** Mojaloop PartyIdInfo ({partyIdType, partyIdentifier}). */
  party_id_info: Record<string, unknown>;
}

// ---------- conversation-service ----------

/** Conversation (conversation-service GET /v1/conversations). */
export interface Conversation {
  id: string;
  tenant_id: string;
  site_slug: string;
  /** "voice" | "chat" | "phone" | "api" | "whatsapp" | "telegram" */
  channel: string;
  contact_phone?: string | null;
  started_at: string;
  ended_at?: string | null;
}

/** Conversation detail including scored turns (GET /v1/conversations/{id}). */
export interface ConversationWithTurns extends Conversation {
  turns: {
    role: string;
    text: string;
    /** lexicon sentiment score in [-1, 1]; null when unscored */
    sentiment?: number | null;
    ts?: string;
  }[];
}

// ---------- billing-engine (SPEC-W7 Part B) ----------

export type InvoiceStatus =
  | "draft"
  | "issued"
  | "paid"
  | "void"
  | "past_due"
  | string;

/** One rated usage line on an invoice (billing-engine line_items jsonb). */
export interface InvoiceLineItem {
  metric: string;
  /** total metered units in the period */
  quantity: number;
  /** units covered by the plan quota */
  included: number;
  /** billable units = max(0, quantity - included) */
  billable: number;
  unit_price_cents: number;
  amount_cents: number;
}

/** Invoice (billing-engine GET /v1/invoices). */
export interface Invoice {
  id: string;
  tenant_id: string;
  /** billing period, "YYYY-MM" */
  period: string;
  status: InvoiceStatus;
  subtotal_cents: number;
  currency: string;
  line_items?: InvoiceLineItem[];
  payment_ref?: string | null;
  created_at: string;
  issued_at?: string | null;
  paid_at?: string | null;
}

// ---------- booking-service: geospatial (SPEC-W8) ----------

/** One booking-joined contact point (GET /v1/locations/summary). */
export interface LocationPoint {
  lat: number;
  lng: number;
  booking_id?: string;
  offering_id?: string;
  starts_at?: string;
}

/** Server-side cluster cell (returned instead of raw points above 500 rows). */
export interface LocationCluster {
  lat: number;
  lng: number;
  count: number;
}

export interface LocationsSummary {
  points?: LocationPoint[];
  clusters?: LocationCluster[];
}

/**
 * Saved service area (GET /v1/service-areas). The geometry may come back as
 * `geojson` (object or string) or `geom` depending on the service build.
 */
export interface ServiceArea {
  id: string;
  tenant_id?: string;
  name: string;
  geojson?: unknown;
  geom?: unknown;
  meta?: Record<string, unknown>;
  created_at?: string;
}

/** Audience preview (POST /v1/geo/audience/preview). */
export interface GeoAudiencePreview {
  count: number;
  sample?: { contact_id: string; phone_masked: string }[];
}

export type GeoCampaignStatus =
  | "draft"
  | "running"
  | "completed"
  | "failed"
  | string;

/** Geo-targeted campaign (GET /v1/geo/campaigns). */
export interface GeoCampaign {
  id: string;
  tenant_id?: string;
  name: string;
  channel: string;
  message?: string;
  audience_count: number;
  status: GeoCampaignStatus;
  created_at: string;
  started_at?: string | null;
  completed_at?: string | null;
}

// ---------- conversation / voice ----------

export interface ChatRequest {
  tenant: string;
  message: string;
  conversation_id?: string;
  /** site slug when the chat originates from a public booking page */
  site_slug?: string;
}

export interface ChatResponse {
  conversation_id: string;
  reply: string;
  tool_calls?: { name: string; result: unknown }[];
}

export interface VoiceSession {
  /** LiveKit access token for the browser client */
  token: string;
  /** LiveKit websocket URL, e.g. ws://localhost:7880 */
  url: string;
  room: string;
}

// ---------- gateway-edge WebSocket events ----------

export interface BookingEvent {
  type:
    | "BookingCreated"
    | "BookingConfirmed"
    | "BookingRescheduled"
    | "BookingCancelled"
    | "BookingNoShow"
    | string;
  tenant_id: string;
  booking: Booking;
  time: string;
}

/**
 * Warm-handoff escalation (voice runtime, innovation 1): the receptionist
 * opened a LiveKit room and asks for a human. Fanned out on the /ws channel.
 */
export interface EscalationRequestedEvent {
  type: "EscalationRequested";
  data: {
    conversation_id: string;
    room: string;
    join_token_staff: string;
    site_slug?: string;
  };
  tenant_id?: string;
  time?: string;
}

export type WsEvent = BookingEvent | EscalationRequestedEvent;

// ---------- generic envelopes ----------

export interface ListResponse<T> {
  items: T[];
  total?: number;
}

export interface ApiProblem {
  error: string;
  message?: string;
  status?: number;
}

// ---------- notification-worker: outbound webhooks (Wave 5 #10) ----------

/** Subscription as returned by GET /v1/webhooks (secret masked). */
export interface WebhookSubscription {
  id: string;
  url: string;
  events: string[];
  active: boolean;
  secret_set: boolean;
  created_at: string;
}

/** Create response — the ONLY time the signing secret is shown. */
export interface WebhookSubscriptionCreated {
  id: string;
  tenant_id: string;
  url: string;
  events: string[];
  active: boolean;
  created_at: string;
  secret: string;
}

export type WebhookDeliveryStatus = "pending" | "retrying" | "delivered" | "dlq";

export interface WebhookDelivery {
  id: string;
  sub_id: string;
  tenant_id: string;
  event_id: string;
  event_type: string;
  status: WebhookDeliveryStatus;
  attempts: number;
  last_status_code?: number | null;
  next_retry_at?: string | null;
  created_at: string;
  updated_at: string;
}

/* ------------------------------------------------------------------ */
/* SPEC-W10 — voice runtime control-plane contract (Agent A / Agent C) */
/* ------------------------------------------------------------------ */

/** A single selectable voice as reported by a TTS provider. */
export interface VoiceInfo {
  id: string;
  languages: string[];
  gender?: string | null;
  labels?: string[];
}

/** A configured TTS provider and its voice catalog (GET /voice/voices). */
export interface VoiceProviderInfo {
  name: string;
  available: boolean;
  voices: VoiceInfo[];
}

export interface VoicesCatalog {
  providers: VoiceProviderInfo[];
}

/** Response of POST /voice/voices/enroll (XTTS brand-voice enrollment). */
export interface VoiceEnrollResponse {
  voice_id: string;
}


/* ------------------------------------------------------------------ */
/* SPEC-W18 — app platform registry (identity-service, contract §1)    */
/* ------------------------------------------------------------------ */

/**
 * Plan tier an app defaults to (drives the tier badge in the apps portal).
 * Contract §1 names starter | growth | scale; the seeded catalog ships the
 * real billing-engine tiers free | standard | pro (starter→free,
 * growth→standard, scale→pro) — accept both spellings.
 */
export type AppPlanTier =
  | "starter"
  | "growth"
  | "scale"
  | "free"
  | "standard"
  | "pro"
  | string;

/** Lifecycle status of a tenant's app installation (tenant_apps.status). */
export type TenantAppStatus =
  | "enabled"
  | "disabled"
  | "suspended"
  | "not_provisioned"
  | string;

/** Platform app catalog row (identity-service platform_apps, GET /v1/apps). */
export interface PlatformApp {
  app_id: string;
  name: string;
  version?: string;
  description?: string;
  category: string;
  /** emoji or short text glyph shown as the app icon */
  icon?: string;
  nav_route?: string;
  required_perms?: string[];
  default_plan_tier: AppPlanTier;
  /**
   * Honest provisioning note, e.g. "backend lands W19" — the portal renders
   * it as a subtle "module ships separately" hint when present.
   */
  backend_note?: string;
}

/**
 * Tenant-scoped app row (GET /v1/tenants/{slug}/apps): the catalog row LEFT
 * JOINed with tenant_apps, so every catalog app is present and status falls
 * back to "not_provisioned" when the tenant has no tenant_apps row.
 */
export interface TenantApp extends PlatformApp {
  status: TenantAppStatus;
  config?: Record<string, unknown>;
  provisioned_at?: string | null;
  provisioned_by?: string | null;
  updated_at?: string | null;
}

/* ------------------------------------------------------------------ */
/* SPEC-W38 — agents registry & capture primitive (conversation-service) */
/* ------------------------------------------------------------------ */

/** Declarative agent definition (agents.definition JSONB, SPEC-W38 F2). */
export interface AgentVoice {
  provider: string;
  voice_id: string;
  language: string;
}

export interface AgentOpsRules {
  max_call_seconds?: number;
  escalation_phone?: string;
}

export interface AgentDefinition {
  persona?: string;
  voice?: AgentVoice;
  instructions?: string;
  context_budget_tokens?: number;
  tool_allowlist?: string[];
  knowledge_packs?: string[];
  ops_rules?: AgentOpsRules;
}

export type AgentStatus = "active" | "disabled";

/** Agent row (GET/POST /v1/agents via APISIX /api/agents, SPEC-W38 F1). */
export interface Agent {
  id: string;
  tenant_id: string;
  name: string;
  slug: string;
  purpose: string;
  phone_number: string | null;
  status: AgentStatus;
  definition: AgentDefinition;
  created_at: string;
  updated_at: string;
}

export type CaptureFieldType = "string" | "number" | "boolean" | "enum";

/** One field of a capture schema (SPEC-W38 F3). */
export interface CaptureField {
  key: string;
  type: CaptureFieldType;
  label: string;
  required: boolean;
  /** allowed values — only meaningful when type === "enum" */
  options?: string[];
}

export interface CaptureSchema {
  id: string;
  tenant_id: string;
  agent_id: string;
  name: string;
  schema: { fields: CaptureField[] };
  active: boolean;
  created_at: string;
  updated_at: string;
}

/** Extracted post-call capture record (GET /v1/capture-records). */
export interface CaptureRecord {
  id: string;
  tenant_id: string;
  capture_schema_id: string;
  agent_id: string;
  conversation_id: string;
  data: Record<string, unknown>;
  extraction_confidence: number | null;
  created_at: string;
}
