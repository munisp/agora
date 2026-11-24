/**
 * SPEC-W115 C7: pure helpers for chat-widget caller document uploads.
 * Mirrors the lib/knowledge-upload.ts (W114) idiom: no DOM, no fetch —
 * unit-tested via tsc -> node --test (scripts/test-chat-upload.sh).
 */

export const CHAT_UPLOAD_EXTENSIONS = [
  "jpg",
  "jpeg",
  "png",
  "webp",
  "pdf",
] as const;

/** Keep in lockstep with voice-agent-runtime VOICE_MAX_DOCUMENT_BYTES. */
export const CHAT_UPLOAD_MAX_BYTES = 10 * 1024 * 1024;

/** Caller-shareable kinds (voice runtime CALLER_DOCUMENT_KINDS mirror,
 * SPEC-W116: all non-KYC kinds). */
export const CHAT_DOC_KINDS = ["pay_stub", "receipt", "document"] as const;
export type ChatDocKind = (typeof CHAT_DOC_KINDS)[number];

export type Verdict = "validated" | "needs_review" | "rejected";

export interface FileLike {
  name: string;
  size: number;
}

export function fileExtension(name: string): string {
  const idx = name.lastIndexOf(".");
  if (idx < 0) return "";
  return name.slice(idx + 1).toLowerCase();
}

export function validateChatUpload(
  file: FileLike,
): { ok: true } | { ok: false; error: string } {
  const ext = fileExtension(file.name);
  if (!(CHAT_UPLOAD_EXTENSIONS as readonly string[]).includes(ext)) {
    return {
      ok: false,
      error: `Unsupported file type ".${ext || "?"}" — please share a photo (jpg, png, webp) or a PDF.`,
    };
  }
  if (file.size <= 0) {
    return { ok: false, error: "That file is empty." };
  }
  if (file.size > CHAT_UPLOAD_MAX_BYTES) {
    return { ok: false, error: "That file is too large (10 MB maximum)." };
  }
  return { ok: true };
}

export function verdictChip(verdict: string): {
  label: string;
  tone: "success" | "warning" | "danger" | "neutral";
} {
  switch (verdict) {
    case "validated":
      return { label: "Checks passed", tone: "success" };
    case "needs_review":
      return { label: "Under review", tone: "warning" };
    case "rejected":
      return { label: "Not accepted", tone: "danger" };
    default:
      return { label: "Received", tone: "neutral" };
  }
}

const REASON_PHRASES: Record<string, string> = {
  net_exceeds_gross: "the net pay is higher than the gross pay",
  period_inverted: "the pay period dates are reversed",
  arithmetic_inconsistent: "the pay figures do not add up",
  period_unparseable: "the pay period dates could not be read",
  currency_missing: "the currency is missing",
  no_validator_for_kind: "this document type has no automatic checks",
};

function humanReason(reason: string): string {
  if (reason.startsWith("missing_required_field:")) {
    const field = reason.slice("missing_required_field:".length).replace(/_/g, " ");
    return `the ${field} is missing`;
  }
  return REASON_PHRASES[reason] ?? reason.replace(/_/g, " ");
}

/** One display sentence — mirror of voice-runtime verdict_summary (honest
 * heuristic language: never "authentic"/"genuine"). */
export function verdictSummary(
  kind: string,
  verdict: string,
  reasons: string[],
): string {
  const label =
    kind === "pay_stub" ? "pay stub" : kind === "receipt" ? "receipt" : "document";
  if (verdict === "validated") {
    return `Your ${label} was received and passed all consistency checks.`;
  }
  if (verdict === "rejected") {
    const detail = reasons.length > 0 ? `: ${humanReason(reasons[0])}` : "";
    return `We could not accept this ${label}${detail}.`;
  }
  return `Your ${label} was received; a team member will review it shortly.`;
}
