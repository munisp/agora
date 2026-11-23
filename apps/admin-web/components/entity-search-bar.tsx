"use client";

/**
 * SPEC-W119 §4: descriptor-driven entity search bar for the
 * bookings/contacts/leads panels. Controlled — the parent (EntitySearchPanel)
 * owns EntitySearchState and performs the fetch; this component renders the
 * inputs, validates the date range, and shows filter chips. Date inputs
 * render only when the descriptor has a date field (contacts: none).
 */

import * as React from "react";
import { Search, X } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  clearEntityChip,
  describeEntityChips,
  validateEntityDateRange,
  type EntityDescriptor,
  type EntitySearchState,
} from "@/lib/entity-search";

export function EntitySearchBar({
  descriptor,
  state,
  onChange,
  onSearch,
  searching,
}: {
  descriptor: EntityDescriptor;
  state: EntitySearchState;
  onChange: (next: EntitySearchState) => void;
  onSearch: () => void;
  searching: boolean;
}) {
  const dateError = validateEntityDateRange(descriptor, state.fromTs, state.toTs);
  const chips = describeEntityChips(state, descriptor);

  const set = (patch: Partial<EntitySearchState>) => onChange({ ...state, ...patch });
  const setFilter = (key: string, value: string) =>
    onChange({ ...state, filters: { ...state.filters, [key]: value } });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!dateError) onSearch();
  };

  const inputClass =
    "h-9 rounded-md border border-input bg-card px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

  return (
    <form onSubmit={submit} className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <input
          value={state.q}
          onChange={(e) => set({ q: e.target.value })}
          placeholder={descriptor.qPlaceholder}
          aria-label="Search text"
          className={cn(inputClass, "min-w-56 flex-1")}
        />
        {descriptor.selects.map((sel) => (
          <select
            key={sel.key}
            value={state.filters[sel.key] ?? ""}
            onChange={(e) => setFilter(sel.key, e.target.value)}
            aria-label={sel.label}
            className={inputClass}
          >
            <option value="">All {sel.label.toLowerCase()}s</option>
            {sel.options.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </select>
        ))}
        {descriptor.dateField ? (
          <>
            <input
              type="date"
              value={state.fromTs}
              onChange={(e) => set({ fromTs: e.target.value })}
              aria-label="From date"
              className={inputClass}
            />
            <input
              type="date"
              value={state.toTs}
              onChange={(e) => set({ toTs: e.target.value })}
              aria-label="To date"
              className={inputClass}
            />
          </>
        ) : null}
        <button
          type="submit"
          disabled={searching || Boolean(dateError)}
          className="flex h-9 items-center gap-1.5 rounded-md bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50 cursor-pointer"
        >
          <Search className="h-4 w-4" />
          {searching ? "Searching…" : "Search"}
        </button>
      </div>
      {dateError ? <p className="text-xs text-destructive">{dateError}</p> : null}
      {chips.length > 0 ? (
        <div className="flex flex-wrap gap-1.5">
          {chips.map((chip) => (
            <span
              key={chip.key}
              className="inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground"
            >
              {chip.label}
              <button
                type="button"
                onClick={() => onChange(clearEntityChip(state, descriptor, chip.key))}
                aria-label={`Remove filter ${chip.label}`}
                className="rounded-full hover:text-foreground cursor-pointer"
              >
                <X className="h-3 w-3" />
              </button>
            </span>
          ))}
        </div>
      ) : null}
    </form>
  );
}
