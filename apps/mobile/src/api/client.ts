/**
 * Booking-service BFF client (SPEC-W16 §5).
 *
 * Every request goes to the APISIX gateway under /api/bookings (base URL
 * from app config) with:
 *   - Authorization: Bearer <keycloak access token>   (expo-secure-store)
 *   - X-Tenant-Slug: <tenant slug>                    (expo-secure-store)
 * matching the booking-service tenantMiddleware contract
 * (services/booking-service/internal/httpapi/server.go: tenant from the JWT
 * claim or the X-Tenant-Slug header, validated by middleware).
 *
 * Typed endpoint functions mirror the real BFF shapes — see ./types.ts for
 * the exact Go sources each interface mirrors.
 */
import { apiBase } from "../config";
import { getAccessToken, getTenantSlug } from "../auth/session";
import type {
  Booking,
  CreateLeadRequest,
  CreateLeadResponse,
  CreateReferralRequest,
  CreateReferralResponse,
  FieldCaptureRequest,
  Incident,
  IncidentDelivery,
  IncidentStatus,
  Lead,
  LeadChannel,
  LeadStatus,
  Payout,
  Referral,
  ReferralStatus,
  RegisterDeviceRequest,
} from "./types";

export class ApiError extends Error {
  status: number;
  body: string;
  constructor(status: number, body: string) {
    super(`API ${status}: ${body.slice(0, 200)}`);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }
}

/** Thrown when there is no usable session (caller routes to /login). */
export class NotAuthenticatedError extends Error {
  constructor() {
    super("no session: sign in first");
    this.name = "NotAuthenticatedError";
  }
}

interface RequestOptions {
  method?: "GET" | "POST" | "PUT" | "DELETE";
  body?: unknown;
  query?: Record<string, string | number | undefined>;
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const [token, tenantSlug] = await Promise.all([getAccessToken(), getTenantSlug()]);
  if (!token || !tenantSlug) throw new NotAuthenticatedError();

  const url = new URL(`${apiBase()}${path}`);
  if (opts.query) {
    for (const [k, v] of Object.entries(opts.query)) {
      if (v !== undefined && v !== "") url.searchParams.set(k, String(v));
    }
  }

  const headers: Record<string, string> = {
    accept: "application/json",
    authorization: `Bearer ${token}`,
    "x-tenant-slug": tenantSlug,
  };
  let body: string | undefined;
  if (opts.body !== undefined) {
    headers["content-type"] = "application/json";
    body = JSON.stringify(opts.body);
  }

  const res = await fetch(url.toString(), {
    method: opts.method ?? "GET",
    headers,
    body,
  });
  const text = await res.text();
  if (!res.ok) throw new ApiError(res.status, text);
  if (text.length === 0) return undefined as T;
  return JSON.parse(text) as T;
}

// ---------------------------------------------------------------------------
// Bookings — GET /v1/bookings (today dashboard). `mine=true` restricts to
// the caller's team member via the JWT email claim (server-side).
// ---------------------------------------------------------------------------

export interface ListBookingsFilter {
  status?: string;
  mine?: boolean;
  team_member_id?: string;
}

export async function listBookings(f: ListBookingsFilter = {}): Promise<Booking[]> {
  const data = await request<{ bookings: Booking[] }>("/v1/bookings", {
    query: {
      status: f.status,
      mine: f.mine === undefined ? undefined : String(f.mine),
      team_member_id: f.team_member_id,
    },
  });
  return data.bookings ?? [];
}

// ---------------------------------------------------------------------------
// Leads (SPEC-W13) — GET/POST /v1/leads, POST /v1/leads/{id}/status
// ---------------------------------------------------------------------------

export interface ListLeadsFilter {
  status?: LeadStatus;
  channel?: LeadChannel;
  campaign_id?: string;
  from?: string; // RFC3339 or YYYY-MM-DD
  to?: string;
}

export async function listLeads(f: ListLeadsFilter = {}): Promise<Lead[]> {
  const data = await request<{ leads: Lead[] }>("/v1/leads", {
    query: {
      status: f.status,
      channel: f.channel,
      campaign_id: f.campaign_id,
      from: f.from,
      to: f.to,
    },
  });
  return data.leads ?? [];
}

export async function createLead(req: CreateLeadRequest): Promise<CreateLeadResponse> {
  return request<CreateLeadResponse>("/v1/leads", { method: "POST", body: req });
}

export async function transitionLead(id: string, status: LeadStatus): Promise<Lead> {
  const data = await request<{ lead: Lead }>(`/v1/leads/${id}/status`, {
    method: "POST",
    body: { status },
  });
  return data.lead;
}

// ---------------------------------------------------------------------------
// Field capture (SPEC-W16 §4 / Agent B) — POST /v1/field/capture (batched,
// idempotent on client_id). Used by the offline flush; the lead-capture
// modal posts /v1/leads directly when online.
// ---------------------------------------------------------------------------

export async function submitFieldCapture(items: FieldCaptureRequest): Promise<void> {
  await request<unknown>("/v1/field/capture", { method: "POST", body: items });
}

// ---------------------------------------------------------------------------
// Referrals + payouts (SPEC-W14) — /v1/referrals, /v1/payouts
// ---------------------------------------------------------------------------

export async function listReferrals(status?: ReferralStatus): Promise<Referral[]> {
  const data = await request<{ referrals: Referral[] }>("/v1/referrals", {
    query: { status },
  });
  return data.referrals ?? [];
}

export async function createReferral(
  req: CreateReferralRequest,
): Promise<CreateReferralResponse> {
  return request<CreateReferralResponse>("/v1/referrals", {
    method: "POST",
    body: req,
  });
}

export async function listPayouts(status?: string, limit?: number): Promise<Payout[]> {
  const data = await request<{ payouts: Payout[] }>("/v1/payouts", {
    query: { status, limit },
  });
  return data.payouts ?? [];
}

// ---------------------------------------------------------------------------
// Incidents (SPEC-W11) — GET /v1/incidents, POST /v1/incidents/{id}/dispatch
// ---------------------------------------------------------------------------

export interface ListIncidentsFilter {
  status?: IncidentStatus;
  from?: string;
  to?: string;
}

export async function listIncidents(f: ListIncidentsFilter = {}): Promise<Incident[]> {
  const data = await request<{ incidents: Incident[] }>("/v1/incidents", {
    query: { status: f.status, from: f.from, to: f.to },
  });
  return data.incidents ?? [];
}

export async function dispatchIncident(id: string): Promise<IncidentDelivery[]> {
  const data = await request<{ deliveries: IncidentDelivery[] }>(
    `/v1/incidents/${id}/dispatch`,
    { method: "POST" },
  );
  return data.deliveries ?? [];
}

// ---------------------------------------------------------------------------
// Devices (SPEC-W16 §1) — POST /v1/devices, DELETE /v1/devices/{token}
// ---------------------------------------------------------------------------

export async function registerDevice(req: RegisterDeviceRequest): Promise<void> {
  await request<unknown>("/v1/devices", { method: "POST", body: req });
}

export async function deleteDevice(token: string): Promise<void> {
  await request<unknown>(`/v1/devices/${encodeURIComponent(token)}`, {
    method: "DELETE",
  });
}
