"use client";

/**
 * SLA policy editor (SPEC-W19 Agent A): list of per-priority-tier policies
 * with inline edit + a create form. One policy per priority tier is the
 * intended shape (new tickets auto-attach the active policy matching their
 * priority; PATCH re-attaches on priority change).
 *
 * Data:
 *   - GET   /api/bookings/v1/helpdesk/sla-policies
 *   - POST  /api/bookings/v1/helpdesk/sla-policies
 *   - PATCH /api/bookings/v1/helpdesk/sla-policies/{id}
 */
import * as React from "react";
import { Pencil, Plus, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import {
  formatMinutes,
  PRIORITIES,
  PRIORITY_META,
  type SLAPolicy,
  type TicketPriority,
} from "@/components/apps/helpdesk/types";

interface PolicyForm {
  name: string;
  priority: TicketPriority;
  firstResponse: string; // minutes, string while editing
  resolve: string;
  active: boolean;
}

const EMPTY_FORM: PolicyForm = {
  name: "",
  priority: "normal",
  firstResponse: "60",
  resolve: "480",
  active: true,
};

export function SLAPolicyEditor({
  orgSlug,
  policies,
  canManage,
  onChanged,
}: {
  orgSlug: string;
  policies: SLAPolicy[];
  /** owner/admin only — others see the read-only list */
  canManage: boolean;
  onChanged: () => void;
}) {
  const { toast } = useToast();
  const [editing, setEditing] = React.useState<string | null>(null); // policy id | "new" | null
  const [form, setForm] = React.useState<PolicyForm>(EMPTY_FORM);
  const [formError, setFormError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);

  const startCreate = () => {
    setEditing("new");
    setForm(EMPTY_FORM);
    setFormError(null);
  };

  const startEdit = (p: SLAPolicy) => {
    setEditing(p.id);
    setForm({
      name: p.name,
      priority: p.priority,
      firstResponse: String(p.first_response_minutes),
      resolve: String(p.resolve_minutes),
      active: p.active,
    });
    setFormError(null);
  };

  const save = async () => {
    setFormError(null);
    const firstResponse = Number(form.firstResponse);
    const resolve = Number(form.resolve);
    if (!form.name.trim()) {
      setFormError("Name is required.");
      return;
    }
    if (!Number.isFinite(firstResponse) || firstResponse <= 0) {
      setFormError("First-response minutes must be a positive number.");
      return;
    }
    if (!Number.isFinite(resolve) || resolve <= 0) {
      setFormError("Resolve minutes must be a positive number.");
      return;
    }
    setBusy(true);
    try {
      if (editing === "new") {
        await api.post(
          "/api/bookings/v1/helpdesk/sla-policies",
          {
            name: form.name.trim(),
            priority: form.priority,
            first_response_minutes: Math.round(firstResponse),
            resolve_minutes: Math.round(resolve),
            active: form.active,
          },
          { tenant: orgSlug },
        );
        toast({ title: "SLA policy created", variant: "success" });
      } else if (editing) {
        await api.patch(
          `/api/bookings/v1/helpdesk/sla-policies/${editing}`,
          {
            name: form.name.trim(),
            priority: form.priority,
            first_response_minutes: Math.round(firstResponse),
            resolve_minutes: Math.round(resolve),
            active: form.active,
          },
          { tenant: orgSlug },
        );
        toast({ title: "SLA policy updated", variant: "success" });
      }
      setEditing(null);
      onChanged();
    } catch (e) {
      setFormError(
        e instanceof ApiError ? e.message : "The helpdesk service may be offline.",
      );
    } finally {
      setBusy(false);
    }
  };

  const formBlock = (
    <div className="space-y-3 rounded-md border border-border bg-muted/40 p-3">
      <div className="flex items-center justify-between">
        <h4 className="text-sm font-semibold">
          {editing === "new" ? "New SLA policy" : "Edit SLA policy"}
        </h4>
        <button
          type="button"
          onClick={() => setEditing(null)}
          className="rounded-md p-1 text-muted-foreground hover:bg-muted"
          aria-label="Cancel"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <Label htmlFor="sla-name">Name</Label>
          <Input
            id="sla-name"
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
            placeholder="e.g. Urgent tier"
          />
        </div>
        <div>
          <Label htmlFor="sla-priority">Priority tier</Label>
          <Select
            id="sla-priority"
            value={form.priority}
            onChange={(e) =>
              setForm({ ...form, priority: e.target.value as TicketPriority })
            }
          >
            {PRIORITIES.map((p) => (
              <option key={p} value={p}>
                {PRIORITY_META[p].label}
              </option>
            ))}
          </Select>
        </div>
        <div>
          <Label htmlFor="sla-fr">First response (minutes)</Label>
          <Input
            id="sla-fr"
            type="number"
            min={1}
            value={form.firstResponse}
            onChange={(e) => setForm({ ...form, firstResponse: e.target.value })}
          />
        </div>
        <div>
          <Label htmlFor="sla-resolve">Resolve (minutes)</Label>
          <Input
            id="sla-resolve"
            type="number"
            min={1}
            value={form.resolve}
            onChange={(e) => setForm({ ...form, resolve: e.target.value })}
          />
        </div>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={form.active}
          onChange={(e) => setForm({ ...form, active: e.target.checked })}
        />
        Active (new tickets of this priority attach this policy)
      </label>
      {formError ? <p className="text-sm text-destructive">{formError}</p> : null}
      <div className="flex gap-2">
        <Button size="sm" disabled={busy} onClick={() => void save()}>
          {editing === "new" ? "Create policy" : "Save changes"}
        </Button>
        <Button size="sm" variant="outline" onClick={() => setEditing(null)}>
          Cancel
        </Button>
      </div>
    </div>
  );

  return (
    <div className="space-y-3">
      {canManage && editing === null ? (
        <Button size="sm" variant="outline" onClick={startCreate}>
          <Plus className="mr-1 h-4 w-4" />
          New policy
        </Button>
      ) : null}
      {editing !== null ? formBlock : null}

      {policies.length === 0 ? (
        <p className="rounded-md border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
          No SLA policies yet — tickets get no due clocks until a policy exists.
        </p>
      ) : (
        <div className="overflow-hidden rounded-md border border-border">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border bg-muted/40 text-left text-xs text-muted-foreground">
                <th className="px-3 py-2 font-medium">Policy</th>
                <th className="px-3 py-2 font-medium">Priority</th>
                <th className="px-3 py-2 font-medium">First response</th>
                <th className="px-3 py-2 font-medium">Resolve</th>
                <th className="px-3 py-2 font-medium">Status</th>
                {canManage ? <th className="px-3 py-2" /> : null}
              </tr>
            </thead>
            <tbody>
              {policies.map((p) => (
                <tr key={p.id} className="border-b border-border last:border-0">
                  <td className="px-3 py-2 font-medium">{p.name}</td>
                  <td className="px-3 py-2">
                    <Badge variant={PRIORITY_META[p.priority]?.variant ?? "outline"}>
                      {PRIORITY_META[p.priority]?.label ?? p.priority}
                    </Badge>
                  </td>
                  <td className="px-3 py-2">{formatMinutes(p.first_response_minutes)}</td>
                  <td className="px-3 py-2">{formatMinutes(p.resolve_minutes)}</td>
                  <td className="px-3 py-2">
                    <Badge variant={p.active ? "success" : "secondary"}>
                      {p.active ? "Active" : "Inactive"}
                    </Badge>
                  </td>
                  {canManage ? (
                    <td className="px-3 py-2 text-right">
                      <Button size="sm" variant="ghost" onClick={() => startEdit(p)}>
                        <Pencil className="mr-1 h-3.5 w-3.5" />
                        Edit
                      </Button>
                    </td>
                  ) : null}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
