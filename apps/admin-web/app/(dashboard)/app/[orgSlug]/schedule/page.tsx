import { auth } from "@/lib/auth";
import { ScheduleClient } from "./schedule-client";

export const metadata = { title: "My schedule" };

export default async function SchedulePage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  return (
    <ScheduleClient
      orgSlug={orgSlug}
      token={session?.accessToken}
      email={session?.user?.email ?? undefined}
    />
  );
}
