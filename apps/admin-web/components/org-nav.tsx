"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  LayoutDashboard,
  CalendarCheck,
  CalendarDays,
  Store,
  Users,
  Clock,
  BookOpen,
  Mic,
  Globe,
  CreditCard,
  UsersRound,
  BarChart3,
  LineChart,
  Webhook,
  MessagesSquare,
  MapPin,
  Target,
  AudioLines,
  Settings,
} from "lucide-react";
import { cn } from "@/lib/utils";
import {
  canViewAnalytics,
  canViewBilling,
  canViewGeoCampaigns,
  canViewLocations,
  canViewVoices,
} from "@/lib/roles";

/** Self-hosted Twenty CRM UI (SPEC-CRM §A). */
const CRM_URL = "http://localhost:3100";
/** Grafana dashboards (observability profile, SPEC-W3 §1). */
const GRAFANA_URL = "http://localhost:3002";

type NavItem =
  | { segment: string; label: string; icon: typeof LayoutDashboard }
  | { external: string; label: string; icon: typeof LayoutDashboard };

const items: NavItem[] = [
  { segment: "", label: "Overview", icon: LayoutDashboard },
  { segment: "bookings", label: "Bookings", icon: CalendarCheck },
  { segment: "schedule", label: "My Schedule", icon: CalendarDays },
  { segment: "offerings", label: "Offerings", icon: Store },
  { segment: "team", label: "Team", icon: Users },
  { segment: "availability", label: "Availability", icon: Clock },
  { segment: "knowledge", label: "Knowledge", icon: BookOpen },
  { segment: "voice-agent", label: "Voice Agent", icon: Mic },
  { segment: "public-site", label: "Public Site", icon: Globe },
  { segment: "billing", label: "Billing", icon: CreditCard },
  { segment: "analytics", label: "Analytics", icon: BarChart3 },
  { segment: "webhooks", label: "Webhooks", icon: Webhook },
  { segment: "channels", label: "Channels", icon: MessagesSquare },
  { segment: "locations", label: "Locations", icon: MapPin },
  { segment: "geo-campaigns", label: "Geo campaigns", icon: Target },
  { segment: "voices", label: "Voices", icon: AudioLines },
  { external: CRM_URL, label: "CRM", icon: UsersRound },
  { external: GRAFANA_URL, label: "Grafana", icon: LineChart },
  { segment: "settings", label: "Settings", icon: Settings },
];

const linkClass = (active: boolean) =>
  cn(
    "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
    active
      ? "bg-secondary text-secondary-foreground"
      : "text-muted-foreground hover:bg-accent hover:text-foreground",
  );

export function OrgNav({
  orgSlug,
  roles = [],
}: {
  orgSlug: string;
  /** caller's Keycloak realm roles (session.realmRoles) — gates nav entries */
  roles?: string[];
}) {
  const pathname = usePathname();
  const base = `/app/${orgSlug}`;

  // Role-based visibility (SPEC-W7 Part C): billing is owner/billing only,
  // analytics is owner/admin/analyst; staff/viewer see neither. The pages
  // enforce the same rule server-side — this is the client-side hiding.
  const visible = items.filter((item) => {
    if ("segment" in item && item.segment === "billing")
      return canViewBilling(roles);
    if ("segment" in item && item.segment === "analytics")
      return canViewAnalytics(roles);
    // SPEC-W8 Part C: locations is owner/admin/staff; geo campaigns is
    // owner/admin only. The pages enforce the same rule server-side.
    if ("segment" in item && item.segment === "locations")
      return canViewLocations(roles);
    if ("segment" in item && item.segment === "geo-campaigns")
      return canViewGeoCampaigns(roles);
    // SPEC-W10 Part C: voices studio is owner/admin/staff; the page enforces
    // the same rule server-side.
    if ("segment" in item && item.segment === "voices")
      return canViewVoices(roles);
    return true;
  });

  return (
    <nav className="flex flex-col gap-0.5 px-3">
      {visible.map((item) => {
        const Icon = item.icon;
        if ("external" in item) {
          return (
            <a
              key={item.label}
              href={item.external}
              target="_blank"
              rel="noreferrer"
              className={linkClass(false)}
            >
              <Icon className="h-4 w-4 shrink-0" />
              {item.label}
            </a>
          );
        }
        const href = item.segment ? `${base}/${item.segment}` : base;
        const active =
          item.segment === "" ? pathname === base : pathname.startsWith(href);
        return (
          <Link key={href} href={href} className={linkClass(active)}>
            <Icon className="h-4 w-4 shrink-0" />
            {item.label}
          </Link>
        );
      })}
    </nav>
  );
}
