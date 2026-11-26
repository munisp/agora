import { DocumentUploadClient } from "./upload-client";
import { CHAT_DOC_KINDS, type ChatDocKind } from "@/lib/chat-upload";

export const metadata = { title: "Share a document", robots: { index: false, follow: false } };

/** 2026-11-26: the page behind the secure upload link the receptionist WhatsApps to a caller during a call. The
 * signed token in the path is the only credential; the runtime checks it on upload. */
export default async function DocumentUploadPage({
  params,
  searchParams,
}: {
  params: Promise<{ token: string }>;
  searchParams: Promise<{ kind?: string }>;
}) {
  const { token } = await params;
  const { kind } = await searchParams;
  const initialKind: ChatDocKind = (CHAT_DOC_KINDS as readonly string[]).includes(kind ?? "")
    ? (kind as ChatDocKind)
    : "document";
  return (
    <main className="flex min-h-screen items-start justify-center bg-muted/30 px-4 py-10">
      <DocumentUploadClient token={token} initialKind={initialKind} />
    </main>
  );
}
