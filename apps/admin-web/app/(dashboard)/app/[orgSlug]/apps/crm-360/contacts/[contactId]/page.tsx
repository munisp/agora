import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { unwrap } from "@/components/apps/types";
import type {
  CrmNote,
  Profile360,
  TimelineItem,
} from "@/components/apps/crm-360/types";
import { ContactProfileClient } from "./contact-profile-client";

export const metadata = { title: "Contact 360" };

const ROLLING_OUT =
  "CRM-360 is not available yet — the booking-service CRM API may still be rolling out.";

/**
 * CRM-360 contact profile page (SPEC-W20 Agent A; SPEC-W82 B1.4): the
 * unified 360 view + timeline + notes editor + tag chips for one contact.
 * Same role guard as the search page (owner/admin/staff; viewers/analysts
 * bounced home).
 *
 * SPEC-W82 B1.4: the 360 aggregate, timeline and notes are fetched
 * server-side in parallel (the existing serverApi initialData pattern
 * from apps/crm-360/page.tsx) so the profile paints on first render;
 * the client keeps its background refresh. A 404 (or unreachable API)
 * degrades to the same "still rolling out" copy the client used.
 */
export default async function ContactProfilePage({
  params,
}: {
  params: Promise<{ orgSlug: string; contactId: string }>;
}) {
  const { orgSlug, contactId } = await params;
  const session = await auth();
  const roles = session?.realmRoles;
  if (!hasAnyRole(roles, ["owner", "admin", "staff"])) {
    redirect(`/app/${orgSlug}`);
  }

  const base = `/api/bookings/v1/crm/contacts/${contactId}`;
  const [profileR, timelineR, notesR] = await Promise.allSettled([
    serverApi<{ profile: Profile360 }>(`${base}/360`, {
      query: { tenant: orgSlug },
    }),
    serverApi<unknown>(`${base}/timeline`, {
      query: { tenant: orgSlug, limit: 100 },
    }),
    serverApi<unknown>(`${base}/notes`, { query: { tenant: orgSlug } }),
  ]);

  let initialProfile: Profile360 | null = null;
  let initialError: string | null = null;
  if (profileR.status === "fulfilled") {
    initialProfile = profileR.value.profile ?? null;
  } else {
    const e = profileR.reason;
    initialError =
      e instanceof ApiError && e.status !== 404 ? e.message : ROLLING_OUT;
  }

  // Timeline/notes are additive context — failures degrade to empty, the
  // profile sections still carry the page (same semantics as the client).
  const initialTimeline: TimelineItem[] =
    timelineR.status === "fulfilled" ? unwrap<TimelineItem>(timelineR.value) : [];
  const initialNotes: CrmNote[] =
    notesR.status === "fulfilled" ? unwrap<CrmNote>(notesR.value) : [];

  return (
    <ContactProfileClient
      orgSlug={orgSlug}
      contactId={contactId}
      canWork
      initialProfile={initialProfile}
      initialTimeline={initialTimeline}
      initialNotes={initialNotes}
      initialError={initialError}
    />
  );
}
