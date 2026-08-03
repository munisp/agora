"use client";

/**
 * CAC overview cards (SPEC-W13 Agent D): blended CAC, payback estimate,
 * LTV/CAC ratio and conversion totals from contract §5. Mirrors the
 * KpiDashboard card language (analytics/kpi-dashboard.tsx).
 */
import {
  CircleDollarSign,
  Hourglass,
  Scale,
  UserCheck,
} from "lucide-react";
import {
  Card,
  CardContent,
  CardHeader,
} from "@/components/ui/card";
import {
  formatCount,
  formatNaira,
  type CacSummary,
} from "@/components/cac/types";

export function CacSummaryCards({
  summary,
  loading,
  days,
}: {
  summary: CacSummary | null;
  loading: boolean;
  days: number;
}) {
  const totals = summary
    ? summary.by_channel.reduce(
        (acc, row) => ({
          leads: acc.leads + row.leads,
          conversions: acc.conversions + row.conversions,
        }),
        { leads: 0, conversions: 0 },
      )
    : { leads: 0, conversions: 0 };

  const ltv = summary?.ltv_ngn;
  const ltvCacRatio =
    summary && ltv && summary.blended_cac_ngn > 0
      ? ltv / summary.blended_cac_ngn
      : null;

  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <CacCard
        icon={<CircleDollarSign className="h-4 w-4 text-muted-foreground" />}
        label={`Blended CAC · last ${days}d`}
        value={
          loading || !summary ? "—" : formatNaira(summary.blended_cac_ngn)
        }
        hint="Total spend ÷ conversions, all channels"
      />
      <CacCard
        icon={<Hourglass className="h-4 w-4 text-muted-foreground" />}
        label="Payback estimate"
        value={
          loading || !summary
            ? "—"
            : summary.payback_days_estimate === null
              ? "—"
              : `${Math.round(summary.payback_days_estimate)} days`
        }
        hint="Blended CAC ÷ avg monthly gross margin per converted lead"
      />
      <CacCard
        icon={<Scale className="h-4 w-4 text-muted-foreground" />}
        label="LTV / CAC"
        value={
          loading || !summary
            ? "—"
            : ltvCacRatio === null
              ? "—"
              : `${ltvCacRatio.toFixed(1)}×`
        }
        hint={
          ltvCacRatio === null
            ? "Shown once the analytics service reports an LTV estimate"
            : `LTV ${formatNaira(ltv ?? 0)} vs blended CAC`
        }
      />
      <CacCard
        icon={<UserCheck className="h-4 w-4 text-muted-foreground" />}
        label="Conversions"
        value={loading || !summary ? "—" : formatCount(totals.conversions)}
        hint={`${formatCount(totals.leads)} leads in period`}
      />
    </div>
  );
}

function CacCard({
  icon,
  label,
  value,
  hint,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  hint: string;
}) {
  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between space-y-0 pb-2">
        <p className="text-sm font-medium text-muted-foreground">{label}</p>
        {icon}
      </CardHeader>
      <CardContent>
        <p className="text-2xl font-bold">{value}</p>
        <p className="text-xs text-muted-foreground">{hint}</p>
      </CardContent>
    </Card>
  );
}
