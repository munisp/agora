/**
 * Social Publisher app types + display helpers (SPEC-W21 Agent B). Mirrors
 * the booking-service internal/socialpub JSON shapes exactly (snake_case)
 * — the BFF path is /api/bookings/v1/social/*. List unwrap uses the shared
 * tolerant unwrap<T>() from components/apps/types.ts (READ-ONLY import).
 *
 * Budgets are kobo int64 everywhere (₦1 = 100 kobo).
 */
import { unwrap } from "@/components/apps/types";
import { formatMoney, formatDateTime } from "@/lib/utils";

export { unwrap };

// ---------------------------------------------------------------------------
// Enums (backend mirrors)
// ---------------------------------------------------------------------------

export const PROVIDERS = ["meta", "tiktok", "x"] as const;
export type Provider = (typeof PROVIDERS)[number];

export const ACCOUNT_STATUSES = ["connected", "expired", "revoked"] as const;
export type AccountStatus = (typeof ACCOUNT_STATUSES)[number];

export const CREATIVE_KINDS = ["text", "image", "video"] as const;
export type CreativeKind = (typeof CREATIVE_KINDS)[number];

export const POST_STATUSES = [
  "draft",
  "queued",
  "publishing",
  "published",
  "failed",
] as const;
export type PostStatus = (typeof POST_STATUSES)[number];

export const AD_OBJECTIVES = ["awareness", "traffic", "engagement"] as const;
export type AdObjective = (typeof AD_OBJECTIVES)[number];

export const AD_STATUSES = [
  "draft",
  "review",
  "active",
  "paused",
  "rejected",
] as const;
export type AdStatus = (typeof AD_STATUSES)[number];

type BadgeVariant =
  | "default"
  | "secondary"
  | "outline"
  | "success"
  | "warning"
  | "destructive"
  | "info";

export const PROVIDER_META: Record<string, { label: string; variant: BadgeVariant }> = {
  meta: { label: "Meta", variant: "info" },
  tiktok: { label: "TikTok", variant: "default" },
  x: { label: "X", variant: "secondary" },
};

export const ACCOUNT_STATUS_META: Record<
  string,
  { label: string; variant: BadgeVariant }
> = {
  connected: { label: "Connected", variant: "success" },
  expired: { label: "Expired", variant: "warning" },
  revoked: { label: "Revoked", variant: "destructive" },
};

export const POST_STATUS_META: Record<
  string,
  { label: string; variant: BadgeVariant }
> = {
  draft: { label: "Draft", variant: "outline" },
  queued: { label: "Queued", variant: "info" },
  publishing: { label: "Publishing", variant: "warning" },
  published: { label: "Published", variant: "success" },
  failed: { label: "Failed", variant: "destructive" },
};

export const AD_STATUS_META: Record<
  string,
  { label: string; variant: BadgeVariant }
> = {
  draft: { label: "Draft", variant: "outline" },
  review: { label: "In review", variant: "warning" },
  active: { label: "Active", variant: "success" },
  paused: { label: "Paused", variant: "secondary" },
  rejected: { label: "Rejected", variant: "destructive" },
};

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

/** Account row of GET /v1/social/accounts. */
export interface SocialAccount {
  id: string;
  tenant_id?: string;
  provider: Provider;
  account_ref: string;
  display_name: string;
  status: AccountStatus;
  political_ads_authorized: boolean;
  created_at?: string;
  updated_at?: string;
}

/** Creative row of GET /v1/social/creatives. */
export interface SocialCreative {
  id: string;
  tenant_id?: string;
  name: string;
  kind: CreativeKind;
  body: string;
  media_url: string | null;
  disclaimer_text: string | null;
  created_at?: string;
  updated_at?: string;
}

/** Post row of GET /v1/social/posts. */
export interface SocialPost {
  id: string;
  tenant_id?: string;
  account_id: string;
  creative_id: string;
  status: PostStatus;
  provider_post_id: string | null;
  error: string | null;
  published_at: string | null;
  created_at?: string;
}

/** Targeting jsonb of an ad. */
export interface SocialTargeting {
  lgas: string[];
  age_min: number;
  age_max: number;
  interests: string[];
}

/** Ad row of GET /v1/social/ads. */
export interface SocialAd {
  id: string;
  tenant_id?: string;
  account_id: string;
  creative_id: string;
  name: string;
  objective: AdObjective;
  budget_kobo: number;
  daily_budget_kobo: number;
  targeting: SocialTargeting;
  political: boolean;
  disclaimer_text: string | null;
  status: AdStatus;
  provider_ad_id: string | null;
  error: string | null;
  created_at?: string;
  updated_at?: string;
}

/** Response of GET /v1/social/ads/{id}/stats. */
export interface AdStatsResponse {
  ad_id: string;
  provider_ad_id: string;
  provider: string;
  /** Honest disclosure: true while the deterministic mock is the default. */
  mock: boolean;
  stats: {
    impressions: number;
    reach: number;
    clicks: number;
    spend_kobo: number;
  };
}

/** Response of POST /v1/social/ads/{id}/launch. */
export interface LaunchResponse {
  ad: SocialAd;
  rejected: boolean;
  reason?: string;
}

// ---------------------------------------------------------------------------
// Display helpers
// ---------------------------------------------------------------------------

/** Kobo → ₦ formatting (kobo is the NGN cent unit). */
export function formatKobo(kobo: number): string {
  return formatMoney(kobo, "NGN", "en-NG");
}

export function formatTs(iso?: string | null): string {
  if (!iso) return "—";
  try {
    return formatDateTime(iso);
  } catch {
    return iso;
  }
}

/** Short id rendering for uuid columns. */
export function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

/** The effective disclaimer the launch gate checks: the ad's own, else the
 * creative's (mirrors the backend EffectiveDisclaimer). */
export function effectiveDisclaimer(
  ad: Pick<SocialAd, "disclaimer_text">,
  creative?: SocialCreative | null,
): string {
  const own = ad.disclaimer_text?.trim();
  if (own) return own;
  return creative?.disclaimer_text?.trim() ?? "";
}

/** Whether the launch gate would pass for this ad+account pair (UI-side
 * pre-check; the backend re-enforces it — 422/409 on violation). */
export function launchGateBlocker(
  ad: SocialAd,
  account: SocialAccount | undefined,
  creative: SocialCreative | undefined,
): string | null {
  if (!account) return "Account not found";
  if (account.status !== "connected") {
    return `Account is ${account.status} — reconnect before launch`;
  }
  if (ad.political) {
    if (!account.political_ads_authorized) {
      return "Account is not authorized for political ads (external Meta process — see docs)";
    }
    if (!effectiveDisclaimer(ad, creative)) {
      return "Political ads require a disclaimer (ad or creative)";
    }
  }
  return null;
}
