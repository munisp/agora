import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { ContactsClient } from "./contacts-client";
import type { Contact } from "@/lib/types";

export const metadata = { title: "Contacts" };

/**
 * SPEC-W47 UIWEB-15: contacts management. The CRM-360 page searches and
 * annotates contacts but cannot create/edit/delete them; this page owns the
 * full CRUD against /api/bookings/v1/contacts. SSR initialData (W46 AW-2).
 */
export default async function ContactsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!session) redirect(`/app/${orgSlug}`);

  let initialContacts: Contact[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ contacts?: Contact[] }>(
      "/api/bookings/v1/contacts",
      { query: { tenant: orgSlug } },
    );
    initialContacts = data.contacts ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError ? e.message : "Failed to load contacts.";
  }

  return (
    <ContactsClient
      orgSlug={orgSlug}
      initialContacts={initialContacts}
      initialError={initialError}
    />
  );
}
