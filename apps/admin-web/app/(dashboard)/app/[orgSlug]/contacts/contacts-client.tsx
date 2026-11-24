"use client";

import * as React from "react";
import Link from "next/link";
import { MessageCircle, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { useNavLabels } from "@/components/terminology-provider";
import { ErrorNote } from "@/components/error-note";
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
import { CallButton, PhoneCallDialog, type CallTarget } from "@/components/phone-call";
import type { Contact } from "@/lib/types";
import { EntitySearchPanel } from "@/components/entity-search-panel";
import { CONTACTS_DESCRIPTOR } from "@/lib/entity-search";

const EMPTY_FORM = { name: "", phone: "", email: "", notes: "" };

export function ContactsClient({
  orgSlug,
  initialContacts,
  initialError,
}: {
  orgSlug: string;
  initialContacts: Contact[];
  initialError: string | null;
}) {
  const labels = useNavLabels();
  const { toast } = useToast();
  const [contacts, setContacts] = React.useState<Contact[]>(initialContacts);
  const [error, setError] = React.useState<string | null>(initialError);
  const [loading, setLoading] = React.useState(false);
  const [editing, setEditing] = React.useState<Contact | null>(null);
  const [callTarget, setCallTarget] = React.useState<CallTarget | null>(null); // 2026-11-22: "Call back"
  const [createOpen, setCreateOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState<Contact | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [form, setForm] = React.useState(EMPTY_FORM);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<{ contacts?: Contact[] }>(
        "/api/bookings/v1/contacts",
        { tenant: orgSlug },
      );
      setContacts(data.contacts ?? []);
    } catch (e) {
      setError(
        e instanceof ApiError ? e.message : "Failed to load contacts.",
      );
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  const openCreate = () => {
    setForm(EMPTY_FORM);
    setCreateOpen(true);
  };
  const openEdit = (c: Contact) => {
    setForm({
      name: c.name,
      phone: c.phone,
      email: c.email,
      notes: c.notes,
    });
    setEditing(c);
  };

  const saveCreate = async () => {
    if (!form.name.trim()) return;
    setBusy(true);
    try {
      await api.post<Contact>(
        "/api/bookings/v1/contacts",
        {
          name: form.name.trim(),
          phone: form.phone.trim(),
          email: form.email.trim(),
          notes: form.notes.trim(),
        },
        { tenant: orgSlug },
      );
      toast({ title: "Contact added", variant: "success" });
      setCreateOpen(false);
      await load();
    } catch (e) {
      toast({
        title: "Could not add the contact",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const saveEdit = async () => {
    if (!editing || !form.name.trim()) return;
    setBusy(true);
    try {
      await api.put<Contact>(
        `/api/bookings/v1/contacts/${editing.id}`,
        {
          name: form.name.trim(),
          phone: form.phone.trim(),
          email: form.email.trim(),
          notes: form.notes.trim(),
        },
        { tenant: orgSlug },
      );
      toast({ title: "Contact updated", variant: "success" });
      setEditing(null);
      await load();
    } catch (e) {
      toast({
        title: "Could not update the contact",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!deleting) return;
    setBusy(true);
    try {
      await api.delete(`/api/bookings/v1/contacts/${deleting.id}`, {
        tenant: orgSlug,
      });
      toast({ title: "Contact deleted", variant: "success" });
      setDeleting(null);
      await load();
    } catch (e) {
      toast({
        title: "Could not delete the contact",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const formDialog = (open: boolean, onClose: () => void, title: string, onSave: () => void) => (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="ct-name">Name</Label>
            <Input
              id="ct-name"
              value={form.name}
              onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="ct-phone">Phone</Label>
            <Input
              id="ct-phone"
              type="tel"
              value={form.phone}
              onChange={(e) => setForm((f) => ({ ...f, phone: e.target.value }))}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="ct-email">Email</Label>
            <Input
              id="ct-email"
              type="email"
              value={form.email}
              onChange={(e) => setForm((f) => ({ ...f, email: e.target.value }))}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="ct-notes">Notes</Label>
            <Textarea
              id="ct-notes"
              value={form.notes}
              onChange={(e) => setForm((f) => ({ ...f, notes: e.target.value }))}
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Back
          </Button>
          <Button onClick={onSave} disabled={busy || !form.name.trim()}>
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );

  return (
    <div>
      <PageHeader
        title={labels.contacts}
        description="Your customer directory — the people bookings, leads and conversations resolve against."
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void load()}
              disabled={loading}
            >
              <RefreshCw className={loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
              Refresh
            </Button>
            <Button size="sm" onClick={openCreate}>
              <Plus className="h-4 w-4" />
              Add contact
            </Button>
          </>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      {/* SPEC-W119: OpenSearch advanced search (indexed; alphabetical).
          Contacts carry no date filters — the table has no timestamps. */}
      <div className="mb-4">
        <EntitySearchPanel
          orgSlug={orgSlug}
          descriptor={CONTACTS_DESCRIPTOR}
          title="Advanced search"
          description="Searches the OpenSearch contacts index by name, email or notes, with an optional source filter."
          columns={["Name", "Phone", "Email", "Notes"]}
          hitKey={(h) => String(h.contact_id ?? JSON.stringify(h))}
          renderRow={(h) => (
            <>
              <TableCell className="pl-5 font-medium">{String(h.name ?? "—")}</TableCell>
              <TableCell>{String(h.phone ?? "") || "—"}</TableCell>
              <TableCell>{String(h.email ?? "") || "—"}</TableCell>
              <TableCell className="max-w-64 truncate text-xs text-muted-foreground">
                {String(h.notes ?? "") || "—"}
              </TableCell>
            </>
          )}
        />
      </div>

      {/* SPEC-W82 B1.2: table on ≥sm, stacked cards below. */}
      <Card className="hidden sm:block">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Name</TableHead>
              <TableHead>Phone</TableHead>
              <TableHead>Email</TableHead>
              <TableHead>Notes</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {contacts.length === 0 ? (
              <TableEmpty colSpan={5}>
                {loading
                  ? "Loading…"
                  : "No contacts yet — they are created automatically from bookings, or add one manually."}
              </TableEmpty>
            ) : (
              contacts.map((c) => (
                <TableRow key={c.id}>
                  <TableCell className="pl-5 font-medium">{c.name}</TableCell>
                  <TableCell>{c.phone || "—"}</TableCell>
                  <TableCell>{c.email || "—"}</TableCell>
                  <TableCell className="max-w-64 truncate text-xs text-muted-foreground">
                    {c.notes || "—"}
                  </TableCell>
                  <TableCell className="pr-5 text-right">
                    <div className="flex justify-end gap-1">
                      {c.phone ? (
                        <Link
                          href={`/app/${orgSlug}/conversations?contact=${encodeURIComponent(c.phone)}`}
                          className="inline-flex h-8 items-center gap-1.5 rounded-md px-3 text-xs hover:bg-accent"
                        >
                          <MessageCircle className="h-3.5 w-3.5" />
                          Conversations
                        </Link>
                      ) : null}
                      {c.phone ? (
                        <CallButton purpose="callback" onClick={() => setCallTarget({ purpose: "callback", phone: c.phone, name: c.name })} />
                      ) : null}
                      <Button variant="ghost" size="sm" onClick={() => openEdit(c)}>
                        <Pencil className="h-3.5 w-3.5" />
                        Edit
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-destructive"
                        onClick={() => setDeleting(c)}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      {/* SPEC-W82 B1.2: stacked contact cards below sm:. */}
      <div className="space-y-2 sm:hidden">
        {contacts.length === 0 ? (
          <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
            {loading
              ? "Loading…"
              : "No contacts yet — they are created automatically from bookings, or add one manually."}
          </p>
        ) : (
          contacts.map((c) => (
            <div
              key={c.id}
              className="rounded-md border border-border bg-card px-4 py-3"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <p className="font-medium">{c.name}</p>
                  <p className="mt-0.5 text-xs text-muted-foreground">
                    {[c.phone, c.email].filter(Boolean).join(" · ") ||
                      "No phone or email"}
                  </p>
                  {c.notes ? (
                    <p className="mt-1 text-xs text-muted-foreground">
                      {c.notes}
                    </p>
                  ) : null}
                </div>
                <div className="flex shrink-0 gap-1">
                  <Button variant="ghost" size="sm" onClick={() => openEdit(c)}>
                    <Pencil className="h-3.5 w-3.5" />
                    Edit
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    className="text-destructive"
                    onClick={() => setDeleting(c)}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                </div>
              </div>
            </div>
          ))
        )}
      </div>

      {formDialog(createOpen, () => setCreateOpen(false), "Add contact", () => void saveCreate())}
      {formDialog(!!editing, () => setEditing(null), `Edit ${editing?.name ?? "contact"}`, () => void saveEdit())}
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title="Delete this contact?"
        description={
          deleting
            ? `${deleting.name} will be removed. Existing bookings keep their history.`
            : undefined
        }
        confirmLabel="Delete contact"
        destructive
        busy={busy}
        onConfirm={() => void remove()}
      />
      <PhoneCallDialog orgSlug={orgSlug} target={callTarget} onClose={() => setCallTarget(null)} />
    </div>
  );
}
