import { CallClient } from "./call-client";

export const metadata = { title: "Join call" };

export default async function CallPage({
  params,
  searchParams,
}: {
  params: Promise<{ orgSlug: string }>;
  searchParams: Promise<{ room?: string; token?: string }>;
}) {
  const { orgSlug } = await params;
  const { room, token } = await searchParams;
  return <CallClient orgSlug={orgSlug} room={room ?? ""} token={token ?? ""} />;
}
