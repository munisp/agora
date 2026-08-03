"use client";

/**
 * Commission rules editor (SPEC-W14 Agent C, contract §2). Self-contained
 * section (mirrors the billing page's InvoicesPanel): fetches the rule list
 * itself and implements create / edit / active-toggle / delete against
 *   GET    /api/bookings/v1/commissions/rules
 *   POST   /api/bookings/v1/commissions/rules
 *   PUT    /api/bookings/v1/commissions/rules/{id}
 *   DELETE /api/bookings/v1/commissions/rules/{id}
 * (booking-service registers the routes under /v1/commissions/rules and the
 * update verb is PUT, a full replacement — see server.go / referrals.go.)
 *
 * Money is integer kobo end-to-end (contract §2): the form takes whole
 * naira / basis points and converts to kobo with Math.round(x * 100) — no
 * float money ever reaches the wire. Rules are evaluated server-side in
 * priority order (asc); inactive rules are skipped.
 */
import * as React from "react";
import { Pencil, Plus, RefreshCw, Trash2, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
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
import { titleCase } from "@/lib/utils";
import { ErrorNote } from "@/components/error-note";
import {
  ruleAmountLabel,
  triggerLabel,
  unwrap,
  type CommissionRule,
} from "@/components/growth/types";

const TRIGGERS = [
  { value: "signup_verified", label: "Signup verified" },
  { value: "first_booking", label: "First booking" },
  { value: "first_txn", label: "First transaction" },
  { value: "sale", label: "Sale" },
];

const BENEFICIARIES = [
  { value: "referrer", label: "Referrer" },
  { value: "agent", label: "Agent" },
  { value: "staff", label: "Staff" },
];

interface RuleDraft {
  name: string;
  trigger: string;
  beneficiary: string;
  amount_type: string;
  /** whole naira for flat rules (converted to kobo on submit) */
  amountNaira: string;
  /** basis points for percent rules */
  bps: string;
  /** whole naira, empty = no cap */
  capNaira: string;
  priority: string;
  active: boolean;
}

const EMPTY_DRAFT: RuleDraft = {
  name: "",
  trigger: "signup_verified",
  beneficiary: "referrer",
  amount_type: "flat",
  amountNaira: "",
  bps: "",
  capNaira: "",
  priority: "100",
  active: true,
};

function draftFromRule(rule: CommissionRule): RuleDraft {
  return {
    name: rule.name,
    trigger: rule.trigger,
    beneficiary: rule.beneficiary,
    amount_type: rule.amount_type,
    amountNaira:
      rule.amount_type === "flat" && rule.amount_ngn != null
        ? (rule.amount_ngn / 100).toString()
        : "",
    bps:
      rule.amount_type === "percent" && rule.bps != null
        ? String(rule.bps)
        : "",
    capNaira: rule.cap_ngn != null ? (rule.cap_ngn / 100).toString() : "",
    priority: String(rule.priority),
    active: rule.active,
  };
}

export function CommissionRulesEditor({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [rules, setRules] = React.useState<CommissionRule[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [draft, setDraft] = React.useState<RuleDraft>(EMPTY_DRAFT);
  const [editingId, setEditingId] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<unknown>(
        "/api/bookings/v1/commissions/rules",
        { tenant: orgSlug },
      );
      setRules(unwrap<CommissionRule>(data));
    } catch (e) {
      setRules([]);
      setError(
        e instanceof ApiError && e.status !== 404
          ? e.message
          : "Commission rules are not available yet — the booking-service referrals API may still be rolling out.",
      );
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const sorted = [...rules].sort(
    (a, b) => a.priority - b.priority || a.name.localeCompare(b.name),
  );

  const amountKobo = Math.round(parseFloat(draft.amountNaira || "0") * 100);
  const bpsInt = parseInt(draft.bps || "0", 10);
  const capKobo = draft.capNaira.trim()
    ? Math.round(parseFloat(draft.capNaira) * 100)
    : null;
  const priorityInt = parseInt(draft.priority || "0", 10);
  const amountValid =
    draft.amount_type === "percent"
      ? Number.isInteger(bpsInt) && bpsInt > 0 && bpsInt <= 10000
      : Number.isFinite(amountKobo) && amountKobo > 0;
  const valid =
    draft.name.trim().length > 0 &&
    amountValid &&
    Number.isInteger(priorityInt) &&
    (capKobo === null || (Number.isFinite(capKobo) && capKobo >= 0));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || busy) return;
    setBusy(true);
    const body: Record<string, unknown> = {
      name: draft.name.trim(),
      trigger: draft.trigger,
      beneficiary: draft.beneficiary,
      amount_type: draft.amount_type,
      amount_ngn: draft.amount_type === "flat" ? amountKobo : null,
      bps: draft.amount_type === "percent" ? bpsInt : null,
      cap_ngn: capKobo,
      priority: priorityInt,
      active: draft.active,
    };
    try {
      if (editingId) {
        await api.put(
          `/api/bookings/v1/commissions/rules/${editingId}`,
          body,
          { tenant: orgSlug },
        );
        toast({ title: "Rule updated", variant: "success" });
      } else {
        await api.post("/api/bookings/v1/commissions/rules", body, {
          tenant: orgSlug,
        });
        toast({ title: "Rule created", variant: "success" });
      }
      setDraft(EMPTY_DRAFT);
      setEditingId(null);
      await load();
    } catch (err) {
      toast({
        title: editingId ? "Failed to update rule" : "Failed to create rule",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const toggleActive = async (rule: CommissionRule) => {
    try {
      // PUT is a full replacement server-side, so resend the whole rule
      // with only `active` flipped (a partial body would zero the rest).
      await api.put(
        `/api/bookings/v1/commissions/rules/${rule.rule_id}`,
        {
          name: rule.name,
          trigger: rule.trigger,
          beneficiary: rule.beneficiary,
          amount_type: rule.amount_type,
          amount_ngn: rule.amount_type === "flat" ? (rule.amount_ngn ?? 0) : 0,
          bps: rule.amount_type === "percent" ? (rule.bps ?? 0) : 0,
          cap_ngn: rule.cap_ngn ?? null,
          priority: rule.priority,
          active: !rule.active,
        },
        { tenant: orgSlug },
      );
      await load();
    } catch (err) {
      toast({
        title: "Failed to toggle rule",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    }
  };

  const remove = async (rule: CommissionRule) => {
    if (!window.confirm(`Delete rule “${rule.name}”? This cannot be undone.`)) {
      return;
    }
    try {
      await api.delete(`/api/bookings/v1/commissions/rules/${rule.rule_id}`, {
        tenant: orgSlug,
      });
      toast({ title: "Rule deleted", variant: "success" });
      if (editingId === rule.rule_id) {
        setEditingId(null);
        setDraft(EMPTY_DRAFT);
      }
      await load();
    } catch (err) {
      toast({
        title: "Failed to delete rule",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    }
  };

  const set = <K extends keyof RuleDraft>(key: K, value: RuleDraft[K]) =>
    setDraft((d) => ({ ...d, [key]: value }));

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>
            {editingId ? "Edit commission rule" : "New commission rule"}
          </CardTitle>
          <CardDescription>
            Rules fire when a referral is verified. Multiple rules may fire on
            the same trigger; evaluation order is priority (lowest first) and
            inactive rules are skipped. Percent rules use basis points (100
            bps = 1%).
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="space-y-3">
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <div className="space-y-1.5">
                <Label htmlFor="rule-name">Name</Label>
                <Input
                  id="rule-name"
                  value={draft.name}
                  onChange={(e) => set("name", e.target.value)}
                  placeholder="Refer-a-friend bounty"
                  required
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="rule-trigger">Trigger</Label>
                <Select
                  id="rule-trigger"
                  value={draft.trigger}
                  onChange={(e) => set("trigger", e.target.value)}
                >
                  {TRIGGERS.map((t) => (
                    <option key={t.value} value={t.value}>
                      {t.label}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="rule-beneficiary">Beneficiary</Label>
                <Select
                  id="rule-beneficiary"
                  value={draft.beneficiary}
                  onChange={(e) => set("beneficiary", e.target.value)}
                >
                  {BENEFICIARIES.map((b) => (
                    <option key={b.value} value={b.value}>
                      {b.label}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="rule-amount-type">Amount type</Label>
                <Select
                  id="rule-amount-type"
                  value={draft.amount_type}
                  onChange={(e) => set("amount_type", e.target.value)}
                >
                  <option value="flat">Flat amount (₦)</option>
                  <option value="percent">Percent (basis points)</option>
                </Select>
              </div>
              {draft.amount_type === "flat" ? (
                <div className="space-y-1.5">
                  <Label htmlFor="rule-amount">Amount (₦)</Label>
                  <Input
                    id="rule-amount"
                    type="number"
                    min="0"
                    step="0.01"
                    inputMode="decimal"
                    value={draft.amountNaira}
                    onChange={(e) => set("amountNaira", e.target.value)}
                    placeholder="500.00"
                    required
                  />
                </div>
              ) : (
                <div className="space-y-1.5">
                  <Label htmlFor="rule-bps">Basis points</Label>
                  <Input
                    id="rule-bps"
                    type="number"
                    min="1"
                    max="10000"
                    step="1"
                    inputMode="numeric"
                    value={draft.bps}
                    onChange={(e) => set("bps", e.target.value)}
                    placeholder="250 = 2.5%"
                    required
                  />
                </div>
              )}
              <div className="space-y-1.5">
                <Label htmlFor="rule-cap">Cap (₦, optional)</Label>
                <Input
                  id="rule-cap"
                  type="number"
                  min="0"
                  step="0.01"
                  inputMode="decimal"
                  value={draft.capNaira}
                  onChange={(e) => set("capNaira", e.target.value)}
                  placeholder="no cap"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="rule-priority">Priority</Label>
                <Input
                  id="rule-priority"
                  type="number"
                  step="1"
                  inputMode="numeric"
                  value={draft.priority}
                  onChange={(e) => set("priority", e.target.value)}
                  required
                />
              </div>
              <div className="flex items-end gap-2 pb-2">
                <input
                  id="rule-active"
                  type="checkbox"
                  checked={draft.active}
                  onChange={(e) => set("active", e.target.checked)}
                  className="h-4 w-4 accent-primary"
                />
                <Label htmlFor="rule-active">Active</Label>
              </div>
            </div>
            <div className="flex items-center gap-2">
              <Button type="submit" disabled={!valid || busy}>
                <Plus className="h-3.5 w-3.5" />
                {busy
                  ? "Saving…"
                  : editingId
                    ? "Save changes"
                    : "Create rule"}
              </Button>
              {editingId ? (
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => {
                    setEditingId(null);
                    setDraft(EMPTY_DRAFT);
                  }}
                >
                  <X className="h-3.5 w-3.5" />
                  Cancel edit
                </Button>
              ) : null}
            </div>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle>Rules</CardTitle>
            <CardDescription>
              Evaluation order: priority ascending. Inactive rules never fire.
            </CardDescription>
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        </CardHeader>
        <CardContent>
          {error ? <ErrorNote message={error} /> : null}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-16">Priority</TableHead>
                <TableHead>Name</TableHead>
                <TableHead>Trigger</TableHead>
                <TableHead>Beneficiary</TableHead>
                <TableHead>Amount</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? (
                <TableEmpty colSpan={7}>Loading rules…</TableEmpty>
              ) : sorted.length === 0 ? (
                <TableEmpty colSpan={7}>
                  No commission rules yet — create the first one above.
                </TableEmpty>
              ) : (
                sorted.map((rule) => (
                  <TableRow key={rule.rule_id}>
                    <TableCell className="text-muted-foreground">
                      {rule.priority}
                    </TableCell>
                    <TableCell className="font-medium">{rule.name}</TableCell>
                    <TableCell>{triggerLabel(rule.trigger)}</TableCell>
                    <TableCell>{titleCase(rule.beneficiary)}</TableCell>
                    <TableCell>{ruleAmountLabel(rule)}</TableCell>
                    <TableCell>
                      <Badge variant={rule.active ? "success" : "secondary"}>
                        {rule.active ? "Active" : "Inactive"}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => {
                            setEditingId(rule.rule_id);
                            setDraft(draftFromRule(rule));
                          }}
                        >
                          <Pencil className="h-3.5 w-3.5" />
                          Edit
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => void toggleActive(rule)}
                        >
                          {rule.active ? "Deactivate" : "Activate"}
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => void remove(rule)}
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                          Delete
                        </Button>
                      </div>
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
