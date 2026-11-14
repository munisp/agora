import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { PlanClient } from "./plan-client";
import type { Tenant } from "@/lib/types";

export const metadata = { title: "Plan" };

/** Decode the payload of a JWT without verifying (claims come from Keycloak
 * via the Auth.js session, mirroring lib/auth.ts decodeClaims). */
function decodeSub(token: string): string | null {
  try {
    const payload = token.split(".")[1];
    const claims = JSON.parse(Buffer.from(payload, "base64url").toString("utf8"));
    return typeof claims.sub === "string" ? claims.sub : null;
  } catch {
    return null;
  }
}

/**
 * SPEC-W45 K18 plan page. The PATCH /v1/tenants/{slug}/plan endpoint is
 * owner/platform-admin only; the change control is hidden for everyone
 * else (determined from the caller's JWT sub against the member list).
 */
export default async function PlanSettingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!session) redirect("/sign-in");
  const currentUserId = session.accessToken ? decodeSub(session.accessToken) : null;
  const isPlatformAdmin = (session.realmRoles ?? []).includes("platform-admin");

  // SPEC-W46 AW-2: tenant + membership load server-side in parallel instead
  // of two serial post-hydration BFF round trips. Owner determination keeps
  // its fail-safe semantics: a failed/missing member read leaves the change
  // control hidden (the server-side 403 remains the enforcement of record).
  const [tenantRes, membersRes] = await Promise.allSettled([
    serverApi<Tenant>(`/api/identity/v1/tenants/${orgSlug}`),
    currentUserId
      ? serverApi<{ members?: { user_id: string; role: string }[] }>(
          `/api/identity/v1/tenants/${orgSlug}/members`,
        )
      : Promise.resolve(null),
  ]);

  const initialTenant = tenantRes.status === "fulfilled" ? tenantRes.value : null;
  const initialError =
    tenantRes.status === "rejected"
      ? tenantRes.reason instanceof ApiError
        ? tenantRes.reason.message
        : "Failed to load plan."
      : null;
  const initialIsOwner =
    membersRes.status === "fulfilled" && membersRes.value !== null
      ? (membersRes.value.members ?? []).some(
          (m) => m.user_id === currentUserId && m.role === "owner",
        )
      : false;

  return (
    <PlanClient
      orgSlug={orgSlug}
      currentUserId={currentUserId}
      isPlatformAdmin={isPlatformAdmin}
      initialTenant={initialTenant}
      initialIsOwner={initialIsOwner}
      initialError={initialError}
    />
  );
}
