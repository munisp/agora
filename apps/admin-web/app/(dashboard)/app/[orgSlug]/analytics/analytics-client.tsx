"use client";

import * as React from "react";
import { ChevronDown, ChevronRight, Database, Send } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
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
import type { AnalyticsQueryResult } from "@/lib/types";
import { KpiDashboard } from "./kpi-dashboard";

const EXAMPLES = [
  "How many bookings did we have per day last week?",
  "Which offering brings the most revenue this month?",
  "What is our no-show rate by weekday?",
];

/**
 * Conversational analytics (innovation 8): natural-language questions over
 * the lakehouse gold marts. The knowledge-service generates guarded,
 * read-only Trino SQL (single SELECT, gold.* allowlist, tenant filter
 * injected) and returns the result set plus the SQL for audit.
 */
export function AnalyticsClient({ orgSlug }: { orgSlug: string }) {
  const [question, setQuestion] = React.useState("");
  const [running, setRunning] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [result, setResult] = React.useState<AnalyticsQueryResult | null>(null);
  const [sqlOpen, setSqlOpen] = React.useState(false);

  const ask = async (q: string) => {
    const trimmed = q.trim();
    if (!trimmed || running) return;
    setRunning(true);
    setError(null);
    setResult(null);
    try {
      const res = await api.post<AnalyticsQueryResult>(
        "/api/knowledge/v1/analytics/query",
        { tenant: orgSlug, question: trimmed },
      );
      setResult(res);
      setSqlOpen(false);
    } catch (e) {
      setError(
        e instanceof ApiError
          ? e.message
          : "Analytics query failed — the text-to-SQL service may be offline.",
      );
    } finally {
      setRunning(false);
    }
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    void ask(question);
  };

  return (
    <div className="max-w-5xl">
      <PageHeader
        title="Analytics"
        description="KPIs at a glance, plus plain-language questions over your bookings, revenue and customers — answered from the lakehouse gold marts."
      />

      <KpiDashboard orgSlug={orgSlug} />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Database className="h-4 w-4" /> Talk to your data
          </CardTitle>
          <CardDescription>
            Read-only. Generated SQL is validated (single SELECT, curated
            tables, tenant-scoped) before it runs against Trino.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="flex gap-2">
            <Input
              value={question}
              onChange={(e) => setQuestion(e.target.value)}
              placeholder="Ask a question about your business…"
              className="max-w-2xl"
            />
            <Button type="submit" disabled={running || !question.trim()}>
              <Send className="h-4 w-4" />
              {running ? "Running…" : "Ask"}
            </Button>
          </form>
          <div className="mt-3 flex flex-wrap gap-2">
            {EXAMPLES.map((ex) => (
              <button
                key={ex}
                type="button"
                onClick={() => {
                  setQuestion(ex);
                  void ask(ex);
                }}
                className="rounded-full border border-border px-3 py-1 text-xs text-muted-foreground hover:bg-accent hover:text-foreground cursor-pointer"
              >
                {ex}
              </button>
            ))}
          </div>
        </CardContent>
      </Card>

      {error ? (
        <div className="mt-4">
          <ErrorNote message={error} />
        </div>
      ) : null}

      {result ? (
        <Card className="mt-6">
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <div>
              <CardTitle>Result</CardTitle>
              <CardDescription>
                {result.rows.length} row{result.rows.length === 1 ? "" : "s"}
                {result.truncated ? " (truncated — refine the question for detail)" : ""}
              </CardDescription>
            </div>
            {result.truncated ? (
              <Badge variant="warning">Truncated</Badge>
            ) : null}
          </CardHeader>
          <CardContent>
            {result.explanation ? (
              <p className="mb-4 text-sm text-muted-foreground">
                {result.explanation}
              </p>
            ) : null}
            <Table>
              <TableHeader>
                <TableRow>
                  {result.columns.map((c) => (
                    <TableHead key={c}>{c}</TableHead>
                  ))}
                </TableRow>
              </TableHeader>
              <TableBody>
                {result.rows.length === 0 ? (
                  <TableEmpty colSpan={Math.max(1, result.columns.length)}>
                    The query ran but returned no rows.
                  </TableEmpty>
                ) : (
                  result.rows.map((row, i) => (
                    <TableRow key={i}>
                      {result.columns.map((_, j) => (
                        <TableCell key={j}>{formatCell(row[j])}</TableCell>
                      ))}
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>

            <button
              type="button"
              onClick={() => setSqlOpen((o) => !o)}
              className="mt-4 inline-flex items-center gap-1 text-xs font-medium text-muted-foreground hover:text-foreground cursor-pointer"
            >
              {sqlOpen ? (
                <ChevronDown className="h-3.5 w-3.5" />
              ) : (
                <ChevronRight className="h-3.5 w-3.5" />
              )}
              Generated SQL
            </button>
            {sqlOpen ? (
              <pre className="mt-2 overflow-x-auto rounded-md bg-muted p-3 text-xs">
                <code>{result.sql}</code>
              </pre>
            ) : null}
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}

function formatCell(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "number") return Number.isInteger(value) ? String(value) : value.toFixed(2);
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}
