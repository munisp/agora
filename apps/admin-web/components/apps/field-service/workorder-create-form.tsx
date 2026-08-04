"use client";

/**
 * New work-order form (SPEC-W19 Agent B): title, description, optional
 * scheduled window, GPS fix and a comma-separated checklist shorthand.
 * Orders are created in the "created" lane; dispatch happens from the
 * board detail panel.
 */
import * as React from "react";
import { Button } from "@/components/ui/button";
import { Input, Label, Textarea } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

export interface NewWorkOrder {
  title: string;
  description?: string;
  scheduled_start?: string;
  scheduled_end?: string;
  gps?: { lat: number; lng: number; accuracy?: number };
  checklist?: { label: string; done: boolean }[];
}

export function WorkOrderCreateForm({
  busy,
  onCreate,
  onCancel,
}: {
  busy: boolean;
  onCreate: (input: NewWorkOrder) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [title, setTitle] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [start, setStart] = React.useState("");
  const [end, setEnd] = React.useState("");
  const [lat, setLat] = React.useState("");
  const [lng, setLng] = React.useState("");
  const [checklistRaw, setChecklistRaw] = React.useState("");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const input: NewWorkOrder = { title: title.trim() };
    if (description.trim()) input.description = description.trim();
    if (start) input.scheduled_start = new Date(start).toISOString();
    if (end) input.scheduled_end = new Date(end).toISOString();
    if (lat.trim() !== "" && lng.trim() !== "") {
      const la = Number(lat);
      const ln = Number(lng);
      if (!Number.isFinite(la) || !Number.isFinite(ln)) return;
      input.gps = { lat: la, lng: ln };
    }
    const items = checklistRaw
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean)
      .map((label) => ({ label, done: false }));
    if (items.length > 0) input.checklist = items;
    const ok = await onCreate(input);
    if (ok) {
      setTitle("");
      setDescription("");
      setStart("");
      setEnd("");
      setLat("");
      setLng("");
      setChecklistRaw("");
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">New work order</CardTitle>
        <CardDescription>
          Created in the &quot;Created&quot; lane — dispatch it to a team member
          from the board.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="space-y-3">
          <div className="space-y-1">
            <Label htmlFor="wo-title">Title</Label>
            <Input
              id="wo-title"
              required
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="Fix AC unit — Lekki office"
            />
          </div>
          <div className="space-y-1">
            <Label htmlFor="wo-desc">Description</Label>
            <Textarea
              id="wo-desc"
              rows={2}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Access notes, parts required, …"
            />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="wo-start">Scheduled start</Label>
              <Input
                id="wo-start"
                type="datetime-local"
                value={start}
                onChange={(e) => setStart(e.target.value)}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="wo-end">Scheduled end</Label>
              <Input
                id="wo-end"
                type="datetime-local"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
              />
            </div>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="wo-lat">GPS latitude</Label>
              <Input
                id="wo-lat"
                inputMode="decimal"
                value={lat}
                onChange={(e) => setLat(e.target.value)}
                placeholder="6.5244"
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="wo-lng">GPS longitude</Label>
              <Input
                id="wo-lng"
                inputMode="decimal"
                value={lng}
                onChange={(e) => setLng(e.target.value)}
                placeholder="3.3792"
              />
            </div>
          </div>
          <div className="space-y-1">
            <Label htmlFor="wo-checklist">Checklist (one item per line)</Label>
            <Textarea
              id="wo-checklist"
              rows={3}
              value={checklistRaw}
              onChange={(e) => setChecklistRaw(e.target.value)}
              placeholder={"Inspect unit\nReplace filter\nTest cooling"}
            />
          </div>
          <div className="flex items-center gap-2">
            <Button type="submit" disabled={busy || !title.trim()}>
              Create work order
            </Button>
            <Button type="button" variant="ghost" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}
