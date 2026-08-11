"use client";

import { Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import type { CaptureField, CaptureFieldType } from "@/lib/types";

const FIELD_TYPES: CaptureFieldType[] = ["string", "number", "boolean", "enum"];

const EMPTY_FIELD: CaptureField = {
  key: "",
  label: "",
  type: "string",
  required: false,
};

/**
 * SPEC-W38 F3 — capture-schema field editor. Used by the wizard (step 6) and
 * the agent edit page. `options` is edited as a comma-separated list and only
 * shown for enum fields.
 */
export function SchemaFieldEditor({
  fields,
  onChange,
}: {
  fields: CaptureField[];
  onChange: (fields: CaptureField[]) => void;
}) {
  const setField = (i: number, patch: Partial<CaptureField>) =>
    onChange(fields.map((f, j) => (j === i ? { ...f, ...patch } : f)));

  const setType = (i: number, type: CaptureFieldType) =>
    setField(i, {
      type,
      ...(type === "enum" ? { options: fields[i].options ?? [] } : {}),
    });

  const setOptions = (i: number, raw: string) =>
    setField(i, {
      options: raw
        .split(",")
        .map((o) => o.trim())
        .filter(Boolean),
    });

  return (
    <div className="grid gap-3">
      {fields.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          No capture fields yet — add the details the agent should pull out of
          each call (e.g. caller name, reason, callback preference).
        </p>
      ) : (
        fields.map((field, i) => (
          <div
            key={i}
            className="grid gap-2 rounded-lg border border-border bg-muted/40 p-3"
          >
            <div className="grid gap-2 sm:grid-cols-[1fr_1fr_140px_auto]">
              <div className="grid gap-1">
                <Label htmlFor={`field-key-${i}`} className="text-xs">
                  Key
                </Label>
                <Input
                  id={`field-key-${i}`}
                  value={field.key}
                  onChange={(e) =>
                    setField(i, {
                      key: e.target.value
                        .toLowerCase()
                        .replace(/[^a-z0-9]+/g, "_")
                        .replace(/^_+|_+$/g, ""),
                    })
                  }
                  placeholder="caller_name"
                  className="font-mono"
                />
              </div>
              <div className="grid gap-1">
                <Label htmlFor={`field-label-${i}`} className="text-xs">
                  Label
                </Label>
                <Input
                  id={`field-label-${i}`}
                  value={field.label}
                  onChange={(e) => setField(i, { label: e.target.value })}
                  placeholder="Caller name"
                />
              </div>
              <div className="grid gap-1">
                <Label htmlFor={`field-type-${i}`} className="text-xs">
                  Type
                </Label>
                <Select
                  id={`field-type-${i}`}
                  value={field.type}
                  onChange={(e) =>
                    setType(i, e.target.value as CaptureFieldType)
                  }
                >
                  {FIELD_TYPES.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="flex items-end justify-end pb-0.5">
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Remove field"
                  onClick={() => onChange(fields.filter((_, j) => j !== i))}
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-4">
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={field.required}
                  onChange={(e) => setField(i, { required: e.target.checked })}
                  className="h-4 w-4 accent-primary"
                />
                Required
              </label>
              {field.type === "enum" ? (
                <div className="flex min-w-0 flex-1 items-center gap-2">
                  <Label htmlFor={`field-options-${i}`} className="text-xs">
                    Options
                  </Label>
                  <Input
                    id={`field-options-${i}`}
                    value={(field.options ?? []).join(", ")}
                    onChange={(e) => setOptions(i, e.target.value)}
                    placeholder="low, medium, high"
                    className="h-8 flex-1"
                  />
                </div>
              ) : null}
            </div>
          </div>
        ))
      )}
      <div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => onChange([...fields, { ...EMPTY_FIELD }])}
        >
          <Plus className="h-4 w-4" /> Add field
        </Button>
      </div>
    </div>
  );
}
