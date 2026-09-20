"use client";

/**
 * SPEC-W45 K16 tenant member lifecycle (identity-service):
 *   GET    /api/identity/v1/tenants/{slug}/members            → { members }
 *   POST   /api/identity/v1/tenants/{slug}/members            → 201 { user_id, role }
 *          409 { error: "already_invited_or_member", resend: true } on re-invite
 *          403 { error, plan, limit, upgrade: true } when the plan cap is hit
 *   PATCH  /api/identity/v1/tenants/{slug}/members/{user_id}  { role }
 *   DELETE /api/identity/v1/tenants/{slug}/members/{user_id}
 * All mutations are owner/admin-gated server-side; the owner role is
 * owner-only. Warnings arrays in mutation responses are surfaced verbatim.
 */
import * as React from "react";
import Link from "next/link";
import { ArrowLeft, MailPlus, Trash2 } from "lucide-react";
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

interface IdentityMember {
  tenant_id: string;
  user_id: string;
  role: string;
}

const MEMBER_ROLES = ["owner", "admin", "staff", "viewer"] as const;

/** Extract the trailing warnings array some identity mutations return. */
function warningsOf(body: unknown): string[] {
  if (
    typeof body === "object" &&
    body !== null &&
    Array.isArray((body as { warnings?: unknown }).warnings)
  ) {
    return (body as { warnings: unknown[] }).warnings.map(String);
  }
  return [];
}

export function MembersClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [members, setMembers] = React.useState<IdentityMember[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [inviting, setInviting] = React.useState(false);
  const [removing, setRemoving] = React.useState<IdentityMember | null>(null);
  const [roleEdits, setRoleEdits] = React.useState<Record<string, string>>({});
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<{ members?: IdentityMember[] }>(
        `/api/identity/v1/tenants/${orgSlug}/members`,
      );
      setMembers(data.members ?? []);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load members.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const changeRole = async (m: IdentityMember) => {
    const role = roleEdits[m.user_id];
    if (!role || role === m.role) return;
    setBusy(true);
    try {
      const res = await api.patch<{ warnings?: string[] }>(
        `/api/identity/v1/tenants/${orgSlug}/members/${m.user_id}`,
        { role },
      );
      const warnings = warningsOf(res);
      toast({
        title: "Role updated",
        description: warnings.length ? warnings.join(" · ") : undefined,
        variant: warnings.length ? "warning" : "success",
      });
      await load();
    } catch (e) {
      toast({
        title: "Role change failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!removing) return;
    setBusy(true);
    try {
      const res = await api.delete<unknown>(
        `/api/identity/v1/tenants/${orgSlug}/members/${removing.user_id}`,
      );
      const warnings = warningsOf(res);
      toast({
        title: "Member removed",
        description: warnings.length ? warnings.join(" · ") : undefined,
        variant: warnings.length ? "warning" : "success",
      });
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
        title="Members"
        description="Sign-in accounts with access to this organisation."
        actions={
          <div className="flex gap-2">
            <Link href={`/app/${orgSlug}/settings`}>
              <Button variant="outline" size="sm">
                <ArrowLeft className="h-4 w-4" /> Settings
              </Button>
            </Link>
            <Button size="sm" onClick={() => setInviting(true)}>
              <MailPlus className="h-4 w-4" /> Invite member
            </Button>
          </div>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">User ID</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Change role</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {members.length === 0 ? (
              <TableEmpty colSpan={4}>
                {loading ? "Loading…" : "No members found."}
              </TableEmpty>
            ) : (
              members.map((m) => (
                <TableRow key={m.user_id}>
                  <TableCell className="pl-5 font-mono text-xs">
                    {m.user_id}
                  </TableCell>
                  <TableCell>
                    <Badge variant="secondary">{titleCase(m.role)}</Badge>
                  </TableCell>
                  <TableCell>
                    <div className="flex items-center gap-2">
                      <Select
                        aria-label={`Role for ${m.user_id}`}
                        value={roleEdits[m.user_id] ?? m.role}
                        onChange={(e) =>
                          setRoleEdits((prev) => ({
                            ...prev,
                            [m.user_id]: e.target.value,
                          }))
                        }
                      >
                        {MEMBER_ROLES.map((r) => (
                          <option key={r} value={r}>
                            {titleCase(r)}
                          </option>
                        ))}
                      </Select>
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={
                          busy || (roleEdits[m.user_id] ?? m.role) === m.role
                        }
                        onClick={() => void changeRole(m)}
                      >
                        Save
                      </Button>
                    </div>
                  </TableCell>
                  <TableCell className="pr-5 text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label="Remove member"
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

      <InviteDialog
        orgSlug={orgSlug}
        open={inviting}
        busy={busy}
        setBusy={setBusy}
        onClose={() => setInviting(false)}
        onInvited={() => void load()}
      />
      <ConfirmDialog
        open={removing !== null}
        onOpenChange={(open) => !open && setRemoving(null)}
        title={`Remove ${removing?.user_id ?? ""}?`}
        description="The account is disabled and its sessions revoked. Audit history is retained."
        confirmLabel="Remove member"
        destructive
        busy={busy}
        onConfirm={remove}
      />
    </div>
  );
}

function InviteDialog({
  orgSlug,
  open,
  busy,
  setBusy,
  onClose,
  onInvited,
}: {
  orgSlug: string;
  open: boolean;
  busy: boolean;
  setBusy: (b: boolean) => void;
  onClose: () => void;
  onInvited: () => void;
}) {
  const { toast } = useToast();
  const [email, setEmail] = React.useState("");
  const [firstName, setFirstName] = React.useState("");
  const [lastName, setLastName] = React.useState("");
  const [role, setRole] = React.useState("staff");
  /** K16: set when the last invite attempt hit the 409 resend path. */
  const [alreadyInvited, setAlreadyInvited] = React.useState(false);
  /** K18: plan-limit 403 message (upgrade cue from the response body). */
  const [limitMessage, setLimitMessage] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (open) {
      setEmail("");
      setFirstName("");
      setLastName("");
      setRole("staff");
      setAlreadyInvited(false);
      setLimitMessage(null);
    }
  }, [open]);

  const valid = /.+@.+\..+/.test(email);

  const send = async () => {
    setBusy(true);
    setAlreadyInvited(false);
    setLimitMessage(null);
    try {
      await api.post(`/api/identity/v1/tenants/${orgSlug}/members`, {
        email: email.trim(),
        first_name: firstName.trim(),
        last_name: lastName.trim(),
        role,
      });
      toast({ title: "Invitation sent", variant: "success" });
      onClose();
      onInvited();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        // K16 re-invite semantics: { error: already_invited_or_member,
        // resend: true } — offer the resend affordance instead of an error.
        setAlreadyInvited(true);
      } else if (e instanceof ApiError && e.status === 403) {
        // K18 plan limit: the response carries the upgrade message.
        setLimitMessage(e.message);
      } else {
        toast({
          title: "Invite failed",
          description: e instanceof ApiError ? e.message : undefined,
          variant: "destructive",
        });
      }
    } finally {
      setBusy(false);
    }
  };

  const resend = async () => {
    // Re-issue the identical invite — the 409 body's resend flag marks this
    // as the resend rail (the backend dedupes the account and re-signals the
    // invite notification path).
    setBusy(true);
    try {
      await api.post(`/api/identity/v1/tenants/${orgSlug}/members`, {
        email: email.trim(),
        first_name: firstName.trim(),
        last_name: lastName.trim(),
        role,
      });
      toast({ title: "Invitation sent", variant: "success" });
      onClose();
      onInvited();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        toast({
          title: "Invitation re-requested",
          description:
            "The account already exists; the invite notification was re-requested.",
          variant: "success",
        });
        onClose();
      } else {
        toast({
          title: "Resend failed",
          description: e instanceof ApiError ? e.message : undefined,
          variant: "destructive",
        });
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>Invite member</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="im-email">Email</Label>
            <Input
              id="im-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="ada@example.com"
            />
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="im-first">First name</Label>
              <Input
                id="im-first"
                value={firstName}
                onChange={(e) => setFirstName(e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="im-last">Last name</Label>
              <Input
                id="im-last"
                value={lastName}
                onChange={(e) => setLastName(e.target.value)}
              />
            </div>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="im-role">Role</Label>
            <Select
              id="im-role"
              value={role}
              onChange={(e) => setRole(e.target.value)}
            >
              {MEMBER_ROLES.map((r) => (
                <option key={r} value={r}>
                  {titleCase(r)}
                </option>
              ))}
            </Select>
          </div>
          {alreadyInvited ? (
            <p className="rounded-md border border-border bg-muted/40 p-3 text-sm">
              This email is already invited or already a member. You can
              resend the invitation below.
            </p>
          ) : null}
          {limitMessage ? (
            <p className="rounded-md border border-border bg-muted/40 p-3 text-sm">
              {limitMessage}{" "}
              <Link
                href={`/app/${orgSlug}/settings/plan`}
                className="font-medium text-primary underline underline-offset-2"
              >
                Manage plan
              </Link>
            </p>
          ) : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          {alreadyInvited ? (
            <Button onClick={() => void resend()} disabled={busy}>
              {busy ? "Resending…" : "Resend invitation"}
            </Button>
          ) : (
            <Button onClick={() => void send()} disabled={busy || !valid}>
              {busy ? "Inviting…" : "Send invitation"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
