import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { IncidentsClient } from "./incidents-client";
import type { Incident, IncidentDispatchEndpoint } from "@/lib/types";

export const metadata = { title: "Incidents" };

/**
 * SPEC-W47 UIWEB-13: IoT/civic incident ops surface. Machine ingest lives on
 * /v1/incidents/ingest (internal); this page consumes the admin read/action
 * routes. SSR initialData pattern (W46 AW-2): the incident list and the
 * dispatch endpoints load server-side.
 *
 * Incident lifecycle is delivery-driven (new → dispatched → acknowledged →
 * closed via the signed endpoint callbacks); the staff action is DISPATCH —
 * there is no manual "resolve" route, so none is offered.
 */
export default async function IncidentsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!session) redirect(`/app/${orgSlug}`);

  let initialIncidents: Incident[] = [];
  let initialEndpoints: IncidentDispatchEndpoint[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ incidents?: Incident[] }>(
      "/api/bookings/v1/incidents",
      { query: { tenant: orgSlug } },
    );
    initialIncidents = data.incidents ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError ? e.message : "Failed to load incidents.";
  }
  try {
    const data = await serverApi<{ endpoints?: IncidentDispatchEndpoint[] }>(
      "/api/bookings/v1/incidents/dispatch-endpoints",
      { query: { tenant: orgSlug } },
    );
    initialEndpoints = data.endpoints ?? [];
  } catch {
    initialEndpoints = [];
  }

  return (
    <IncidentsClient
      orgSlug={orgSlug}
      initialIncidents={initialIncidents}
      initialEndpoints={initialEndpoints}
      initialError={initialError}
    />
  );
}
