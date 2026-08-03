"use client";

/**
 * Top promo codes & campaign spend (SPEC-W13 Agent D, contract §6/§4).
 * Data comes from booking-service list endpoints (GET /v1/promo,
 * GET /v1/campaigns); both are optional reads — when either is unavailable
 * the corresponding table degrades to a muted note instead of an error.
 */
import { Megaphone, TicketPercent } from "lucide-react";
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
import { titleCase } from "@/lib/utils";
import {
  formatCount,
  formatNaira,
  type CampaignRow,
  type PromoCodeRow,
} from "@/components/cac/types";

export function CacPromoTables({
  promos,
  campaigns,
  loading,
  promosUnavailable,
  campaignsUnavailable,
}: {
  promos: PromoCodeRow[];
  campaigns: CampaignRow[];
  loading: boolean;
  promosUnavailable: string | null;
  campaignsUnavailable: string | null;
}) {
  const topPromos = [...promos]
    .sort((a, b) => b.redeemed_count - a.redeemed_count)
    .slice(0, 10);
  const topCampaigns = [...campaigns]
    .sort((a, b) => (b.spend_ngn ?? 0) - (a.spend_ngn ?? 0))
    .slice(0, 10);

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <TicketPercent className="h-4 w-4" /> Top promo codes
          </CardTitle>
          <CardDescription>
            Most-redeemed codes. Redeeming a promo creates or updates a lead
            with first-touch attribution (contract §6).
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Code</TableHead>
                <TableHead>Discount</TableHead>
                <TableHead className="text-right">Redeemed</TableHead>
                <TableHead className="text-right">Cap</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? (
                <TableEmpty colSpan={4}>Loading promo codes…</TableEmpty>
              ) : promosUnavailable ? (
                <TableEmpty colSpan={4}>{promosUnavailable}</TableEmpty>
              ) : topPromos.length === 0 ? (
                <TableEmpty colSpan={4}>
                  No promo codes yet — create one in booking-service to start
                  tracking code-driven acquisition.
                </TableEmpty>
              ) : (
                topPromos.map((p) => (
                  <TableRow key={p.code}>
                    <TableCell className="font-medium">{p.code}</TableCell>
                    <TableCell>
                      {p.discount_ngn ? formatNaira(p.discount_ngn) : "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      {formatCount(p.redeemed_count)}
                    </TableCell>
                    <TableCell className="text-right">
                      {p.max_redemptions
                        ? formatCount(p.max_redemptions)
                        : "∞"}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Megaphone className="h-4 w-4" /> Campaign spend
          </CardTitle>
          <CardDescription>
            Campaigns by recorded spend (contract §4 — spend enters via
            POST /v1/campaigns/&#123;id&#125;/spend).
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Campaign</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead className="text-right">Spend</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? (
                <TableEmpty colSpan={3}>Loading campaigns…</TableEmpty>
              ) : campaignsUnavailable ? (
                <TableEmpty colSpan={3}>{campaignsUnavailable}</TableEmpty>
              ) : topCampaigns.length === 0 ? (
                <TableEmpty colSpan={3}>
                  No campaigns yet — record spend against a campaign to see it
                  here.
                </TableEmpty>
              ) : (
                topCampaigns.map((c) => (
                  <TableRow key={c.id}>
                    <TableCell className="font-medium">
                      {c.name ?? c.id}
                    </TableCell>
                    <TableCell>
                      {c.channel ? titleCase(c.channel) : "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      {c.spend_ngn ? formatNaira(c.spend_ngn) : "—"}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}
