"use client";

/**
 * 2026-11-26: Document checks (policy rules). Staff upload the business's policy; ocr-service has a language model
 * draft checks from it in a fixed vocabulary (fields the reader already extracts from pay stubs and receipts); staff
 * review each rule against the policy line it came from, untick any they don't want and turn them on. From then on
 * every document a caller shares (web chat, WhatsApp, call link) is checked against them, and the receptionist tells
 * the caller which rule was not met.
 */

import * as React from "react";
import { FileCheck, FileUp, Loader2, Power, Quote, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { ConfirmDialog, Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input, Label } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";

interface Rule {
  id: string;
  kind: "pay_stub" | "receipt";
  field: string;
  check: string;
  value: unknown;
  severity: "reject" | "review";
  text: string;
  source: string;
  enabled: boolean;
}

interface Policy {
  id: string;
  title: string;
  filename: string;
  status: "reading" | "draft" | "active" | "off" | "failed";
  rules: Rule[];
  error: string | null;
  pages: number;
  created_at: string;
  activated_at: string | null;
  activated_by: string | null;
}

const BASE = "/api/ocr/v1/ocr/policies";
const KIND_LABEL: Record<string, string> = { pay_stub: "Pay stubs", receipt: "Receipts" };
const STATUS: Record<Policy["status"], { label: string; variant: "success" | "warning" | "outline" | "destructive" }> = {
  reading: { label: "Reading…", variant: "outline" },
  draft: { label: "Ready to review", variant: "warning" },
  active: { label: "On", variant: "success" },
  off: { label: "Off", variant: "outline" },
  failed: { label: "Could not read", variant: "destructive" },
};

export function DocumentChecksClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [policies, setPolicies] = React.useState<Policy[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [uploading, setUploading] = React.useState(false);
  const [deleting, setDeleting] = React.useState<Policy | null>(null);

  const load = React.useCallback(async () => {
    try {
      const data = await api.get<{ items: Policy[] }>(BASE, { tenant: orgSlug });
      setPolicies(data.items ?? []);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load the document checks.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  // A policy being read: check again every few seconds until the rules are drafted.
  const reading = policies.some((p) => p.status === "reading");
  React.useEffect(() => {
    if (!reading) return;
    const t = setInterval(() => void load(), 4000);
    return () => clearInterval(t);
  }, [reading, load]);

  const save = async (p: Policy, status: "active" | "off", enabled?: string[]) => {
    try {
      await api.put(`${BASE}/${p.id}`, { status, enabled }, { tenant: orgSlug });
      toast({
        title: status === "active" ? "Checks turned on" : "Checks turned off",
        description:
          status === "active"
            ? "Every pay stub or receipt a caller shares is now checked against these rules."
            : "Documents are checked with the built-in checks only.",
        variant: "success",
      });
      await load();
    } catch (e) {
      toast({ title: "Could not save", description: e instanceof ApiError ? e.message : undefined, variant: "destructive" });
    }
  };

  const remove = async () => {
    if (!deleting) return;
    try {
      await api.delete(`${BASE}/${deleting.id}`, { tenant: orgSlug });
      setDeleting(null);
      await load();
    } catch (e) {
      toast({ title: "Delete failed", description: e instanceof ApiError ? e.message : undefined, variant: "destructive" });
    }
  };

  const active = policies.filter((p) => p.status === "active");
  const activeCount = active.reduce((n, p) => n + p.rules.filter((r) => r.enabled).length, 0);

  return (
    <div>
      <PageHeader
        title="Document checks"
        description="Your own rules for the pay stubs and receipts callers share, read from your policy document."
        actions={
          <Button size="sm" onClick={() => setUploading(true)}>
            <FileUp className="h-4 w-4" /> Upload policy
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card className="mb-6">
        <CardContent className="flex flex-wrap items-center gap-3 py-4 text-sm">
          <FileCheck className="h-5 w-5 text-muted-foreground" />
          {activeCount > 0 ? (
            <span>
              <strong>{activeCount}</strong> policy {activeCount === 1 ? "rule is" : "rules are"} checking every document
              callers share, on top of the built-in checks.
            </span>
          ) : (
            <span className="text-muted-foreground">
              No policy rules are on. Documents get the built-in checks only (required details, the figures add up).
            </span>
          )}
        </CardContent>
      </Card>

      {loading ? <p className="text-sm text-muted-foreground">Loading…</p> : null}
      {!loading && policies.length === 0 ? (
        <Card>
          <CardContent className="py-10 text-center text-sm text-muted-foreground">
            Upload your policy (PDF, Word or text). The rules it sets for documents — how recent a pay stub must be, a
            minimum income, the currency — are drafted for you to review before anything changes.
          </CardContent>
        </Card>
      ) : null}

      <div className="space-y-6">
        {policies.map((p) => (
          <PolicyCard key={p.id} policy={p} onSave={save} onDelete={() => setDeleting(p)} />
        ))}
      </div>

      <UploadDialog
        orgSlug={orgSlug}
        open={uploading}
        onClose={() => setUploading(false)}
        onUploaded={async () => {
          setUploading(false);
          await load();
        }}
      />
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
        title="Delete this policy?"
        description="Its rules stop applying to new documents. Documents already checked keep their result."
        confirmLabel="Delete"
        destructive
        onConfirm={remove}
      />
    </div>
  );
}

function PolicyCard({
  policy: p,
  onSave,
  onDelete,
}: {
  policy: Policy;
  onSave: (p: Policy, status: "active" | "off", enabled?: string[]) => Promise<void>;
  onDelete: () => void;
}) {
  const [chosen, setChosen] = React.useState<Set<string>>(() => new Set(p.rules.filter((r) => r.enabled).map((r) => r.id)));
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    setChosen(new Set(p.rules.filter((r) => r.enabled).map((r) => r.id)));
  }, [p.rules]);
  const editable = p.status === "draft" || p.status === "off";
  const st = STATUS[p.status];
  const kinds = Array.from(new Set(p.rules.map((r) => r.kind)));

  const act = async (status: "active" | "off") => {
    setBusy(true);
    await onSave(p, status, status === "active" ? Array.from(chosen) : undefined);
    setBusy(false);
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <CardTitle className="flex items-center gap-2">
              {p.title} <Badge variant={st.variant}>{st.label}</Badge>
            </CardTitle>
            <CardDescription>
              {p.filename} · uploaded {formatDateTime(p.created_at)}
              {p.status === "active" && p.activated_at ? ` · on since ${formatDateTime(p.activated_at)}` : ""}
            </CardDescription>
          </div>
          <div className="flex gap-2">
            {editable && p.rules.length > 0 ? (
              <Button size="sm" onClick={() => void act("active")} disabled={busy || chosen.size === 0}>
                <Power className="h-4 w-4" /> Turn on {chosen.size} {chosen.size === 1 ? "check" : "checks"}
              </Button>
            ) : null}
            {p.status === "active" ? (
              <Button size="sm" variant="outline" onClick={() => void act("off")} disabled={busy}>
                <Power className="h-4 w-4" /> Turn off
              </Button>
            ) : null}
            <Button size="sm" variant="ghost" className="text-destructive" onClick={onDelete} aria-label="Delete policy">
              <Trash2 className="h-4 w-4" />
            </Button>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        {p.status === "reading" ? (
          <p className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" /> Reading the policy and drafting checks — about a minute.
          </p>
        ) : null}
        {p.error ? <p className="text-sm text-muted-foreground">{p.error}</p> : null}
        {p.status === "draft" ? (
          <p className="mb-3 text-sm text-muted-foreground">
            Drafted from your policy. Check each rule against the line it came from, untick any you don&apos;t want, then
            turn them on. Nothing changes until you do.
          </p>
        ) : null}
        {kinds.map((kind) => (
          <div key={kind} className="mb-4 last:mb-0">
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              {KIND_LABEL[kind] ?? kind}
            </h3>
            <ul className="divide-y divide-border rounded-md border border-border">
              {p.rules
                .filter((r) => r.kind === kind)
                .map((r) => (
                  <li key={r.id} className="flex gap-3 px-3 py-3">
                    <input
                      type="checkbox"
                      className="mt-1 h-4 w-4 shrink-0"
                      aria-label={`Use: ${r.text}`}
                      checked={chosen.has(r.id)}
                      disabled={!editable}
                      onChange={(e) =>
                        setChosen((s) => {
                          const n = new Set(s);
                          if (e.target.checked) n.add(r.id);
                          else n.delete(r.id);
                          return n;
                        })
                      }
                    />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-2 text-sm font-medium">
                        {r.text}
                        <Badge variant={r.severity === "reject" ? "destructive" : "warning"}>
                          {r.severity === "reject" ? "Refuse the document" : "Send for review"}
                        </Badge>
                      </div>
                      {r.source ? (
                        <p className="mt-1 flex gap-1.5 text-xs italic text-muted-foreground">
                          <Quote className="mt-0.5 h-3 w-3 shrink-0" />
                          <span>{r.source}</span>
                        </p>
                      ) : null}
                    </div>
                  </li>
                ))}
            </ul>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

function UploadDialog({
  orgSlug,
  open,
  onClose,
  onUploaded,
}: {
  orgSlug: string;
  open: boolean;
  onClose: () => void;
  onUploaded: () => Promise<void>;
}) {
  const { toast } = useToast();
  const [file, setFile] = React.useState<File | null>(null);
  const [title, setTitle] = React.useState("");
  const [busy, setBusy] = React.useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!file) return;
    setBusy(true);
    try {
      const form = new FormData();
      form.append("file", file);
      if (title.trim()) form.append("title", title.trim());
      form.append("tenant_id", orgSlug);
      const res = await fetch(BASE, { method: "POST", body: form, headers: { "x-tenant-slug": orgSlug }, cache: "no-store" });
      if (!res.ok) {
        let msg = `Upload failed (${res.status})`;
        try {
          const body = await res.json();
          if (typeof body?.detail === "string") msg = body.detail;
        } catch {
          /* keep the status message */
        }
        throw new Error(msg);
      }
      setFile(null);
      setTitle("");
      await onUploaded();
    } catch (err) {
      toast({ title: "Upload failed", description: err instanceof Error ? err.message : undefined, variant: "destructive" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>Upload policy</DialogTitle>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <p className="text-sm text-muted-foreground">
              The policy is read and the rules it sets for pay stubs and receipts are drafted for you to review. They
              only apply once you turn them on.
            </p>
            <div className="space-y-1.5">
              <Label htmlFor="policy-check-file">Policy document (PDF, Word, .txt)</Label>
              <Input
                id="policy-check-file"
                type="file"
                accept=".pdf,.docx,.txt,.md"
                onChange={(e) => setFile(e.target.files?.[0] ?? null)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="policy-check-title">Title (optional)</Label>
              <Input
                id="policy-check-title"
                value={title}
                onChange={(e) => setTitle(e.target.value)}
                placeholder="e.g. Contribution evidence policy"
              />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={!file || busy}>
              {busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <FileUp className="h-4 w-4" />} Upload
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
