"use client";

/**
 * Ad stats tiles (SPEC-W21 Agent B): impressions / reach / clicks / spend
 * for one launched ad, with the honest "mock data" disclosure while the
 * deterministic provider mock is the default.
 *
 * Data: GET /api/bookings/v1/social/ads/{id}/stats
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import {
  formatKobo,
  type AdStatsResponse,
} from "@/components/apps/social-publisher/types";

function fmtInt(n: number): string {
  return new Intl.NumberFormat("en-NG").format(n);
}

export function StatsTiles({ stats }: { stats: AdStatsResponse }) {
  const ctr =
    stats.stats.reach > 0
      ? `${((stats.stats.clicks / stats.stats.reach) * 100).toFixed(1)}%`
      : "—";
  return (
    <div>
      <div className="mb-2 flex items-center gap-2">
        <p className="text-sm text-muted-foreground">
          Lifetime stats · {stats.provider} · {stats.provider_ad_id}
        </p>
        {stats.mock ? (
          <Badge variant="outline" title="Deterministic provider mock — no real network calls">
            mock data
          </Badge>
        ) : null}
      </div>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Card>
          <CardContent className="p-4">
            <div className="text-xs text-muted-foreground">Impressions</div>
            <div className="text-xl font-semibold">{fmtInt(stats.stats.impressions)}</div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="p-4">
            <div className="text-xs text-muted-foreground">Reach</div>
            <div className="text-xl font-semibold">{fmtInt(stats.stats.reach)}</div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="p-4">
            <div className="text-xs text-muted-foreground">Clicks (CTR)</div>
            <div className="text-xl font-semibold">
              {fmtInt(stats.stats.clicks)}{" "}
              <span className="text-sm text-muted-foreground">({ctr})</span>
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="p-4">
            <div className="text-xs text-muted-foreground">Spend</div>
            <div className="text-xl font-semibold">
              {formatKobo(stats.stats.spend_kobo)}
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
