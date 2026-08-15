"use client";

/**
 * Segment builder (SPEC-W19 Agent D): filter-rows editor over the segment
 * definition jsonb {filters:[{field,op,value}]}, with the read-only count
 * evaluation (POST /segments/{id}/count — 100k-row ceiling documented
 * server-side; the response carries truncated=true when hit).
 */

import * as React from "react";
import { Plus, Trash2, Users } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import {
  FILTER_FIELDS,
  FILTER_OPS,
  STUDIO_API,
  type Segment,
  type SegmentFilter,
} from "./types";

type FilterRow = { field: string; op: SegmentFilter["op"]; value: string };

function rowsFromDefinition(seg: Segment | null): FilterRow[] {
  if (!seg) return [{ field: "source", op: "eq", value: "" }];
  return seg.definition.filters.map((f) => ({
    field: f.field,
    op: f.op,
    value: Array.isArray(f.value) ? (f.value as string[]).join(", ") : String(f.value ?? ""),
  }));
}

function definitionFromRows(rows: FilterRow[]): { filters: SegmentFilter[] } {
  return {
    filters: rows.map((r) => ({
      field: r.field,
      op: r.op,
      value:
        r.op === "in"
          ? r.value.split(",").map((v) => v.trim()).filter(Boolean)
          : r.value,
    })),
  };
}

export function SegmentBuilder({
  orgSlug,
  canManage,
  segments,
  onChanged,
}: {
  orgSlug: string;
  canManage: boolean;
  segments: Segment[];
  onChanged: () => void;
}) {
  const { toast } = useToast();
  const [selectedId, setSelectedId] = React.useState<string | null>(null);

  const [name, setName] = React.useState("");
  const [rows, setRows] = React.useState<FilterRow[]>(rowsFromDefinition(null));
  const [saving, setSaving] = React.useState(false);
  const [counting, setCounting] = React.useState(false);
  const [countResult, setCountResult] = React.useState<string | null>(null);
  const [formError, setFormError] = React.useState<string | null>(null);

  const editSegment = (seg: Segment | null) => {
    setSelectedId(seg?.id ?? null);
    setName(seg?.name ?? "");
    setRows(rowsFromDefinition(seg));
    setCountResult(null);
    setFormError(null);
  };

  const setRow = (idx: number, patch: Partial<FilterRow>) =>
    setRows((rs) => rs.map((r, i) => (i === idx ? { ...r, ...patch } : r)));

  const save = async () => {
    setFormError(null);
    if (!name.trim()) {
      setFormError("Name is required.");
      return;
    }
    if (rows.some((r) => !r.value.trim())) {
      setFormError("Every filter row needs a value.");
      return;
    }
    setSaving(true);
    try {
      const body = { name: name.trim(), definition: definitionFromRows(rows) };
      if (selectedId) {
        await api.patch(`${STUDIO_API}/segments/${selectedId}`, body, { tenant: orgSlug });
        toast({ title: "Segment updated" });
      } else {
        await api.post(`${STUDIO_API}/segments`, body, { tenant: orgSlug });
        toast({ title: "Segment created" });
      }
      editSegment(null);
      onChanged();
    } catch (e) {
      setFormError(e instanceof ApiError ? e.message : "Save failed.");
    } finally {
      setSaving(false);
    }
  };

  const runCount = async (seg: Segment) => {
    setCounting(true);
    setCountResult(null);
    try {
      const res = await api.post<{ count: number; truncated: boolean }>(
        `${STUDIO_API}/segments/${seg.id}/count`,
        {},
        { tenant: orgSlug },
      );
      setCountResult(
        res.truncated
          ? `≈ ${res.count.toLocaleString()} contacts (100k-row scan ceiling hit — narrow the filters)`
          : `${res.count.toLocaleString()} contacts`,
      );
      onChanged(); // approx_count stamped server-side
    } catch (e) {
      setCountResult(e instanceof ApiError ? e.message : "Count failed.");
    } finally {
      setCounting(false);
    }
  };

  return (
    <div className="grid gap-6 lg:grid-cols-2">
      {/* Segment list */}
      <div>
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-medium">Segments ({segments.length})</h2>
          {canManage ? (
            <Button size="sm" variant="outline" onClick={() => editSegment(null)}>
              <Plus className="mr-1 h-3.5 w-3.5" /> New segment
            </Button>
          ) : null}
        </div>
        {segments.length === 0 ? (
          <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            No segments yet — build one from contact and lead attributes.
          </p>
        ) : (
          <ul className="space-y-2">
            {segments.map((seg) => (
              <li
                key={seg.id}
                className="flex items-center justify-between gap-3 rounded-md border p-3"
              >
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium">{seg.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {seg.definition.filters.length} filter
                    {seg.definition.filters.length === 1 ? "" : "s"} · ≈{" "}
                    {seg.approx_count.toLocaleString()} contacts
                  </p>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={counting}
                    onClick={() => void runCount(seg)}
                  >
                    <Users className="mr-1 h-3.5 w-3.5" /> Count
                  </Button>
                  {canManage ? (
                    <Button size="sm" variant="outline" onClick={() => editSegment(seg)}>
                      Edit
                    </Button>
                  ) : null}
                </div>
              </li>
            ))}
          </ul>
        )}
        {countResult ? (
          <p className="mt-3 rounded-md bg-muted px-3 py-2 text-sm">{countResult}</p>
        ) : null}
      </div>

      {/* Builder form */}
      {canManage ? (
        <div className="rounded-md border p-4">
          <h2 className="mb-3 text-sm font-medium">
            {selectedId ? "Edit segment" : "New segment"}
          </h2>
          <div className="space-y-3">
            <div>
              <Label htmlFor="seg-name">Name</Label>
              <Input
                id="seg-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. Qualified leads from the web"
              />
            </div>
            <div className="space-y-2">
              <Label>Filters (all must match)</Label>
              {rows.map((row, idx) => (
                <div key={idx} className="flex items-center gap-2">
                  <Select
                    aria-label="Field"
                    value={row.field}
                    onChange={(e) => setRow(idx, { field: e.target.value })}
                    className="w-44"
                  >
                    {FILTER_FIELDS.map((f) => (
                      <option key={f.value} value={f.value}>
                        {f.label}
                      </option>
                    ))}
                  </Select>
                  <Select
                    aria-label="Operator"
                    value={row.op}
                    onChange={(e) => setRow(idx, { op: e.target.value as SegmentFilter["op"] })}
                    className="w-48"
                  >
                    {FILTER_OPS.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </Select>
                  <Input
                    aria-label="Value"
                    value={row.value}
                    onChange={(e) => setRow(idx, { value: e.target.value })}
                    placeholder={row.op === "in" ? "new, qualified" : "value"}
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label="Remove filter"
                    disabled={rows.length <= 1}
                    onClick={() => setRows((rs) => rs.filter((_, i) => i !== idx))}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                </div>
              ))}
              <Button
                size="sm"
                variant="outline"
                onClick={() => setRows((rs) => [...rs, { field: "source", op: "eq", value: "" }])}
              >
                <Plus className="mr-1 h-3.5 w-3.5" /> Add filter
              </Button>
            </div>
            {formError ? <p className="text-sm text-destructive">{formError}</p> : null}
            <div className="flex gap-2">
              <Button disabled={saving} onClick={() => void save()}>
                {saving ? "Saving…" : selectedId ? "Save changes" : "Create segment"}
              </Button>
              {selectedId ? (
                <Button variant="outline" onClick={() => editSegment(null)}>
                  Cancel edit
                </Button>
              ) : null}
            </div>
          </div>
        </div>
      ) : null}
    </div>
  );
}
