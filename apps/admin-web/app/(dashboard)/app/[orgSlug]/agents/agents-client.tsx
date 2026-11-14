"use client";

import * as React from "react";
import Link from "next/link";
import { Bot, Pencil, Plus } from "lucide-react";
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
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/utils";
import { unwrapList } from "@/components/agents/agent-draft";
import type { Agent } from "@/lib/types";

/**
 * SPEC-W38 F4 — agents registry list. Data comes from conversation-service
 * via the BFF catch-all → APISIX /api/agents (tenant resolved through the
 * x-tenant-slug header set from the ?tenant= query, same as /v1/conversations).
 */
export function AgentsClient({ orgSlug }: { orgSlug: string }) {
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);

  const load = React.useCallback(async () => {
    try {
      const data = await api.get<unknown>("/api/agents", { tenant: orgSlug });
      setAgents(unwrapList<Agent>(data, "agents", "items"));
      setError(null);
    } catch (e) {
      setError(
        e instanceof ApiError
          ? e.message
          : "Failed to load agents — the conversation service may be offline.",
      );
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Agents"
        description="Voice agents as first-class products — each with its own number, persona, tools and capture schema."
        actions={
          <Link href={`/app/${orgSlug}/agents/new`}>
            <Button size="sm">
              <Plus className="h-4 w-4" /> New agent
            </Button>
          </Link>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Bot className="h-4 w-4" /> Agents
          </CardTitle>
          <CardDescription>
            Dialed numbers resolve to agents from this registry; the legacy
            TENANT_PHONE_MAP remains the fallback.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Purpose</TableHead>
                <TableHead>Phone number</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {agents.length === 0 ? (
                <TableEmpty colSpan={6}>
                  {loading
                    ? "Loading…"
                    : "No agents yet — create one with the wizard."}
                </TableEmpty>
              ) : (
                agents.map((agent) => (
                  <TableRow key={agent.id}>
                    <TableCell>
                      <Link
                        href={`/app/${orgSlug}/agents/${agent.id}`}
                        className="font-medium text-primary hover:underline"
                      >
                        {agent.name}
                      </Link>
                      <span className="block font-mono text-xs text-muted-foreground">
                        {agent.slug}
                      </span>
                    </TableCell>
                    <TableCell className="max-w-[280px] truncate text-sm">
                      {agent.purpose || "—"}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {agent.phone_number ?? "—"}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          agent.status === "active" ? "success" : "secondary"
                        }
                      >
                        {agent.status}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {formatDateTime(agent.created_at)}
                    </TableCell>
                    <TableCell className="text-right">
                      <Link href={`/app/${orgSlug}/agents/${agent.id}`}>
                        <Button size="sm" variant="outline">
                          <Pencil className="h-3 w-3" /> Edit
                        </Button>
                      </Link>
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
