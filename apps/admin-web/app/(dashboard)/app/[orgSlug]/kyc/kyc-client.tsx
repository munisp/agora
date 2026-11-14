"use client";

/**
 * SPEC-W65 Scope 2 (D): KYC document review queue (owner/admin only — the
 * page redirects other roles server-side; the booking-service KYC routes
 * enforce the same gate, SPEC-W58 P5).
 *
 * API surface (booking-service staff proxy → kyc-service, riding the
 * EXISTING /api/bookings/* APISIX OIDC wildcard — no new gateway route):
 *
 *   GET  /api/bookings/v1/kyc/documents?review_state=&limit=&offset=
 *     → {items: [hash-only Document projections incl. subject_phone],
 *        limit, offset} — newest-first paginated queue (W65; the W58
 *    session-local list is REPLACED by this real server-side queue).
 *   GET  /api/bookings/v1/kyc/documents/{id}
 *     → {document_id, reference, status, review_state, fields?, fields_available}
 *   POST /api/bookings/v1/kyc/documents/{id}/review
 *     {decision: "approved"|"rejected", reason: string}
 *
 * Honesty notes (SPEC-W58/W65 cross-scope contract + pinned decisions):
 *  - The queue rows are the hash-only durable projections (P8) — extracted
 *    field VALUES are only on the detail GET while inside the 72h OCR TTL
 *    (P4). After expiry the API returns fields_available:false and this
 *    page renders "expired — hashes only" instead of field values.
 *  - Extracted fields are PRE-FILL for human review only — OCR never
 *    auto-verifies (P7).
 *  - Document BYTES are never served by any API — reviewers compare the
 *    extracted fields against the physical/original document.
 *  - mrz_valid (kyc_passport) is an ICAO 9303 check-digit result computed
 *    in deterministic code by ocr-service — it is NOT a model claim and
 *    NOT an authenticity verdict.
 *  - Lookup-by-id is kept as a fallback (paste a document_id or reference
 *    returned at upload) — useful for deep links; the queue is primary.
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

/** Shared detail response envelope (SPEC-W58 cross-scope contract). */
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
  subject_phone?: string | null;
  created_at?: string | null;
  reviewed_at?: string | null;
  reviewer_actor?: string | null;
}

/**
 * Queue row: the hash-only kyc-service Document projection (SPEC-W65
 * pinned contract — subject_phone included; NO extracted field values).
 */
interface KycDocumentListItem {
  document_id: string;
  subject_phone?: string | null;
  doc_type?: string | null;
  status?: string | null;
  review_state?: string | null;
  reviewer_actor?: string | null;
  reviewed_at?: string | null;
  created_at?: string | null;
}

type ReviewStateFilter = "" | "pending_review" | "approved" | "rejected";

const REVIEW_STATE_FILTERS: { value: ReviewStateFilter; label: string }[] = [
  { value: "", label: "All" },
  { value: "pending_review", label: "Pending review" },
  { value: "approved", label: "Approved" },
  { value: "rejected", label: "Rejected" },
];

/** Offset pagination (media-inbox PAGE_SIZE precedent). */
const PAGE_SIZE = 50;

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

/** Tolerant unwrap — the endpoint returns {items: [...]} or a bare array. */
function unwrapItems(data: unknown): KycDocumentListItem[] {
  if (Array.isArray(data)) return data as KycDocumentListItem[];
  if (
    typeof data === "object" &&
    data !== null &&
    Array.isArray((data as { items?: unknown }).items)
  ) {
    return (data as { items: KycDocumentListItem[] }).items;
  }
  return [];
}

export function KycReviewClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  // Server-fetched queue (SPEC-W65).
  const [filter, setFilter] = React.useState<ReviewStateFilter>("pending_review");
  const [queue, setQueue] = React.useState<KycDocumentListItem[]>([]);
  const [queueLoading, setQueueLoading] = React.useState(true);
  const [queueError, setQueueError] = React.useState<string | null>(null);
  const [hasMore, setHasMore] = React.useState(false);
  // Lookup-by-id fallback + selected document detail.
  const [lookup, setLookup] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [selected, setSelected] = React.useState<KycDocumentDetail | null>(null);
  const [reviewing, setReviewing] = React.useState<{
    decision: "approved" | "rejected";
  } | null>(null);
  const [busy, setBusy] = React.useState(false);

  /**
   * Fetch one queue page. offset=0 replaces the queue; any other offset
   * appends (load-more). hasMore is inferred from a full page coming back.
   */
  const loadQueue = React.useCallback(
    async (state: ReviewStateFilter, offset: number, initial = false) => {
      if (initial) setQueueLoading(true);
      setQueueError(null);
      try {
        const data = await api.get<
          { items: KycDocumentListItem[] } | KycDocumentListItem[]
        >("/api/bookings/v1/kyc/documents", {
          tenant: orgSlug,
          review_state: state || undefined,
          limit: PAGE_SIZE,
          offset,
        });
        const page = unwrapItems(data);
        setQueue((prev) => (offset === 0 ? page : [...prev, ...page]));
        setHasMore(page.length >= PAGE_SIZE);
      } catch (err) {
        setQueueError(
          err instanceof ApiError && err.status === 403
            ? "KYC document review requires the owner or admin role."
            : err instanceof ApiError
              ? err.message
              : "Failed to load the KYC document queue.",
        );
        if (initial) {
          setQueue([]);
          setHasMore(false);
        }
      } finally {
        setQueueLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    void loadQueue(filter, 0, true);
  }, [filter, loadQueue]);

  const refreshQueue = () => void loadQueue(filter, 0, true);
  const loadMore = () => void loadQueue(filter, queue.length);

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

  /** Open a document detail from the queue or the lookup fallback. */
  const openDocument = async (id: string) => {
    const trimmed = id.trim();
    if (!trimmed || loading) return;
    setLoading(true);
    setError(null);
    try {
      const doc = await fetchDocument(trimmed);
      setSelected(doc);
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

  const search = async (e: React.FormEvent) => {
    e.preventDefault();
    await openDocument(lookup);
  };

  const refresh = async () => {
    if (!selected || loading) return;
    await openDocument(selected.document_id);
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
      // Re-fetch so the card shows the authoritative state, and refresh
      // the queue so the row leaves/enters the filter bucket.
      const doc = await fetchDocument(selected.document_id);
      setSelected(doc);
      refreshQueue();
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
            onClick={refreshQueue}
            disabled={queueLoading}
          >
            <RefreshCw
              className={queueLoading ? "h-4 w-4 animate-spin" : "h-4 w-4"}
            />
            {queueLoading ? "Loading…" : "Refresh"}
          </Button>
        }
      />

      {/* Review-state filter — pending_review is the default queue. */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        {REVIEW_STATE_FILTERS.map((f) => (
          <Button
            key={f.value || "all"}
            variant={filter === f.value ? "default" : "outline"}
            size="sm"
            onClick={() => setFilter(f.value)}
            disabled={queueLoading}
          >
            {f.label}
          </Button>
        ))}
      </div>

      {queueError ? <ErrorNote message={queueError} /> : null}

      <Card className="mb-6">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <ShieldCheck className="h-5 w-5" />
            Review queue
          </CardTitle>
          <CardDescription>
            Server-side queue from kyc-service via the booking proxy — newest
            first. Rows are the hash-only durable records; open one to see
            the OCR-extracted fields (while inside the 72-hour retention
            window).
          </CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-5">Uploaded</TableHead>
                <TableHead>Document</TableHead>
                <TableHead>Subject</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="pr-5">Review</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {queue.length === 0 && !queueLoading ? (
                <TableEmpty colSpan={6}>
                  {queueError
                    ? "Could not load the review queue."
                    : `No ${filter ? REVIEW_STATE_FILTERS.find((f) => f.value === filter)?.label.toLowerCase() : ""} KYC documents.`}
                </TableEmpty>
              ) : (
                queue.map((d) => (
                  <TableRow
                    key={d.document_id}
                    className="cursor-pointer"
                    onClick={() => void openDocument(d.document_id)}
                  >
                    <TableCell className="pl-5 text-sm text-muted-foreground">
                      {d.created_at ? formatDateTime(d.created_at) : "—"}
                    </TableCell>
                    <TableCell>
                      <span
                        className="font-mono text-xs text-primary underline-offset-2 hover:underline"
                        title={d.document_id}
                      >
                        {d.document_id}
                      </span>
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {d.subject_phone ?? "—"}
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
                    <TableCell className="pr-5">
                      <Badge variant={reviewVariant(d.review_state)}>
                        {(d.review_state ?? "pending_review").replace(/_/g, " ")}
                      </Badge>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
          {hasMore ? (
            <div className="my-4 flex justify-center">
              <Button
                variant="outline"
                size="sm"
                onClick={loadMore}
                disabled={queueLoading}
              >
                {queueLoading ? "Loading…" : "Load more"}
              </Button>
            </div>
          ) : null}
        </CardContent>
      </Card>

      <Card className="mb-6">
        <CardHeader>
          <CardTitle>Open by id or reference</CardTitle>
          <CardDescription>
            Fallback lookup — paste the{" "}
            <span className="font-mono">document_id</span> or{" "}
            <span className="font-mono">reference</span> returned at upload
            (e.g. from a deep link outside the queue).
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

      {error ? <ErrorNote message={error} /> : null}

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
                {selected.subject_phone
                  ? ` · subject ${selected.subject_phone}`
                  : ""}
                {selected.created_at
                  ? ` · uploaded ${formatDateTime(selected.created_at)}`
                  : ""}
                {" · "}
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-6 px-2"
                  onClick={() => void refresh()}
                  disabled={loading}
                >
                  <RefreshCw className="h-3 w-3" />
                  Refresh
                </Button>
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
