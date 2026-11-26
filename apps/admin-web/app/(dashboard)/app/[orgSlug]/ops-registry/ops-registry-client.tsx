"use client";

/**
 * SPEC-W77: platform-admin registry stats (cross-tenant doc-fraud registry).
 *   GET /api/ocr-registry/registry-stats  (BFF direct exception →
 *   ocr-service /v1/internal/ocr/registry-stats; session + platform-admin
 *   gated, the internal credential is injected server-side by the BFF —
 *   this bundle never sees it)
 *   → { enabled, keys_total, memberships_total, multi_tenant_keys,
 *       alerted_memberships, ttl_days, reconcile_minutes }
 *
 * R1 count-not-identity: the payload is aggregate integers + posture fields
 * ONLY. There is deliberately NO per-key data anywhere on this page — no
 * registry keys, no tenant ids, no hashes, no timestamps of individual
 * keys. Per-incident triage stays alert-driven (W67 design).
 */
import * as React from "react";
import {
  RefreshCw,
  Fingerprint,
  Users,
  Network,
  Siren,
  Timer,
  Repeat,
  Power,
} from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";

export interface RegistryStats {
  enabled: boolean;
  keys_total: number;
  memberships_total: number;
  multi_tenant_keys: number;
  alerted_memberships: number;
  ttl_days: number;
  reconcile_minutes: number;
}

function StatCard({
  label,
  value,
  icon: Icon,
  unavailable = false,
}: {
  label: string;
  value: string | number;
  icon: React.ComponentType<{ className?: string }>;
  /** W44/F15-11 posture: explicit error state — never render a misleading 0. */
  unavailable?: boolean;
}) {
  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between space-y-0 pb-2">
        <p className="text-sm font-medium text-muted-foreground">{label}</p>
        <Icon className="h-4 w-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        {unavailable ? (
          <>
            <p className="text-3xl font-bold text-muted-foreground">—</p>
            <p className="mt-1 text-xs text-destructive">Unavailable</p>
          </>
        ) : (
          <p className="text-3xl font-bold">{value}</p>
        )}
      </CardContent>
    </Card>
  );
}

export function OpsRegistryClient({
  initialStats,
  initialError,
}: {
  /** kept for parity with the other ops pages; the stats are platform-scope
   * (no tenant binding, R3), so the slug is only used server-side. */
  orgSlug: string;
  /** SPEC-W77 R5: first fetch done server-side in page.tsx. */
  initialStats: RegistryStats | null;
  initialError: string | null;
}) {
  const [stats, setStats] = React.useState<RegistryStats | null>(initialStats);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(initialError);

  // Keep in sync when the server props change (router.refresh()).
  React.useEffect(() => {
    setStats(initialStats);
    setError(initialError);
  }, [initialStats, initialError]);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<RegistryStats>(
        "/api/ocr-registry/registry-stats",
      );
      setStats(data);
    } catch (e) {
      setError(
        e instanceof ApiError && e.status === 403
          ? "Registry stats require the platform-admin role."
          : e instanceof ApiError
            ? e.message
            : "Failed to load registry stats.",
      );
    } finally {
      setLoading(false);
    }
  }, []);

  const unavailable = stats === null;

  return (
    <div>
      <PageHeader
        title="Registry stats"
        description="Cross-tenant document-fraud registry posture — platform aggregates only."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className="h-4 w-4" />
            {loading ? "Refreshing…" : "Refresh"}
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      {stats !== null && !stats.enabled ? (
        <div className="mb-4 flex items-start gap-2 rounded-md border border-border bg-muted px-3 py-2 text-sm text-muted-foreground">
          <Power className="mt-0.5 h-4 w-4 shrink-0" />
          <span>
            The registry is <strong>disabled</strong> (no HMAC secret
            configured on the ocr-service). Counts are reported as zero — this
            is a posture statement, not an error.
          </span>
        </div>
      ) : null}

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Status"
          value={unavailable ? "—" : stats.enabled ? "Enabled" : "Disabled"}
          icon={Power}
          unavailable={unavailable}
        />
        <StatCard
          label="Fingerprint keys"
          value={stats?.keys_total ?? 0}
          icon={Fingerprint}
          unavailable={unavailable}
        />
        <StatCard
          label="Tenant memberships"
          value={stats?.memberships_total ?? 0}
          icon={Users}
          unavailable={unavailable}
        />
        <StatCard
          label="Multi-tenant keys"
          value={stats?.multi_tenant_keys ?? 0}
          icon={Network}
          unavailable={unavailable}
        />
        <StatCard
          label="Alerted memberships"
          value={stats?.alerted_memberships ?? 0}
          icon={Siren}
          unavailable={unavailable}
        />
        <StatCard
          label="Membership TTL (days)"
          value={stats?.ttl_days ?? 0}
          icon={Timer}
          unavailable={unavailable}
        />
        <StatCard
          label="Reconcile cadence (min)"
          value={stats?.reconcile_minutes ?? 0}
          icon={Repeat}
          unavailable={unavailable}
        />
      </div>

      <Card className="mt-6">
        <CardContent className="pt-5">
          <p className="text-sm text-muted-foreground">
            <Badge variant="secondary">count-not-identity</Badge>{" "}
            This page shows aggregate counts only. It never exposes registry
            keys, tenant identities, hashes, or per-key rows — per-incident
            triage remains alert-driven via the ops alert stream (W67
            zero-disclosure design).
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
