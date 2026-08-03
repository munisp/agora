"use client";

/**
 * CAC by-LGA section (SPEC-W13 Agent D): MapLibre choropleth over the
 * contract §5 `by_lga` rows plus a table fallback. Rows whose `geom` is
 * null (analytics-service could not resolve the LGA boundary) are listed
 * below the map — nothing is silently dropped.
 */
import * as React from "react";
import dynamic from "next/dynamic";
import { MapPinOff } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";
import {
  formatCount,
  formatNaira,
  lgaToFeature,
  type CacLgaRow,
} from "@/components/cac/types";
import type { LgaMetric } from "@/components/cac/cac-lga-map";

// WebGL must never run during SSR — the map is client-only.
const CacLgaMap = dynamic(() => import("@/components/cac/cac-lga-map"), {
  ssr: false,
  loading: () => (
    <div className="flex h-[480px] w-full items-center justify-center rounded-md border border-border bg-muted text-sm text-muted-foreground">
      Loading map…
    </div>
  ),
});

const METRICS: { key: LgaMetric; label: string }[] = [
  { key: "leads", label: "Leads" },
  { key: "conversions", label: "Conversions" },
  { key: "cac_ngn", label: "CAC" },
];

export function CacLgaSection({
  rows,
  loading,
  days,
}: {
  rows: CacLgaRow[];
  loading: boolean;
  days: number;
}) {
  const [metric, setMetric] = React.useState<LgaMetric>("leads");
  const [selectedLgaId, setSelectedLgaId] = React.useState<number | null>(null);

  const mappable = React.useMemo(
    () => rows.filter((r) => lgaToFeature(r, 0) !== null),
    [rows],
  );
  const unmappable = React.useMemo(
    () => rows.filter((r) => lgaToFeature(r, 0) === null),
    [rows],
  );
  const sorted = React.useMemo(
    () =>
      [...rows].sort((a, b) =>
        metric === "cac_ngn" ? b.cac_ngn - a.cac_ngn : b[metric] - a[metric],
      ),
    [rows, metric],
  );
  const selected = rows.find((r) => r.lga_id === selectedLgaId) ?? null;

  const maxMetric = Math.max(
    0,
    ...mappable.map((r) => (metric === "cac_ngn" ? r.cac_ngn : r[metric])),
  );

  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between space-y-0">
        <div>
          <CardTitle>CAC by LGA</CardTitle>
          <CardDescription>
            Acquisition funnel per local government area, last {days} days.
            Darker areas have a higher {METRICS.find((m) => m.key === metric)?.label.toLowerCase()}.
          </CardDescription>
        </div>
        <div className="flex gap-1 rounded-md border border-border p-0.5">
          {METRICS.map((m) => (
            <button
              key={m.key}
              type="button"
              onClick={() => setMetric(m.key)}
              className={cn(
                "rounded px-2.5 py-1 text-xs font-medium cursor-pointer",
                metric === m.key
                  ? "bg-secondary text-secondary-foreground"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {m.label}
            </button>
          ))}
        </div>
      </CardHeader>
      <CardContent>
        {loading ? (
          <div className="flex h-[480px] w-full items-center justify-center rounded-md border border-border bg-muted text-sm text-muted-foreground">
            Loading LGA metrics…
          </div>
        ) : rows.length === 0 ? (
          <div className="flex items-start gap-3 rounded-md border border-dashed border-border bg-accent px-4 py-3">
            <MapPinOff className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
            <div>
              <p className="text-sm font-medium">No LGA breakdown yet</p>
              <p className="mt-0.5 text-xs text-muted-foreground">
                Leads need an LGA assigned (from a shared location, address or
                QR campaign) before they appear on this map. Try widening the
                date range if you expected data.
              </p>
            </div>
          </div>
        ) : (
          <>
            {mappable.length > 0 ? (
              <>
                <CacLgaMap
                  rows={rows}
                  metric={metric}
                  selectedLgaId={selectedLgaId}
                  onSelectLga={setSelectedLgaId}
                />
                <div className="mt-2 flex items-center justify-between text-[10px] text-muted-foreground">
                  <span>0</span>
                  <span
                    className="mx-2 h-2 flex-1 rounded-full"
                    style={{
                      background:
                        "linear-gradient(to right, #efe4d3, #8a6d4b)",
                    }}
                  />
                  <span>
                    {metric === "cac_ngn"
                      ? formatNaira(maxMetric)
                      : formatCount(maxMetric)}
                  </span>
                </div>
              </>
            ) : (
              <p className="mb-3 rounded-md border border-border bg-accent px-3 py-2 text-xs text-muted-foreground">
                The analytics service did not return LGA boundary geometry for
                this period, so the choropleth cannot render — the full
                breakdown is in the table below.
              </p>
            )}

            {selected ? (
              <div className="mt-3 flex flex-wrap items-center gap-x-6 gap-y-1 rounded-md border border-border bg-accent px-3 py-2 text-sm">
                <span className="font-medium">
                  {selected.lga_name ?? `LGA ${selected.lga_id}`}
                </span>
                <span className="text-muted-foreground">
                  {formatCount(selected.leads)} leads
                </span>
                <span className="text-muted-foreground">
                  {formatCount(selected.conversions)} conversions
                </span>
                <span className="text-muted-foreground">
                  CAC {formatNaira(selected.cac_ngn)}
                </span>
                <button
                  type="button"
                  onClick={() => setSelectedLgaId(null)}
                  className="ml-auto text-xs font-medium text-muted-foreground hover:text-foreground cursor-pointer"
                >
                  Clear selection
                </button>
              </div>
            ) : null}

            <div className="mt-4">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>LGA</TableHead>
                    <TableHead className="text-right">Leads</TableHead>
                    <TableHead className="text-right">Conversions</TableHead>
                    <TableHead className="text-right">CAC</TableHead>
                    {unmappable.length > 0 ? (
                      <TableHead className="text-right">On map</TableHead>
                    ) : null}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {sorted.length === 0 ? (
                    <TableEmpty colSpan={unmappable.length > 0 ? 5 : 4}>
                      No LGA rows in this period.
                    </TableEmpty>
                  ) : (
                    sorted.map((row) => (
                      <TableRow
                        key={row.lga_id}
                        className={cn(
                          "cursor-pointer",
                          selectedLgaId === row.lga_id && "bg-muted/80",
                        )}
                        onClick={() =>
                          setSelectedLgaId((cur) =>
                            cur === row.lga_id ? null : row.lga_id,
                          )
                        }
                      >
                        <TableCell className="font-medium">
                          {row.lga_name ?? `LGA ${row.lga_id}`}
                        </TableCell>
                        <TableCell className="text-right">
                          {formatCount(row.leads)}
                        </TableCell>
                        <TableCell className="text-right">
                          {formatCount(row.conversions)}
                        </TableCell>
                        <TableCell className="text-right">
                          {formatNaira(row.cac_ngn)}
                        </TableCell>
                        {unmappable.length > 0 ? (
                          <TableCell className="text-right text-xs text-muted-foreground">
                            {lgaToFeature(row, 0) ? "Yes" : "No geometry"}
                          </TableCell>
                        ) : null}
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
