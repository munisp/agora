"use client";

/**
 * SPEC-W45 K18 plan management (identity-service):
 *   GET   /api/identity/v1/tenants/{slug}       → { ..., plan }
 *   PATCH /api/identity/v1/tenants/{slug}/plan  { plan }  (owner/platform-admin)
 * Plan member limits (identity plan.go): free ≤ 3 members, pro ≤ 20,
 * enterprise unlimited. The change control is hidden for non-owners —
 * the server enforces the same rule with 403.
 */
import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { ArrowLeft } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Label, Select } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import type { Tenant } from "@/lib/types";

const PLANS = [
  { id: "free", label: "Free", members: "Up to 3 members" },
  { id: "pro", label: "Pro", members: "Up to 20 members" },
  { id: "enterprise", label: "Enterprise", members: "Unlimited members" },
] as const;

export function PlanClient({
  orgSlug,
  isPlatformAdmin,
  initialTenant,
  initialIsOwner,
  initialError,
}: {
  orgSlug: string;
  currentUserId: string | null;
  isPlatformAdmin: boolean;
  /** SPEC-W46 AW-2: fetched server-side in page.tsx. */
  initialTenant: Tenant | null;
  initialIsOwner: boolean;
  initialError: string | null;
}) {
  const { toast } = useToast();
  const router = useRouter();
  const [tenant, setTenant] = React.useState<Tenant | null>(initialTenant);
  const [isOwner, setIsOwner] = React.useState(initialIsOwner);
  const [selected, setSelected] = React.useState<string>(
    initialTenant?.plan ?? "",
  );
  const [error, setError] = React.useState<string | null>(initialError);
  const [busy, setBusy] = React.useState(false);

  // Sync when the server props change (router.refresh() after save). The
  // in-progress plan selection is deliberately not overwritten.
  React.useEffect(() => {
    setTenant(initialTenant);
    setIsOwner(initialIsOwner);
    setError(initialError);
  }, [initialTenant, initialIsOwner, initialError]);

  const canChangePlan = isPlatformAdmin || isOwner;

  const save = async () => {
    if (!tenant || selected === tenant.plan) return;
    setBusy(true);
    try {
      const res = await api.patch<{ plan: string; changed: boolean }>(
        `/api/identity/v1/tenants/${orgSlug}/plan`,
        { plan: selected },
      );
      setTenant({ ...tenant, plan: res.plan });
      toast({
        title: res.changed ? "Plan updated" : "Plan unchanged",
        variant: "success",
      });
      // SPEC-W46 AW-2: revalidate the server-fetched props.
      router.refresh();
    } catch (e) {
      toast({
        title: "Plan change failed",
        description:
          e instanceof ApiError && e.status === 403
            ? "Only a tenant owner or platform-admin can change the plan."
            : e instanceof ApiError
              ? e.message
              : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="max-w-2xl">
      <PageHeader
        title="Plan"
        description="Your subscription plan and member limits."
        actions={
          <Link href={`/app/${orgSlug}/settings`}>
            <Button variant="outline" size="sm">
              <ArrowLeft className="h-4 w-4" /> Settings
            </Button>
          </Link>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="space-y-6">
        <Card>
          <CardHeader>
            <CardTitle>Current plan</CardTitle>
            <CardDescription>
              Member limits are enforced when inviting from Settings →
              Members.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="flex items-center gap-3">
              <Badge variant="secondary" className="text-sm">
                {tenant?.plan ?? "—"}
              </Badge>
              <p className="text-sm text-muted-foreground">
                {PLANS.find((p) => p.id === tenant?.plan)?.members ?? ""}
              </p>
            </div>
            <dl className="grid gap-3 sm:grid-cols-3">
              {PLANS.map((p) => (
                <div
                  key={p.id}
                  className="rounded-md border border-border p-3"
                >
                  <dt className="text-sm font-medium">{p.label}</dt>
                  <dd className="text-xs text-muted-foreground">{p.members}</dd>
                </div>
              ))}
            </dl>
          </CardContent>
        </Card>

        {canChangePlan ? (
          <Card>
            <CardHeader>
              <CardTitle>Change plan</CardTitle>
              <CardDescription>
                Owner only. The change is audit-logged and takes effect
                immediately.
              </CardDescription>
            </CardHeader>
            <CardContent className="flex items-end gap-3">
              <div className="grid gap-1.5">
                <Label htmlFor="plan-select">New plan</Label>
                <Select
                  id="plan-select"
                  value={selected}
                  onChange={(e) => setSelected(e.target.value)}
                >
                  {PLANS.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.label}
                    </option>
                  ))}
                </Select>
              </div>
              <Button
                onClick={() => void save()}
                disabled={busy || !tenant || selected === tenant.plan}
              >
                {busy ? "Saving…" : "Save plan"}
              </Button>
            </CardContent>
          </Card>
        ) : null}
      </div>
    </div>
  );
}
