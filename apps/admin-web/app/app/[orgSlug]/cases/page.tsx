import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewLocations, hasAnyRole } from "@/lib/roles";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import {
  CIVIC_API,
  normalizeCase,
  normalizeCategory,
  unwrapList,
  type CivicCase,
  type CivicCategory,
} from "@/components/cases/types";
import { CasesClient } from "./cases-client";

export const metadata = { title: "Cases" };

/**
 * SPEC-W46 AW-2: the unfiltered triage queue + category list are fetched
 * server-side in parallel and handed to the client as initialData — the
 * console paints with data instead of a Loading shell. Filter changes and
 * mutation reloads stay client-side.
 */
export default async function CasesPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W32 WS-C): civic cases are operational —
  // same gate as locations (owner/admin/staff); viewers and analysts are
  // bounced to the overview.
  const session = await auth();
  if (!canViewLocations(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }

  // Same degradation semantics the client applied: a 404 means the civic
  // module is not deployed on this workspace (clean empty state), other
  // failures render an error note, a categories failure is silent.
  const [casesRes, categoriesRes] = await Promise.allSettled([
    serverApi<unknown>(`${CIVIC_API}/cases`, { query: { tenant: orgSlug } }),
    serverApi<unknown>(`${CIVIC_API}/categories`, {
      query: { tenant: orgSlug },
    }),
  ]);

  let initialCases: CivicCase[] = [];
  let initialUnavailable = false;
  let initialError: string | null = null;
  if (casesRes.status === "fulfilled") {
    initialCases = unwrapList<unknown>(casesRes.value).map(normalizeCase);
  } else if (
    casesRes.reason instanceof ApiError &&
    casesRes.reason.status === 404
  ) {
    initialUnavailable = true;
  } else {
    initialError = "Cases unavailable — the booking service may be offline.";
  }
  const initialCategories: CivicCategory[] =
    categoriesRes.status === "fulfilled"
      ? unwrapList<unknown>(categoriesRes.value).map(normalizeCategory)
      : [];

  // Anonymous reporter unmasking is owner/admin only (SPEC §2/§4 gate 4);
  // staff operate the queue but never see a masked reporter's identity.
  return (
    <CasesClient
      orgSlug={orgSlug}
      canRevealReporter={hasAnyRole(session?.realmRoles, ["owner", "admin"])}
      initialCases={initialCases}
      initialCategories={initialCategories}
      initialUnavailable={initialUnavailable}
      initialError={initialError}
    />
  );
}
