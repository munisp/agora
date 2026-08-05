"use client";

import * as React from "react";
import { MessageCircle, Send, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { cn } from "@/lib/utils";
import type { ChatResponse } from "@/lib/types";

interface Turn {
  role: "user" | "agent";
  text: string;
}

interface ChatResult {
  reply: string;
  conversationId?: string;
}

/**
 * Streaming chat (I13): POST /voice/chat with stream:true returns
 * text/event-stream frames `data: {"delta": "..."}` … `data: {"done": true}`.
 * Returns null when the runtime does not honour streaming (older deploys or
 * the request was rejected), so the caller can fall back to request/response.
 */
async function sendStreaming(
  payload: Record<string, unknown>,
  onDelta: (text: string) => void,
): Promise<ChatResult | null> {
  let res: Response;
  try {
    res = await fetch("/voice/chat", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        accept: "text/event-stream",
      },
      body: JSON.stringify({ ...payload, stream: true }),
      cache: "no-store",
    });
  } catch {
    return null;
  }
  const contentType = res.headers.get("content-type") ?? "";
  if (!res.ok || !contentType.includes("text/event-stream") || !res.body) {
    return null;
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let reply = "";
  let conversationId: string | undefined;
  let sawDone = false;

  const handleEvent = (data: string) => {
    if (!data || data === "[DONE]") return;
    try {
      const evt = JSON.parse(data) as {
        delta?: string;
        done?: boolean;
        reply?: string;
        conversation_id?: string;
      };
      if (typeof evt.conversation_id === "string") {
        conversationId = evt.conversation_id;
      }
      if (typeof evt.delta === "string") {
        reply += evt.delta;
        onDelta(reply);
      }
      if (typeof evt.reply === "string" && !reply) {
        reply = evt.reply;
        onDelta(reply);
      }
      if (evt.done) sawDone = true;
    } catch {
      // ignore malformed frames
    }
  };

  // Line-oriented SSE parse: frames are `data: <json>` separated by blank lines.
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split("\n");
    buffer = lines.pop() ?? "";
    for (const line of lines) {
      const trimmed = line.trim();
      if (trimmed.startsWith("data:")) handleEvent(trimmed.slice(5).trim());
    }
  }
  if (buffer.trim().startsWith("data:")) {
    handleEvent(buffer.trim().slice(5).trim());
  }
  if (!reply && !sawDone) return null;
  return { reply, conversationId };
}

/**
 * Bottom-right chat widget for public booking pages. Talks to the same
 * receptionist as the voice channel via POST /voice/chat (through APISIX,
 * same-origin via the Next.js /voice/* rewrite). Streams tokens over SSE when
 * the voice runtime supports it, with a non-streaming fallback.
 */
export function ChatWidget({
  tenant,
  siteSlug,
  accent = "#7c5b3e",
}: {
  tenant: string;
  siteSlug: string;
  accent?: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [turns, setTurns] = React.useState<Turn[]>([]);
  const [draft, setDraft] = React.useState("");
  const [conversationId, setConversationId] = React.useState<string | undefined>();
  const [sending, setSending] = React.useState(false);
  const scrollRef = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [turns, open]);

  const send = async (e: React.FormEvent) => {
    e.preventDefault();
    const message = draft.trim();
    if (!message || sending) return;
    setDraft("");
    setTurns((t) => [...t, { role: "user", text: message }]);
    setSending(true);
    const payload = {
      tenant,
      site_slug: siteSlug,
      message,
      conversation_id: conversationId,
    };
    try {
      // 1. Streaming path: append an empty agent turn and fill it as deltas arrive.
      let streamedIdx = -1;
      const streamed = await sendStreaming(payload, (text) => {
        setTurns((t) => {
          if (streamedIdx === -1) {
            streamedIdx = t.length;
            return [...t, { role: "agent", text }];
          }
          const next = t.slice();
          next[streamedIdx] = { role: "agent", text };
          return next;
        });
      });

      if (streamed) {
        if (streamed.conversationId) setConversationId(streamed.conversationId);
        if (!streamed.reply) {
          // Stream ended without content — drop the placeholder turn.
          setTurns((t) => t.filter((_, i) => i !== streamedIdx));
          throw new Error("empty stream");
        }
        return;
      }

      // 2. Fallback: classic request/response.
      const res = await api.post<ChatResponse>("/voice/chat", payload);
      setConversationId(res.conversation_id);
      setTurns((t) => [...t, { role: "agent", text: res.reply }]);
    } catch (err) {
      setTurns((t) => [
        ...t,
        {
          role: "agent",
          text: `Sorry, I can't respond right now (${err instanceof ApiError ? err.message : "service unreachable"}). Please use the booking form.`,
        },
      ]);
    } finally {
      setSending(false);
    }
  };

  return (
    <div className="fixed bottom-4 right-4 z-50 flex flex-col items-end gap-3">
      {open ? (
        <div className="flex h-96 w-80 flex-col overflow-hidden rounded-xl border border-border bg-card shadow-xl">
          <div
            className="flex items-center justify-between px-4 py-3 text-white"
            style={{ backgroundColor: accent }}
          >
            <div>
              <p className="text-sm font-semibold">Receptionist</p>
              <p className="text-xs opacity-80">
                Ask anything — I can book for you too
              </p>
            </div>
            <button
              onClick={() => setOpen(false)}
              aria-label="Close chat"
              className="rounded p-1 hover:bg-white/20 cursor-pointer"
            >
              <X className="h-4 w-4" />
            </button>
          </div>
          <div
            ref={scrollRef}
            className="min-h-0 flex-1 space-y-2 overflow-y-auto bg-muted/30 p-3"
          >
            {turns.length === 0 ? (
              <p className="py-6 text-center text-xs text-muted-foreground">
                Hi! Ask me about services, hours, or say “book me in Friday
                morning”.
              </p>
            ) : (
              turns.map((t, i) => (
                <div
                  key={i}
                  className={cn(
                    "max-w-[85%] rounded-lg px-3 py-2 text-sm",
                    t.role === "user"
                      ? "ml-auto text-white"
                      : "border border-border bg-card",
                  )}
                  style={
                    t.role === "user" ? { backgroundColor: accent } : undefined
                  }
                >
                  {t.text}
                </div>
              ))
            )}
            {sending ? (
              <div className="max-w-[85%] rounded-lg border border-border bg-card px-3 py-2 text-sm text-muted-foreground">
                Typing…
              </div>
            ) : null}
          </div>
          <form onSubmit={send} className="flex gap-2 border-t border-border p-2">
            <input
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="Type a message…"
              className="h-9 flex-1 rounded-md border border-input bg-card px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
            <button
              type="submit"
              disabled={sending || !draft.trim()}
              aria-label="Send"
              className="flex h-9 w-9 items-center justify-center rounded-md text-white disabled:opacity-50 cursor-pointer"
              style={{ backgroundColor: accent }}
            >
              <Send className="h-4 w-4" />
            </button>
          </form>
        </div>
      ) : null}
      <button
        onClick={() => setOpen((o) => !o)}
        aria-label={open ? "Close chat" : "Open chat"}
        className="flex h-12 w-12 items-center justify-center rounded-full text-white shadow-lg transition-transform hover:scale-105 cursor-pointer"
        style={{ backgroundColor: accent }}
      >
        <MessageCircle className="h-5 w-5" />
      </button>
    </div>
  );
}
