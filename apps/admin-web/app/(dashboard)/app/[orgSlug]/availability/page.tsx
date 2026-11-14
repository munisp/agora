import { AvailabilityClient } from "./availability-client";

export const metadata = { title: "Availability" };

export default async function AvailabilityPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <AvailabilityClient orgSlug={orgSlug} />;
}
