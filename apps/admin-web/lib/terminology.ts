/**
 * Terminology resolution (SPEC-CRM §D).
 *
 * Dashboard copy is resolved with the precedence:
 *   tenant.terminology overrides  >  pack.dashboardLabels  >  built-in defaults
 *
 * The tenant's industry pack is embedded in the identity-service tenant
 * response (`tenant.pack.dashboardLabels`); older responses without a pack
 * simply fall through to the defaults.
 */

import type { Tenant } from "./types";

export interface BookingLabels {
  /** e.g. "Booking" / "Reservation" / "Ticket" */
  bookingSingular: string;
  /** e.g. "Bookings" / "Reservations" / "Tickets" */
  bookingPlural: string;
  /** e.g. "Guest" / "Patient" / "Client" */
  customerTerm: string;
}

export const DEFAULT_BOOKING_LABELS: BookingLabels = {
  bookingSingular: "Booking",
  bookingPlural: "Bookings",
  customerTerm: "Guest",
};

export function resolveBookingLabels(
  tenant: Pick<Tenant, "terminology" | "pack"> | null | undefined,
): BookingLabels {
  const term = tenant?.terminology ?? {};
  const packLabels = tenant?.pack?.dashboardLabels ?? {};

  const singular =
    term.booking ?? packLabels.bookingSingular ?? DEFAULT_BOOKING_LABELS.bookingSingular;

  return {
    bookingSingular: singular,
    bookingPlural:
      term.bookings ?? packLabels.bookingPlural ?? `${singular}s`,
    customerTerm:
      term.contact ??
      term.customer ??
      packLabels.customerTerm ??
      DEFAULT_BOOKING_LABELS.customerTerm,
  };
}


/** Sidebar + page-title wording from the tenant's industry pack (SPEC-CRM §D): an insurer sees "Policyholders",
 * "Advisors" and "Services" where a salon sees "Clients", "Stylists" and "Services". */
export interface NavLabels {
  bookings: string;
  contacts: string;
  team: string;
  offerings: string;
}

export const DEFAULT_NAV_LABELS: NavLabels = {
  bookings: "Bookings",
  contacts: "Contacts",
  team: "Team",
  offerings: "Offerings",
};

const UNCOUNTABLE = new Set(["staff", "personnel", "crew"]);

function pluralWord(word: string): string {
  const w = word.toLowerCase();
  if (UNCOUNTABLE.has(w)) return word;
  if (/[^aeiou]y$/i.test(word)) return word.slice(0, -1) + "ies";
  if (/(s|sh|ch|x|z)$/i.test(word)) return word + "es";
  return word + "s";
}

/** "relationship manager" -> "Relationship Managers" (title case, plural on the last word). */
export function pluralLabel(term: string): string {
  const words = term.trim().split(/\s+/).filter(Boolean);
  if (!words.length) return term;
  words[words.length - 1] = pluralWord(words[words.length - 1]);
  return words.map((w) => w.charAt(0).toUpperCase() + w.slice(1)).join(" ");
}

export function resolveNavLabels(
  tenant: Pick<Tenant, "terminology" | "pack"> | null | undefined,
): NavLabels {
  const term = (tenant?.terminology ?? {}) as Record<string, string | undefined>;
  const booking = resolveBookingLabels(tenant);
  const pick = (value: string | undefined, fallback: string) =>
    value && value.trim() ? pluralLabel(value) : fallback;
  return {
    bookings: term.booking || tenant?.pack?.dashboardLabels?.bookingPlural ? booking.bookingPlural : DEFAULT_NAV_LABELS.bookings,
    contacts: pick(term.contact ?? tenant?.pack?.dashboardLabels?.customerTerm, DEFAULT_NAV_LABELS.contacts),
    team: pick(term.team_member, DEFAULT_NAV_LABELS.team),
    offerings: pick(term.offering, DEFAULT_NAV_LABELS.offerings),
  };
}
