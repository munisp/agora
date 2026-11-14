import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics } from "@/lib/roles";
import { AnalyticsClient } from "./analytics-client";

export const metadata = { title: "Analytics" };

export default async function AnalyticsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W7 Part C): analytics is visible to
  // owner/admin/analyst only; everyone else is bounced to the overview.
  const session = await auth();
  if (!canViewAnalytics(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <AnalyticsClient orgSlug={orgSlug} />;
}
