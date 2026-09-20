import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { PlanClient } from "./plan-client";

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
  return (
    <PlanClient
      orgSlug={orgSlug}
      currentUserId={currentUserId}
      isPlatformAdmin={isPlatformAdmin}
    />
  );
}
