"use client";

import * as React from "react";
import Link from "next/link";
import { Mail, MessageCircle, Phone, PhoneCall, RefreshCw, Search, Wrench, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { describeToolLine, type NameLookup } from "@/lib/tool-lines";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, Label } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useNavLabels } from "@/components/terminology-provider";
import { cn, formatDateTime } from "@/lib/utils";
import { CallButton, PhoneCallDialog, type CallTarget } from "@/components/phone-call";

export interface ConversationSummary {
  id: string;
  site_slug: string;
  channel: string;
  contact_phone?: string | null;
  started_at: string;
  ended_at?: string | null;
  contact_name?: string | null;
  caller_phone?: string | null;
  /** 2026-11-23: the call's facts, the same ones the CRM note shows (conversation-service call_details). */
  call_details?: CallDetails | null;
}

export interface CallDetails {
  direction?: "inbound" | "outbound";
  purpose?: string;
  contact_name?: string;
  channel?: string;
  duration_s?: number;
  outcome?: string; // booked | rescheduled | cancelled | none, or an outbound call's recorded result (confirmed, ...)
  escalated?: boolean;
  confirmed_phone?: string;
  avg_sentiment?: number;
  whatsapp_sent?: boolean;
  summary?: string;
}

const PURPOSE_LABEL: Record<string, string> = {
  confirm_appointment: "Confirm appointment",
  waitlist_offer: "Waiting-list slot offer",
  no_show_rebook: "Rebook a missed appointment",
  callback: "Call back",
  general: "General call",
};

/** The same facts, in the same words, as the CRM call note (crm-sync twentyc.CallSummaryNote). */
function CallDetailsBox({ d }: { d: CallDetails }) {
  const length = typeof d.duration_s === "number"
    ? d.duration_s < 60 ? `${Math.round(d.duration_s)} s` : `${Math.floor(d.duration_s / 60)} min ${Math.round(d.duration_s % 60)} s`
    : null;
  const booking: Record<string, string> = { booked: "Yes", rescheduled: "Rescheduled", cancelled: "Cancelled", none: "No" };
  // 2026-11-23: an outbound call's recorded result (record_call_outcome), shown instead of "Appointment booked: No".
  const result: Record<string, string> = {
    confirmed: "Confirmed they will attend", declined: "Declined", resolved: "Request handled",
    call_back_later: "Asked us to call back later", wrong_person: "Reached someone else", no_answer: "No answer (voicemail)",
  };
  const outcomeKey = d.outcome ?? "none";
  const outcome = booking[outcomeKey] ?? result[outcomeKey] ?? "No";
  const mood = typeof d.avg_sentiment === "number"
    ? d.avg_sentiment >= 0.25 ? "Positive" : d.avg_sentiment <= -0.25 ? "Negative" : "Neutral"
    : null;
  const rows: [string, string][] = [];
  if (d.direction === "outbound") rows.push(["Call", `Outbound (we called) — ${PURPOSE_LABEL[d.purpose ?? "general"] ?? d.purpose}`]);
  else if (d.direction === "inbound") rows.push(["Call", "Inbound (they called)"]);
  if (length) rows.push(["Call length", length]);
  if (d.confirmed_phone) rows.push(["Phone confirmed", d.confirmed_phone]);
  rows.push([outcomeKey in result ? "Outcome" : "Appointment booked", outcome]);
  rows.push(["Passed to a person", d.escalated ? "Yes" : "No"]);
  if (d.whatsapp_sent) rows.push(["WhatsApp confirmation", "Sent"]);
  if (mood) rows.push(["Caller's mood", mood]);
  return (
    <div className="mb-4 rounded-md border border-border bg-muted/40 p-3 text-sm" aria-label="Call details">
      <p className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">Call details</p>
      {d.summary ? <p className="mb-2">{d.summary}</p> : null}
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
        {rows.map(([k, v]) => (
          <React.Fragment key={k}>
            <dt className="text-muted-foreground">{k}</dt>
            <dd>{v}</dd>
          </React.Fragment>
        ))}
      </dl>
    </div>
  );
}

/** Who the conversation was with: the stored contact marker, else the name/phone the caller gave when booking. */
function contributorOf(c: ConversationSummary): string | null {
  const phone = c.contact_phone || c.caller_phone || null;
  if (c.contact_name && phone) return `${c.contact_name} · ${phone}`;
  return c.contact_name || phone;
}

interface Turn {
  id: string;
  seq: number;
  role: "user" | "agent" | "system" | "tool" | string;
  text: string;
  created_at?: string;
  sentiment?: number | null;
  /** English translation of a Pidgin/Yoruba/Hausa/Igbo line ("" = English already; null = not translated yet). */
  translation_en?: string | null;
}

const CHANNEL: Record<string, { label: string; icon: typeof MessageCircle }> = {
  chat: { label: "Web chat", icon: MessageCircle },
  voice: { label: "Voice call", icon: PhoneCall },
  phone: { label: "Phone call", icon: Phone },
  whatsapp: { label: "WhatsApp", icon: MessageCircle },
  telegram: { label: "Telegram", icon: MessageCircle },
  email: { label: "E-mail", icon: Mail },
  ussd: { label: "USSD", icon: Phone },
  api: { label: "API", icon: MessageCircle },
};

function channelOf(c: string) {
  return CHANNEL[c] ?? { label: c, icon: MessageCircle };
}

export function ConversationsClient({
  orgSlug,
  contact,
  initial,
  initialError,
}: {
  orgSlug: string;
  contact: string | null;
  initial: ConversationSummary[];
  initialError: string | null;
}) {
  const labels = useNavLabels();
  const [rows, setRows] = React.useState(initial);
  const [error, setError] = React.useState(initialError);
  const [loading, setLoading] = React.useState(false);
  const [channel, setChannel] = React.useState("");
  const [open, setOpen] = React.useState<ConversationSummary | null>(null);
  const [callTarget, setCallTarget] = React.useState<CallTarget | null>(null); // 2026-11-22: "Call back"
  const [turns, setTurns] = React.useState<Turn[] | null>(null);
  const [turnsError, setTurnsError] = React.useState<string | null>(null);
  const [translating, setTranslating] = React.useState(false);
  // Service and advisor names, so the receptionist's actions read as plain sentences.
  const [names, setNames] = React.useState<NameLookup>({ offerings: {}, team: {} });
  React.useEffect(() => {
    const byId = (rows: unknown): Record<string, string> => {
      const list = Array.isArray(rows) ? rows : Object.values((rows as Record<string, unknown>) ?? {}).find(Array.isArray) ?? [];
      return Object.fromEntries((list as { id?: string; name?: string }[]).filter((r) => r.id && r.name).map((r) => [r.id as string, r.name as string]));
    };
    void Promise.allSettled([
      api.get<unknown>("/api/bookings/v1/offerings", { tenant: orgSlug }),
      api.get<unknown>("/api/bookings/v1/team-members", { tenant: orgSlug }),
    ]).then(([o, t]) =>
      setNames({
        offerings: o.status === "fulfilled" ? byId(o.value) : {},
        team: t.status === "fulfilled" ? byId(t.value) : {},
      }),
    );
  }, [orgSlug]);

  const refresh = React.useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.get<{ conversations?: ConversationSummary[] }>("/api/conversations/v1/conversations", {
        tenant: orgSlug,
        limit: 200, // the API maximum: the channel filter works on what is loaded (100 hid older WhatsApp chats)
        ...(contact ? { contact } : {}),
      });
      setRows(data.conversations ?? []);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load conversations.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug, contact]);

  const show = React.useCallback(
    async (c: ConversationSummary) => {
      setOpen(c);
      setTurns(null);
      setTurnsError(null);
      try {
        const detail = await api.get<{ turns?: Turn[] }>(`/api/conversations/v1/conversations/${c.id}`, { tenant: orgSlug });
        const loaded = [...(detail.turns ?? [])].sort((a, b) => a.seq - b.seq);
        setTurns(loaded);
        // Lines not yet checked for language get an English translation (once; stored by the conversation service).
        if (loaded.some((t) => (t.role === "user" || t.role === "agent") && t.translation_en == null && t.text?.trim())) {
          setTranslating(true);
          try {
            const tr = await api.post<{ turns?: Turn[] }>(`/api/conversations/v1/conversations/${c.id}/translate`, undefined, { tenant: orgSlug });
            setTurns([...(tr.turns ?? loaded)].sort((a, b) => a.seq - b.seq));
          } catch {
            // the transcript still shows; translations come next time
          } finally {
            setTranslating(false);
          }
        }
      } catch (e) {
        setTurnsError(e instanceof ApiError ? e.message : "Failed to load the transcript.");
      }
    },
    [orgSlug],
  );

  const visible = channel ? rows.filter((r) => r.channel === channel) : rows;
  const customerWord = labels.contacts.replace(/s$/, "").toLowerCase() || "customer";

  return (
    <div>
      <PageHeader
        title="Conversations"
        description={
          contact
            ? `Every chat and call with ${contact}.`
            : `Every web chat and voice call your receptionist handled, with the full transcript: what each ${customerWord} asked, what it answered and what it did.`
        }
        actions={
          <div className="flex gap-2">
            <Link href={`/app/${orgSlug}/conversations/search`}>
              <Button variant="outline">
                <Search className="h-4 w-4" /> Search transcripts
              </Button>
            </Link>
            <Button variant="outline" onClick={() => void refresh()} disabled={loading}>
              <RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} /> Refresh
            </Button>
          </div>
        }
      />
      {error ? <ErrorNote message={error} /> : null}
      <div className="mb-4 flex flex-wrap items-end gap-4">
        <div className="w-48">
          <Label htmlFor="conv-channel">Channel</Label>
          <Select id="conv-channel" value={channel} onChange={(e) => setChannel(e.target.value)}>
            <option value="">All</option>
            <option value="chat">Web chat</option>
            <option value="voice">Voice call</option>
            <option value="phone">Phone call</option>
            <option value="whatsapp">WhatsApp</option>
            <option value="telegram">Telegram</option>
            <option value="email">E-mail</option>
          </Select>
        </div>
        {contact ? (
          <Link href={`/app/${orgSlug}/conversations`} className="pb-2 text-sm underline">
            Show everyone
          </Link>
        ) : null}
      </div>
      <div className={cn("grid gap-4", open ? "lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]" : "")}>
        <Card>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-5">When</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead>{labels.contacts.replace(/s$/, "")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.length === 0 ? (
                <TableEmpty colSpan={3}>No conversations yet. They appear here as customers chat or call.</TableEmpty>
              ) : (
                visible.map((c) => {
                  const ch = channelOf(c.channel);
                  const Icon = ch.icon;
                  return (
                    <TableRow
                      key={c.id}
                      className={cn("cursor-pointer", open?.id === c.id && "bg-muted")}
                      onClick={() => void show(c)}
                    >
                      <TableCell className="pl-5">{formatDateTime(c.started_at)}</TableCell>
                      <TableCell>
                        <span className="inline-flex items-center gap-1.5">
                          <Icon className="h-4 w-4" /> {ch.label}
                        </span>
                      </TableCell>
                      <TableCell>{contributorOf(c) || <span className="text-muted-foreground">Not given</span>}</TableCell>
                    </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
        </Card>
        {open ? (
          <Card>
            <CardHeader className="flex flex-row items-start justify-between gap-2">
              <div>
                <CardTitle>{channelOf(open.channel).label}</CardTitle>
                <p className="text-sm text-muted-foreground">
                  {formatDateTime(open.started_at)}
                  {contributorOf(open) ? ` · ${contributorOf(open)}` : ""}
                </p>
              </div>
              <div className="flex items-center gap-1">
                {open.contact_phone || open.caller_phone ? (
                  <CallButton
                    purpose="callback"
                    onClick={() =>
                      setCallTarget({ purpose: "callback", phone: (open.contact_phone || open.caller_phone) as string, name: open.contact_name })
                    }
                  />
                ) : null}
                <Button variant="ghost" size="sm" aria-label="Close transcript" onClick={() => setOpen(null)}>
                  <X className="h-4 w-4" />
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              {open.call_details ? <CallDetailsBox d={open.call_details} /> : null}
              {turnsError ? <ErrorNote message={turnsError} /> : null}
              {turns === null && !turnsError ? <p className="text-sm text-muted-foreground">Loading the transcript…</p> : null}
              {translating ? <p className="text-xs text-muted-foreground">Translating into English…</p> : null}
              {turns && turns.length === 0 ? <p className="text-sm text-muted-foreground">No messages recorded.</p> : null}
              <ol className="space-y-2" aria-label="Transcript">
                {(turns ?? []).map((t) =>
                  t.role === "tool" || t.role === "system" ? (
                    <li key={t.id} className="flex items-center gap-2 text-xs text-muted-foreground">
                      <Wrench className="h-3 w-3" /> <span>{t.role === "tool" ? describeToolLine(t.text, names) : t.text}</span>
                    </li>
                  ) : (
                    <li key={t.id} className={cn("flex", t.role === "agent" ? "justify-end" : "justify-start")}>
                      <div
                        className={cn(
                          "max-w-[85%] rounded-lg px-3 py-2 text-sm",
                          t.role === "agent" ? "bg-primary text-primary-foreground" : "bg-muted",
                        )}
                      >
                        <div className="mb-0.5 text-[11px] opacity-70">
                          {t.role === "agent" ? "Receptionist" : titleCaseWord(customerWord)}
                          {typeof t.sentiment === "number" ? (
                            <Badge variant={t.sentiment < -0.2 ? "warning" : "outline"} className="ml-2 align-middle">
                              {t.sentiment < -0.2 ? "upset" : t.sentiment > 0.2 ? "positive" : "neutral"}
                            </Badge>
                          ) : null}
                        </div>
                        {t.text}
                        {t.translation_en ? (
                          <div className="mt-1.5 border-t border-current/20 pt-1.5 text-[12px] italic opacity-85" data-translation>
                            <span className="not-italic font-semibold">English: </span>
                            {t.translation_en}
                          </div>
                        ) : null}
                      </div>
                    </li>
                  ),
                )}
              </ol>
            </CardContent>
          </Card>
        ) : null}
      </div>
      <PhoneCallDialog orgSlug={orgSlug} target={callTarget} onClose={() => setCallTarget(null)} />
    </div>
  );
}

function titleCaseWord(w: string) {
  return w.charAt(0).toUpperCase() + w.slice(1);
}
