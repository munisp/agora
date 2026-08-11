"use client";

import * as React from "react";
import Link from "next/link";
import { ArrowLeft, Bot, Database, Loader2, Save } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Label, Select } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";
import {
  draftFromAgent,
  patchBodyFromDraft,
  unwrapList,
  type AgentDraft,
} from "@/components/agents/agent-draft";
import { StepBasics } from "@/components/agents/step-basics";
import { StepPhone } from "@/components/agents/step-phone";
import { StepPersona } from "@/components/agents/step-persona";
import { StepTools } from "@/components/agents/step-tools";
import { StepKnowledge } from "@/components/agents/step-knowledge";
import { StepCapture } from "@/components/agents/step-capture";
import type {
  Agent,
  AgentStatus,
  CaptureField,
  CaptureRecord,
  CaptureSchema,
} from "@/lib/types";

/**
 * SPEC-W38 F4 — agent edit page: the same sections as the wizard (PATCHed)
 * plus the capture-records viewer fed by the post-call capture pipeline (F3).
 */
export function AgentDetailClient({
  orgSlug,
  agentId,
}: {
  orgSlug: string;
  agentId: string;
}) {
  const { toast } = useToast();
  const [draft, setDraft] = React.useState<AgentDraft | null>(null);
  const [schema, setSchema] = React.useState<CaptureSchema | null>(null);
  const [records, setRecords] = React.useState<CaptureRecord[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const patch = React.useCallback(
    (p: Partial<AgentDraft>) =>
      setDraft((d) => (d ? { ...d, ...p } : d)),
    [],
  );

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const [agentData, schemasData, recordsData] = await Promise.all([
        api.get<Agent>(`/api/agents/${agentId}`, { tenant: orgSlug }),
        api
          .get<unknown>("/api/capture-schemas", {
            tenant: orgSlug,
            agent_id: agentId,
          })
          .catch(() => null),
        api
          .get<unknown>("/api/capture-records", {
            tenant: orgSlug,
            agent_id: agentId,
          })
          .catch(() => null),
      ]);
      const schemas = unwrapList<CaptureSchema>(
        schemasData,
        "capture_schemas",
        "schemas",
        "items",
      );
      const active = schemas.find((s) => s.active) ?? schemas[0] ?? null;
      setSchema(active);
      setDraft(
        draftFromAgent(
          agentData,
          active?.schema?.fields ?? [],
          active?.name ?? "Call capture",
        ),
      );
      setRecords(
        unwrapList<CaptureRecord>(recordsData, "capture_records", "records", "items"),
      );
      setError(null);
    } catch (e) {
      setError(
        e instanceof ApiError
          ? e.message
          : "Failed to load the agent — the conversation service may be offline.",
      );
    } finally {
      setLoading(false);
    }
  }, [orgSlug, agentId]);

  React.useEffect(() => {
    load();
  }, [load]);

  const save = async () => {
    if (!draft) return;
    setSaving(true);
    try {
      await api.patch<Agent>(
        `/api/agents/${agentId}`,
        patchBodyFromDraft(draft),
        { tenant: orgSlug },
      );
      // Capture schema: create, update, or deactivate when cleared.
      if (draft.captureFields.length > 0) {
        const body = {
          agent_id: agentId,
          name: draft.captureSchemaName.trim() || "Call capture",
          schema: { fields: draft.captureFields },
          active: true,
        };
        if (schema) {
          await api.patch<CaptureSchema>(
            `/api/capture-schemas/${schema.id}`,
            body,
            { tenant: orgSlug },
          );
        } else {
          await api.post<CaptureSchema>("/api/capture-schemas", body, {
            tenant: orgSlug,
          });
        }
      } else if (schema && schema.active) {
        await api.patch<CaptureSchema>(
          `/api/capture-schemas/${schema.id}`,
          { active: false },
          { tenant: orgSlug },
        );
      }
      toast({ title: "Agent saved", variant: "success" });
      await load();
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

  // Viewer columns come from the active schema; when it is gone (or predates
  // records) fall back to the union of keys present in the record data.
  const columns = React.useMemo<CaptureField[]>(() => {
    if (schema?.schema?.fields?.length) return schema.schema.fields;
    const keys = new Set<string>();
    for (const r of records) for (const k of Object.keys(r.data ?? {})) keys.add(k);
    return [...keys].map((key) => ({
      key,
      type: "string" as const,
      label: key,
      required: false,
    }));
  }, [schema, records]);

  if (loading && !draft) {
    return (
      <div className="space-y-6 p-6">
        <PageHeader title="Agent" description="Loading…" />
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" /> Loading agent…
        </p>
      </div>
    );
  }

  if (!draft) {
    return (
      <div className="space-y-6 p-6">
        <PageHeader title="Agent" />
        {error ? <ErrorNote message={error} /> : null}
        <Link href={`/app/${orgSlug}/agents`}>
          <Button variant="outline" size="sm">
            <ArrowLeft className="h-4 w-4" /> Back to agents
          </Button>
        </Link>
      </div>
    );
  }

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title={draft.name || "Agent"}
        description="Edit the agent definition and review what the capture pipeline extracted from calls."
        actions={
          <>
            <Link href={`/app/${orgSlug}/agents`}>
              <Button variant="outline" size="sm">
                <ArrowLeft className="h-4 w-4" /> Agents
              </Button>
            </Link>
            <Button size="sm" onClick={() => void save()} disabled={saving}>
              {saving ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Save className="h-4 w-4" />
              )}
              {saving ? "Saving…" : "Save changes"}
            </Button>
          </>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="grid gap-6 xl:grid-cols-2">
        <div className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Bot className="h-4 w-4" /> Basics
              </CardTitle>
            </CardHeader>
            <CardContent className="grid gap-4">
              <StepBasics draft={draft} patch={patch} slugReadOnly />
              <div className="grid gap-1.5">
                <Label htmlFor="agent-status">Status</Label>
                <Select
                  id="agent-status"
                  value={draft.status}
                  onChange={(e) =>
                    patch({ status: e.target.value as AgentStatus })
                  }
                >
                  <option value="active">active</option>
                  <option value="disabled">disabled</option>
                </Select>
                <p className="text-xs text-muted-foreground">
                  Disabled agents keep their configuration but no longer take
                  calls.
                </p>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Phone number</CardTitle>
            </CardHeader>
            <CardContent>
              <StepPhone draft={draft} patch={patch} />
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Persona & voice</CardTitle>
            </CardHeader>
            <CardContent>
              <StepPersona draft={draft} patch={patch} />
            </CardContent>
          </Card>
        </div>

        <div className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle>Tools</CardTitle>
            </CardHeader>
            <CardContent>
              <StepTools draft={draft} patch={patch} />
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Knowledge</CardTitle>
            </CardHeader>
            <CardContent>
              <StepKnowledge draft={draft} patch={patch} />
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Capture schema</CardTitle>
              <CardDescription>
                Saved together with the agent. Clearing all fields deactivates
                the schema.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <StepCapture draft={draft} patch={patch} />
            </CardContent>
          </Card>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Database className="h-4 w-4" /> Capture records
          </CardTitle>
          <CardDescription>
            Data extracted from calls after they end (GET
            /api/capture-records?agent_id=…), newest first.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                {columns.map((c) => (
                  <TableHead key={c.key}>{c.label}</TableHead>
                ))}
                <TableHead>Confidence</TableHead>
                <TableHead>Captured</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {records.length === 0 ? (
                <TableEmpty colSpan={columns.length + 2}>
                  No capture records yet — they appear after the agent's next
                  completed call.
                </TableEmpty>
              ) : (
                records.map((record) => (
                  <TableRow key={record.id}>
                    {columns.map((c) => (
                      <TableCell key={c.key} className="max-w-[220px] truncate">
                        {formatValue(record.data?.[c.key])}
                      </TableCell>
                    ))}
                    <TableCell>
                      {record.extraction_confidence === null ? (
                        "—"
                      ) : (
                        <Badge
                          variant={
                            record.extraction_confidence >= 0.8
                              ? "success"
                              : record.extraction_confidence >= 0.5
                                ? "warning"
                                : "destructive"
                          }
                        >
                          {Math.round(record.extraction_confidence * 100)}%
                        </Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {formatDateTime(record.created_at)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}

function formatValue(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "boolean") return value ? "yes" : "no";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}
