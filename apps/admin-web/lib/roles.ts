/**
 * Shared realm-role helpers (SPEC-W7 Part C). The roles come from the
 * Keycloak `realm_access.roles` claim, surfaced on the Auth.js session as
 * `session.realmRoles` (see lib/auth.ts). Use these helpers for both
 * server-side guards (page redirects) and client-side nav/section hiding so
 * the two never drift apart.
 */

/** Keycloak realm roles understood by the dashboard. */
export type RealmRole =
  | "owner"
  | "admin"
  | "staff"
  | "viewer"
  | "analyst"
  | "billing";

/** Analytics dashboard (KPIs, trends, text-to-SQL) — read-only. */
export const ANALYTICS_ROLES: readonly RealmRole[] = [
  "owner",
  "admin",
  "analyst",
];

/** Billing section (invoices, rate cards, payouts, QR payment links). */
export const BILLING_ROLES: readonly RealmRole[] = ["owner", "billing"];

/** Locations map dashboard (SPEC-W8 Part C) — operational staff can view. */
export const LOCATIONS_ROLES: readonly RealmRole[] = [
  "owner",
  "admin",
  "staff",
];

/** Geo-targeted campaigns (SPEC-W8 Part C) — messaging spend, owner/admin. */
export const GEO_CAMPAIGN_ROLES: readonly RealmRole[] = ["owner", "admin"];

/** Voices studio (SPEC-W10 Part C) — operational staff can browse/preview. */
export const VOICES_ROLES: readonly RealmRole[] = ["owner", "admin", "staff"];

/** Brand-voice enrollment (SPEC-W10 Part C) — consent-sensitive, owner/admin. */
export const VOICES_ENROLL_ROLES: readonly RealmRole[] = ["owner", "admin"];

export function hasAnyRole(
  roles: readonly string[] | undefined | null,
  allowed: readonly RealmRole[],
): boolean {
  if (!roles) return false;
  return allowed.some((r) => roles.includes(r));
}

/** owner/admin/analyst — staff and viewer never see analytics. */
export function canViewAnalytics(
  roles: readonly string[] | undefined | null,
): boolean {
  return hasAnyRole(roles, ANALYTICS_ROLES);
}

/** owner/billing — staff, viewer and analyst never see billing. */
export function canViewBilling(
  roles: readonly string[] | undefined | null,
): boolean {
  return hasAnyRole(roles, BILLING_ROLES);
}

/** owner/admin/staff — viewers and analysts never see the locations map. */
export function canViewLocations(
  roles: readonly string[] | undefined | null,
): boolean {
  return hasAnyRole(roles, LOCATIONS_ROLES);
}

/** owner/admin — staff, viewer, analyst and billing never launch geo campaigns. */
export function canViewGeoCampaigns(
  roles: readonly string[] | undefined | null,
): boolean {
  return hasAnyRole(roles, GEO_CAMPAIGN_ROLES);
}

/** owner/admin/staff — viewers, analysts and billing never see the voices studio. */
export function canViewVoices(
  roles: readonly string[] | undefined | null,
): boolean {
  return hasAnyRole(roles, VOICES_ROLES);
}

/** owner/admin — only they may enroll brand voices (voice-cloning consent). */
export function canEnrollVoices(
  roles: readonly string[] | undefined | null,
): boolean {
  return hasAnyRole(roles, VOICES_ENROLL_ROLES);
}
