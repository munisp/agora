"use client";

import * as React from "react";
import { FileUp, Loader2 } from "lucide-react";
import {
  CHAT_UPLOAD_EXTENSIONS,
  validateChatUpload,
  verdictChip,
  verdictSummary,
  type ChatDocKind,
} from "@/lib/chat-upload";

type Phase =
  | { step: "pick" }
  | { step: "sending" }
  | { step: "checking" }
  | { step: "done"; text: string; chip: ReturnType<typeof verdictChip> }
  | { step: "error"; text: string };

const CHIP_TONE: Record<string, string> = {
  success: "bg-emerald-100 text-emerald-800",
  warning: "bg-amber-100 text-amber-800",
  danger: "bg-red-100 text-red-800",
  neutral: "bg-muted text-muted-foreground",
};

interface JobResult {
  status?: string;
  job_id?: string;
  kind?: string;
  validation_verdict?: string;
  validation_reasons?: string[];
  summary?: string;
}

/** Reading takes about two minutes: poll the accepted upload's job (up to ~6 minutes). */
async function waitForJob(jobId: string): Promise<JobResult | null> {
  for (let i = 0; i < 72; i += 1) {
    await new Promise((r) => setTimeout(r, 5000));
    try {
      const res = await fetch(`/voice/documents/jobs/${encodeURIComponent(jobId)}`, { cache: "no-store" });
      if (res.status === 404) return null;
      if (!res.ok) continue;
      const job = (await res.json()) as JobResult;
      if (job.status && job.status !== "processing") return job;
    } catch {
      /* a dropped poll: try again */
    }
  }
  return null;
}

export function DocumentUploadClient({ token, initialKind }: { token: string; initialKind: ChatDocKind }) {
  const [file, setFile] = React.useState<File | null>(null);
  const [kind, setKind] = React.useState<ChatDocKind>(initialKind);
  const [phase, setPhase] = React.useState<Phase>({ step: "pick" });
  const [pickError, setPickError] = React.useState("");

  function onPick(e: React.ChangeEvent<HTMLInputElement>) {
    const f = e.target.files?.[0] ?? null;
    setPickError("");
    if (f) {
      const check = validateChatUpload(f);
      if (!check.ok) {
        setPickError(check.error);
        setFile(null);
        return;
      }
    }
    setFile(f);
  }

  async function upload() {
    if (!file) return;
    setPhase({ step: "sending" });
    const form = new FormData();
    form.append("file", file);
    form.append("kind", kind);
    form.append("respond", "async");
    try {
      const res = await fetch(`/voice/documents/claim/${encodeURIComponent(token)}`, {
        method: "POST",
        body: form,
        cache: "no-store",
      });
      if (res.status === 401) {
        setPhase({ step: "error", text: "This link has expired or is not valid. Please ask for a new one." });
        return;
      }
      if (!res.ok) {
        setPhase({ step: "error", text: "We couldn't receive that file just now. Please try again in a moment." });
        return;
      }
      const accepted = (await res.json()) as JobResult;
      if (accepted.status !== "processing" || !accepted.job_id) {
        const verdict = accepted.validation_verdict ?? "needs_review";
        setPhase({
          step: "done",
          text: accepted.summary ?? verdictSummary(kind, verdict, accepted.validation_reasons ?? []),
          chip: verdictChip(verdict),
        });
        return;
      }
      setPhase({ step: "checking" });
      const job = await waitForJob(accepted.job_id);
      if (job && job.status === "done") {
        const verdict = job.validation_verdict ?? "needs_review";
        setPhase({
          step: "done",
          text: job.summary ?? verdictSummary(kind, verdict, job.validation_reasons ?? []),
          chip: verdictChip(verdict),
        });
      } else {
        setPhase({
          step: "done",
          text: "We received your document. It is still being checked; the team will see it.",
          chip: verdictChip("received"),
        });
      }
    } catch {
      setPhase({ step: "error", text: "The upload didn't go through. Check your connection and try again." });
    }
  }

  return (
    <div className="w-full max-w-md rounded-xl border border-border bg-card p-6 shadow-sm">
      <h1 className="text-lg font-semibold">Share a document</h1>
      <p className="mt-1 text-sm text-muted-foreground">
        Upload the document you discussed on your call. It goes only to the business you spoke with. You can go back to
        your call while it is checked.
      </p>

      {(phase.step === "pick" || phase.step === "error") && (
        <div className="mt-5 space-y-4">
          <label className="block text-sm font-medium">
            Document type
            <select
              value={kind}
              onChange={(e) => setKind(e.target.value as ChatDocKind)}
              className="mt-1 block h-10 w-full rounded-md border border-input bg-card px-2 text-sm"
            >
              <option value="pay_stub">Pay stub</option>
              <option value="receipt">Receipt</option>
              <option value="document">Other document</option>
            </select>
          </label>
          <label className="block text-sm font-medium">
            File (photo or PDF, up to 10 MB)
            <input
              type="file"
              accept={CHAT_UPLOAD_EXTENSIONS.map((e) => `.${e}`).join(",")}
              onChange={onPick}
              className="mt-1 block w-full text-sm file:mr-3 file:rounded-md file:border-0 file:bg-muted file:px-3 file:py-2"
            />
          </label>
          {pickError && <p className="text-sm text-red-700">{pickError}</p>}
          {phase.step === "error" && <p className="text-sm text-red-700">{phase.text}</p>}
          <button
            type="button"
            onClick={upload}
            disabled={!file}
            className="inline-flex w-full items-center justify-center gap-2 rounded-md bg-primary px-4 py-2.5 text-sm font-medium text-primary-foreground disabled:opacity-50"
          >
            <FileUp className="h-4 w-4" /> Upload
          </button>
        </div>
      )}

      {(phase.step === "sending" || phase.step === "checking") && (
        <div className="mt-6 flex items-start gap-3 text-sm">
          <Loader2 className="mt-0.5 h-4 w-4 shrink-0 animate-spin" />
          <p>
            {phase.step === "sending"
              ? "Sending your document…"
              : "Received. We're checking it now — this takes about two minutes. You can keep talking on your call."}
          </p>
        </div>
      )}

      {phase.step === "done" && (
        <div className="mt-6 space-y-2 text-sm">
          <span className={`inline-block rounded-full px-2 py-0.5 text-xs font-medium ${CHIP_TONE[phase.chip.tone]}`}>
            {phase.chip.label}
          </span>
          <p>{phase.text}</p>
          <p className="text-muted-foreground">You can close this page.</p>
        </div>
      )}
    </div>
  );
}
