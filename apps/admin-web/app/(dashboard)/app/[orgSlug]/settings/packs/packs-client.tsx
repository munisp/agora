"use client";

import * as React from "react";
import { CheckCircle2, Loader2, PackageOpen, Search } from "lucide-react";
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
import { Input, Label } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { titleCase } from "@/lib/utils";
import type { IndustryPackSummary, Tenant } from "@/lib/types";
import { PACK_CATALOG, type PackCatalogEntry } from "./catalog-data";

/** lib/types' IndustryPackSummary is a subset of the Go pack Summary JSON;
 * the fields below are also emitted by GET /v1/tenants/{slug}. */
type TenantPackSummary = IndustryPackSummary & {
  temporalWorkflow?: string;
};
type TenantWithPack = Omit<Tenant, "pack"> & {
  pack?: TenantPackSummary | null;
};

/**
 * Wave 15 (SPEC-W15, Agent D) — Industry pack catalog under Settings.
 *
 * Wiring honesty:
 * - The tenant's ACTIVE pack comes from the real endpoint
 *   GET /api/identity/v1/tenants/{slug} (tenant.industry + resolved
 *   tenant.pack summary) via the BFF proxy → APISIX → identity-service.
 * - identity-service does NOT expose a pack-catalog endpoint (no
 *   GET /v1/packs — see services/identity-service/internal/httpapi/server.go),
 *   so the browsable catalog below is the build-time snapshot of
 *   industries/*.yaml + industries/index.json in ./catalog-data.ts.
 * - Activation reuses the same call the general settings page already makes
 *   (PATCH /api/identity/v1/tenants/{slug} with { industry }). On identity
 *   builds without a tenant-update handler this fails; the failure is
 *   surfaced verbatim with the gap note instead of being hidden.
 */

const ACTIVATION_GAP_NOTE =
  "Pack activation could not be applied: identity-service does not expose a " +
  "tenant-update endpoint on this build (PATCH /v1/tenants/{slug}). Today a " +
  "pack is bound at tenant provisioning time (POST /v1/tenants); changing it " +
  "afterwards needs the backend endpoint tracked as a platform gap.";

type LoadState = "loading" | "ready" | "error";

function matches(entry: PackCatalogEntry, q: string): boolean {
  if (!q) return true;
  const hay = `${entry.id} ${entry.displayName} ${entry.languages.join(" ")}`.toLowerCase();
  return hay.includes(q.toLowerCase());
}

function PackCard({
  entry,
  active,
  selected,
  onSelect,
}: {
  entry: PackCatalogEntry;
  active: boolean;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      className={`w-full rounded-lg border p-3 text-left shadow-sm transition-colors ${
        selected
          ? "border-primary bg-accent"
          : "border-border bg-card hover:bg-accent/50"
      }`}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">{entry.displayName}</span>
        {active ? <Badge variant="success">Active</Badge> : null}
        {!entry.indexed ? <Badge variant="warning">Not in index</Badge> : null}
      </div>
      <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
        <Badge variant="outline">{entry.id}</Badge>
        <Badge variant="secondary">v{entry.version}</Badge>
        {entry.languages.map((lang) => (
          <Badge key={lang} variant="info">
            {lang}
          </Badge>
        ))}
      </div>
    </button>
  );
}

function PreviewSection({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </h3>
      <div className="mt-1.5">{children}</div>
    </div>
  );
}

export function PacksClient({
  orgSlug,
  canManage,
}: {
  orgSlug: string;
  /** owner/admin — gates the Activate button */
  canManage: boolean;
}) {
  const { toast } = useToast();

  const [tenant, setTenant] = React.useState<TenantWithPack | null>(null);
  const [loadState, setLoadState] = React.useState<LoadState>("loading");
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [query, setQuery] = React.useState("");
  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  const [activating, setActivating] = React.useState(false);
  const [activationNote, setActivationNote] = React.useState<string | null>(null);

  const loadTenant = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoadState("loading");
      setLoadError(null);
      try {
        const t = await api.get<TenantWithPack>(
          `/api/identity/v1/tenants/${orgSlug}`,
          undefined,
          signal,
        );
        setTenant(t);
        setLoadState("ready");
      } catch (e) {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setLoadError(
          e instanceof ApiError ? e.message : "Failed to load the organisation.",
        );
        setLoadState("error");
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const ctrl = new AbortController();
    void loadTenant(ctrl.signal);
    return () => ctrl.abort();
  }, [loadTenant]);

  const activeId = tenant?.industry ?? null;
  const sorted = React.useMemo(
    () =>
      [...PACK_CATALOG].sort((a, b) => {
        if (a.id === activeId) return -1;
        if (b.id === activeId) return 1;
        return a.displayName.localeCompare(b.displayName);
      }),
    [activeId],
  );
  const visible = sorted.filter((e) => matches(e, query));
  const selected =
    visible.find((e) => e.id === selectedId) ??
    sorted.find((e) => e.id === activeId) ??
    null;

  const activate = async (entry: PackCatalogEntry) => {
    setActivating(true);
    setActivationNote(null);
    try {
      await api.patch<TenantWithPack>(`/api/identity/v1/tenants/${orgSlug}`, {
        industry: entry.id,
      });
      await loadTenant();
      toast({
        title: `Pack “${entry.displayName}” activated`,
        variant: "success",
      });
    } catch (e) {
      const detail =
        e instanceof ApiError ? `${e.status} ${e.message}` : String(e);
      // Honest soft-fail: surface the real upstream status plus the gap note.
      setActivationNote(`${ACTIVATION_GAP_NOTE} Upstream response: ${detail}`);
      toast({
        title: "Activation not applied",
        description: "The backend has no pack-activation endpoint yet.",
        variant: "destructive",
      });
    } finally {
      setActivating(false);
    }
  };

  return (
    <div className="max-w-5xl">
      <PageHeader
        title="Industry packs"
        description="Browse the pack catalog, preview what callers hear, and activate a pack for this organisation."
      />

      {loadState === "error" && loadError ? (
        <ErrorNote message={loadError} />
      ) : null}
      {activationNote ? (
        <div className="mb-4">
          <ErrorNote message={activationNote} />
        </div>
      ) : null}

      <Card className="mb-6">
        <CardHeader>
          <CardTitle>Current pack</CardTitle>
          <CardDescription>
            Resolved live from identity-service
            {" (GET /v1/tenants/{slug})"}.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {loadState === "loading" ? (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" /> Loading…
            </p>
          ) : activeId ? (
            <div className="flex flex-wrap items-center gap-2">
              <Badge variant="success" className="text-sm">
                {tenant?.pack?.displayName ?? titleCase(activeId)}
              </Badge>
              <span className="text-xs text-muted-foreground">{activeId}</span>
              {tenant?.pack?.temporalWorkflow ? (
                <Badge variant="outline">{tenant.pack.temporalWorkflow}</Badge>
              ) : null}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">
              No industry pack is associated with this organisation yet.
            </p>
          )}
        </CardContent>
      </Card>

      <div className="grid gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
        <div>
          <div className="mb-3 grid gap-1.5">
            <Label htmlFor="pack-search" className="sr-only">
              Search packs
            </Label>
            <div className="relative">
              <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
              <Input
                id="pack-search"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Search packs…"
                className="pl-8"
              />
            </div>
          </div>
          <p className="mb-2 text-xs text-muted-foreground">
            {visible.length} of {PACK_CATALOG.length} packs — catalog snapshot
            from the industries/ registry shipped with this build
            (identity-service has no GET /v1/packs catalog endpoint yet).
          </p>
          <div className="max-h-[36rem] space-y-2 overflow-y-auto pr-1">
            {visible.map((entry) => (
              <PackCard
                key={entry.id}
                entry={entry}
                active={entry.id === activeId}
                selected={selected?.id === entry.id}
                onSelect={() => setSelectedId(entry.id)}
              />
            ))}
            {visible.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No packs match “{query}”.
              </p>
            ) : null}
          </div>
        </div>

        <div>
          {selected ? (
            <Card>
              <CardHeader>
                <CardTitle className="flex flex-wrap items-center gap-2">
                  <PackageOpen className="h-4 w-4" />
                  {selected.displayName}
                  <Badge variant="secondary">v{selected.version}</Badge>
                  {selected.id === activeId ? (
                    <Badge variant="success">Active</Badge>
                  ) : null}
                </CardTitle>
                <CardDescription>
                  {selected.id}
                  {selected.temporalWorkflow
                    ? ` · workflow ${selected.temporalWorkflow}`
                    : ""}
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-4">
                {selected.personaExcerpt ? (
                  <PreviewSection title="Agent persona (opening instructions)">
                    <p className="whitespace-pre-line text-sm">
                      {selected.personaExcerpt}
                    </p>
                  </PreviewSection>
                ) : null}

                {selected.i18n && Object.keys(selected.i18n).length > 0 ? (
                  <PreviewSection title="Greetings (localised preview)">
                    <div className="space-y-1.5">
                      {Object.entries(selected.i18n).map(([locale, strings]) =>
                        strings.greeting ? (
                          <p key={locale} className="text-sm">
                            <Badge variant="info" className="mr-1.5">
                              {locale}
                            </Badge>
                            <span className="italic">“{strings.greeting}”</span>
                          </p>
                        ) : null,
                      )}
                    </div>
                  </PreviewSection>
                ) : null}

                <PreviewSection title="Spoken disclosure">
                  {selected.disclosure ? (
                    <div className="space-y-1.5">
                      <div className="flex flex-wrap gap-1.5">
                        {selected.disclosure.spokenAiDisclosure ? (
                          <Badge variant="info">AI disclosure spoken</Badge>
                        ) : null}
                        {selected.disclosure.recordingConsent ? (
                          <Badge variant="info">Recording notice</Badge>
                        ) : null}
                      </div>
                      {selected.disclosure.text ? (
                        <p className="text-sm italic">
                          “{selected.disclosure.text}”
                        </p>
                      ) : null}
                    </div>
                  ) : (
                    <p className="text-sm text-muted-foreground">
                      This pack defines no disclosure block.
                    </p>
                  )}
                </PreviewSection>

                <PreviewSection title="USSD menu">
                  {selected.ussdMenu && selected.ussdMenu.length > 0 ? (
                    <ol className="space-y-1">
                      {selected.ussdMenu.map((item) => (
                        <li
                          key={item.key}
                          className="flex items-center gap-2 text-sm"
                        >
                          <Badge variant="outline">{item.key}</Badge>
                          <span>{item.label}</span>
                          {item.action ? (
                            <Badge variant="secondary">{item.action}</Badge>
                          ) : null}
                        </li>
                      ))}
                    </ol>
                  ) : (
                    <p className="text-sm text-muted-foreground">
                      No USSD menu — the gateway runs pass-through text mode
                      for this pack.
                    </p>
                  )}
                  {selected.i18n
                    ? Object.entries(selected.i18n).map(([locale, strings]) =>
                        strings.ussdPrompt ? (
                          <p
                            key={locale}
                            className="mt-1.5 text-xs text-muted-foreground"
                          >
                            <Badge variant="outline" className="mr-1">
                              {locale}
                            </Badge>
                            {strings.ussdPrompt}
                          </p>
                        ) : null,
                      )
                    : null}
                </PreviewSection>

                {selected.consentTextExcerpt ? (
                  <PreviewSection title="Consent notice (excerpt)">
                    <p className="whitespace-pre-line text-sm">
                      {selected.consentTextExcerpt}
                    </p>
                  </PreviewSection>
                ) : null}

                {Object.keys(selected.terminology).length > 0 ? (
                  <PreviewSection title="Terminology">
                    <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm sm:grid-cols-3">
                      {Object.entries(selected.terminology).map(([k, v]) => (
                        <div key={k}>
                          <dt className="text-xs text-muted-foreground">{k}</dt>
                          <dd>{v}</dd>
                        </div>
                      ))}
                    </dl>
                  </PreviewSection>
                ) : null}

                {selected.bookingPolicy ? (
                  <PreviewSection title="Booking policy">
                    <div className="flex flex-wrap gap-1.5">
                      {selected.bookingPolicy.intakeRequired ? (
                        <Badge variant="outline">Intake form required</Badge>
                      ) : null}
                      {selected.bookingPolicy.phoneConfirmation ? (
                        <Badge variant="outline">Phone confirmation</Badge>
                      ) : null}
                      {selected.bookingPolicy.depositPercent != null ? (
                        <Badge variant="outline">
                          Deposit {selected.bookingPolicy.depositPercent}%
                        </Badge>
                      ) : null}
                      {selected.bookingPolicy.cancellationWindowHours != null ? (
                        <Badge variant="outline">
                          Cancel window{" "}
                          {selected.bookingPolicy.cancellationWindowHours}h
                        </Badge>
                      ) : null}
                    </div>
                  </PreviewSection>
                ) : null}

                {selected.growth ? (
                  <PreviewSection title="Growth (CAC playbook)">
                    <div className="flex flex-wrap gap-1.5">
                      {selected.growth.cacTargetNgn != null ? (
                        <Badge variant="secondary">
                          CAC target ₦
                          {selected.growth.cacTargetNgn.toLocaleString("en-NG")}
                        </Badge>
                      ) : null}
                      {selected.growth.referralBountyNgn != null ? (
                        <Badge variant="secondary">
                          Referral bounty ₦
                          {selected.growth.referralBountyNgn.toLocaleString(
                            "en-NG",
                          )}
                        </Badge>
                      ) : null}
                      {(selected.growth.primaryChannels ?? []).map((ch) => (
                        <Badge key={ch} variant="outline">
                          {ch}
                        </Badge>
                      ))}
                    </div>
                    {Object.values(selected.i18n ?? {}).some(
                      (s) => s.referralLine,
                    ) ? (
                      <p className="mt-1.5 text-xs italic text-muted-foreground">
                        “
                        {
                          Object.values(selected.i18n ?? {}).find(
                            (s) => s.referralLine,
                          )?.referralLine
                        }
                        ”
                      </p>
                    ) : null}
                  </PreviewSection>
                ) : null}

                <div className="flex items-center justify-between gap-3 border-t border-border pt-4">
                  {selected.id === activeId ? (
                    <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
                      <CheckCircle2 className="h-4 w-4 text-success" />
                      This pack is active for the organisation.
                    </p>
                  ) : canManage ? (
                    <Button
                      onClick={() => void activate(selected)}
                      disabled={activating}
                    >
                      {activating ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                      ) : null}
                      {activating ? "Activating…" : "Activate this pack"}
                    </Button>
                  ) : (
                    <p className="text-sm text-muted-foreground">
                      Pack activation requires an owner or admin role.
                    </p>
                  )}
                </div>
              </CardContent>
            </Card>
          ) : (
            <Card>
              <CardContent className="py-10 text-center text-sm text-muted-foreground">
                Select a pack to preview its disclosure, USSD menu and
                terminology.
              </CardContent>
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}
