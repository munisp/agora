"use client";

/**
 * SPEC-W58 Scope D: KYC document review queue (owner/admin only — the page
 * redirects other roles server-side; the booking-service KYC routes enforce
 * the same gate, RECON §5 R4/P5).
 *
 * API surface (booking-service staff proxy → kyc-service, riding the
 * EXISTING /api/bookings/* APISIX OIDC wildcard — no new gateway route):
 *
 *   GET  /api/bookings/v1/kyc/documents/{id}
 *     → {document_id, reference, status, review_state, fields?, fields_available}
 *   POST /api/bookings/v1/kyc/documents/{id}/review
 *     {decision: "approved"|"rejected", reason: string}
 *
 * Honesty notes (SPEC-W58 cross-scope contract + pinned decisions):
 *  - NO list endpoint exists in the W58 contract (kyc-service exposes
 *    GET-by-id only, P13: the review queue polls). This page therefore
 *    works as a lookup-by-id: paste a document_id or reference (both are
 *    returned by POST /v1/kyc/documents at upload). Documents opened this
 *    session are kept in a local list below — every entry is a real API
 *    response, nothing is invented. A list endpoint is a follow-up gap.
 *  - Extracted fields are PRE-FILL for human review only — OCR never
 *    auto-verifies (P7). The durable kyc-service record is hash-only (P8);
 *    raw fields live in the ocr DB under a 72h TTL (P4). After expiry the
 *    API returns fields_available:false and this page renders
 *    "expired — hashes only" instead of field values.
 *  - Document BYTES are never served by any API — reviewers compare the
 *    extracted fields against the physical/original document.
 *  - mrz_valid (kyc_passport) is an ICAO 9303 check-digit result computed
 *    in deterministic code by ocr-service — it is NOT a model claim and
 *    NOT an authenticity verdict.
 */
import * as React from "react";
import { Check, RefreshCw, Search, ShieldCheck, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Textarea } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";

/** Shared response envelope (SPEC-W58 cross-scope contract). */
export interface KycDocumentDetail {
  document_id: string;
  reference?: string | null;
  status?: string | null;
  review_state?: string | null;
  /** Extracted fields — present only while inside the OCR TTL window. */
  fields?: Record<string, unknown> | null;
  /** false after the 72h TTL sweep — only HMAC hashes remain (P4/P8). */
  fields_available: boolean;
  /** Tolerated extras (doc_type, subject_phone, timestamps, ...). */
  doc_type?: string | null;
  created_at?: string | null;
  reviewed_at?: string | null;
  reviewer_actor?: string | null;
}

/** Display labels for the P9 per-kind field whitelists. */
const FIELD_LABELS: Record<string, string> = {
  full_name: "Full name",
  dob: "Date of birth",
  nin: "NIN",
  address: "Address",
  license_no: "License no.",
  expiry: "Expiry",
  passport_no: "Passport no.",
  nationality: "Nationality",
  vin: "VIN",
  company_name: "Company name",
  rc_number: "RC number",
  registration_date: "Registration date",
  mrz_valid: "MRZ check digits",
};

const DOC_TYPE_LABELS: Record<string, string> = {
  kyc_nin_slip: "NIN slip",
  kyc_drivers_license: "Driver's license",
  kyc_passport: "International passport",
  kyc_voters_card: "Voter's card",
  kyc_cac_document: "CAC document",
};

function fieldLabel(key: string): string {
  return FIELD_LABELS[key] ?? key.replace(/_/g, " ");
}

function reviewVariant(
  state: string | null | undefined,
): "success" | "destructive" | "warning" | "secondary" {
  const v = (state ?? "").toLowerCase();
  if (v === "approved") return "success";
  if (v === "rejected") return "destructive";
  if (v === "pending_review") return "warning";
  return "secondary";
}

function statusVariant(
  status: string | null | undefined,
): "success" | "destructive" | "secondary" {
  const v = (status ?? "").toLowerCase();
  if (v === "extracted") return "success";
  if (v === "failed" || v === "erased") return "destructive";
  return "secondary";
}

function fieldRows(
  fields: Record<string, unknown> | null | undefined,
): [string, string][] {
  if (!fields || typeof fields !== "object") return [];
  const rows: [string, string][] = [];
  for (const [k, v] of Object.entries(fields)) {
    if (v === null || v === undefined) {
      rows.push([k, "—"]); // tolerant parse: null = not legible on the document
    } else if (typeof v === "boolean") {
      rows.push([k, v ? "valid" : "invalid"]);
    } else if (typeof v === "string" || typeof v === "number") {
      const s = String(v).trim();
      rows.push([k, s === "" ? "—" : s]);
    } else {
      rows.push([k, JSON.stringify(v)]);
    }
  }
  return rows;
}

export function KycReviewClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [lookup, setLookup] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [selected, setSelected] = React.useState<KycDocumentDetail | null>(null);
  /**
   * Session-local list of documents actually opened via the API this
   * session (keyed by document_id). This is NOT a server-side listing —
   * no list endpoint exists in the W58 contract (see header comment).
   */
  const [opened, setOpened] = React.useState<KycDocumentDetail[]>([]);
  const [reviewing, setReviewing] = React.useState<{
    decision: "approved" | "rejected";
  } | null>(null);
  const [busy, setBusy] = React.useState(false);

  const remember = React.useCallback((doc: KycDocumentDetail) => {
    setOpened((prev) => {
      const rest = prev.filter((d) => d.document_id !== doc.document_id);
      return [doc, ...rest];
    });
  }, []);

  const fetchDocument = React.useCallback(
    async (id: string) => {
      const data = await api.get<KycDocumentDetail>(
        `/api/bookings/v1/kyc/documents/${encodeURIComponent(id)}`,
        { tenant: orgSlug },
      );
      return data;
    },
    [orgSlug],
  );

  const search = async (e: React.FormEvent) => {
    e.preventDefault();
    const id = lookup.trim();
    if (!id || loading) return;
    setLoading(true);
    setError(null);
    try {
      const doc = await fetchDocument(id);
      setSelected(doc);
      remember(doc);
    } catch (err) {
      setSelected(null);
      setError(
        err instanceof ApiError && err.status === 403
          ? "KYC document review requires the owner or admin role."
          : err instanceof ApiError && err.status === 404
            ? "No KYC document with that id/reference exists for this organisation."
            : err instanceof ApiError
              ? err.message
              : "Failed to load the KYC document.",
      );
    } finally {
      setLoading(false);
    }
  };

  const refresh = async () => {
    if (!selected || loading) return;
    setLoading(true);
    setError(null);
    try {
      const doc = await fetchDocument(selected.document_id);
      setSelected(doc);
      remember(doc);
    } catch (err) {
      setError(
        err instanceof ApiError ? err.message : "Failed to refresh the document.",
      );
    } finally {
      setLoading(false);
    }
  };

  const review = async (decision: "approved" | "rejected", reason: string) => {
    if (!selected) return;
    setBusy(true);
    try {
      await api.post(
        `/api/bookings/v1/kyc/documents/${encodeURIComponent(selected.document_id)}/review`,
        { decision, reason: reason.trim() },
        { tenant: orgSlug },
      );
      toast({
        title: decision === "approved" ? "Document approved" : "Document rejected",
        description: "The review decision and reason were written to the audit log.",
        variant: "success",
      });
      setReviewing(null);
      // Re-fetch so the card + session list show the authoritative state.
      const doc = await fetchDocument(selected.document_id);
      setSelected(doc);
      remember(doc);
    } catch (err) {
      toast({
        title: "Review failed",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const pending =
    (selected?.review_state ?? "pending_review").toLowerCase() ===
    "pending_review";

  return (
    <div>
      <PageHeader
        title="KYC document review"
        description="OCR pre-fills the fields; a human reviewer approves or rejects — extraction never auto-verifies. Owner/admin only; every document view is audit-logged."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void refresh()}
            disabled={!selected || loading}
          >
            <RefreshCw className="h-4 w-4" />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card className="mb-6">
        <CardHeader>
          <CardTitle>Open a document</CardTitle>
          <CardDescription>
            There is no list endpoint in the KYC document contract (W58) —
            documents are opened one at a time by the{" "}
            <span className="font-mono">document_id</span> or{" "}
            <span className="font-mono">reference</span> returned at upload.
            Documents you open this session are listed below.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={(e) => void search(e)} className="flex gap-2">
            <Input
              value={lookup}
              onChange={(e) => setLookup(e.target.value)}
              placeholder="document_id or reference…"
              className="max-w-xl font-mono"
              aria-label="Document id or reference"
            />
            <Button type="submit" variant="secondary" disabled={loading || !lookup.trim()}>
              <Search className="h-4 w-4" />
              {loading ? "Opening…" : "Open"}
            </Button>
          </form>
        </CardContent>
      </Card>

      {selected ? (
        <Card className="mb-6">
          <CardHeader className="flex-row items-start justify-between space-y-0">
            <div>
              <CardTitle className="flex items-center gap-2">
                <ShieldCheck className="h-4 w-4" />
                {DOC_TYPE_LABELS[selected.doc_type ?? ""] ?? "KYC document"}
              </CardTitle>
              <CardDescription>
                <span className="font-mono">{selected.document_id}</span>
                {selected.reference ? (
                  <>
                    {" · ref "}
                    <span className="font-mono">{selected.reference}</span>
                  </>
                ) : null}
                {selected.created_at
                  ? ` · uploaded ${formatDateTime(selected.created_at)}`
                  : ""}
              </CardDescription>
            </div>
            <div className="flex shrink-0 gap-2">
              {selected.status ? (
                <Badge variant={statusVariant(selected.status)}>
                  {selected.status}
                </Badge>
              ) : null}
              <Badge variant={reviewVariant(selected.review_state)}>
                {(selected.review_state ?? "pending_review").replace(/_/g, " ")}
              </Badge>
            </div>
          </CardHeader>
          <CardContent>
            {selected.fields_available ? (
              <ExtractedFieldsTable fields={selected.fields} />
            ) : (
              <p className="rounded-md border border-border bg-muted/40 px-3 py-2 text-sm text-muted-foreground">
                Extracted fields are no longer available — the 72-hour OCR
                retention window expired (or the document was erased) and only
                the hash-only record remains. Compare against the audit trail,
                not this page.
              </p>
            )}

            {selected.reviewed_at || selected.reviewer_actor ? (
              <p className="mt-3 text-xs text-muted-foreground">
                {selected.reviewer_actor
                  ? `Reviewed by ${selected.reviewer_actor}`
                  : "Reviewed"}
                {selected.reviewed_at
                  ? ` on ${formatDateTime(selected.reviewed_at)}`
                  : ""}
                .
              </p>
            ) : null}

            {pending ? (
              <div className="mt-4 flex gap-2">
                <Button
                  size="sm"
                  onClick={() => setReviewing({ decision: "approved" })}
                >
                  <Check className="h-3.5 w-3.5" /> Approve
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  className="text-destructive"
                  onClick={() => setReviewing({ decision: "rejected" })}
                >
                  <X className="h-3.5 w-3.5" /> Reject
                </Button>
              </div>
            ) : null}
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle>Opened this session</CardTitle>
            <CardDescription>
              Documents fetched from the API during this session — not a
              server-side queue.
            </CardDescription>
          </div>
          <Badge variant="secondary">{opened.length}</Badge>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-5">Document</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Review</TableHead>
                <TableHead className="pr-5">Fields</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {opened.length === 0 ? (
                <TableEmpty colSpan={5}>
                  No documents opened yet — use the lookup above.
                </TableEmpty>
              ) : (
                opened.map((d) => (
                  <TableRow
                    key={d.document_id}
                    className="cursor-pointer"
                    onClick={() => setSelected(d)}
                  >
                    <TableCell className="pl-5">
                      <span
                        className="font-mono text-xs text-primary underline-offset-2 hover:underline"
                        title={d.document_id}
                      >
                        {d.reference ?? d.document_id}
                      </span>
                    </TableCell>
                    <TableCell className="text-sm">
                      {DOC_TYPE_LABELS[d.doc_type ?? ""] ??
                        (d.doc_type ? d.doc_type.replace(/_/g, " ") : "—")}
                    </TableCell>
                    <TableCell>
                      <Badge variant={statusVariant(d.status)}>
                        {d.status ?? "—"}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <Badge variant={reviewVariant(d.review_state)}>
                        {(d.review_state ?? "pending_review").replace(/_/g, " ")}
                      </Badge>
                    </TableCell>
                    <TableCell className="pr-5 text-sm text-muted-foreground">
                      {d.fields_available ? "available" : "expired — hashes only"}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <ReviewDialog
        review={reviewing}
        busy={busy}
        onClose={() => setReviewing(null)}
        onSubmit={review}
      />
    </div>
  );
}

/**
 * Side-by-side rendering of the OCR-extracted fields (label | value). The
 * original document image is NOT served by any API — reviewers compare
 * these values against the physical/original document.
 */
function ExtractedFieldsTable({
  fields,
}: {
  fields: Record<string, unknown> | null | undefined;
}) {
  const rows = fieldRows(fields);
  if (rows.length === 0) {
    return (
      <p className="rounded-md border border-border bg-muted/40 px-3 py-2 text-sm text-muted-foreground">
        The OCR service returned no legible fields for this document —
        review against the original document.
      </p>
    );
  }
  return (
    <>
      <dl className="rounded-md border border-border text-sm">
        {rows.map(([k, v]) => (
          <div
            key={k}
            className="flex justify-between gap-4 border-b border-border px-3 py-1.5 last:border-b-0"
          >
            <dt className="text-muted-foreground">{fieldLabel(k)}</dt>
            <dd className="text-right font-medium">
              {k === "mrz_valid" ? (
                <Badge
                  variant={
                    v === "valid"
                      ? "success"
                      : v === "invalid"
                        ? "destructive"
                        : "secondary"
                  }
                >
                  {v}
                </Badge>
              ) : (
                v
              )}
            </dd>
          </div>
        ))}
      </dl>
      {fields && fields.mrz_valid !== undefined ? (
        <p className="mt-2 text-xs text-muted-foreground">
          MRZ check digits are validated by deterministic code (ICAO 9303),
          not by the model — a &quot;valid&quot; result is a checksum pass, not
          proof of authenticity.
        </p>
      ) : null}
    </>
  );
}

function ReviewDialog({
  review,
  busy,
  onClose,
  onSubmit,
}: {
  review: { decision: "approved" | "rejected" } | null;
  busy: boolean;
  onClose: () => void;
  onSubmit: (decision: "approved" | "rejected", reason: string) => void;
}) {
  const [reason, setReason] = React.useState("");

  React.useEffect(() => {
    if (review) setReason("");
  }, [review]);

  const rejecting = review?.decision === "rejected";
  /** Reason is mandatory on rejection (audit); optional note on approval. */
  const valid = review !== null && (!rejecting || reason.trim().length > 0);

  return (
    <Dialog open={review !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>
            {rejecting ? "Reject KYC document" : "Approve KYC document"}
          </DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          {rejecting
            ? "Rejecting marks the document as failed review. A reason is required and is written to the append-only audit log."
            : "Approving confirms you compared the extracted fields against the original document. The decision and actor are written to the append-only audit log."}
        </p>
        <div className="grid gap-1.5">
          <Label htmlFor="kyc-review-reason">
            Reason{rejecting ? "" : " (optional)"}
          </Label>
          <Textarea
            id="kyc-review-reason"
            rows={4}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder={
              rejecting
                ? "e.g. extracted name does not match the document photo…"
                : "e.g. fields verified against the original document…"
            }
          />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant={rejecting ? "destructive" : "default"}
            onClick={() => review && onSubmit(review.decision, reason)}
            disabled={busy || !valid}
          >
            {busy
              ? "Submitting…"
              : rejecting
                ? "Reject document"
                : "Approve document"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
