import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { ReconciliationClient } from "./reconciliation-client";
import type { ReconFinding } from "./reconciliation-client";

export const metadata = { title: "Receipt reconciliation" };

/**
 * SPEC-W56 Scope C: receipt ↔ payment reconciliation read-back — the
 * first-ever read path for receipt data (RECON §5 R4). The first page of
 * findings is fetched server-side (same serverApi pattern as the ops-alerts
 * page, SPEC-W46 AW-2) and handed to the client as initialData.
 *
 * Endpoint (booking-service, manage_bookings):
 *   GET /api/bookings/v1/recon/receipts?status=open|resolved|all&limit=
 * proxied by the EXISTING /api/bookings/* APISIX OIDC wildcard
 * (infra/apisix/apisix.yaml:1008-1011) — NO new gateway route (SPEC-W56
 * NO-GO list). The endpoint is gated by the manage_bookings permission
 * rather than a realm role, so there is no client-side role redirect here —
 * a 403 surfaces as an explanatory error note instead.
 */
export default async function ReconciliationPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;

  let initialFindings: ReconFinding[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ findings?: ReconFinding[] }>(
      "/api/bookings/v1/recon/receipts",
      // Keep in sync with DEFAULT_STATUS / PAGE_SIZE in
      // reconciliation-client.tsx.
      { query: { tenant: orgSlug, status: "open", limit: 100 } },
    );
    initialFindings = data.findings ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError && e.status === 403
        ? "Receipt reconciliation requires the manage_bookings permission."
        : e instanceof ApiError
          ? e.message
          : "Failed to load reconciliation findings.";
  }

  return (
    <ReconciliationClient
      orgSlug={orgSlug}
      initialFindings={initialFindings}
      initialError={initialError}
    />
  );
}
