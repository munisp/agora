import { OfferingsClient } from "./offerings-client";

export const metadata = { title: "Offerings" };

export default async function OfferingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <OfferingsClient orgSlug={orgSlug} />;
}
