"use client";

import * as React from "react";
import { Check, FileText, FileUp, Plus, Search, Sparkles, Trash2, Upload, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import {
  chooseUploadMode,
  pollDelayMs,
  reduceJobStatus,
  validateUploadFile,
  UPLOAD_EXTENSIONS,
  UPLOAD_MAX_BYTES,
} from "@/lib/knowledge-upload";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  ConfirmDialog,
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Textarea } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useToast } from "@/components/ui/toast";
import type { KbSuggestion, KnowledgeDocument, KnowledgeSearchHit } from "@/lib/types";
import { listFrom } from "@/lib/utils";

export function KnowledgeClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [docs, setDocs] = React.useState<KnowledgeDocument[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [adding, setAdding] = React.useState(false);
  const [scanning, setScanning] = React.useState(false);
  const [uploadingPolicy, setUploadingPolicy] = React.useState(false);
  const [uploading, setUploading] = React.useState(false);
  /** SPEC-W53: OCR-extracted draft that pre-fills the add dialog. */
  const [prefill, setPrefill] = React.useState<{
    title: string;
    body: string;
    source_url: string;
  } | null>(null);
  const [deleting, setDeleting] = React.useState<KnowledgeDocument | null>(null);
  const [busy, setBusy] = React.useState(false);

  const [query, setQuery] = React.useState("");
  const [hits, setHits] = React.useState<KnowledgeSearchHit[] | null>(null);
  const [searching, setSearching] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<KnowledgeDocument[] | { items: KnowledgeDocument[] }>(
        "/api/knowledge/v1/documents",
        { tenant: orgSlug },
      );
      setDocs(listFrom(data));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load documents.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const add = async (form: { title: string; body: string; source_url: string }) => {
    setBusy(true);
    try {
      await api.post(
        "/api/knowledge/v1/documents",
        {
          // knowledge-service requires the tenant in the body (422 without it); it must match the session's tenant.
          tenant: orgSlug,
          title: form.title.trim(),
          body: form.body.trim(),
          source_url: form.source_url.trim() || undefined,
        },
        { tenant: orgSlug },
      );
      toast({
        title: "Document added",
        description: "It will be chunked, embedded and searchable shortly.",
        variant: "success",
      });
      setAdding(false);
      await load();
    } catch (e) {
      toast({
        title: "Add failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!deleting) return;
    setBusy(true);
    try {
      await api.delete(`/api/knowledge/v1/documents/${deleting.id}`, {
        tenant: orgSlug,
      });
      toast({ title: "Document deleted", variant: "success" });
      setDeleting(null);
      await load();
    } catch (e) {
      toast({
        title: "Delete failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const search = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!query.trim()) return;
    setSearching(true);
    try {
      const data = await api.get<KnowledgeSearchHit[] | { items: KnowledgeSearchHit[] }>(
        "/api/knowledge/v1/search",
        { tenant: orgSlug, q: query.trim() },
      );
      setHits(listFrom(data));
    } catch (err) {
      toast({
        title: "Search failed",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setSearching(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Knowledge base"
        description="Documents that ground the receptionist's answers (hybrid BM25 + vector search)."
        actions={
          <div className="flex gap-2">
            <Button size="sm" variant="outline" onClick={() => setUploadingPolicy(true)}>
              <FileText className="h-4 w-4" /> Upload policy document
            </Button>
            <Button size="sm" variant="outline" onClick={() => setUploading(true)}>
              <FileUp className="h-4 w-4" /> Upload document
            </Button>
            <Button size="sm" variant="outline" onClick={() => setScanning(true)}>
              <Upload className="h-4 w-4" /> Upload scan
            </Button>
            <Button size="sm" onClick={() => setAdding(true)}>
              <Plus className="h-4 w-4" /> Add document
            </Button>
          </div>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Tabs defaultValue="documents">
        <TabsList>
          <TabsTrigger value="documents">Documents</TabsTrigger>
          <TabsTrigger value="suggestions">Review queue</TabsTrigger>
        </TabsList>

        <TabsContent value="suggestions">
          <SuggestionsPanel orgSlug={orgSlug} onApproved={load} />
        </TabsContent>

        <TabsContent value="documents">
      <Card className="mb-6">
        <CardHeader>
          <CardTitle>Test search</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={search} className="flex gap-2">
            <Input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Ask something a customer would ask…"
              className="max-w-xl"
            />
            <Button type="submit" variant="secondary" disabled={searching}>
              <Search className="h-4 w-4" />
              {searching ? "Searching…" : "Search"}
            </Button>
          </form>
          {hits !== null ? (
            <div className="mt-4 space-y-3">
              {hits.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No matching chunks. Try adding more documents.
                </p>
              ) : (
                hits.map((h, i) => (
                  <div
                    key={`${h.document_id}-${h.chunk_id ?? i}`}
                    className="rounded-md border border-border bg-muted/40 p-3"
                  >
                    <div className="flex items-center justify-between">
                      <p className="text-sm font-medium">{h.title}</p>
                      <span className="text-xs text-muted-foreground">
                        score {h.score.toFixed(3)}
                      </span>
                    </div>
                    <p className="mt-1 text-sm text-muted-foreground">{h.snippet}</p>
                  </div>
                ))
              )}
            </div>
          ) : null}
        </CardContent>
      </Card>

      <div className="grid gap-3">
        {docs.length === 0 ? (
          <Card>
            <CardContent className="py-10 text-center text-sm text-muted-foreground">
              {loading ? "Loading…" : "No documents yet — add FAQs, policies, service details."}
            </CardContent>
          </Card>
        ) : (
          docs.map((d) => (
            <Card key={d.id}>
              <CardContent className="flex items-start justify-between gap-4 py-4">
                <div className="min-w-0">
                  <p className="font-medium">{d.title}</p>
                  <p className="mt-1 line-clamp-2 text-sm text-muted-foreground">
                    {d.body}
                  </p>
                  <p className="mt-2 text-xs text-muted-foreground">
                    Added {new Date(d.created_at).toLocaleDateString()}
                    {d.source_url ? ` · ${d.source_url}` : ""}
                  </p>
                </div>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Delete document"
                  className="shrink-0 text-destructive"
                  onClick={() => setDeleting(d)}
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </CardContent>
            </Card>
          ))
        )}
      </div>
        </TabsContent>
      </Tabs>

      <AddDocumentDialog
        open={adding}
        busy={busy}
        initial={prefill}
        onClose={() => {
          setAdding(false);
          setPrefill(null);
        }}
        onAdd={add}
      />
      <DoclingUploadDialog
        orgSlug={orgSlug}
        open={uploading}
        onClose={() => setUploading(false)}
        onDone={() => {
          setUploading(false);
          void load();
        }}
      />
      <PolicyUploadDialog
        orgSlug={orgSlug}
        open={uploadingPolicy}
        onClose={() => setUploadingPolicy(false)}
        onUploaded={(title, chunks) => {
          setUploadingPolicy(false);
          toast({
            title: "Document added",
            description: `${title}: ${chunks} searchable section${chunks === 1 ? "" : "s"}. The receptionist can now answer questions about it.`,
            variant: "success",
          });
          void load();
        }}
      />
      <ScanUploadDialog
        orgSlug={orgSlug}
        open={scanning}
        onClose={() => setScanning(false)}
        onUseText={(draft) => {
          setScanning(false);
          setPrefill(draft);
          setAdding(true);
        }}
      />
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete “${deleting?.title ?? ""}”?`}
        description="Its chunks are removed from the search index; the receptionist stops using it immediately."
        confirmLabel="Delete document"
        destructive
        busy={busy}
        onConfirm={remove}
      />
    </div>
  );
}

function AddDocumentDialog({
  open,
  busy,
  initial,
  onClose,
  onAdd,
}: {
  open: boolean;
  busy: boolean;
  /** SPEC-W53: OCR-extracted draft to pre-fill the form with, if any. */
  initial?: { title: string; body: string; source_url: string } | null;
  onClose: () => void;
  onAdd: (form: { title: string; body: string; source_url: string }) => void;
}) {
  const [title, setTitle] = React.useState("");
  const [body, setBody] = React.useState("");
  const [sourceUrl, setSourceUrl] = React.useState("");

  React.useEffect(() => {
    if (open) {
      setTitle(initial?.title ?? "");
      setBody(initial?.body ?? "");
      setSourceUrl(initial?.source_url ?? "");
    }
  }, [open, initial]);

  const valid = title.trim().length > 0 && body.trim().length > 0;

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Add document</DialogTitle>
        </DialogHeader>
        {initial?.body ? (
          <p className="mb-1 rounded-md border border-border bg-accent px-3 py-2 text-xs text-muted-foreground">
            Pre-filled from an OCR scan — review the extracted text for
            recognition errors before adding it to the knowledge base.
          </p>
        ) : null}
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="kb-title">Title</Label>
            <Input
              id="kb-title"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="Cancellation policy"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="kb-body">Body</Label>
            <Textarea
              id="kb-body"
              rows={8}
              value={body}
              onChange={(e) => setBody(e.target.value)}
              placeholder="The full text the receptionist may quote from…"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="kb-source">Source URL (optional)</Label>
            <Input
              id="kb-source"
              value={sourceUrl}
              onChange={(e) => setSourceUrl(e.target.value)}
              placeholder="https://example.com/policies"
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => onAdd({ title, body, source_url: sourceUrl })}
            disabled={busy || !valid}
          >
            {busy ? "Adding…" : "Add document"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Self-improving KB review queue (innovation 4): questions the receptionist
 * could not answer (search score below threshold) land here as drafts.
 * Approving creates a real, embedded document; rejecting drops the draft.
 */
function SuggestionsPanel({
  orgSlug,
  onApproved,
}: {
  orgSlug: string;
  onApproved: () => Promise<void>;
}) {
  const { toast } = useToast();
  const [suggestions, setSuggestions] = React.useState<KbSuggestion[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [unsupported, setUnsupported] = React.useState(false);
  const [approving, setApproving] = React.useState<KbSuggestion | null>(null);
  const [rejecting, setRejecting] = React.useState<KbSuggestion | null>(null);
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    setUnsupported(false);
    try {
      const data = await api.get<KbSuggestion[] | { items: KbSuggestion[] }>(
        "/api/knowledge/v1/suggestions",
        { tenant: orgSlug, status: "pending" },
      );
      setSuggestions(listFrom(data));
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        setUnsupported(true);
      } else {
        setError(e instanceof ApiError ? e.message : "Failed to load suggestions.");
      }
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const approve = async (form: { title: string; body: string }) => {
    if (!approving) return;
    setBusy(true);
    try {
      await api.post(
        `/api/knowledge/v1/suggestions/${approving.id}/approve`,
        { title: form.title.trim(), body: form.body.trim() },
        { tenant: orgSlug },
      );
      toast({
        title: "Suggestion approved",
        description: "A knowledge document was created and will be indexed shortly.",
        variant: "success",
      });
      setApproving(null);
      await load();
      await onApproved();
    } catch (e) {
      toast({
        title: "Approve failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const reject = async () => {
    if (!rejecting) return;
    setBusy(true);
    try {
      await api.delete(`/api/knowledge/v1/suggestions/${rejecting.id}`, {
        tenant: orgSlug,
      });
      toast({ title: "Suggestion rejected", variant: "success" });
      setRejecting(null);
      await load();
    } catch (e) {
      toast({
        title: "Reject failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  if (unsupported) {
    return (
      <Card>
        <CardContent className="py-10 text-center text-sm text-muted-foreground">
          This deployment&apos;s knowledge-service does not expose the review
          queue yet (GET /v1/suggestions). Upgrade to enable the self-improving
          knowledge loop.
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-3">
      {error ? <ErrorNote message={error} /> : null}
      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle className="flex items-center gap-2">
              <Sparkles className="h-4 w-4" /> Pending suggestions
            </CardTitle>
            <CardDescription>
              Questions customers asked that the knowledge base could not answer.
            </CardDescription>
          </div>
          <Badge variant="secondary">{suggestions.length}</Badge>
        </CardHeader>
      </Card>

      {suggestions.length === 0 ? (
        <Card>
          <CardContent className="py-10 text-center text-sm text-muted-foreground">
            {loading
              ? "Loading…"
              : "Queue is empty — the receptionist is answering everything, or no low-score questions have come in yet."}
          </CardContent>
        </Card>
      ) : (
        suggestions.map((s) => (
          <Card key={s.id}>
            <CardContent className="flex items-start justify-between gap-4 py-4">
              <div className="min-w-0">
                <p className="font-medium">“{s.question}”</p>
                {s.suggested_answer ? (
                  <p className="mt-1 line-clamp-2 text-sm text-muted-foreground">
                    {s.suggested_answer}
                  </p>
                ) : null}
                <p className="mt-2 text-xs text-muted-foreground">
                  {new Date(s.created_at).toLocaleString()}
                  {typeof s.score === "number"
                    ? ` · best score ${s.score.toFixed(3)}`
                    : ""}
                </p>
              </div>
              <div className="flex shrink-0 gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setApproving(s)}
                >
                  <Check className="h-3.5 w-3.5" /> Approve
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-destructive"
                  onClick={() => setRejecting(s)}
                >
                  <X className="h-3.5 w-3.5" /> Reject
                </Button>
              </div>
            </CardContent>
          </Card>
        ))
      )}

      <ApproveSuggestionDialog
        suggestion={approving}
        busy={busy}
        onClose={() => setApproving(null)}
        onApprove={approve}
      />
      <ConfirmDialog
        open={rejecting !== null}
        onOpenChange={(open) => !open && setRejecting(null)}
        title="Reject this suggestion?"
        description={
          rejecting
            ? `“${rejecting.question}” will be dropped from the review queue.`
            : undefined
        }
        confirmLabel="Reject suggestion"
        destructive
        busy={busy}
        onConfirm={reject}
      />
    </div>
  );
}

function ApproveSuggestionDialog({
  suggestion,
  busy,
  onClose,
  onApprove,
}: {
  suggestion: KbSuggestion | null;
  busy: boolean;
  onClose: () => void;
  onApprove: (form: { title: string; body: string }) => void;
}) {
  const [title, setTitle] = React.useState("");
  const [body, setBody] = React.useState("");

  React.useEffect(() => {
    if (suggestion) {
      setTitle(suggestion.suggested_title ?? suggestion.question);
      setBody(suggestion.suggested_answer ?? "");
    }
  }, [suggestion]);

  const valid = title.trim().length > 0 && body.trim().length > 0;

  return (
    <Dialog open={suggestion !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Approve as knowledge document</DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          Write the canonical answer. It becomes a real document — chunked,
          embedded and used by the receptionist from then on.
        </p>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="sg-title">Title</Label>
            <Input
              id="sg-title"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="sg-body">Answer</Label>
            <Textarea
              id="sg-body"
              rows={8}
              value={body}
              onChange={(e) => setBody(e.target.value)}
              placeholder="The approved answer the receptionist may quote…"
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => onApprove({ title, body })}
            disabled={busy || !valid}
          >
            {busy ? "Publishing…" : "Approve & publish"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/* ------------------------------------------------------------------ */
/* SPEC-W53 Scope C: scan/image/PDF upload via the ocr-service.        */
/* ------------------------------------------------------------------ */

/** Extensions the ocr-service accepts (magic bytes verified server-side). */
const SCAN_ACCEPT = ".jpg,.jpeg,.png,.webp,.pdf";
/** Mirror of the ocr-service hard cap (OCR_MAX_BYTES, 25 MB). */
const SCAN_MAX_BYTES = 25 * 1024 * 1024;

interface ScanExtraction {
  documentId: string | null;
  status: string | null;
  pageCount: number | null;
  /** Extracted markdown/text ready for the knowledge ingest path. */
  text: string | null;
  /** First few structured fields, for the confirmation summary. */
  fields: [string, string][];
}

function asString(v: unknown): string | null {
  return typeof v === "string" && v.trim() !== "" ? v : null;
}

/**
 * Tolerant decode of the ocr-service extract response
 * {document_id, status, extracted_fields, page_count}. The extracted body
 * text rides either as top-level `extracted_text` or as a text/markdown key
 * inside extracted_fields — accept both so the A-side contract can settle
 * without another UI change.
 */
function normalizeExtraction(data: unknown): ScanExtraction {
  const o = (typeof data === "object" && data !== null ? data : {}) as Record<
    string,
    unknown
  >;
  const fieldsRaw =
    typeof o.extracted_fields === "object" && o.extracted_fields !== null
      ? (o.extracted_fields as Record<string, unknown>)
      : {};
  const pageCount =
    typeof o.page_count === "number" && Number.isFinite(o.page_count)
      ? o.page_count
      : null;
  const text =
    asString(o.extracted_text) ??
    asString(fieldsRaw.markdown) ??
    asString(fieldsRaw.text) ??
    asString(fieldsRaw.content);
  const fields: [string, string][] = [];
  for (const [k, v] of Object.entries(fieldsRaw)) {
    if (fields.length >= 5) break;
    if (typeof v === "string" && v.trim() !== "" && v.length <= 120) {
      fields.push([k, v.trim()]);
    } else if (typeof v === "number" && Number.isFinite(v)) {
      fields.push([k, String(v)]);
    }
  }
  return {
    documentId: asString(o.document_id),
    status: asString(o.status),
    pageCount,
    text,
    fields,
  };
}

/**
 * Upload a scanned document/photo → ocr-service → review the extraction →
 * confirm into the EXISTING knowledge ingest path (the same
 * POST /api/knowledge/v1/documents the textarea uses, via AddDocumentDialog).
 *
 * Multipart POST goes through the BFF binary passthrough (W52-D) to
 * /api/ocr/v1/ocr/extract with kind=knowledge_scan; api.post is JSON-only so
 * this fetch mirrors its x-tenant-slug header convention directly (same
 * pattern as locations/boundary-import.tsx).
 */
function ScanUploadDialog({
  orgSlug,
  open,
  onClose,
  onUseText,
}: {
  orgSlug: string;
  open: boolean;
  onClose: () => void;
  onUseText: (draft: { title: string; body: string; source_url: string }) => void;
}) {
  const [file, setFile] = React.useState<File | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [result, setResult] = React.useState<ScanExtraction | null>(null);

  React.useEffect(() => {
    if (open) {
      setFile(null);
      setError(null);
      setResult(null);
    }
  }, [open]);

  const pick = (f: File | null) => {
    setResult(null);
    setError(null);
    if (f && f.size > SCAN_MAX_BYTES) {
      setFile(null);
      setError("File exceeds the 25 MB upload cap — compress or split it and retry.");
      return;
    }
    setFile(f);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!file || busy) return;
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const form = new FormData();
      form.append("file", file);
      form.append("kind", "knowledge_scan");
      const res = await fetch("/api/ocr/v1/ocr/extract", {
        method: "POST",
        headers: { "x-tenant-slug": orgSlug, accept: "application/json" },
        body: form,
        cache: "no-store",
      });
      const text = await res.text();
      let data: unknown = null;
      if (text) {
        try {
          data = JSON.parse(text);
        } catch {
          data = text;
        }
      }
      if (!res.ok) throw new ApiError(res.status, data);
      const extraction = normalizeExtraction(data);
      if (extraction.status === "failed") {
        const msg =
          typeof data === "object" && data !== null && "error" in data
            ? String((data as { error: unknown }).error)
            : "extraction failed";
        throw new Error(`The OCR service could not extract this document: ${msg}`);
      }
      setResult(extraction);
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.message
          : err instanceof Error
            ? err.message
            : "Extraction failed — the ocr-service may be offline.",
      );
    } finally {
      setBusy(false);
    }
  };

  const useText = () => {
    if (!result?.text || !file) return;
    const stem = file.name.replace(/\.[^.]+$/, "").trim() || "Scanned document";
    onUseText({ title: stem, body: result.text, source_url: "" });
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Upload a scan</DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          Upload a photo or PDF of a document. The OCR service extracts the
          text with a local vision model; you review it, then add it to the
          knowledge base.
        </p>

        {error ? <ErrorNote message={error} /> : null}

        {result ? (
          <div className="space-y-3">
            <div className="rounded-md border border-border bg-accent px-3 py-2 text-sm">
              <p className="font-medium">
                Extracted {result.text ? result.text.length.toLocaleString() : 0}{" "}
                characters
                {result.pageCount !== null
                  ? ` from ${result.pageCount} page${result.pageCount === 1 ? "" : "s"}`
                  : ""}
              </p>
              {result.documentId ? (
                <p className="mt-0.5 text-xs text-muted-foreground">
                  Document <span className="font-mono">{result.documentId}</span>
                  {result.status ? ` · ${result.status}` : ""}
                </p>
              ) : null}
            </div>
            {result.fields.length > 0 ? (
              <dl className="rounded-md border border-border px-3 py-2 text-xs">
                {result.fields.map(([k, v]) => (
                  <div key={k} className="flex justify-between gap-3 py-0.5">
                    <dt className="text-muted-foreground">{k}</dt>
                    <dd className="truncate font-medium">{v}</dd>
                  </div>
                ))}
              </dl>
            ) : null}
            {result.text ? (
              <p className="line-clamp-4 whitespace-pre-wrap rounded-md border border-border bg-muted/40 p-3 text-xs text-muted-foreground">
                {result.text}
              </p>
            ) : (
              <ErrorNote message="No text could be read from this file — try a sharper scan." />
            )}
            <DialogFooter>
              <Button
                variant="outline"
                onClick={() => {
                  setResult(null);
                  setFile(null);
                }}
              >
                Scan another file
              </Button>
              <Button onClick={useText} disabled={!result.text}>
                Review &amp; add to knowledge base
              </Button>
            </DialogFooter>
          </div>
        ) : (
          <form onSubmit={(e) => void submit(e)} className="space-y-4">
            <div className="grid gap-1.5">
              <Label htmlFor="scan-file">Scanned document</Label>
              <Input
                id="scan-file"
                type="file"
                accept={SCAN_ACCEPT}
                onChange={(e) => pick(e.target.files?.[0] ?? null)}
              />
              <p className="text-xs text-muted-foreground">
                {SCAN_ACCEPT} · up to 25 MB. Uploaded files may contain personal
                data and are stored under your organisation&apos;s document
                bucket.
              </p>
            </div>
            {busy ? (
              <p className="rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                Uploading and extracting… OCR runs on a local vision model and
                can take a minute or more per page on CPU. Don&apos;t close this
                dialog.
              </p>
            ) : null}
            <DialogFooter>
              <Button variant="outline" onClick={onClose} disabled={busy}>
                Cancel
              </Button>
              <Button type="submit" disabled={!file || busy}>
                <Upload className="h-4 w-4" />
                {busy ? "Extracting…" : "Extract text"}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}


/**
 * 2026-11-21: upload a policy document (PDF with a text layer, Word .docx, .txt or .md). knowledge-service extracts
 * the text and ingests it like a typed document (chunk -> embed -> index), so contributors can ask the receptionist
 * questions about it in chat or on a call. Scanned PDFs get a pointer to "Upload scan" (OCR). Multipart goes through
 * the BFF binary passthrough, mirroring ScanUploadDialog's fetch.
 */
const POLICY_ACCEPT = ".pdf,.docx,.txt,.md";
function PolicyUploadDialog({
  orgSlug,
  open,
  onClose,
  onUploaded,
}: {
  orgSlug: string;
  open: boolean;
  onClose: () => void;
  onUploaded: (title: string, chunks: number) => void;
}) {
  const [file, setFile] = React.useState<File | null>(null);
  const [title, setTitle] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (open) {
      setFile(null);
      setTitle("");
      setError(null);
    }
  }, [open]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!file || busy) return;
    if (file.size > SCAN_MAX_BYTES) {
      setError("File exceeds the 25 MB upload cap — compress or split it and retry.");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const form = new FormData();
      form.append("tenant", orgSlug);
      form.append("file", file);
      if (title.trim()) form.append("title", title.trim());
      const res = await fetch("/api/knowledge/v1/documents/upload-file", {
        method: "POST",
        headers: { "x-tenant-slug": orgSlug, accept: "application/json" },
        body: form,
        cache: "no-store",
      });
      const text = await res.text();
      let data: unknown = null;
      try {
        data = text ? JSON.parse(text) : null;
      } catch {
        data = text;
      }
      if (!res.ok) {
        const detail =
          typeof data === "object" && data !== null && "detail" in data
            ? String((data as { detail: unknown }).detail)
            : null;
        throw detail ? new Error(detail) : new ApiError(res.status, data);
      }
      const d = (data ?? {}) as { title?: string; chunks?: number; chunk_count?: number };
      onUploaded(d.title ?? (title.trim() || file.name), Number(d.chunks ?? d.chunk_count ?? 0));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Upload failed — the knowledge service may be offline.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Upload policy document</DialogTitle>
        </DialogHeader>
        <form onSubmit={submit} className="grid gap-4">
          <p className="text-sm text-muted-foreground">
            Upload a policy, product guide or FAQ. The receptionist reads it and answers contributors&apos; questions
            from it in chat and on calls. PDF, Word (.docx), .txt or .md, up to 25 MB.
          </p>
          <div className="grid gap-1.5">
            <Label htmlFor="policy-file">Document</Label>
            <Input
              id="policy-file"
              type="file"
              accept={POLICY_ACCEPT}
              onChange={(e) => setFile(e.target.files?.[0] ?? null)}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="policy-title">Title (optional)</Label>
            <Input
              id="policy-title"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="e.g. Contributor benefits policy 2026"
            />
          </div>
          {error ? <ErrorNote message={error} /> : null}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" disabled={!file || busy}>
              {busy ? "Reading document…" : "Upload"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/* ------------------------------------------------------------------------ */
/* SPEC-W114: direct document upload via the docling pipeline (W107/W111).  */
/* PDF/DOCX/PPTX/XLSX/HTML/MD/TXT + scanned images; tables become Markdown. */
/* Files above ASYNC_THRESHOLD_BYTES submit wait=false (W111 job) and the   */
/* dialog polls the tenant-bound job status endpoint until done/failed.     */
/* Distinct from the W53 "Upload scan" flow (review OCR text BEFORE add).   */
/* ------------------------------------------------------------------------ */

type UploadJob = {
  job_id: string;
  status: string;
  document_id: string | null;
  chars: number | null;
  error: string | null;
};

/** Honest message for the knowledge-service upload failure contract. */
function uploadErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const detail =
      typeof err.body === "object" && err.body !== null && "detail" in err.body
        ? String((err.body as { detail: unknown }).detail)
        : null;
    if (detail) return detail;
    switch (err.status) {
      case 413:
        return "The file is too large for the document pipeline — compress or split it and retry.";
      case 415:
        return "Unsupported file type for the document pipeline.";
      case 422:
        return "The document pipeline could not extract any text from this file.";
      case 503:
        return "The document-intelligence service is offline — try again in a few minutes.";
      default:
        return err.message;
    }
  }
  return err instanceof Error ? err.message : "Upload failed.";
}

function readFileBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("Could not read the file."));
    reader.onload = () => {
      const url = String(reader.result ?? "");
      const comma = url.indexOf(",");
      resolve(comma >= 0 ? url.slice(comma + 1) : url);
    };
    reader.readAsDataURL(file);
  });
}

function DoclingUploadDialog({
  orgSlug,
  open,
  onClose,
  onDone,
}: {
  orgSlug: string;
  open: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const [file, setFile] = React.useState<File | null>(null);
  const [title, setTitle] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  /** pick -> uploading -> processing (async job) -> done */
  const [phase, setPhase] = React.useState<"pick" | "uploading" | "processing" | "done">("pick");
  const [doneDetail, setDoneDetail] = React.useState<string | null>(null);
  const cancelRef = React.useRef(false);

  React.useEffect(() => {
    if (open) {
      cancelRef.current = false;
      setFile(null);
      setTitle("");
      setError(null);
      setPhase("pick");
      setDoneDetail(null);
    }
    return () => {
      cancelRef.current = true; // closing stops polling; the job is server-side
    };
  }, [open]);

  const pick = (f: File | null) => {
    setError(null);
    if (!f) {
      setFile(null);
      return;
    }
    const v = validateUploadFile(f.name, f.size);
    if (!v.ok) {
      setFile(null);
      setError(v.error);
      return;
    }
    setFile(f);
    setTitle(f.name.replace(/\.[^.]+$/, ""));
  };

  const pollJob = async (jobId: string) => {
    for (let attempt = 0; ; attempt += 1) {
      if (cancelRef.current) return;
      await new Promise((r) => setTimeout(r, pollDelayMs(attempt)));
      if (cancelRef.current) return;
      const job = await api.get<UploadJob>(
        `/api/knowledge/v1/documents/upload/${jobId}`,
        { tenant: orgSlug },
      );
      const state = reduceJobStatus(job.status);
      if (state === "done") {
        setDoneDetail(
          job.chars !== null
            ? `${job.chars.toLocaleString()} characters extracted`
            : null,
        );
        setPhase("done");
        return;
      }
      if (state === "failed") {
        throw new Error(
          job.error ?? "The document pipeline marked this upload as failed.",
        );
      }
    }
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!file || phase === "uploading" || phase === "processing") return;
    setError(null);
    const mode = chooseUploadMode(file.size);
    setPhase("uploading");
    try {
      const contentBase64 = await readFileBase64(file);
      const resp = await api.post<
        { job_id?: string } & ({ id?: string; chunk_count?: number } | Record<string, never>)
      >(
        "/api/knowledge/v1/documents/upload",
        {
          tenant: orgSlug,
          filename: file.name,
          content_base64: contentBase64,
          title: title.trim() || undefined,
          wait: mode === "sync",
        },
        { tenant: orgSlug },
      );
      if (mode === "async" && resp.job_id) {
        setPhase("processing");
        await pollJob(resp.job_id);
      } else {
        const n = typeof resp.chunk_count === "number" ? resp.chunk_count : null;
        setDoneDetail(n !== null ? `indexed into ${n} chunks` : null);
        setPhase("done");
      }
    } catch (err) {
      setError(uploadErrorMessage(err));
      setPhase("pick");
    }
  };

  const busy = phase === "uploading" || phase === "processing";

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Upload a document</DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          Upload a PDF, Word, PowerPoint, Excel, HTML, Markdown or text file —
          or a scanned image. The document-intelligence pipeline extracts the
          text (layout-aware; tables become Markdown; scans go through OCR
          models) and indexes it into the knowledge base immediately.
        </p>

        {error ? <ErrorNote message={error} /> : null}

        {phase === "done" ? (
          <div className="space-y-4">
            <div className="rounded-md border border-border bg-accent px-3 py-2 text-sm">
              <p className="font-medium">
                Document indexed
                {doneDetail !== null ? ` — ${doneDetail}` : ""}
                . The receptionist can use it right away.
              </p>
            </div>
            <DialogFooter>
              <Button onClick={onDone}>Done</Button>
            </DialogFooter>
          </div>
        ) : (
          <form onSubmit={(e) => void submit(e)} className="space-y-4">
            <div className="grid gap-1.5">
              <Label htmlFor="doc-file">Document file</Label>
              <Input
                id="doc-file"
                type="file"
                accept={UPLOAD_EXTENSIONS.map((e) => `.${e}`).join(",")}
                disabled={busy}
                onChange={(e) => pick(e.target.files?.[0] ?? null)}
              />
              <p className="text-xs text-muted-foreground">
                {UPLOAD_EXTENSIONS.join(", ")} · up to{" "}
                {UPLOAD_MAX_BYTES / (1024 * 1024)} MB. Larger files are
                processed as a background job and indexed when ready.
              </p>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="doc-title">Title (optional)</Label>
              <Input
                id="doc-title"
                value={title}
                disabled={busy}
                onChange={(e) => setTitle(e.target.value)}
                placeholder="Defaults to the file name"
              />
            </div>
            {phase === "uploading" ? (
              <p className="rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                Uploading and extracting… the document pipeline runs on CPU and
                can take a minute or more per page. Don&apos;t close this dialog.
              </p>
            ) : null}
            {phase === "processing" ? (
              <p className="rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                Processing in the background… this large file runs as an upload
                job; the dialog polls until it is indexed. Closing the dialog
                does not cancel the job.
              </p>
            ) : null}
            <DialogFooter>
              <Button variant="outline" onClick={onClose} disabled={phase === "uploading"}>
                {phase === "processing" ? "Close (job continues)" : "Cancel"}
              </Button>
              <Button type="submit" disabled={!file || busy}>
                <FileUp className="h-4 w-4" />
                {busy ? "Uploading…" : "Upload &amp; index"}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
