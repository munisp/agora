"use client";

import * as React from "react";
import { Plus, Pencil, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  ConfirmDialog,
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Textarea } from "@/components/ui/input";
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
import { formatMoney } from "@/lib/utils";
import type { Offering } from "@/lib/types";

interface OfferingForm {
  name: string;
  description: string;
  duration_min: string;
  buffer_min: string;
  price: string; // major units, e.g. "45.00"
  capacity: string;
  bookable: boolean;
}

const emptyForm: OfferingForm = {
  name: "",
  description: "",
  duration_min: "60",
  buffer_min: "0",
  price: "0.00",
  capacity: "1",
  bookable: true,
};

function toForm(o: Offering): OfferingForm {
  return {
    name: o.name,
    description: o.description,
    duration_min: String(o.duration_min),
    buffer_min: String(o.buffer_min),
    price: (o.price_cents / 100).toFixed(2),
    capacity: String(o.capacity),
    bookable: o.bookable,
  };
}

export function OfferingsClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [offerings, setOfferings] = React.useState<Offering[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [editing, setEditing] = React.useState<Offering | "new" | null>(null);
  const [deleting, setDeleting] = React.useState<Offering | null>(null);
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<Offering[] | { items: Offering[] }>(
        "/api/bookings/v1/offerings",
        { tenant: orgSlug },
      );
      setOfferings(Array.isArray(data) ? data : (data.items ?? []));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load offerings.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const save = async (form: OfferingForm) => {
    setBusy(true);
    const payload = {
      name: form.name.trim(),
      description: form.description.trim(),
      duration_min: Math.max(5, parseInt(form.duration_min, 10) || 0),
      buffer_min: Math.max(0, parseInt(form.buffer_min, 10) || 0),
      price_cents: Math.round(parseFloat(form.price || "0") * 100),
      capacity: Math.max(1, parseInt(form.capacity, 10) || 1),
      bookable: form.bookable,
    };
    try {
      if (editing === "new") {
        await api.post("/api/bookings/v1/offerings", payload, { tenant: orgSlug });
        toast({ title: "Offering created", variant: "success" });
      } else if (editing) {
        await api.patch(`/api/bookings/v1/offerings/${editing.id}`, payload, {
          tenant: orgSlug,
        });
        toast({ title: "Offering updated", variant: "success" });
      }
      setEditing(null);
      await load();
    } catch (e) {
      toast({
        title: "Save failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const toggleBookable = async (o: Offering) => {
    try {
      // Send the full record (not just {bookable}): the W47-A PATCH alias
      // maps to the PUT handler, which validates a complete body.
      await api.patch(
        `/api/bookings/v1/offerings/${o.id}`,
        {
          name: o.name,
          description: o.description,
          duration_min: o.duration_min,
          buffer_min: o.buffer_min,
          price_cents: o.price_cents,
          currency: o.currency,
          capacity: o.capacity,
          bookable: !o.bookable,
        },
        { tenant: orgSlug },
      );
      setOfferings((prev) =>
        prev.map((p) => (p.id === o.id ? { ...p, bookable: !p.bookable } : p)),
      );
    } catch (e) {
      toast({
        title: "Could not toggle bookable",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    }
  };

  const remove = async () => {
    if (!deleting) return;
    setBusy(true);
    try {
      await api.delete(`/api/bookings/v1/offerings/${deleting.id}`, {
        tenant: orgSlug,
      });
      toast({ title: "Offering deleted", variant: "success" });
      setDeleting(null);
      await load();
    } catch (e) {
      toast({
        title: "Delete failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Offerings"
        description="The services your receptionist can book: duration, buffer, price and capacity."
        actions={
          <Button size="sm" onClick={() => setEditing("new")}>
            <Plus className="h-4 w-4" /> New offering
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Name</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead>Buffer</TableHead>
              <TableHead>Price</TableHead>
              <TableHead>Capacity</TableHead>
              <TableHead>Bookable</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {offerings.length === 0 ? (
              <TableEmpty colSpan={7}>
                {loading ? "Loading…" : "No offerings yet — create the first one."}
              </TableEmpty>
            ) : (
              offerings.map((o) => (
                <TableRow key={o.id}>
                  <TableCell className="pl-5">
                    <div className="font-medium">{o.name}</div>
                    {o.description ? (
                      <div className="max-w-72 truncate text-xs text-muted-foreground">
                        {o.description}
                      </div>
                    ) : null}
                  </TableCell>
                  <TableCell>{o.duration_min} min</TableCell>
                  <TableCell>{o.buffer_min} min</TableCell>
                  <TableCell>{formatMoney(o.price_cents, o.currency)}</TableCell>
                  <TableCell>{o.capacity}</TableCell>
                  <TableCell>
                    <button
                      onClick={() => void toggleBookable(o)}
                      aria-label="Toggle bookable"
                      className="cursor-pointer"
                    >
                      <Badge variant={o.bookable ? "success" : "secondary"}>
                        {o.bookable ? "Bookable" : "Hidden"}
                      </Badge>
                    </button>
                  </TableCell>
                  <TableCell className="pr-5">
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label="Edit"
                        onClick={() => setEditing(o)}
                      >
                        <Pencil className="h-4 w-4" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label="Delete"
                        className="text-destructive"
                        onClick={() => setDeleting(o)}
                      >
                        <Trash2 className="h-4 w-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      <OfferingDialog
        editing={editing}
        busy={busy}
        onClose={() => setEditing(null)}
        onSave={save}
      />
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete “${deleting?.name ?? ""}”?`}
        description="Future bookings for this offering remain on record, but the receptionist will stop offering it immediately."
        confirmLabel="Delete offering"
        destructive
        busy={busy}
        onConfirm={remove}
      />
    </div>
  );
}

function OfferingDialog({
  editing,
  busy,
  onClose,
  onSave,
}: {
  editing: Offering | "new" | null;
  busy: boolean;
  onClose: () => void;
  onSave: (form: OfferingForm) => void;
}) {
  const [form, setForm] = React.useState<OfferingForm>(emptyForm);

  React.useEffect(() => {
    if (editing === "new") setForm(emptyForm);
    else if (editing) setForm(toForm(editing));
  }, [editing]);

  const set = <K extends keyof OfferingForm>(key: K, value: OfferingForm[K]) =>
    setForm((f) => ({ ...f, [key]: value }));

  const valid = form.name.trim().length > 0 && Number(form.duration_min) > 0;

  return (
    <Dialog open={editing !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>
            {editing === "new" ? "New offering" : "Edit offering"}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="off-name">Name</Label>
            <Input
              id="off-name"
              value={form.name}
              onChange={(e) => set("name", e.target.value)}
              placeholder="Initial consultation"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="off-desc">Description</Label>
            <Textarea
              id="off-desc"
              value={form.description}
              onChange={(e) => set("description", e.target.value)}
              placeholder="What the guest gets, what to prepare…"
            />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="off-duration">Duration (min)</Label>
              <Input
                id="off-duration"
                type="number"
                min={5}
                value={form.duration_min}
                onChange={(e) => set("duration_min", e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="off-buffer">Buffer (min)</Label>
              <Input
                id="off-buffer"
                type="number"
                min={0}
                value={form.buffer_min}
                onChange={(e) => set("buffer_min", e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="off-price">Price</Label>
              <Input
                id="off-price"
                type="number"
                min={0}
                step="0.01"
                value={form.price}
                onChange={(e) => set("price", e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="off-capacity">Capacity</Label>
              <Input
                id="off-capacity"
                type="number"
                min={1}
                value={form.capacity}
                onChange={(e) => set("capacity", e.target.value)}
              />
            </div>
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={form.bookable}
              onChange={(e) => set("bookable", e.target.checked)}
              className="h-4 w-4 accent-primary"
            />
            Bookable by the receptionist and on the public site
          </label>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={() => onSave(form)} disabled={busy || !valid}>
            {busy ? "Saving…" : "Save offering"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
