import { MembersClient } from "./members-client";

export const metadata = { title: "Members" };

export default async function MembersSettingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <MembersClient orgSlug={orgSlug} />;
}
