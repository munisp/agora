"use client";

import * as React from "react";
import { Save } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Label, Select } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { cn, minutesToTime, timeToMinutes, WEEKDAYS } from "@/lib/utils";
import type { AvailabilityRule, TeamMember } from "@/lib/types";

interface DayWindow {
  enabled: boolean;
  start: string; // "09:00"
  end: string; // "17:00"
}

type WeekGrid = DayWindow[]; // index 0 = Sunday

const defaultWeek = (): WeekGrid =>
  WEEKDAYS.map((_, i) => ({
    enabled: i >= 1 && i <= 5,
    start: "09:00",
    end: "17:00",
  }));

function rulesToGrid(rules: AvailabilityRule[]): WeekGrid {
  const grid = WEEKDAYS.map(() => ({ enabled: false, start: "09:00", end: "17:00" }));
  for (const r of rules) {
    const toHHMM = (min: number) =>
      `${String(Math.floor(min / 60)).padStart(2, "0")}:${String(min % 60).padStart(2, "0")}`;
    grid[r.weekday] = {
      enabled: true,
      start: toHHMM(r.start_min),
      end: toHHMM(r.end_min),
    };
  }
  return grid;
}

export function AvailabilityClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [members, setMembers] = React.useState<TeamMember[]>([]);
  const [memberId, setMemberId] = React.useState<string>("");
  const [grid, setGrid] = React.useState<WeekGrid>(defaultWeek());
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    (async () => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<TeamMember[] | { items: TeamMember[] }>(
          "/api/bookings/v1/team-members",
          { tenant: orgSlug },
        );
        const items = (Array.isArray(data) ? data : (data.items ?? [])).filter(
          (m) => m.active,
        );
        setMembers(items);
        if (items.length > 0) setMemberId(items[0].id);
      } catch (e) {
        setError(e instanceof ApiError ? e.message : "Failed to load team.");
      } finally {
        setLoading(false);
      }
    })();
  }, [orgSlug]);

  React.useEffect(() => {
    if (!memberId) return;
    (async () => {
      try {
        const data = await api.get<AvailabilityRule[] | { items: AvailabilityRule[] }>(
          `/api/bookings/v1/team-members/${memberId}/availability`,
          { tenant: orgSlug },
        );
        const rules = Array.isArray(data) ? data : (data.items ?? []);
        setGrid(rules.length > 0 ? rulesToGrid(rules) : defaultWeek());
      } catch (e) {
        setError(
          e instanceof ApiError ? e.message : "Failed to load availability.",
        );
      }
    })();
  }, [orgSlug, memberId]);

  const setDay = (weekday: number, patch: Partial<DayWindow>) =>
    setGrid((g) => g.map((d, i) => (i === weekday ? { ...d, ...patch } : d)));

  const invalidDays = grid
    .map((d, i) => ({ d, i }))
    .filter(({ d }) => {
      if (!d.enabled) return false;
      const s = timeToMinutes(d.start);
      const e = timeToMinutes(d.end);
      return s === null || e === null || e <= s;
    })
    .map(({ i }) => WEEKDAYS[i]);

  const save = async () => {
    if (!memberId || invalidDays.length > 0) return;
    setSaving(true);
    const rules = grid
      .map((d, weekday) => ({ d, weekday }))
      .filter(({ d }) => d.enabled)
      .map(({ d, weekday }) => ({
        weekday,
        start_min: timeToMinutes(d.start) ?? 0,
        end_min: timeToMinutes(d.end) ?? 0,
      }));
    try {
      // PUT replaces the member's full weekly rule set.
      await api.put(
        `/api/bookings/v1/team-members/${memberId}/availability`,
        { rules },
        { tenant: orgSlug },
      );
      toast({ title: "Availability saved", variant: "success" });
    } catch (e) {
      toast({
        title: "Save failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Availability"
        description="Weekly booking windows per team member. The receptionist never books outside these."
        actions={
          <Button
            size="sm"
            onClick={() => void save()}
            disabled={saving || !memberId || invalidDays.length > 0}
          >
            <Save className="h-4 w-4" />
            {saving ? "Saving…" : "Save rules"}
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card className="max-w-2xl">
        <CardHeader>
          <div className="grid gap-1.5">
            <Label htmlFor="av-member">Team member</Label>
            <Select
              id="av-member"
              value={memberId}
              onChange={(e) => setMemberId(e.target.value)}
              disabled={loading || members.length === 0}
              className="max-w-xs"
            >
              {members.length === 0 ? (
                <option value="">No active team members</option>
              ) : (
                members.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.name}
                  </option>
                ))
              )}
            </Select>
          </div>
        </CardHeader>
        <CardContent>
          <div className="grid gap-2">
            {WEEKDAYS.map((day, i) => (
              <div
                key={day}
                className={cn(
                  "flex items-center gap-4 rounded-md border border-border px-3 py-2",
                  grid[i].enabled ? "bg-card" : "bg-muted/50",
                )}
              >
                <label className="flex w-32 items-center gap-2 text-sm font-medium">
                  <input
                    type="checkbox"
                    checked={grid[i].enabled}
                    onChange={(e) => setDay(i, { enabled: e.target.checked })}
                    className="h-4 w-4 accent-primary"
                  />
                  {day}
                </label>
                {grid[i].enabled ? (
                  <div className="flex items-center gap-2 text-sm">
                    <input
                      type="time"
                      aria-label={`${day} start`}
                      value={grid[i].start}
                      onChange={(e) => setDay(i, { start: e.target.value })}
                      className="h-8 rounded-md border border-input bg-card px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    />
                    <span className="text-muted-foreground">to</span>
                    <input
                      type="time"
                      aria-label={`${day} end`}
                      value={grid[i].end}
                      onChange={(e) => setDay(i, { end: e.target.value })}
                      className="h-8 rounded-md border border-input bg-card px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    />
                    <span className="ml-2 text-xs text-muted-foreground">
                      {minutesToTime(timeToMinutes(grid[i].start) ?? 0)} –{" "}
                      {minutesToTime(timeToMinutes(grid[i].end) ?? 0)}
                    </span>
                  </div>
                ) : (
                  <span className="text-sm text-muted-foreground">Closed</span>
                )}
              </div>
            ))}
          </div>
          {invalidDays.length > 0 ? (
            <p className="mt-3 text-sm text-destructive">
              End time must be after start time on: {invalidDays.join(", ")}.
            </p>
          ) : null}
        </CardContent>
      </Card>
    </div>
  );
}
