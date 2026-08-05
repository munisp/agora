"use client";

/**
 * SPEC-W28 WS-C: Ask box — natural-language questions over the tenant graph.
 *
 * POST /v1/graph/ask {question} → graph-service runs NL→Cypher via the local
 * Ollama model (schema-prompted, read-only templates, tenant filter injected,
 * result capped — SPEC-W28 §4 WS-B) and answers with rows + the generated
 * Cypher. Rows render as a table; the generated Cypher sits behind a
 * collapsible disclosure for transparency.
 *
 * Degradation: when Ollama (or the graph store) is down the service answers
 * 503 and the box shows an explicit "assistant offline" state instead of a
 * generic failure.
 */
import * as React from "react";
import { ChevronDown, Loader2, Sparkles } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { GRAPH_API, unwrapList, type AskAnswer } from "./types";

const SUGGESTIONS = [
  "How many people consented to marketing this year?",
  "Which LGA has the most lapsed customers?",
  "Who referred the most new customers?",
];

const MAX_ROWS = 50;

type AskState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "ok"; answer: AskAnswer }
  | { status: "error"; message: string; offline: boolean };

export function AskBox({ orgSlug }: { orgSlug: string }) {
  const [question, setQuestion] = React.useState("");
  const [state, setState] = React.useState<AskState>({ status: "idle" });
  const [showCypher, setShowCypher] = React.useState(false);

  async function ask(q: string) {
    const trimmed = q.trim();
    if (!trimmed) return;
    setQuestion(trimmed);
    setState({ status: "loading" });
    setShowCypher(false);
    try {
      const data = await api.post<unknown>(
        `${GRAPH_API}/ask`,
        { question: trimmed },
        { tenant: orgSlug },
      );
      setState({ status: "ok", answer: normalizeAsk(data) });
    } catch (e) {
      const offline =
        e instanceof ApiError && (e.status === 503 || e.status === 502);
      setState({
        status: "error",
        offline,
        message: offline
          ? "The graph assistant is offline (the local model or graph store is unavailable). Try again in a moment."
          : e instanceof ApiError
            ? e.message
            : "The question could not be answered right now.",
      });
    }
  }

  const answer = state.status === "ok" ? state.answer : null;
  const columns = answer?.rows.length ? Object.keys(answer.rows[0]) : [];

  return (
    <Card>
      <CardHeader>
        <CardTitle>Ask your customer graph</CardTitle>
        <CardDescription>
          Ask a question in plain language — a local model turns it into a
          read-only graph query over your tenant&apos;s data only.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            void ask(question);
          }}
        >
          <Input
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            placeholder="Ask about customers, consents, bookings, referrals…"
            maxLength={500}
            aria-label="Question for the customer graph"
          />
          <Button type="submit" disabled={state.status === "loading" || question.trim() === ""}>
            {state.status === "loading" ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <Sparkles className="h-4 w-4" />
            )}
            Ask
          </Button>
        </form>

        <div className="flex flex-wrap gap-2">
          {SUGGESTIONS.map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => void ask(s)}
              disabled={state.status === "loading"}
              className="rounded-full border border-border bg-card px-3 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-50 cursor-pointer"
            >
              {s}
            </button>
          ))}
        </div>

        {state.status === "error" ? (
          <div
            role="alert"
            className={cn(
              "rounded-md border px-3 py-2 text-sm",
              state.offline
                ? "border-border bg-warning-soft text-warning"
                : "border-border bg-danger-soft text-destructive",
            )}
          >
            {state.message}
          </div>
        ) : null}

        {answer ? (
          <div className="space-y-3" aria-live="polite">
            {answer.summary ? (
              <p className="text-sm text-muted-foreground">{answer.summary}</p>
            ) : null}
            {answer.rows.length === 0 ? (
              <p className="py-4 text-center text-sm text-muted-foreground">
                The query returned no rows.
              </p>
            ) : (
              <div className="overflow-x-auto rounded-md border border-border">
                <Table>
                  <TableHeader>
                    <TableRow>
                      {columns.map((c) => (
                        <TableHead key={c}>{c}</TableHead>
                      ))}
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {answer.rows.length === 0 ? (
                      <TableEmpty colSpan={Math.max(columns.length, 1)}>
                        No rows.
                      </TableEmpty>
                    ) : (
                      answer.rows.slice(0, MAX_ROWS).map((row, i) => (
                        <TableRow key={i}>
                          {columns.map((c) => (
                            <TableCell key={c} className="text-sm">
                              {formatCell(row[c])}
                            </TableCell>
                          ))}
                        </TableRow>
                      ))
                    )}
                  </TableBody>
                </Table>
                {answer.rows.length > MAX_ROWS ? (
                  <p className="border-t border-border px-3 py-2 text-xs text-muted-foreground">
                    Showing {MAX_ROWS} of {answer.rows.length} rows.
                  </p>
                ) : null}
              </div>
            )}
            {answer.cypher ? (
              <div className="rounded-md border border-border bg-card px-3 py-2 text-xs">
                <button
                  type="button"
                  onClick={() => setShowCypher((v) => !v)}
                  className="flex w-full items-center justify-between font-medium text-muted-foreground cursor-pointer"
                  aria-expanded={showCypher}
                >
                  Generated graph query (read-only)
                  <ChevronDown
                    className={cn(
                      "h-3.5 w-3.5 transition-transform",
                      showCypher && "rotate-180",
                    )}
                  />
                </button>
                {showCypher ? (
                  <pre className="mt-2 overflow-x-auto rounded bg-muted p-2 font-mono text-[11px] leading-relaxed">
                    {answer.cypher}
                  </pre>
                ) : null}
              </div>
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

/** Tolerant decode of the ask response: rows from the first array property
 * (commonly "rows"/"answer"/"results"), Cypher from the usual keys. */
function normalizeAsk(data: unknown): AskAnswer {
  const obj = (typeof data === "object" && data !== null ? data : {}) as Record<
    string,
    unknown
  >;
  const rows = unwrapList<Record<string, unknown>>(data).map((r) =>
    typeof r === "object" && r !== null ? r : { value: r },
  );
  let cypher = "";
  for (const key of ["cypher", "query", "generated_cypher"]) {
    if (typeof obj[key] === "string") {
      cypher = obj[key] as string;
      break;
    }
  }
  const summary =
    typeof obj.summary === "string"
      ? obj.summary
      : typeof obj.answer === "string"
        ? obj.answer
        : undefined;
  return { rows, cypher, summary };
}

function formatCell(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "boolean") return v ? "yes" : "no";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
