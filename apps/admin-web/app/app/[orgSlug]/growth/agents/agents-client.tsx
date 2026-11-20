"use client";

/**
 * Referral agents registry client (SPEC-W92 B5): agent list with status
 * filter, staff registration form, and approve/suspend actions.
 *
 * Data sources (all through the BFF with the x-tenant-slug header attached):
 *   - GET   /api/bookings/v1/referrals/agents?status=   (registry list)
 *   - POST  /api/bookings/v1/referrals/agents           (register: name+phone;
 *     the agent starts PENDING — approval is the admin-only PATCH)
 *   - PATCH /api/bookings/v1/referrals/agents/{id}      (approve/suspend;
 *     SPEC-W45 STK O10 admin-only server-side — a non-admin caller gets 403
 *     and the UI surfaces it via toast; the action is never hidden because
 *     the permission boundary must live in exactly one place)
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
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
import { useToast } from "@/components/ui/toast";
import { GrowthTabs } from "@/components/growth/growth-tabs";
import {
  formatCount,
  shortId,
  unwrap,
} from "@/components/growth/types";
import { formatDateTime, titleCase } from "@/lib/utils";

/** Referral agent row as returned by GET /v1/referrals/agents. */
export interface ReferralAgent {
  id: string;
  tenant_id?: string;
  name: string;
  phone: string;
  status: "pending" | "approved" | "suspended" | string;
  beneficiary_id?: string | null;
  created_at?: string;
  updated_at?: string;
}

/** Badge variant per agent status (warm, low-saturation palette). */
function agentStatusVariant(
  status: string,
): "success" | "warning" | "secondary" | "destructive" {
  switch (status) {
    case "approved":
      return "success";
    case "pending":
      return "warning";
    case "suspended":
      return "destructive";
    default:
      return "secondary";
  }
}

const STATUS_FILTERS = ["", "pending", "approved", "suspended"] as const;

export function AgentsClient({
  orgSlug,
  canAnalytics,
  canBilling,
}: {
  orgSlug: string;
  canAnalytics: boolean;
  canBilling: boolean;
}) {
  const { toast } = useToast();
  const [agents, setAgents] = React.useState<ReferralAgent[]>([]);
  const [statusFilter, setStatusFilter] = React.useState<string>("");
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [creating, setCreating] = React.useState(false);
  const [patchingId, setPatchingId] = React.useState<string | null>(null);
  const [name, setName] = React.useState("");
  const [phone, setPhone] = React.useState("");

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/referrals/agents", {
          tenant: orgSlug,
          status: statusFilter,
        });
        if (signal?.aborted) return;
        setAgents(unwrap<ReferralAgent>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setAgents([]);
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "The referral agents API is not available yet — the booking-service referrals API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, statusFilter],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const register = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim() || !phone.trim()) {
      toast({ title: "Name and phone are required", variant: "destructive" });
      return;
    }
    setCreating(true);
    try {
      await api.post(
        "/api/bookings/v1/referrals/agents",
        { name: name.trim(), phone: phone.trim() },
        { tenant: orgSlug },
      );
      toast({
        title: "Agent registered as pending — an admin approves before payouts flow",
        variant: "success",
      });
      setName("");
      setPhone("");
      await load();
    } catch (err) {
      toast({
        title: err instanceof ApiError ? err.message : "Could not register the agent",
        variant: "destructive",
      });
    } finally {
      setCreating(false);
    }
  };

  const setStatus = async (agent: ReferralAgent, status: "approved" | "suspended") => {
    setPatchingId(agent.id);
    try {
      await api.patch(
        `/api/bookings/v1/referrals/agents/${agent.id}`,
        { status },
        { tenant: orgSlug },
      );
      toast({
        title:
          status === "approved"
            ? `${agent.name} approved — commissions can now pay out`
            : `${agent.name} suspended — payouts blocked`,
        variant: "success",
      });
      await load();
    } catch (err) {
      // The 403 (admin-only PATCH, SPEC-W45 STK O10) lands here verbatim.
      toast({
        title:
          err instanceof ApiError
            ? err.message
            : "Could not update the agent",
        variant: "destructive",
      });
    } finally {
      setPatchingId(null);
    }
  };

  const pendingCount = agents.filter((a) => a.status === "pending").length;

  return (
    <div className="space-y-4">
      <PageHeader
        title="Referral agents"
        description="Field agents who refer customers for commission. Registrations start pending; approval and suspension are admin-only."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className="mr-1.5 h-3.5 w-3.5" />
            Refresh
          </Button>
        }
      />
      <GrowthTabs orgSlug={orgSlug} canAnalytics={canAnalytics} canBilling={canBilling} />

      <Card>
        <CardHeader>
          <CardTitle>Register an agent</CardTitle>
          <CardDescription>
            Staff-side registration — the agent starts <strong>pending</strong> and
            cannot receive payouts until an admin approves.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={register} className="flex flex-wrap items-end gap-3">
            <div className="min-w-48 flex-1">
              <Label htmlFor="agent-name">Full name</Label>
              <Input
                id="agent-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Adaeze Okafor"
                maxLength={120}
                required
              />
            </div>
            <div className="min-w-48 flex-1">
              <Label htmlFor="agent-phone">Phone (E.164)</Label>
              <Input
                id="agent-phone"
                value={phone}
                onChange={(e) => setPhone(e.target.value)}
                placeholder="+2348012345678"
                maxLength={32}
                required
              />
            </div>
            <Button type="submit" disabled={creating}>
              {creating ? "Registering…" : "Register agent"}
            </Button>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <CardTitle>Registry</CardTitle>
              <CardDescription>
                {formatCount(agents.length)} agent{agents.length === 1 ? "" : "s"}
                {statusFilter ? ` (${titleCase(statusFilter)})` : ""}
                {pendingCount > 0 && !statusFilter
                  ? ` — ${formatCount(pendingCount)} awaiting approval`
                  : ""}
              </CardDescription>
            </div>
            <div className="w-44">
              <Label htmlFor="status-filter">Status</Label>
              <Select
                id="status-filter"
                value={statusFilter}
                onChange={(e) => setStatusFilter(e.target.value)}
              >
                {STATUS_FILTERS.map((s) => (
                  <option key={s} value={s}>
                    {s === "" ? "All" : titleCase(s)}
                  </option>
                ))}
              </Select>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          {error ? <ErrorNote message={error} /> : null}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Phone</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Beneficiary</TableHead>
                <TableHead>Registered</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {agents.map((a) => (
                <TableRow key={a.id}>
                  <TableCell className="font-medium">{a.name}</TableCell>
                  <TableCell>{a.phone}</TableCell>
                  <TableCell>
                    <Badge variant={agentStatusVariant(a.status)}>
                      {titleCase(a.status)}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    {a.beneficiary_id ? shortId(a.beneficiary_id) : "—"}
                  </TableCell>
                  <TableCell>
                    {a.created_at ? formatDateTime(a.created_at) : "—"}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1.5">
                      {a.status !== "approved" && (
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={patchingId === a.id}
                          onClick={() => void setStatus(a, "approved")}
                        >
                          Approve
                        </Button>
                      )}
                      {a.status !== "suspended" && (
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={patchingId === a.id}
                          onClick={() => void setStatus(a, "suspended")}
                        >
                          Suspend
                        </Button>
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
              {!loading && agents.length === 0 && !error && (
                <TableEmpty colSpan={6}>
                  No agents{statusFilter ? ` with status ${statusFilter}` : ""} yet —
                  register the first one above.
                </TableEmpty>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}
