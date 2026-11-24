"use client";

/**
 * SPEC-W117 §4: reusable OpenSearch advanced-search bar. Controlled —
 * the parent owns SearchState and performs the fetch; this component
 * renders the inputs, validates the date range, and shows filter chips.
 * Follow-on waves mount it on bookings/contacts/leads screens with the
 * same props contract.
 */

import * as React from "react";
import { Search, X } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  SEARCH_CHANNELS,
  SEARCH_ROLES,
  describeChips,
  validateDateRange,
  type Chip,
  type SearchState,
} from "@/lib/advanced-search";

export function AdvancedSearchBar({
  state,
  onChange,
  onSearch,
  searching,
  placeholder = "Search text…",
}: {
  state: SearchState;
  onChange: (next: SearchState) => void;
  onSearch: () => void;
  searching: boolean;
  placeholder?: string;
}) {
  const dateError = validateDateRange(state.fromTs, state.toTs);
  const chips = describeChips(state);

  const set = (patch: Partial<SearchState>) =>
    onChange({ ...state, ...patch });

  const clearChip = (key: Chip["key"]) => {
    if (key === "q") set({ q: "" });
    else if (key === "channel") set({ channel: "" });
    else if (key === "role") set({ role: "" });
    else if (key === "fromTs") set({ fromTs: "" });
    else if (key === "toTs") set({ toTs: "" });
  };

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
          placeholder={placeholder}
          aria-label="Search text"
          className={cn(inputClass, "min-w-56 flex-1")}
        />
        <select
          value={state.channel}
          onChange={(e) => set({ channel: e.target.value })}
          aria-label="Channel"
          className={inputClass}
        >
          <option value="">All channels</option>
          {SEARCH_CHANNELS.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
        <select
          value={state.role}
          onChange={(e) => set({ role: e.target.value })}
          aria-label="Role"
          className={inputClass}
        >
          <option value="">All roles</option>
          {SEARCH_ROLES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
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
        <button
          type="submit"
          disabled={searching || Boolean(dateError)}
          className="flex h-9 items-center gap-1.5 rounded-md bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50 cursor-pointer"
        >
          <Search className="h-4 w-4" />
          {searching ? "Searching…" : "Search"}
        </button>
      </div>
      {dateError ? (
        <p className="text-xs text-destructive">{dateError}</p>
      ) : null}
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
                onClick={() => clearChip(chip.key)}
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
