"use client";

/**
 * Results dashboard (SPEC-W20 Agent B): headline tile (NPS for kind=nps,
 * mean score for csat/ces/custom), promoters/passives/detractors, the 0–10
 * score distribution as bars, per-question single/multi breakdowns, and
 * the naive VoC themes list. Honest empty states throughout.
 */
import * as React from "react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import type { Survey, SurveyResults, Theme } from "./types";
import { distributionRows, npsTone } from "./types";

function Bar({ value, max }: { value: number; max: number }) {
  const pct = max > 0 ? Math.round((100 * value) / max) : 0;
  return (
    <div className="h-2 w-full rounded bg-secondary">
      <div
        className="h-2 rounded bg-primary"
        style={{ width: `${pct}%` }}
        aria-hidden
      />
    </div>
  );
}

export function ResultsDashboard({
  survey,
  results,
  themes,
  themesNote,
}: {
  survey: Survey;
  results: SurveyResults;
  themes: Theme[];
  themesNote?: string;
}) {
  const distRows = distributionRows(results.score_distribution);
  const distMax = Math.max(0, ...distRows.map(([, n]) => n));
  const headlineIsNps = results.kind === "nps";
  const headline = headlineIsNps ? results.nps : results.mean_score;

  return (
    <div className="space-y-4">
      <div className="grid gap-3 sm:grid-cols-4">
        <Card>
          <CardHeader className="pb-2">
            <CardDescription>
              {headlineIsNps ? "NPS" : "Mean score"}
            </CardDescription>
            <CardTitle
              className={`text-3xl ${headline != null && headlineIsNps ? npsTone(headline) : ""}`}
            >
              {headline == null
                ? "—"
                : headlineIsNps
                  ? Math.round(headline)
                  : headline.toFixed(1)}
            </CardTitle>
          </CardHeader>
          <CardContent className="text-xs text-muted-foreground">
            {results.scored_count > 0
              ? headlineIsNps
                ? "%promoters(9–10) − %detractors(0–6)"
                : `over ${results.scored_count} scored`
              : "No scored responses yet"}
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardDescription>Responses</CardDescription>
            <CardTitle className="text-3xl">{results.response_count}</CardTitle>
          </CardHeader>
          <CardContent className="text-xs text-muted-foreground">
            {results.scored_count} scored
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardDescription>Promoters / passives / detractors</CardDescription>
            <CardTitle className="text-3xl">
              <span className="text-success">{results.promoters}</span>
              {" / "}
              <span className="text-warning">{results.passives}</span>
              {" / "}
              <span className="text-destructive">{results.detractors}</span>
            </CardTitle>
          </CardHeader>
          <CardContent className="text-xs text-muted-foreground">
            9–10 / 7–8 / 0–6
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardDescription>Survey</CardDescription>
            <CardTitle className="text-base leading-snug">{survey.name}</CardTitle>
          </CardHeader>
          <CardContent className="text-xs text-muted-foreground">
            {results.kind.toUpperCase()} · {survey.questions.length} questions
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Score distribution</CardTitle>
            <CardDescription>First rating answer per response</CardDescription>
          </CardHeader>
          <CardContent>
            {distRows.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No scored responses yet — distribution appears after the first rating answer.
              </p>
            ) : (
              <div className="space-y-1.5">
                {distRows.map(([score, n]) => (
                  <div key={score} className="grid grid-cols-[2rem_1fr_2rem] items-center gap-2 text-sm">
                    <span className="text-muted-foreground">{score}</span>
                    <Bar value={n} max={distMax} />
                    <span className="text-right tabular-nums">{n}</span>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">VoC themes</CardTitle>
            <CardDescription>
              {themesNote ?? "Naive keyword frequency over text answers (not NLP)"}
            </CardDescription>
          </CardHeader>
          <CardContent>
            {themes.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No text answers yet — themes appear once respondents leave comments.
              </p>
            ) : (
              <ul className="flex flex-wrap gap-2">
                {themes.map((t) => (
                  <li
                    key={t.term}
                    className="rounded-full border border-border px-2.5 py-0.5 text-xs"
                  >
                    {t.term}{" "}
                    <span className="text-muted-foreground">×{t.count}</span>
                  </li>
                ))}
              </ul>
            )}
          </CardContent>
        </Card>
      </div>

      {results.questions.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Question breakdowns</CardTitle>
            <CardDescription>Single / multi-choice tallies</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4 md:grid-cols-2">
            {results.questions.map((q) => {
              const max = Math.max(0, ...q.options.map((o) => o.count));
              return (
                <div key={q.id} className="space-y-1.5">
                  <p className="text-sm font-medium">
                    {q.label}{" "}
                    <span className="text-xs text-muted-foreground">
                      ({q.type}, {q.answer_count} answered)
                    </span>
                  </p>
                  {q.options.map((o) => (
                    <div
                      key={o.option}
                      className="grid grid-cols-[minmax(0,8rem)_1fr_2rem] items-center gap-2 text-sm"
                    >
                      <span className="truncate text-muted-foreground">{o.option}</span>
                      <Bar value={o.count} max={max} />
                      <span className="text-right tabular-nums">{o.count}</span>
                    </div>
                  ))}
                </div>
              );
            })}
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}
