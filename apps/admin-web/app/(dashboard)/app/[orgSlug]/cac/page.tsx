import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics } from "@/lib/roles";
import { CacClient } from "./cac-client";

export const metadata = { title: "CAC" };

export default async function CacPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W13 Agent D): CAC dashboards read the same
  // view_analytics-gated rollup API as the analytics page — owner/admin/
  // analyst only; everyone else is bounced to the overview.
  const session = await auth();
  if (!canViewAnalytics(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <CacClient orgSlug={orgSlug} />;
}
