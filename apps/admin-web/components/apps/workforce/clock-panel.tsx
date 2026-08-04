"use client";

/**
 * Clock in/out panel (SPEC-W20 Agent D): pick an agent, optionally attach a
 * GPS fix (manual lat/lng or the browser geolocation when available), then
 * clock in/out. One open entry per agent — the backend answers 409 with
 * the open entry id, 404 when clocking out with none; the parent surfaces
 * those messages. Below the controls, the agent's recent entries render
 * with an explicit "open" badge for entries counted to now.
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/utils";
import type { TeamMember, TimeEntry } from "./types";

export interface ClockInInput {
  agent_id: string;
  method: "web" | "field_pwa";
  gps_lat?: number;
  gps_lng?: number;
}

export function ClockPanel({
  members,
  entries,
  loading,
  busy,
  canWrite,
  onClockIn,
  onClockOut,
  onAgentChange,
}: {
  members: TeamMember[];
  entries: TimeEntry[] | null;
  loading: boolean;
  busy: boolean;
  canWrite: boolean;
  onClockIn: (input: ClockInInput) => Promise<boolean>;
  onClockOut: (agentId: string) => Promise<boolean>;
  onAgentChange: (agentId: string) => void;
}) {
  const [agentId, setAgentId] = React.useState("");
  const [method, setMethod] = React.useState<"web" | "field_pwa">("web");
  const [lat, setLat] = React.useState("");
  const [lng, setLng] = React.useState("");
  const [geoNote, setGeoNote] = React.useState<string | null>(null);

  const openEntry = (entries ?? []).find(
    (e) => e.clock_out_at === null && e.agent_id === agentId,
  );

  const fillFromBrowser = () => {
    setGeoNote(null);
    if (typeof navigator === "undefined" || !navigator.geolocation) {
      setGeoNote("Browser geolocation is not available — enter coordinates manually.");
      return;
    }
    navigator.geolocation.getCurrentPosition(
      (pos) => {
        setLat(pos.coords.latitude.toFixed(6));
        setLng(pos.coords.longitude.toFixed(6));
      },
      () => setGeoNote("Location permission denied — enter coordinates manually."),
      { timeout: 8000 },
    );
  };

  const clockIn = async () => {
    if (!agentId) return;
    const input: ClockInInput = { agent_id: agentId, method };
    if (lat.trim() !== "" && lng.trim() !== "") {
      const la = Number(lat);
      const ln = Number(lng);
      if (!Number.isFinite(la) || !Number.isFinite(ln)) return;
      input.gps_lat = la;
      input.gps_lng = ln;
    }
    await onClockIn(input);
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Time tracking</CardTitle>
        <CardDescription>
          One open entry per agent — a second clock-in is rejected until the
          agent clocks out. Times are UTC.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-3 md:grid-cols-4">
          <div>
            <Label htmlFor="wf-clock-agent">Agent</Label>
            <Select
              id="wf-clock-agent"
              value={agentId}
              onChange={(e) => {
                setAgentId(e.target.value);
                onAgentChange(e.target.value);
              }}
            >
              <option value="">Select an agent…</option>
              {members.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
            </Select>
          </div>
          <div>
            <Label htmlFor="wf-clock-method">Method</Label>
            <Select
              id="wf-clock-method"
              value={method}
              onChange={(e) => setMethod(e.target.value as "web" | "field_pwa")}
            >
              <option value="web">web</option>
              <option value="field_pwa">field_pwa</option>
            </Select>
          </div>
          <div>
            <Label htmlFor="wf-clock-lat">GPS lat (optional)</Label>
            <Input
              id="wf-clock-lat"
              inputMode="decimal"
              value={lat}
              onChange={(e) => setLat(e.target.value)}
              placeholder="6.5244"
            />
          </div>
          <div>
            <Label htmlFor="wf-clock-lng">GPS lng (optional)</Label>
            <Input
              id="wf-clock-lng"
              inputMode="decimal"
              value={lng}
              onChange={(e) => setLng(e.target.value)}
              placeholder="3.3792"
            />
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="button"
            variant="secondary"
            size="sm"
            onClick={fillFromBrowser}
            disabled={busy}
          >
            Use my location
          </Button>
          {canWrite ? (
            <>
              <Button
                type="button"
                size="sm"
                onClick={() => void clockIn()}
                disabled={busy || !agentId || !!openEntry}
              >
                Clock in
              </Button>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                onClick={() => void onClockOut(agentId)}
                disabled={busy || !agentId || !openEntry}
              >
                Clock out
              </Button>
            </>
          ) : null}
          {openEntry ? (
            <Badge variant="warning">
              Clocked in since {formatDateTime(openEntry.clock_in_at)} (open)
            </Badge>
          ) : agentId ? (
            <span className="text-xs text-muted-foreground">No open entry</span>
          ) : null}
        </div>
        {geoNote ? <p className="text-xs text-muted-foreground">{geoNote}</p> : null}

        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Clock in</TableHead>
              <TableHead>Clock out</TableHead>
              <TableHead>Method</TableHead>
              <TableHead>GPS</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableEmpty colSpan={5}>Loading entries…</TableEmpty>
            ) : !entries || entries.length === 0 ? (
              <TableEmpty colSpan={5}>
                {agentId
                  ? "No time entries for this agent yet."
                  : "Select an agent to see their time entries."}
              </TableEmpty>
            ) : (
              entries.map((e) => (
                <TableRow key={e.id}>
                  <TableCell>{formatDateTime(e.clock_in_at)}</TableCell>
                  <TableCell>
                    {e.clock_out_at ? formatDateTime(e.clock_out_at) : "—"}
                  </TableCell>
                  <TableCell>{e.method}</TableCell>
                  <TableCell>
                    {e.gps_lat !== null && e.gps_lng !== null
                      ? `${e.gps_lat.toFixed(4)}, ${e.gps_lng.toFixed(4)}`
                      : "—"}
                  </TableCell>
                  <TableCell>
                    {e.clock_out_at === null ? (
                      <Badge variant="warning">open (counting)</Badge>
                    ) : (
                      <Badge variant="secondary">closed</Badge>
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
