import { WebhooksClient } from "./webhooks-client";

export const metadata = { title: "Webhooks" };

export default async function WebhooksPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <WebhooksClient orgSlug={orgSlug} />;
}
