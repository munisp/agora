"use client";

import * as React from "react";
import Link from "next/link";
import { ArrowRight, MessagesSquare, Save } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { formatMoney, titleCase } from "@/lib/utils";
import type { Tenant } from "@/lib/types";

const TIMEZONES = [
  "UTC",
  "America/New_York",
  "America/Chicago",
  "America/Denver",
  "America/Los_Angeles",
  "Europe/London",
  "Europe/Berlin",
  "Africa/Lagos",
  "Asia/Dubai",
  "Asia/Singapore",
  "Australia/Sydney",
];

const CURRENCIES = ["USD", "EUR", "GBP", "NGN", "KES", "ZAR", "INR", "SGD", "AUD"];

export function SettingsClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [tenant, setTenant] = React.useState<Tenant | null>(null);
  const [form, setForm] = React.useState({
    name: "",
    timezone: "UTC",
    currency: "USD",
    locale: "en-US",
  });
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    (async () => {
      try {
        const t = await api.get<Tenant>(`/api/identity/v1/tenants/${orgSlug}`);
        setTenant(t);
        setForm({
          name: t.name,
          timezone: t.timezone,
          currency: t.currency,
          locale: t.locale,
        });
      } catch (e) {
        setError(e instanceof ApiError ? e.message : "Failed to load settings.");
      }
    })();
  }, [orgSlug]);

  const save = async () => {
    setSaving(true);
    try {
      const updated = await api.patch<Tenant>(
        `/api/identity/v1/tenants/${orgSlug}`,
        form,
      );
      setTenant(updated);
      toast({ title: "Settings saved", variant: "success" });
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
    <div className="max-w-2xl">
      <PageHeader
        title="Settings"
        description="Organisation profile, locale and plan."
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="space-y-6">
        <Card>
          <CardHeader>
            <CardTitle>Organisation</CardTitle>
            <CardDescription>
              Used by the receptionist when greeting customers.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="set-name">Display name</Label>
              <Input
                id="set-name"
                value={form.name}
                onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="set-slug">Tenant slug</Label>
              <Input id="set-slug" value={orgSlug} disabled />
              <p className="text-xs text-muted-foreground">
                Slugs are assigned at provisioning and cannot be changed.
              </p>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Locale &amp; booking rules</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="set-tz">Timezone</Label>
              <Select
                id="set-tz"
                value={form.timezone}
                onChange={(e) => setForm((f) => ({ ...f, timezone: e.target.value }))}
              >
                {TIMEZONES.map((tz) => (
                  <option key={tz} value={tz}>
                    {tz}
                  </option>
                ))}
                {!TIMEZONES.includes(form.timezone) ? (
                  <option value={form.timezone}>{form.timezone}</option>
                ) : null}
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="set-currency">Currency</Label>
              <Select
                id="set-currency"
                value={form.currency}
                onChange={(e) => setForm((f) => ({ ...f, currency: e.target.value }))}
              >
                {CURRENCIES.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
                {!CURRENCIES.includes(form.currency) ? (
                  <option value={form.currency}>{form.currency}</option>
                ) : null}
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="set-locale">Locale</Label>
              <Input
                id="set-locale"
                value={form.locale}
                onChange={(e) => setForm((f) => ({ ...f, locale: e.target.value }))}
                placeholder="en-US"
              />
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Plan</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center justify-between">
            <div className="flex items-center gap-3">
              <Badge variant="secondary" className="text-sm">
                {tenant?.plan ?? "—"}
              </Badge>
              <p className="text-sm text-muted-foreground">
                Member since{" "}
                {tenant ? new Date(tenant.created_at).toLocaleDateString() : "—"}
              </p>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Industry pack</CardTitle>
            <CardDescription>
              Workflow pack applied at onboarding. Packs set terminology,
              booking policy and the Temporal workflows used for this
              organisation.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {tenant?.pack ? (
              <div className="space-y-4">
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant="secondary" className="text-sm">
                    {tenant.pack.displayName ??
                      (tenant.industry ? titleCase(tenant.industry) : "—")}
                  </Badge>
                  {tenant.industry ? (
                    <span className="text-xs text-muted-foreground">
                      {tenant.industry}
                    </span>
                  ) : null}
                  {tenant.pack.bookingPolicy?.intakeRequired ? (
                    <Badge variant="outline">Intake form required</Badge>
                  ) : null}
                  {tenant.pack.bookingPolicy?.phoneConfirmation ? (
                    <Badge variant="outline">Phone confirmation</Badge>
                  ) : null}
                </div>
                <dl className="grid gap-3 sm:grid-cols-3">
                  <div>
                    <dt className="text-xs text-muted-foreground">Deposit</dt>
                    <dd className="text-sm font-medium">
                      {tenant.pack.bookingPolicy?.depositPercent != null
                        ? `${tenant.pack.bookingPolicy.depositPercent}%`
                        : "—"}
                    </dd>
                  </div>
                  <div>
                    <dt className="text-xs text-muted-foreground">No-show fee</dt>
                    <dd className="text-sm font-medium">
                      {tenant.pack.bookingPolicy?.noShowFeeCents != null
                        ? formatMoney(
                            tenant.pack.bookingPolicy.noShowFeeCents,
                            tenant.currency,
                            tenant.locale,
                          )
                        : "—"}
                    </dd>
                  </div>
                  <div>
                    <dt className="text-xs text-muted-foreground">
                      Cancellation window
                    </dt>
                    <dd className="text-sm font-medium">
                      {tenant.pack.bookingPolicy?.cancellationWindowHours != null
                        ? `${tenant.pack.bookingPolicy.cancellationWindowHours}h`
                        : "—"}
                    </dd>
                  </div>
                </dl>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">
                {tenant?.industry
                  ? `Industry “${tenant.industry}” — no pack summary was included in the tenant response.`
                  : "No industry pack is associated with this organisation yet."}
              </p>
            )}
            <div className="mt-4">
              <Link href={`/app/${orgSlug}/settings/packs`}>
                <Button variant="outline" size="sm">
                  Browse pack catalog <ArrowRight className="h-3.5 w-3.5" />
                </Button>
              </Link>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <MessagesSquare className="h-4 w-4" /> Channels
            </CardTitle>
            <CardDescription>
              Enable WhatsApp, Telegram and web chat, and generate the
              messaging-gateway configuration for this organisation.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Link href={`/app/${orgSlug}/channels`}>
              <Button variant="outline" size="sm">
                Open channel settings <ArrowRight className="h-3.5 w-3.5" />
              </Button>
            </Link>
          </CardContent>
        </Card>

        <div className="flex justify-end">
          <Button onClick={() => void save()} disabled={saving || !form.name.trim()}>
            <Save className="h-4 w-4" />
            {saving ? "Saving…" : "Save settings"}
          </Button>
        </div>
      </div>
    </div>
  );
}
