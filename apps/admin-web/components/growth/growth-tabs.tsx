"use client";

/**
 * Sub-navigation for the Growth section (SPEC-W14 Agent C). Links between
 * the four growth pages; each link is shown only when the caller's roles
 * pass that page's server-side gate (computed by the server page and passed
 * down as flags), so users never see a link that would bounce them.
 */
import Link from "next/link";
import { usePathname } from "next/navigation";
import { cn } from "@/lib/utils";

const PAGES = [
  { segment: "referrals", label: "Referrals", gate: "analytics" as const },
  { segment: "rules", label: "Commission rules", gate: "billing" as const },
  { segment: "ledger", label: "Ledger", gate: "analytics" as const },
  { segment: "payouts", label: "Payouts", gate: "billing" as const },
];

export function GrowthTabs({
  orgSlug,
  canAnalytics,
  canBilling,
}: {
  orgSlug: string;
  /** session passes canViewAnalytics (referrals/ledger/leaderboard pages) */
  canAnalytics: boolean;
  /** session passes canViewBilling / Permify manage_billing (rules/payouts) */
  canBilling: boolean;
}) {
  const pathname = usePathname();
  const base = `/app/${orgSlug}/growth`;
  const visible = PAGES.filter((p) =>
    p.gate === "analytics" ? canAnalytics : canBilling,
  );
  if (visible.length === 0) return null;
  return (
    <div className="mb-4 flex flex-wrap gap-1 rounded-md border border-border p-0.5 w-fit">
      {visible.map((p) => {
        const href = `${base}/${p.segment}`;
        const active = pathname.startsWith(href);
        return (
          <Link
            key={p.segment}
            href={href}
            className={cn(
              "rounded px-2.5 py-1 text-xs font-medium",
              active
                ? "bg-secondary text-secondary-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {p.label}
          </Link>
        );
      })}
    </div>
  );
}
