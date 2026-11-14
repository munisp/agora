import { PublicSiteClient } from "./public-site-client";

export const metadata = { title: "Public Site" };

export default async function PublicSitePage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <PublicSiteClient orgSlug={orgSlug} />;
}
