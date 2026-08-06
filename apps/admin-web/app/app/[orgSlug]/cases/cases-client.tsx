"use client";

/**
 * SPEC-W32 WS-C: civic cases operator console — triage queue + category &
 * routing config.
 *
 * All data flows through the tenant-authed /api/civic/* gateway mount (JWT
 * attached by the BFF; tenant resolved from the X-Tenant-Slug header):
 *
 *   GET  /api/civic/cases?status=&category=&ward=&sla_breach=&q=   queue
 *   GET  /api/civic/categories                                   filter/config
 *
 * The civic module ships in parallel (SPEC §3 WS-A); a 404 from either read
 * renders a clean empty state instead of an error wall.
 */
// NAV: orchestrator adds Cases item (FileWarning icon)
import * as React from "react";
import { FileWarning, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useToast } from "@/components/ui/toast";
import { CasesTable } from "@/components/cases/cases-table";
import { CaseDetail } from "@/components/cases/case-detail";
import { CategoryConfig } from "@/components/cases/category-config";
import {
  CASE_STATUSES,
  CIVIC_API,
  SLA_BREACH_FILTERS,
  normalizeCase,
  normalizeCategory,
  unwrapList,
  type CivicCase,
  type CivicCategory,
} from "@/components/cases/types";

export function CasesClient({
  orgSlug,
  canRevealReporter,
}: {
  orgSlug: string;
  /** owner/admin only — unmask anonymous reporters (SPEC §4 gate 4). */
  canRevealReporter: boolean;
}) {
  const { toast } = useToast();
  const [cases, setCases] = React.useState<CivicCase[]>([]);
  const [categories, setCategories] = React.useState<CivicCategory[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [unavailable, setUnavailable] = React.useState(false);

  const [status, setStatus] = React.useState("");
  const [category, setCategory] = React.useState("");
  const [ward, setWard] = React.useState("");
  const [slaBreach, setSlaBreach] = React.useState("");
  const [q, setQ] = React.useState("");

  const [selectedIds, setSelectedIds] = React.useState<Set<string>>(new Set());
  const [bulkAssignee, setBulkAssignee] = React.useState("");
  const [bulkBusy, setBulkBusy] = React.useState(false);

  const [selected, setSelected] = React.useState<CivicCase | null>(null);
  const [drawerOpen, setDrawerOpen] = React.useState(false);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>(
          `${CIVIC_API}/cases`,
          {
            tenant: orgSlug,
            status: status || undefined,
            category: category || undefined,
            ward: ward || undefined,
            sla_breach: slaBreach || undefined,
            q: q || undefined,
          },
          signal,
        );
        if (signal?.aborted) return;
        setCases(unwrapList<unknown>(data).map(normalizeCase));
        setUnavailable(false);
      } catch (e) {
        if (signal?.aborted) return;
        setCases([]);
        if (e instanceof ApiError && e.status === 404) {
          // Civic module not deployed on this workspace yet — clean empty
          // state, not an error (the console must degrade gracefully).
          setUnavailable(true);
        } else {
          setUnavailable(false);
          setError("Cases unavailable — the booking service may be offline.");
        }
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, status, category, ward, slaBreach, q],
  );

  const loadCategories = React.useCallback(
    async (signal?: AbortSignal) => {
      try {
        const data = await api.get<unknown>(
          `${CIVIC_API}/categories`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setCategories(unwrapList<unknown>(data).map(normalizeCategory));
      } catch {
        if (!signal?.aborted) setCategories([]);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  React.useEffect(() => {
    const controller = new AbortController();
    void loadCategories(controller.signal);
    return () => controller.abort();
  }, [loadCategories]);

  // Ward suggestions for the free-text ward filter, from the loaded queue.
  const wardOptions = React.useMemo(() => {
    const set = new Set<string>();
    for (const c of cases) if (c.ward) set.add(c.ward);
    return [...set].sort();
  }, [cases]);

  const toggleSelect = (id: string, on: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  };

  const toggleSelectAll = (on: boolean) => {
    setSelectedIds(on ? new Set(cases.map((c) => c.id)) : new Set());
  };

  const bulkAssign = async () => {
    const assignee = bulkAssignee.trim();
    if (!assignee || selectedIds.size === 0) return;
    setBulkBusy(true);
    let ok = 0;
    let failed = 0;
    for (const id of selectedIds) {
      try {
        await api.post(
          `${CIVIC_API}/cases/${encodeURIComponent(id)}/assign`,
          { assignee },
          { tenant: orgSlug },
        );
        ok += 1;
      } catch {
        failed += 1;
      }
    }
    setBulkBusy(false);
    setSelectedIds(new Set());
    setBulkAssignee("");
    toast({
      title: `Bulk assign: ${ok} case${ok === 1 ? "" : "s"} assigned to ${assignee}`,
      variant: failed > 0 ? "warning" : "success",
      description: failed > 0 ? `${failed} failed — refresh to see current state.` : undefined,
    });
    void load();
  };

  return (
    <div className="max-w-6xl space-y-4">
      <PageHeader
        title="Civic cases"
        description="Citizen reports routed to MDAs — triage, assign, track SLAs and merge duplicates. SLA chips run sage → amber → terracotta as deadlines approach."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void load();
              void loadCategories();
            }}
            disabled={loading}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

      <Tabs defaultValue="queue">
        <TabsList>
          <TabsTrigger value="queue">Triage queue</TabsTrigger>
          <TabsTrigger value="config">Categories & routing</TabsTrigger>
        </TabsList>

        <TabsContent value="queue">
          <div className="space-y-4">
            <div className="flex flex-wrap items-end gap-3 rounded-md border border-border bg-card px-3 py-3">
              <div className="space-y-1">
                <Label htmlFor="cases-status" className="text-xs">
                  Status
                </Label>
                <Select
                  id="cases-status"
                  className="w-36"
                  value={status}
                  onChange={(e) => setStatus(e.target.value)}
                >
                  <option value="">All statuses</option>
                  {CASE_STATUSES.map((s) => (
                    <option key={s.value} value={s.value}>
                      {s.label}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="cases-category" className="text-xs">
                  Category
                </Label>
                <Select
                  id="cases-category"
                  className="w-40"
                  value={category}
                  onChange={(e) => setCategory(e.target.value)}
                >
                  <option value="">All categories</option>
                  {categories.map((c) => (
                    <option key={c.id || c.slug} value={c.slug || c.id}>
                      {c.name}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="cases-ward" className="text-xs">
                  Ward
                </Label>
                <Input
                  id="cases-ward"
                  className="w-36"
                  placeholder="Any ward"
                  list="cases-ward-options"
                  value={ward}
                  onChange={(e) => setWard(e.target.value)}
                />
                <datalist id="cases-ward-options">
                  {wardOptions.map((w) => (
                    <option key={w} value={w} />
                  ))}
                </datalist>
              </div>
              <div className="space-y-1">
                <Label htmlFor="cases-sla" className="text-xs">
                  SLA breach
                </Label>
                <Select
                  id="cases-sla"
                  className="w-36"
                  value={slaBreach}
                  onChange={(e) => setSlaBreach(e.target.value)}
                >
                  <option value="">No breach filter</option>
                  {SLA_BREACH_FILTERS.map((f) => (
                    <option key={f.value} value={f.value}>
                      {f.label}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="cases-q" className="text-xs">
                  Search
                </Label>
                <Input
                  id="cases-q"
                  className="w-48"
                  placeholder="Ref or description…"
                  value={q}
                  onChange={(e) => setQ(e.target.value)}
                />
              </div>
            </div>

            {selectedIds.size > 0 ? (
              <div className="flex flex-wrap items-center gap-2 rounded-md border border-border bg-muted px-3 py-2 text-sm">
                <span className="font-medium">
                  {selectedIds.size} selected
                </span>
                <Input
                  className="h-8 w-48"
                  placeholder="Assign to (operator)…"
                  value={bulkAssignee}
                  onChange={(e) => setBulkAssignee(e.target.value)}
                  aria-label="Bulk assign assignee"
                />
                <Button
                  size="sm"
                  onClick={() => void bulkAssign()}
                  disabled={bulkBusy || !bulkAssignee.trim()}
                >
                  {bulkBusy ? "Assigning…" : "Assign selected"}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setSelectedIds(new Set())}
                >
                  Clear
                </Button>
              </div>
            ) : null}

            <CasesTable
              cases={cases}
              categories={categories}
              loading={loading}
              unavailable={unavailable}
              canRevealReporter={canRevealReporter}
              selectedIds={selectedIds}
              onToggleSelect={toggleSelect}
              onToggleSelectAll={toggleSelectAll}
              onOpen={(c) => {
                setSelected(c);
                setDrawerOpen(true);
              }}
            />
          </div>
        </TabsContent>

        <TabsContent value="config">
          <CategoryConfig
            orgSlug={orgSlug}
            categories={categories}
            onCategoriesChanged={() => void loadCategories()}
          />
        </TabsContent>
      </Tabs>

      {unavailable && !loading ? (
        <div className="flex items-start gap-3 rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
          <FileWarning className="mt-0.5 h-5 w-5 shrink-0" />
          <div>
            <p className="font-medium text-foreground">
              Civic reporting is not enabled on this workspace yet.
            </p>
            <p className="mt-1">
              Once the civic module is deployed, citizen reports from the
              public portal will appear here for triage.
            </p>
          </div>
        </div>
      ) : null}

      <CaseDetail
        orgSlug={orgSlug}
        caseItem={selected}
        categories={categories}
        canRevealReporter={canRevealReporter}
        open={drawerOpen}
        onOpenChange={(open) => {
          setDrawerOpen(open);
          if (!open) setSelected(null);
        }}
        onChanged={() => void load()}
      />
    </div>
  );
}
