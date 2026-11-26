import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import type { PublicSite } from "@/lib/types";
import { DemoPhoneClient } from "./demo-phone-client";

export const metadata = { title: "Demo phone" };

/**
 * 2026-11-22: a browser stand-in for a contributor's phone, for showing outbound calls without a carrier. Open it on a
 * phone or second laptop (signed in), switch it on, then use a call button on the dashboard with "Ring the demo
 * phone" ticked: this page rings and the receptionist talks to whoever answers. See components/phone-call.tsx.
 */
export default async function DemoPhonePage({ params }: { params: Promise<{ orgSlug: string }> }) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!session) redirect(`/app/${orgSlug}`);
  let businessName = orgSlug;
  try {
    // The public site record carries the business's display name ("Leadway Pensure PFA").
    const site = await serverApi<PublicSite>(`/api/bookings/public/sites/${encodeURIComponent(orgSlug)}`);
    if (site.business_name) businessName = site.business_name;
  } catch {
    /* the slug will do */
  }
  return <DemoPhoneClient orgSlug={orgSlug} businessName={businessName} />;
}
