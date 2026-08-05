"use client";

import * as React from "react";
import { Check, Plus, Search, Sparkles, Trash2, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
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

export function KnowledgeClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [docs, setDocs] = React.useState<KnowledgeDocument[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [adding, setAdding] = React.useState(false);
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
      setDocs(Array.isArray(data) ? data : (data.items ?? []));
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
      setHits(Array.isArray(data) ? data : (data.items ?? []));
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
          <Button size="sm" onClick={() => setAdding(true)}>
            <Plus className="h-4 w-4" /> Add document
          </Button>
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
        onClose={() => setAdding(false)}
        onAdd={add}
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
  onClose,
  onAdd,
}: {
  open: boolean;
  busy: boolean;
  onClose: () => void;
  onAdd: (form: { title: string; body: string; source_url: string }) => void;
}) {
  const [title, setTitle] = React.useState("");
  const [body, setBody] = React.useState("");
  const [sourceUrl, setSourceUrl] = React.useState("");

  React.useEffect(() => {
    if (open) {
      setTitle("");
      setBody("");
      setSourceUrl("");
    }
  }, [open]);

  const valid = title.trim().length > 0 && body.trim().length > 0;

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Add document</DialogTitle>
        </DialogHeader>
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
      setSuggestions(Array.isArray(data) ? data : (data.items ?? []));
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
