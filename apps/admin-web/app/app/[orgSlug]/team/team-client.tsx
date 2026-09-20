"use client";

import * as React from "react";
import { Plus, Trash2 } from "lucide-react";
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
import { Input, Label, Select } from "@/components/ui/input";
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
import { titleCase } from "@/lib/utils";
import type { TeamMember } from "@/lib/types";

/**
 * SPEC-W45 STK O14: identity tenant member (GET
 * /api/identity/v1/tenants/{slug}/members → { members: [...] }). The
 * booking team-member record can optionally link to one of these login
 * accounts (user_id) so schedule assignments map to a real staff identity.
 */
export interface IdentityMember {
  tenant_id: string;
  user_id: string;
  role: string;
}

export function TeamClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [members, setMembers] = React.useState<TeamMember[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [adding, setAdding] = React.useState(false);
  const [removing, setRemoving] = React.useState<TeamMember | null>(null);
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<TeamMember[] | { items: TeamMember[] }>(
        "/api/bookings/v1/team-members",
        { tenant: orgSlug },
      );
      setMembers(Array.isArray(data) ? data : (data.items ?? []));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load team.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const add = async (form: {
    name: string;
    email: string;
    role: string;
    user_id?: string;
  }) => {
    setBusy(true);
    try {
      // STK O14: user_id is the optional identity-account link. The booking
      // team-members API predates the field; unknown JSON fields are ignored
      // by the Go decoder until the service persists it (CONTRACT NOTE).
      const body = form.user_id
        ? form
        : { name: form.name, email: form.email, role: form.role };
      await api.post("/api/bookings/v1/team-members", body, { tenant: orgSlug });
      toast({ title: "Team member added", variant: "success" });
      setAdding(false);
      await load();
    } catch (e) {
      toast({
        title: "Could not add member",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const toggleActive = async (m: TeamMember) => {
    try {
      await api.patch(
        `/api/bookings/v1/team-members/${m.id}`,
        { active: !m.active },
        { tenant: orgSlug },
      );
      setMembers((prev) =>
        prev.map((p) => (p.id === m.id ? { ...p, active: !p.active } : p)),
      );
    } catch (e) {
      toast({
        title: "Could not update member",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    }
  };

  const remove = async () => {
    if (!removing) return;
    setBusy(true);
    try {
      await api.delete(`/api/bookings/v1/team-members/${removing.id}`, {
        tenant: orgSlug,
      });
      toast({ title: "Member removed", variant: "success" });
      setRemoving(null);
      await load();
    } catch (e) {
      toast({
        title: "Remove failed",
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
        title="Team"
        description="People the receptionist can book time with."
        actions={
          <Button size="sm" onClick={() => setAdding(true)}>
            <Plus className="h-4 w-4" /> Add member
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Name</TableHead>
              <TableHead>Email</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {members.length === 0 ? (
              <TableEmpty colSpan={5}>
                {loading ? "Loading…" : "No team members yet."}
              </TableEmpty>
            ) : (
              members.map((m) => (
                <TableRow key={m.id}>
                  <TableCell className="pl-5 font-medium">{m.name}</TableCell>
                  <TableCell>{m.email}</TableCell>
                  <TableCell>
                    <Badge variant="secondary">{titleCase(m.role)}</Badge>
                  </TableCell>
                  <TableCell>
                    <button
                      onClick={() => void toggleActive(m)}
                      className="cursor-pointer"
                      aria-label="Toggle active"
                    >
                      <Badge variant={m.active ? "success" : "outline"}>
                        {m.active ? "Active" : "Inactive"}
                      </Badge>
                    </button>
                  </TableCell>
                  <TableCell className="pr-5 text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label="Remove"
                      className="text-destructive"
                      onClick={() => setRemoving(m)}
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      <AddMemberDialog
        open={adding}
        busy={busy}
        orgSlug={orgSlug}
        onClose={() => setAdding(false)}
        onAdd={add}
      />
      <ConfirmDialog
        open={removing !== null}
        onOpenChange={(open) => !open && setRemoving(null)}
        title={`Remove ${removing?.name ?? ""}?`}
        description="Their availability rules are removed too; existing bookings stay on record."
        confirmLabel="Remove member"
        destructive
        busy={busy}
        onConfirm={remove}
      />
    </div>
  );
}

function AddMemberDialog({
  open,
  busy,
  orgSlug,
  onClose,
  onAdd,
}: {
  open: boolean;
  busy: boolean;
  orgSlug: string;
  onClose: () => void;
  onAdd: (form: {
    name: string;
    email: string;
    role: string;
    user_id?: string;
  }) => void;
}) {
  const [name, setName] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [role, setRole] = React.useState("staff");
  const [userId, setUserId] = React.useState("");
  const [identityMembers, setIdentityMembers] = React.useState<IdentityMember[]>(
    [],
  );

  React.useEffect(() => {
    if (open) {
      setName("");
      setEmail("");
      setRole("staff");
      setUserId("");
    }
  }, [open]);

  // STK O14: the optional linked-user select lists the tenant's identity
  // members (login accounts). Best-effort — a staff caller without the
  // identity read permission still gets the plain create form.
  React.useEffect(() => {
    if (!open) return;
    let cancelled = false;
    (async () => {
      try {
        const data = await api.get<{ members?: IdentityMember[] }>(
          `/api/identity/v1/tenants/${orgSlug}/members`,
        );
        if (!cancelled) setIdentityMembers(data.members ?? []);
      } catch {
        if (!cancelled) setIdentityMembers([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, orgSlug]);

  const valid = name.trim().length > 0 && /.+@.+\..+/.test(email);

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>Add team member</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="tm-name">Name</Label>
            <Input
              id="tm-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Ada Osei"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="tm-email">Email</Label>
            <Input
              id="tm-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="ada@example.com"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="tm-role">Role</Label>
            <Select
              id="tm-role"
              value={role}
              onChange={(e) => setRole(e.target.value)}
            >
              <option value="staff">Staff</option>
              <option value="admin">Admin</option>
              <option value="owner">Owner</option>
            </Select>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="tm-user">Linked user account (optional)</Label>
            <Select
              id="tm-user"
              value={userId}
              onChange={(e) => setUserId(e.target.value)}
            >
              <option value="">— not linked —</option>
              {identityMembers.map((m) => (
                <option key={m.user_id} value={m.user_id}>
                  {m.user_id} · {titleCase(m.role)}
                </option>
              ))}
            </Select>
            <p className="text-xs text-muted-foreground">
              Links this person to their sign-in account so schedule
              assignments map to a staff identity.
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() =>
              onAdd(userId ? { name, email, role, user_id: userId } : { name, email, role })
            }
            disabled={busy || !valid}
          >
            {busy ? "Adding…" : "Add member"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
