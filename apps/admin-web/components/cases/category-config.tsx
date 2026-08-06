"use client";

/**
 * SPEC-W32 WS-C: category & routing config tab.
 *
 * Categories (civic_categories, SPEC §2): name, slug, MDA dispatch queue,
 * ack/resolve SLA hours, active flag. Routing rules (civic_routing_rules)
 * optionally override the category's default MDA queue for a specific ward
 * — the ward-specific override wins (SPEC §3 WS-A routing precedence).
 *
 *   GET/POST   /api/civic/categories
 *   PATCH      /api/civic/categories/{id}
 *   GET/POST   /api/civic/routing-rules
 *   DELETE     /api/civic/routing-rules/{id}
 *
 * Both reads tolerate 404 (module still rolling out) with an empty state.
 */
import * as React from "react";
import { Loader2, Plus, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import {
  CIVIC_API,
  normalizeRoutingRule,
  unwrapList,
  type CivicCategory,
  type CivicRoutingRule,
} from "./types";

export function CategoryConfig({
  orgSlug,
  categories,
  onCategoriesChanged,
}: {
  orgSlug: string;
  categories: CivicCategory[];
  onCategoriesChanged: () => void;
}) {
  const { toast } = useToast();
  const [rules, setRules] = React.useState<CivicRoutingRule[]>([]);
  const [rulesLoading, setRulesLoading] = React.useState(true);
  const [rulesUnavailable, setRulesUnavailable] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // new category form
  const [name, setName] = React.useState("");
  const [slug, setSlug] = React.useState("");
  const [queue, setQueue] = React.useState("");
  const [ackHours, setAckHours] = React.useState("");
  const [resolveHours, setResolveHours] = React.useState("");

  // new routing rule form
  const [ruleWard, setRuleWard] = React.useState("");
  const [ruleCategory, setRuleCategory] = React.useState("");
  const [ruleQueue, setRuleQueue] = React.useState("");

  const loadRules = React.useCallback(
    async (signal?: AbortSignal) => {
      setRulesLoading(true);
      try {
        const data = await api.get<unknown>(
          `${CIVIC_API}/routing-rules`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setRules(unwrapList<unknown>(data).map(normalizeRoutingRule));
        setRulesUnavailable(false);
      } catch (e) {
        if (signal?.aborted) return;
        setRules([]);
        setRulesUnavailable(e instanceof ApiError && e.status === 404);
      } finally {
        if (!signal?.aborted) setRulesLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void loadRules(controller.signal);
    return () => controller.abort();
  }, [loadRules]);

  const categoryName = (id: string) =>
    categories.find((c) => c.id === id)?.name ?? id;

  const addCategory = async () => {
    if (!name.trim() || !slug.trim()) return;
    setBusy(true);
    try {
      await api.post(
        `${CIVIC_API}/categories`,
        {
          name: name.trim(),
          slug: slug.trim().toLowerCase().replace(/\s+/g, "-"),
          mda_queue: queue.trim() || undefined,
          ack_sla_hours: ackHours ? Number(ackHours) : undefined,
          resolve_sla_hours: resolveHours ? Number(resolveHours) : undefined,
          active: true,
        },
        { tenant: orgSlug },
      );
      toast({ title: `Category “${name.trim()}” added`, variant: "success" });
      setName("");
      setSlug("");
      setQueue("");
      setAckHours("");
      setResolveHours("");
      onCategoriesChanged();
    } catch (e) {
      toast({
        title: "Could not add category",
        description: e instanceof ApiError ? e.message : "Please try again.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const toggleCategory = async (cat: CivicCategory) => {
    setBusy(true);
    try {
      await api.patch(
        `${CIVIC_API}/categories/${encodeURIComponent(cat.id)}`,
        { active: !cat.active },
        { tenant: orgSlug },
      );
      toast({
        title: `${cat.name} ${cat.active ? "deactivated" : "activated"}`,
        variant: "success",
      });
      onCategoriesChanged();
    } catch (e) {
      toast({
        title: "Could not update category",
        description: e instanceof ApiError ? e.message : "Please try again.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const addRule = async () => {
    if (!ruleCategory || !ruleQueue.trim()) return;
    setBusy(true);
    try {
      await api.post(
        `${CIVIC_API}/routing-rules`,
        {
          ward: ruleWard.trim() || undefined,
          category_id: ruleCategory,
          mda_queue: ruleQueue.trim(),
        },
        { tenant: orgSlug },
      );
      toast({ title: "Routing rule added", variant: "success" });
      setRuleWard("");
      setRuleCategory("");
      setRuleQueue("");
      void loadRules();
    } catch (e) {
      toast({
        title: "Could not add routing rule",
        description: e instanceof ApiError ? e.message : "Please try again.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const deleteRule = async (rule: CivicRoutingRule) => {
    setBusy(true);
    try {
      await api.delete(
        `${CIVIC_API}/routing-rules/${encodeURIComponent(rule.id)}`,
        { tenant: orgSlug },
      );
      toast({ title: "Routing rule removed", variant: "success" });
      void loadRules();
    } catch (e) {
      toast({
        title: "Could not remove routing rule",
        description: e instanceof ApiError ? e.message : "Please try again.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle>Categories</CardTitle>
          <p className="text-sm text-muted-foreground">
            What citizens can report and which MDA queue each category
            dispatches to, with ack / resolve SLAs in wall-clock hours.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>MDA queue</TableHead>
                <TableHead>Ack SLA</TableHead>
                <TableHead>Resolve SLA</TableHead>
                <TableHead>Status</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {categories.map((c) => (
                <TableRow key={c.id || c.slug}>
                  <TableCell>
                    <div className="text-sm font-medium">{c.name}</div>
                    <div className="font-mono text-[11px] text-muted-foreground">
                      {c.slug}
                    </div>
                  </TableCell>
                  <TableCell className="text-xs">{c.mda_queue || "—"}</TableCell>
                  <TableCell className="text-xs">
                    {c.ack_sla_hours !== null ? `${c.ack_sla_hours}h` : "—"}
                  </TableCell>
                  <TableCell className="text-xs">
                    {c.resolve_sla_hours !== null ? `${c.resolve_sla_hours}h` : "—"}
                  </TableCell>
                  <TableCell>
                    <Badge variant={c.active ? "success" : "secondary"}>
                      {c.active ? "Active" : "Inactive"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={busy || !c.id}
                      onClick={() => void toggleCategory(c)}
                    >
                      {c.active ? "Deactivate" : "Activate"}
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
              {categories.length === 0 ? (
                <TableEmpty colSpan={6}>
                  No categories yet — or the civic module has not been
                  deployed on this workspace.
                </TableEmpty>
              ) : null}
            </TableBody>
          </Table>

          <div className="space-y-2 rounded-md border border-border bg-muted/40 px-3 py-3">
            <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Add category
            </div>
            <div className="grid grid-cols-2 gap-2">
              <div className="space-y-1">
                <Label htmlFor="cat-name" className="text-xs">Name</Label>
                <Input
                  id="cat-name"
                  placeholder="e.g. Roads"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="cat-slug" className="text-xs">Slug</Label>
                <Input
                  id="cat-slug"
                  placeholder="e.g. roads"
                  value={slug}
                  onChange={(e) => setSlug(e.target.value)}
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="cat-queue" className="text-xs">MDA queue</Label>
                <Input
                  id="cat-queue"
                  placeholder="e.g. works-dept"
                  value={queue}
                  onChange={(e) => setQueue(e.target.value)}
                />
              </div>
              <div className="grid grid-cols-2 gap-2">
                <div className="space-y-1">
                  <Label htmlFor="cat-ack" className="text-xs">Ack SLA (h)</Label>
                  <Input
                    id="cat-ack"
                    type="number"
                    min={0}
                    placeholder="24"
                    value={ackHours}
                    onChange={(e) => setAckHours(e.target.value)}
                  />
                </div>
                <div className="space-y-1">
                  <Label htmlFor="cat-resolve" className="text-xs">Resolve (h)</Label>
                  <Input
                    id="cat-resolve"
                    type="number"
                    min={0}
                    placeholder="72"
                    value={resolveHours}
                    onChange={(e) => setResolveHours(e.target.value)}
                  />
                </div>
              </div>
            </div>
            <Button
              size="sm"
              onClick={() => void addCategory()}
              disabled={busy || !name.trim() || !slug.trim()}
            >
              <Plus className="h-3.5 w-3.5" /> Add category
            </Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Routing rules</CardTitle>
          <p className="text-sm text-muted-foreground">
            Optional ward overrides — a ward-specific rule wins over the
            category&rsquo;s default MDA queue.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Ward</TableHead>
                <TableHead>Category</TableHead>
                <TableHead>Route to</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rules.map((r) => (
                <TableRow key={r.id}>
                  <TableCell className="text-xs">{r.ward ?? "All wards"}</TableCell>
                  <TableCell className="text-xs">{categoryName(r.category_id)}</TableCell>
                  <TableCell className="text-xs font-medium">{r.mda_queue}</TableCell>
                  <TableCell className="text-right">
                    <Button
                      size="icon"
                      variant="ghost"
                      aria-label="Delete routing rule"
                      disabled={busy}
                      onClick={() => void deleteRule(r)}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
              {rules.length === 0 ? (
                <TableEmpty colSpan={4}>
                  {rulesLoading
                    ? "Loading routing rules…"
                    : rulesUnavailable
                      ? "Routing rules are not available yet on this workspace."
                      : "No overrides — every category routes to its default MDA queue."}
                </TableEmpty>
              ) : null}
            </TableBody>
          </Table>

          <div className="space-y-2 rounded-md border border-border bg-muted/40 px-3 py-3">
            <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Add routing rule
            </div>
            <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
              <div className="space-y-1">
                <Label htmlFor="rule-ward" className="text-xs">
                  Ward (blank = all)
                </Label>
                <Input
                  id="rule-ward"
                  placeholder="e.g. Ward 4"
                  value={ruleWard}
                  onChange={(e) => setRuleWard(e.target.value)}
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="rule-category" className="text-xs">Category</Label>
                <Select
                  id="rule-category"
                  value={ruleCategory}
                  onChange={(e) => setRuleCategory(e.target.value)}
                >
                  <option value="">Choose…</option>
                  {categories.map((c) => (
                    <option key={c.id || c.slug} value={c.id}>
                      {c.name}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="rule-queue" className="text-xs">Route to queue</Label>
                <Input
                  id="rule-queue"
                  placeholder="e.g. ward4-rapid-response"
                  value={ruleQueue}
                  onChange={(e) => setRuleQueue(e.target.value)}
                />
              </div>
            </div>
            <Button
              size="sm"
              onClick={() => void addRule()}
              disabled={busy || !ruleCategory || !ruleQueue.trim()}
            >
              {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Plus className="h-3.5 w-3.5" />}
              Add rule
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
