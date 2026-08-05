"use client";

import * as React from "react";
import { Mic, Plus, Save, Send, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";
import type { ChatResponse, Tenant } from "@/lib/types";

const AGENT_TOOLS = [
  "get_business_info",
  "get_availability",
  "book_appointment",
  "lookup_appointment",
  "reschedule_appointment",
  "cancel_appointment",
] as const;

interface TermRow {
  key: string;
  value: string;
}

interface ChatTurn {
  role: "user" | "agent";
  text: string;
}

export function VoiceAgentClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [tenant, setTenant] = React.useState<Tenant | null>(null);
  const [rows, setRows] = React.useState<TermRow[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);

  const [conversationId, setConversationId] = React.useState<string | undefined>();
  const [turns, setTurns] = React.useState<ChatTurn[]>([]);
  const [draft, setDraft] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const scrollRef = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    (async () => {
      try {
        const t = await api.get<Tenant>(`/api/identity/v1/tenants/${orgSlug}`);
        setTenant(t);
        const entries = Object.entries(t.terminology ?? {});
        setRows(
          entries.length > 0
            ? entries.map(([key, value]) => ({ key, value }))
            : [{ key: "appointment", value: "appointment" }],
        );
      } catch (e) {
        setError(e instanceof ApiError ? e.message : "Failed to load tenant config.");
      }
    })();
  }, [orgSlug]);

  React.useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [turns]);

  const setRow = (i: number, patch: Partial<TermRow>) =>
    setRows((r) => r.map((row, j) => (j === i ? { ...row, ...patch } : row)));

  const saveTerminology = async () => {
    setSaving(true);
    const terminology: Record<string, string> = {};
    for (const { key, value } of rows) {
      if (key.trim() && value.trim()) terminology[key.trim()] = value.trim();
    }
    try {
      const updated = await api.patch<Tenant>(
        `/api/identity/v1/tenants/${orgSlug}`,
        { terminology },
      );
      setTenant(updated);
      toast({
        title: "Terminology saved",
        description: "The agent picks this up on its next session.",
        variant: "success",
      });
    } catch (e) {
      toast({
        title: "Save failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setSaving(false);
    }
  };

  const send = async (e: React.FormEvent) => {
    e.preventDefault();
    const message = draft.trim();
    if (!message || sending) return;
    setDraft("");
    setTurns((t) => [...t, { role: "user", text: message }]);
    setSending(true);
    try {
      const res = await api.post<ChatResponse>("/voice/chat", {
        tenant: orgSlug,
        message,
        conversation_id: conversationId,
      });
      setConversationId(res.conversation_id);
      setTurns((t) => [...t, { role: "agent", text: res.reply }]);
    } catch (err) {
      setTurns((t) => [
        ...t,
        {
          role: "agent",
          text: `⚠ ${err instanceof ApiError ? err.message : "The voice service is unreachable."}`,
        },
      ]);
    } finally {
      setSending(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Voice agent"
        description="How your AI receptionist speaks and what it calls things — plus a live test console."
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="grid gap-6 xl:grid-cols-2">
        <div className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Mic className="h-4 w-4" /> Agent configuration
              </CardTitle>
              <CardDescription>
                Resolved from the tenant record at session start.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm">
                <div>
                  <dt className="text-muted-foreground">Business</dt>
                  <dd className="font-medium">{tenant?.name ?? orgSlug}</dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Timezone</dt>
                  <dd className="font-medium">{tenant?.timezone ?? "—"}</dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Locale</dt>
                  <dd className="font-medium">{tenant?.locale ?? "—"}</dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Currency</dt>
                  <dd className="font-medium">{tenant?.currency ?? "—"}</dd>
                </div>
                <div className="col-span-2">
                  <dt className="text-muted-foreground">Voice pipeline</dt>
                  <dd className="font-medium">
                    silero VAD · faster-whisper STT · Ollama llama3.1:8b · Piper TTS
                  </dd>
                </div>
              </dl>
              <div className="mt-4">
                <p className="mb-2 text-sm text-muted-foreground">
                  Tools available to the agent
                </p>
                <div className="flex flex-wrap gap-1.5">
                  {AGENT_TOOLS.map((t) => (
                    <Badge key={t} variant="secondary" className="font-mono text-[11px]">
                      {t}
                    </Badge>
                  ))}
                </div>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Terminology</CardTitle>
              <CardDescription>
                Map generic words to your vocabulary — e.g. “appointment” →
                “session”. Applied to prompts and the public site.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="space-y-2">
                {rows.map((row, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Input
                      aria-label="Generic term"
                      value={row.key}
                      onChange={(e) => setRow(i, { key: e.target.value })}
                      placeholder="appointment"
                    />
                    <span className="text-muted-foreground">→</span>
                    <Input
                      aria-label="Your term"
                      value={row.value}
                      onChange={(e) => setRow(i, { value: e.target.value })}
                      placeholder="session"
                    />
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label="Remove row"
                      onClick={() => setRows((r) => r.filter((_, j) => j !== i))}
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  </div>
                ))}
              </div>
              <div className="mt-4 flex items-center justify-between">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setRows((r) => [...r, { key: "", value: "" }])}
                >
                  <Plus className="h-4 w-4" /> Add mapping
                </Button>
                <Button size="sm" onClick={() => void saveTerminology()} disabled={saving}>
                  <Save className="h-4 w-4" />
                  {saving ? "Saving…" : "Save terminology"}
                </Button>
              </div>
            </CardContent>
          </Card>
        </div>

        <Card className="flex h-[36rem] flex-col">
          <CardHeader>
            <CardTitle>Test console</CardTitle>
            <CardDescription>
              Text chat with the same agent that answers the phone (
              <span className="font-mono text-xs">POST /voice/chat</span>).
              {conversationId ? (
                <span className="ml-1 font-mono text-xs">
                  conv {conversationId.slice(0, 8)}…
                </span>
              ) : null}
            </CardDescription>
          </CardHeader>
          <CardContent className="flex min-h-0 flex-1 flex-col">
            <div
              ref={scrollRef}
              className="min-h-0 flex-1 space-y-3 overflow-y-auto rounded-md border border-border bg-muted/30 p-3"
            >
              {turns.length === 0 ? (
                <p className="py-8 text-center text-sm text-muted-foreground">
                  Try: “What are your opening hours?” or “Book a consultation
                  tomorrow at 10.”
                </p>
              ) : (
                turns.map((t, i) => (
                  <div
                    key={i}
                    className={cn(
                      "max-w-[85%] rounded-lg px-3 py-2 text-sm",
                      t.role === "user"
                        ? "ml-auto bg-primary text-primary-foreground"
                        : "bg-card border border-border",
                    )}
                  >
                    {t.text}
                  </div>
                ))
              )}
              {sending ? (
                <div className="max-w-[85%] rounded-lg border border-border bg-card px-3 py-2 text-sm text-muted-foreground">
                  Thinking…
                </div>
              ) : null}
            </div>
            <form onSubmit={send} className="mt-3 flex gap-2">
              <Input
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                placeholder="Message the receptionist…"
              />
              <Button type="submit" disabled={sending || !draft.trim()}>
                <Send className="h-4 w-4" />
              </Button>
            </form>
            <Label className="mt-2 block text-xs font-normal text-muted-foreground">
              Mutations require phone-number confirmation, exactly like the
              voice channel.
            </Label>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
