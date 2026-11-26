import { SettingsClient } from "./settings-client";

export const metadata = { title: "Settings" };

export default async function SettingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <SettingsClient orgSlug={orgSlug} />;
}
