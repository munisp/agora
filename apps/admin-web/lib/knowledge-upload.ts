/**
 * SPEC-W114: pure upload-policy module for the knowledge-base docling
 * upload dialog. Zero React/DOM deps — compiles standalone with tsc and is
 * unit-tested with node --test (lib/knowledge-upload.test.mjs).
 *
 * Every constant mirrors the knowledge-service contract EXACTLY; drift
 * here means the UI pre-flight lies to the operator about a limit the
 * service will enforce anyway.
 */

/** Exact mirror of knowledge-service ALLOWED_UPLOAD_EXTENSIONS (W107). */
export const UPLOAD_EXTENSIONS: readonly string[] = [
  "pdf",
  "docx",
  "pptx",
  "xlsx",
  "html",
  "htm",
  "md",
  "txt",
  "asciidoc",
  "adoc",
  "png",
  "jpg",
  "jpeg",
  "tiff",
  "tif",
];

/** Mirror of doc_intel_max_bytes (decoded bytes) — the service 413s above. */
export const UPLOAD_MAX_BYTES = 10 * 1024 * 1024;

/**
 * Above this, submit with wait=false (W111 async upload job) instead of
 * blocking the dialog on a multi-minute docling conversion.
 */
export const ASYNC_THRESHOLD_BYTES = 3 * 1024 * 1024;

export type UploadValidation = { ok: true } | { ok: false; error: string };

/** Pre-flight validation so service 413/415s are explained, not hit. */
export function validateUploadFile(name: string, sizeBytes: number): UploadValidation {
  const ext = fileExtension(name);
  if (!ext || !UPLOAD_EXTENSIONS.includes(ext)) {
    return {
      ok: false,
      error: ext
        ? `Unsupported file type ".${ext}". Allowed: ${UPLOAD_EXTENSIONS.join(", ")}.`
        : `The file has no extension. Allowed: ${UPLOAD_EXTENSIONS.join(", ")}.`,
    };
  }
  if (sizeBytes > UPLOAD_MAX_BYTES) {
    return {
      ok: false,
      error:
        `File is ${(sizeBytes / (1024 * 1024)).toFixed(1)} MB; the limit is ` +
        `${UPLOAD_MAX_BYTES / (1024 * 1024)} MB. Compress or split it and retry.`,
    };
  }
  return { ok: true };
}

export function fileExtension(name: string): string {
  const idx = name.lastIndexOf(".");
  return idx >= 0 ? name.slice(idx + 1).toLowerCase() : "";
}

export type UploadMode = "sync" | "async";

/** Small files block the dialog (fast); large scans go to a W111 job. */
export function chooseUploadMode(sizeBytes: number): UploadMode {
  return sizeBytes > ASYNC_THRESHOLD_BYTES ? "async" : "sync";
}

export type JobUiState = "processing" | "done" | "failed";

/**
 * UI state machine over the W111 job row (pending|running|done|failed).
 * Unknown values map to "processing" — a forward-compatible service status
 * must never crash the dialog.
 */
export function reduceJobStatus(status: string): JobUiState {
  if (status === "done") return "done";
  if (status === "failed") return "failed";
  return "processing"; // pending | running | unknown
}

/** Poll backoff: 1s, 1.5s, 2s, then +1s per attempt, capped at 10s. */
export function pollDelayMs(attempt: number): number {
  if (attempt <= 0) return 1000;
  if (attempt === 1) return 1500;
  return Math.min(1000 * attempt, 10_000);
}
