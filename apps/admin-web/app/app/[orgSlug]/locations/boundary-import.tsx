"use client";

/**
 * SPEC-W52 Scope C: bulk import of administrative boundaries (states, LGAs,
 * wards or custom polygons) into the geo-service's `geo_boundaries` table.
 *
 * The form posts multipart/form-data to POST /api/geo/v1/geo/boundaries/import
 * — the admin-web BFF proxy attaches the Keycloak token and forwards to the
 * APISIX gateway, which routes /api/geo/* to the geo-service (terrio parses
 * .geojson/.json/.gpkg/.zip-shapefile server-side, reprojects to EPSG:4326,
 * and upserts on (scope-tenant, kind, code)). The service responds with
 * {imported, skipped, errors[]}; the summary is rendered verbatim below and
 * per-feature errors are surfaced, never swallowed.
 *
 * Attribution reminder: real Nigerian boundary datasets (geoBoundaries
 * CC-BY-4.0, GRID3) require attribution — record it in the Source field.
 */
import * as React from "react";
import { Upload } from "lucide-react";
import { ApiError } from "@/lib/api";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Select } from "@/components/ui/input";

/** Boundary kinds accepted by POST /v1/geo/boundaries/import. */
const KINDS = [
  { value: "state", label: "State" },
  { value: "lga", label: "LGA" },
  { value: "ward", label: "Ward" },
  { value: "custom", label: "Custom" },
] as const;

const SCOPES = [
  { value: "tenant", label: "This organisation only" },
  { value: "reference", label: "Platform reference data" },
] as const;

const ACCEPT = ".geojson,.json,.gpkg,.zip";
/** Mirror of the geo-service ingest cap (100 MB). */
const MAX_FILE_BYTES = 100 * 1024 * 1024;

interface ImportResult {
  imported: number;
  skipped: number;
  errors: string[];
}

function asCount(v: unknown): number {
  const n = typeof v === "string" ? Number(v) : v;
  return typeof n === "number" && Number.isFinite(n) ? n : 0;
}

/** Tolerant decode — the service contract is {imported, skipped, errors[]}. */
function normalizeResult(data: unknown): ImportResult {
  const o = (typeof data === "object" && data !== null ? data : {}) as Record<
    string,
    unknown
  >;
  const rawErrors = Array.isArray(o.errors) ? o.errors : [];
  return {
    imported: asCount(o.imported),
    skipped: asCount(o.skipped),
    errors: rawErrors.map((e) =>
      typeof e === "string" ? e : JSON.stringify(e),
    ),
  };
}

export function BoundaryImportDialog({
  orgSlug,
  open,
  onOpenChange,
}: {
  orgSlug: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [file, setFile] = React.useState<File | null>(null);
  const [kind, setKind] = React.useState<string>("lga");
  const [nameField, setNameField] = React.useState("");
  const [codeField, setCodeField] = React.useState("");
  const [source, setSource] = React.useState("");
  const [scope, setScope] = React.useState<string>("tenant");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [result, setResult] = React.useState<ImportResult | null>(null);

  const reset = () => {
    setFile(null);
    setKind("lga");
    setNameField("");
    setCodeField("");
    setSource("");
    setScope("tenant");
    setError(null);
    setResult(null);
  };

  const close = (next: boolean) => {
    if (!next) reset();
    onOpenChange(next);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!file || busy) return;
    if (file.size > MAX_FILE_BYTES) {
      setError("File exceeds the 100 MB ingest cap — split it and retry.");
      return;
    }
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      // Multipart upload through the BFF — api.post is JSON-only, so this
      // fetch mirrors its tenant-header convention (x-tenant-slug) directly.
      const form = new FormData();
      form.append("file", file);
      form.append("kind", kind);
      form.append("name_field", nameField.trim());
      form.append("code_field", codeField.trim());
      if (source.trim()) form.append("source", source.trim());
      form.append("scope", scope);
      const res = await fetch("/api/geo/v1/geo/boundaries/import", {
        method: "POST",
        headers: { "x-tenant-slug": orgSlug, accept: "application/json" },
        body: form,
        cache: "no-store",
      });
      const text = await res.text();
      let data: unknown = null;
      if (text) {
        try {
          data = JSON.parse(text);
        } catch {
          data = text;
        }
      }
      if (!res.ok) throw new ApiError(res.status, data);
      setResult(normalizeResult(data));
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.message
          : "Import failed — the geo-service may be offline.",
      );
    } finally {
      setBusy(false);
    }
  };

  const canSubmit =
    file !== null && nameField.trim() !== "" && codeField.trim() !== "" && !busy;

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent onClose={() => close(false)}>
        <DialogHeader>
          <DialogTitle>Import boundaries</DialogTitle>
          <DialogDescription>
            Upload a GeoJSON, GeoPackage or zipped shapefile of administrative
            boundaries. Each feature&rsquo;s name/code come from the attribute
            fields you name below. Real datasets (geoBoundaries CC-BY-4.0,
            GRID3) require attribution — record it in the source field.
          </DialogDescription>
        </DialogHeader>

        {error ? <ErrorNote message={error} /> : null}

        {result ? (
          <div className="space-y-3">
            <div className="rounded-md border border-border bg-accent px-3 py-2 text-sm">
              <p className="font-medium">
                Imported {result.imported.toLocaleString()} boundar
                {result.imported === 1 ? "y" : "ies"}
                {result.skipped > 0
                  ? ` · ${result.skipped.toLocaleString()} skipped`
                  : ""}
              </p>
              {result.errors.length > 0 ? (
                <p className="mt-0.5 text-xs text-muted-foreground">
                  {result.errors.length} feature
                  {result.errors.length === 1 ? "" : "s"} failed — see below.
                </p>
              ) : null}
            </div>
            {result.errors.length > 0 ? (
              <ul className="max-h-48 space-y-1 overflow-y-auto rounded-md border border-border px-3 py-2 text-xs text-muted-foreground">
                {result.errors.map((msg, i) => (
                  <li key={i} className="break-words">
                    {msg}
                  </li>
                ))}
              </ul>
            ) : null}
            <div className="flex justify-end gap-2">
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  setResult(null);
                  setFile(null);
                }}
              >
                Import another file
              </Button>
              <Button size="sm" onClick={() => close(false)}>
                Done
              </Button>
            </div>
          </div>
        ) : (
          <form onSubmit={(e) => void submit(e)} className="space-y-4">
            <div className="grid gap-1.5">
              <Label htmlFor="boundary-file">Boundary file</Label>
              <Input
                id="boundary-file"
                type="file"
                accept={ACCEPT}
                onChange={(e) => setFile(e.target.files?.[0] ?? null)}
              />
              <p className="text-xs text-muted-foreground">
                {ACCEPT} · up to 100 MB / 50,000 features. Geometries are
                reprojected to WGS84 and stored as MultiPolygon.
              </p>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="grid gap-1.5">
                <Label htmlFor="boundary-kind">Kind</Label>
                <Select
                  id="boundary-kind"
                  value={kind}
                  onChange={(e) => setKind(e.target.value)}
                >
                  {KINDS.map((k) => (
                    <option key={k.value} value={k.value}>
                      {k.label}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="boundary-scope">Scope</Label>
                <Select
                  id="boundary-scope"
                  value={scope}
                  onChange={(e) => setScope(e.target.value)}
                >
                  {SCOPES.map((s) => (
                    <option key={s.value} value={s.value}>
                      {s.label}
                    </option>
                  ))}
                </Select>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="grid gap-1.5">
                <Label htmlFor="boundary-name-field">Name field</Label>
                <Input
                  id="boundary-name-field"
                  value={nameField}
                  onChange={(e) => setNameField(e.target.value)}
                  placeholder="e.g. NAME_2 or lga_name"
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="boundary-code-field">Code field</Label>
                <Input
                  id="boundary-code-field"
                  value={codeField}
                  onChange={(e) => setCodeField(e.target.value)}
                  placeholder="e.g. HASC_2 or ward_code"
                />
              </div>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="boundary-source">Source (attribution)</Label>
              <Input
                id="boundary-source"
                value={source}
                onChange={(e) => setSource(e.target.value)}
                placeholder="e.g. geoBoundaries NGA ADM2, CC-BY-4.0"
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => close(false)}
                disabled={busy}
              >
                Cancel
              </Button>
              <Button type="submit" size="sm" disabled={!canSubmit}>
                <Upload className="h-3.5 w-3.5" />
                {busy ? "Importing…" : "Import"}
              </Button>
            </div>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
