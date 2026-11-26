"use client";

/**
 * SPEC-W45 K17 tenant API keys (identity-service):
 *   POST   /api/identity/v1/tenants/{slug}/api-keys { name, scopes? }
 *          → 201 { id, name, prefix, key, scopes, created_at }
 *          The plaintext key ("<prefix>.<secret>") is returned exactly ONCE.
 *   GET    /api/identity/v1/tenants/{slug}/api-keys → { api_keys } (no secrets)
 *   DELETE /api/identity/v1/tenants/{slug}/api-keys/{key_id} → { revoked }
 * Owner/admin-gated server-side. v1 scope: bookings:read (read-only external
 * bookings feed via the gateway's /api/ext/booking/* route).
 */
import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { ArrowLeft, Copy, KeyRound, TriangleAlert } from "lucide-react";
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
import { Input, Label } from "@/components/ui/input";
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

export interface ApiKey {
  id: string;
  tenant_id: string;
  name: string;
  prefix: string;
  scopes: string[];
  created_by: string;
  created_at: string;
  revoked_at?: string;
}

interface CreatedKey {
  id: string;
  name: string;
  prefix: string;
  key: string;
  scopes: string[];
  created_at: string;
}

export function ApiKeysClient({
  orgSlug,
  initialKeys,
  initialError,
}: {
  orgSlug: string;
  /** SPEC-W46 AW-2: fetched server-side in page.tsx. */
  initialKeys: ApiKey[];
  initialError: string | null;
}) {
  const { toast } = useToast();
  const router = useRouter();
  const [keys, setKeys] = React.useState<ApiKey[]>(initialKeys);
  const [error, setError] = React.useState<string | null>(initialError);
  const [creating, setCreating] = React.useState(false);
  const [created, setCreated] = React.useState<CreatedKey | null>(null);
  const [revoking, setRevoking] = React.useState<ApiKey | null>(null);
  const [busy, setBusy] = React.useState(false);

  // Sync when the server props change (router.refresh() after mutations).
  React.useEffect(() => {
    setKeys(initialKeys);
    setError(initialError);
  }, [initialKeys, initialError]);

  const create = async (name: string) => {
    setBusy(true);
    try {
      const res = await api.post<CreatedKey>(
        `/api/identity/v1/tenants/${orgSlug}/api-keys`,
        { name, scopes: ["bookings:read"] },
      );
      setCreating(false);
      setCreated(res);
      router.refresh();
    } catch (e) {
      toast({
        title: "Could not create key",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const revoke = async () => {
    if (!revoking) return;
    setBusy(true);
    try {
      await api.delete(
        `/api/identity/v1/tenants/${orgSlug}/api-keys/${revoking.id}`,
      );
      toast({ title: "Key revoked", variant: "success" });
      setRevoking(null);
      router.refresh();
    } catch (e) {
      toast({
        title: "Revoke failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const copyKey = async () => {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created.key);
      toast({ title: "Key copied", variant: "success" });
    } catch {
      toast({
        title: "Copy failed",
        description: "Select the key and copy it manually.",
        variant: "destructive",
      });
    }
  };

  return (
    <div>
      <PageHeader
        title="API keys"
        description="Programmatic credentials for external integrations (read-only bookings scope)."
        actions={
          <div className="flex gap-2">
            <Link href={`/app/${orgSlug}/settings`}>
              <Button variant="outline" size="sm">
                <ArrowLeft className="h-4 w-4" /> Settings
              </Button>
            </Link>
            <Button size="sm" onClick={() => setCreating(true)}>
              <KeyRound className="h-4 w-4" /> Create key
            </Button>
          </div>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      {created ? (
        <Card className="mb-4 border-warning/60 p-5">
          <div className="flex items-start gap-3">
            <TriangleAlert className="mt-0.5 h-5 w-5 shrink-0 text-warning" />
            <div className="min-w-0 flex-1 space-y-3">
              <p className="text-sm font-medium">
                Store this key now — it is shown exactly once and cannot be
                recovered.
              </p>
              <code className="block break-all rounded-md border border-border bg-muted/40 p-3 font-mono text-xs">
                {created.key}
              </code>
              <div className="flex gap-2">
                <Button size="sm" variant="outline" onClick={() => void copyKey()}>
                  <Copy className="h-4 w-4" /> Copy key
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setCreated(null)}>
                  I stored it — dismiss
                </Button>
              </div>
            </div>
          </div>
        </Card>
      ) : null}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Prefix</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Scopes</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.length === 0 ? (
              <TableEmpty colSpan={6}>No API keys yet.</TableEmpty>
            ) : (
              keys.map((k) => (
                <TableRow key={k.id}>
                  <TableCell className="pl-5 font-mono text-xs">
                    {k.prefix}…
                  </TableCell>
                  <TableCell className="font-medium">{k.name}</TableCell>
                  <TableCell className="text-xs">
                    {(k.scopes ?? []).join(", ")}
                  </TableCell>
                  <TableCell className="text-sm">
                    {new Date(k.created_at).toLocaleDateString()}
                  </TableCell>
                  <TableCell>
                    <Badge variant={k.revoked_at ? "outline" : "success"}>
                      {k.revoked_at ? "Revoked" : "Active"}
                    </Badge>
                  </TableCell>
                  <TableCell className="pr-5 text-right">
                    {!k.revoked_at ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-destructive"
                        onClick={() => setRevoking(k)}
                      >
                        Revoke
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      <CreateKeyDialog
        open={creating}
        busy={busy}
        onClose={() => setCreating(false)}
        onCreate={create}
      />
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
        title={`Revoke “${revoking?.name ?? ""}”?`}
        description="Integrations using this key lose access immediately. The row is kept for audit."
        confirmLabel="Revoke key"
        destructive
        busy={busy}
        onConfirm={revoke}
      />
    </div>
  );
}

function CreateKeyDialog({
  open,
  busy,
  onClose,
  onCreate,
}: {
  open: boolean;
  busy: boolean;
  onClose: () => void;
  onCreate: (name: string) => Promise<void>;
}) {
  const [name, setName] = React.useState("");

  React.useEffect(() => {
    if (open) setName("");
  }, [open]);

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>Create API key</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="ak-name">Name</Label>
            <Input
              id="ak-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. BI export job"
            />
          </div>
          <p className="text-xs text-muted-foreground">
            Scope: <code>bookings:read</code> (v1). Send the key as the{" "}
            <code>X-Api-Key</code> header to the external bookings feed
            (/api/ext/booking/*).
          </p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => void onCreate(name.trim())}
            disabled={busy || name.trim().length === 0}
          >
            {busy ? "Creating…" : "Create key"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
