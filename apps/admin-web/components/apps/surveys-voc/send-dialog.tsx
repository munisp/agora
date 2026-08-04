"use client";

/**
 * Send-invites dialog (SPEC-W20 Agent B): paste contact ids (one per line
 * or comma-separated), submit, then an honest result summary — invites
 * created/sent/queued, skipped contacts with reasons, and the operator
 * links (the token IS the public respond capability, so links are shown
 * to the operator only).
 */
import * as React from "react";
import { Button } from "@/components/ui/button";
import { Label, Textarea } from "@/components/ui/input";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import type { SendResult, Survey } from "./types";
import { shortId } from "./types";

const UUID_RE =
  /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

export function SendDialog({
  survey,
  open,
  busy,
  result,
  onOpenChange,
  onSend,
}: {
  survey: Survey | null;
  open: boolean;
  busy: boolean;
  /** last send result for this survey (shown after a successful send) */
  result: SendResult | null;
  onOpenChange: (open: boolean) => void;
  onSend: (contactIds: string[]) => Promise<void>;
}) {
  const [raw, setRaw] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (open) {
      setRaw("");
      setError(null);
    }
  }, [open]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    const parts = raw
      .split(/[\s,]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    const invalid = parts.filter((p) => !UUID_RE.test(p));
    if (invalid.length > 0) {
      setError(`Not contact ids: ${invalid.slice(0, 3).join(", ")}${invalid.length > 3 ? "…" : ""}`);
      return;
    }
    const ids = [...new Set(parts.map((p) => p.toLowerCase()))];
    if (ids.length === 0) {
      setError("Paste at least one contact id.");
      return;
    }
    if (ids.length > 500) {
      setError("At most 500 contacts per send.");
      return;
    }
    await onSend(ids);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg" onClose={() => onOpenChange(false)}>
        <DialogHeader>
          <DialogTitle>Send invites — {survey?.name}</DialogTitle>
          <DialogDescription>
            One invite per contact over {survey?.channel === "sms" ? "SMS" : "marketing push"}.
            Marketing sends are DND/quiet-hours guarded automatically.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="contact-ids">Contact ids (one per line or comma-separated)</Label>
            <Textarea
              id="contact-ids"
              rows={5}
              value={raw}
              onChange={(e) => setRaw(e.target.value)}
              placeholder="6f9619ff-8b86-d011-b42d-00c04fc964ff"
              className="font-mono text-xs"
            />
          </div>
          {error ? <p className="text-sm text-destructive">{error}</p> : null}
          <div className="flex items-center gap-2">
            <Button type="submit" disabled={busy}>
              {busy ? "Sending…" : "Send invites"}
            </Button>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Close
            </Button>
          </div>
        </form>

        {result ? (
          <div className="mt-2 rounded-md border border-border p-3 text-sm space-y-2">
            <p>
              <span className="font-medium">{result.invites_created}</span> invites
              created · <span className="font-medium">{result.sent}</span> sent ·{" "}
              <span className="font-medium">{result.queued}</span> queued
              {result.sends_deferred ? " (notifications topic disabled — sends deferred)" : ""}
            </p>
            {result.skipped.length > 0 ? (
              <p className="text-muted-foreground">
                Skipped:{" "}
                {result.skipped
                  .map((s) => `${shortId(s.contact_id)} (${s.reason.replace("_", " ")})`)
                  .join(", ")}
              </p>
            ) : null}
            {result.invites.length > 0 ? (
              <details>
                <summary className="cursor-pointer text-muted-foreground">
                  Operator links ({result.invites.length})
                </summary>
                <ul className="mt-1 max-h-40 space-y-1 overflow-auto font-mono text-xs">
                  {result.invites.map((inv) => (
                    <li key={inv.id} className="break-all">
                      {inv.link}{" "}
                      <span className="text-muted-foreground">[{inv.status}]</span>
                    </li>
                  ))}
                </ul>
              </details>
            ) : null}
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
