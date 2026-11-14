"use client";

import * as React from "react";
import {
  Activity,
  Copy,
  MessageSquare,
  Phone,
  RefreshCw,
  Send,
} from "lucide-react";
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
import type { Tenant } from "@/lib/types";

/**
 * Wave 6 (SPEC-W6 §C) — per-tenant channel enablement for the omnichannel
 * messaging-gateway (Go, :7011). Dev-grade persistence: toggles and config
 * fields are stored in localStorage keyed by tenant slug; the panel generates
 * the CHANNEL_SITE_MAP env snippet and the provider webhook URLs to paste
 * into the Meta / Telegram consoles.
 */

interface ChannelState {
  whatsapp: { enabled: boolean; phoneNumberId: string; displayNumber: string };
  telegram: { enabled: boolean; botUsername: string };
  web: { enabled: boolean };
  gatewayUrl: string;
}

const DEFAULT_GATEWAY_URL =
  process.env.NEXT_PUBLIC_MESSAGING_GATEWAY_URL ?? "http://localhost:7011";

const DEFAULT_STATE: ChannelState = {
  whatsapp: { enabled: false, phoneNumberId: "", displayNumber: "" },
  telegram: { enabled: false, botUsername: "" },
  web: { enabled: true },
  gatewayUrl: DEFAULT_GATEWAY_URL,
};

function storageKey(orgSlug: string): string {
  return `opendesk:channels:${orgSlug}`;
}

function loadState(orgSlug: string): ChannelState {
  try {
    const raw = window.localStorage.getItem(storageKey(orgSlug));
    if (!raw) return DEFAULT_STATE;
    const parsed = JSON.parse(raw) as Partial<ChannelState>;
    return {
      whatsapp: { ...DEFAULT_STATE.whatsapp, ...parsed.whatsapp },
      telegram: { ...DEFAULT_STATE.telegram, ...parsed.telegram },
      web: { ...DEFAULT_STATE.web, ...parsed.web },
      gatewayUrl: parsed.gatewayUrl || DEFAULT_GATEWAY_URL,
    };
  } catch {
    return DEFAULT_STATE;
  }
}

type Health = "checking" | "healthy" | "unreachable";

function Toggle({
  checked,
  onChange,
  label,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      onClick={() => onChange(!checked)}
      className={`relative h-6 w-11 rounded-full transition-colors cursor-pointer ${checked ? "bg-success" : "bg-input"}`}
    >
      <span
        className={`absolute top-0.5 h-5 w-5 rounded-full bg-card shadow transition-transform ${checked ? "translate-x-5.5 left-0.5" : "left-0.5"}`}
      />
    </button>
  );
}

export function ChannelsClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [state, setState] = React.useState<ChannelState>(DEFAULT_STATE);
  const [hydrated, setHydrated] = React.useState(false);
  const [tenantId, setTenantId] = React.useState<string | null>(null);
  const [tenantError, setTenantError] = React.useState<string | null>(null);
  const [health, setHealth] = React.useState<Health>("checking");

  // Hydrate from localStorage (client-only) on mount.
  React.useEffect(() => {
    setState(loadState(orgSlug));
    setHydrated(true);
  }, [orgSlug]);

  // Persist on every change after hydration.
  React.useEffect(() => {
    if (!hydrated) return;
    try {
      window.localStorage.setItem(storageKey(orgSlug), JSON.stringify(state));
    } catch {
      // Storage full / unavailable — the page still works for this session.
    }
  }, [hydrated, orgSlug, state]);

  // Resolve the tenant UUID for the CHANNEL_SITE_MAP snippet.
  React.useEffect(() => {
    (async () => {
      try {
        const t = await api.get<Tenant>(`/api/identity/v1/tenants/${orgSlug}`);
        setTenantId(t.id);
      } catch (e) {
        setTenantError(
          e instanceof ApiError
            ? e.message
            : "Failed to resolve the tenant id.",
        );
      }
    })();
  }, [orgSlug]);

  const gatewayBase = state.gatewayUrl.trim().replace(/\/+$/, "");

  // Channel health: client-side GET {gateway}/healthz with a bounded timeout.
  const checkHealth = React.useCallback(
    async (signal?: AbortSignal) => {
      if (!gatewayBase) {
        setHealth("unreachable");
        return;
      }
      setHealth("checking");
      try {
        const res = await fetch(`${gatewayBase}/healthz`, {
          signal: signal ?? AbortSignal.timeout(5000),
          cache: "no-store",
        });
        setHealth(res.ok ? "healthy" : "unreachable");
      } catch {
        setHealth("unreachable");
      }
    },
    [gatewayBase],
  );

  React.useEffect(() => {
    if (!hydrated) return;
    const controller = new AbortController();
    // Debounce while the gateway URL is being edited.
    const timer = setTimeout(() => void checkHealth(controller.signal), 400);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [hydrated, checkHealth]);

  const resolvedTenantId = tenantId ?? "<tenant-uuid>";

  // CHANNEL_SITE_MAP (SPEC-W6 §A3): keys are "whatsapp:<phone_number_id>" and
  // "telegram:<bot_username>", values resolve the tenant for the bridge.
  const siteMap = React.useMemo(() => {
    const map: Record<string, { site_slug: string; tenant_id: string }> = {};
    if (state.whatsapp.enabled && state.whatsapp.phoneNumberId.trim()) {
      map[`whatsapp:${state.whatsapp.phoneNumberId.trim()}`] = {
        site_slug: orgSlug,
        tenant_id: resolvedTenantId,
      };
    }
    if (state.telegram.enabled && state.telegram.botUsername.trim()) {
      const username = state.telegram.botUsername.trim().replace(/^@/, "");
      map[`telegram:${username}`] = {
        site_slug: orgSlug,
        tenant_id: resolvedTenantId,
      };
    }
    return map;
  }, [orgSlug, resolvedTenantId, state.whatsapp, state.telegram]);

  const siteMapJson = JSON.stringify(siteMap);

  const whatsappWebhookUrl = `${gatewayBase || "https://<gateway-host>"}/webhooks/whatsapp`;
  const telegramWebhookUrl = `${gatewayBase || "https://<gateway-host>"}/webhooks/telegram`;
  const embedSnippet = `<script src="${typeof window !== "undefined" ? window.location.origin : ""}/embed.js" data-site="${orgSlug}" async></script>`;

  const copy = async (value: string, label: string) => {
    try {
      await navigator.clipboard.writeText(value);
      toast({ title: `${label} copied`, variant: "success" });
    } catch {
      toast({
        title: "Copy failed",
        description: "Select the text and copy it manually.",
        variant: "destructive",
      });
    }
  };

  const healthBadge =
    health === "checking" ? (
      <Badge variant="secondary">Checking…</Badge>
    ) : health === "healthy" ? (
      <Badge variant="success">Gateway healthy</Badge>
    ) : (
      <Badge variant="warning">Gateway unreachable</Badge>
    );

  return (
    <div className="max-w-3xl">
      <PageHeader
        title="Channels"
        description="Connect WhatsApp, Telegram and web chat to the messaging gateway."
        actions={
          <div className="flex items-center gap-2">
            {healthBadge}
            <Button
              variant="outline"
              size="sm"
              onClick={() => void checkHealth()}
            >
              <RefreshCw className="h-3.5 w-3.5" /> Recheck
            </Button>
          </div>
        }
      />
      {tenantError ? (
        <ErrorNote
          message={`Could not load the tenant record (${tenantError}). The snippet below uses a <tenant-uuid> placeholder — replace it before deploying.`}
        />
      ) : null}

      <div className="space-y-6">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Activity className="h-4 w-4" /> Messaging gateway
            </CardTitle>
            <CardDescription>
              Base URL of the messaging-gateway service (default port 7011).
              Used for the health check and to build the webhook URLs below.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="gw-url">Gateway URL</Label>
              <Input
                id="gw-url"
                type="url"
                value={state.gatewayUrl}
                onChange={(e) =>
                  setState((s) => ({ ...s, gatewayUrl: e.target.value }))
                }
                placeholder={DEFAULT_GATEWAY_URL}
              />
              <p className="text-xs text-muted-foreground">
                {health === "unreachable"
                  ? "No response at /healthz — the gateway is offline, the URL is wrong, or the browser was blocked (CORS). Channel settings are still saved locally."
                  : "Health is read from GET /healthz."}{" "}
                In production use the public gateway host so providers can
                reach the webhooks.
              </p>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex-row items-start justify-between space-y-0">
            <div>
              <CardTitle className="flex items-center gap-2">
                <Phone className="h-4 w-4" /> WhatsApp
              </CardTitle>
              <CardDescription>
                Meta WhatsApp Cloud API. Inbound messages arrive at the
                gateway&apos;s /webhooks/whatsapp endpoint.
              </CardDescription>
            </div>
            <Toggle
              checked={state.whatsapp.enabled}
              onChange={(next) =>
                setState((s) => ({
                  ...s,
                  whatsapp: { ...s.whatsapp, enabled: next },
                }))
              }
              label="Enable WhatsApp"
            />
          </CardHeader>
          <CardContent className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="wa-phone-id">Phone number ID</Label>
              <Input
                id="wa-phone-id"
                value={state.whatsapp.phoneNumberId}
                onChange={(e) =>
                  setState((s) => ({
                    ...s,
                    whatsapp: { ...s.whatsapp, phoneNumberId: e.target.value },
                  }))
                }
                placeholder="123456789012345"
                disabled={!state.whatsapp.enabled}
              />
              <p className="text-xs text-muted-foreground">
                From Meta for Developers → WhatsApp → API setup.
              </p>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="wa-display">Display number</Label>
              <Input
                id="wa-display"
                value={state.whatsapp.displayNumber}
                onChange={(e) =>
                  setState((s) => ({
                    ...s,
                    whatsapp: { ...s.whatsapp, displayNumber: e.target.value },
                  }))
                }
                placeholder="+1 555 010 1234"
                disabled={!state.whatsapp.enabled}
              />
              <p className="text-xs text-muted-foreground">
                The customer-facing number (display only).
              </p>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex-row items-start justify-between space-y-0">
            <div>
              <CardTitle className="flex items-center gap-2">
                <Send className="h-4 w-4" /> Telegram
              </CardTitle>
              <CardDescription>
                Telegram Bot API. One bot per gateway deployment; updates
                arrive at /webhooks/telegram.
              </CardDescription>
            </div>
            <Toggle
              checked={state.telegram.enabled}
              onChange={(next) =>
                setState((s) => ({
                  ...s,
                  telegram: { ...s.telegram, enabled: next },
                }))
              }
              label="Enable Telegram"
            />
          </CardHeader>
          <CardContent className="grid gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="tg-username">Bot username</Label>
              <Input
                id="tg-username"
                value={state.telegram.botUsername}
                onChange={(e) =>
                  setState((s) => ({
                    ...s,
                    telegram: { ...s.telegram, botUsername: e.target.value },
                  }))
                }
                placeholder="@your_opendesk_bot"
                disabled={!state.telegram.enabled}
              />
              <p className="text-xs text-muted-foreground">
                Created via @BotFather. Must match the gateway&apos;s
                TELEGRAM_BOT_USERNAME.
              </p>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex-row items-start justify-between space-y-0">
            <div>
              <CardTitle className="flex items-center gap-2">
                <MessageSquare className="h-4 w-4" /> Web chat
              </CardTitle>
              <CardDescription>
                The built-in chat widget on your public booking site and any
                page where you paste the embed snippet.
              </CardDescription>
            </div>
            <Toggle
              checked={state.web.enabled}
              onChange={(next) =>
                setState((s) => ({ ...s, web: { enabled: next } }))
              }
              label="Enable web chat"
            />
          </CardHeader>
          <CardContent>
            {state.web.enabled ? (
              <div className="flex items-start gap-2">
                <pre className="min-w-0 flex-1 overflow-x-auto rounded-md bg-muted p-3 text-xs">
                  <code>{embedSnippet}</code>
                </pre>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void copy(embedSnippet, "Embed snippet")}
                >
                  <Copy className="h-3.5 w-3.5" /> Copy
                </Button>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">
                Web chat is disabled. Re-enable it to show the embed snippet.
              </p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Gateway config</CardTitle>
            <CardDescription>
              Paste this into the messaging-gateway environment as{" "}
              <span className="font-mono">CHANNEL_SITE_MAP</span>, then point
              each provider console at the webhook URLs below.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-1.5">
              <Label>CHANNEL_SITE_MAP</Label>
              <div className="flex items-start gap-2">
                <pre className="min-w-0 flex-1 overflow-x-auto rounded-md bg-muted p-3 text-xs">
                  <code>{siteMapJson}</code>
                </pre>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void copy(siteMapJson, "CHANNEL_SITE_MAP")}
                >
                  <Copy className="h-3.5 w-3.5" /> Copy
                </Button>
              </div>
              {Object.keys(siteMap).length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  Enable WhatsApp or Telegram and fill in its identifier to
                  generate a mapping entry.
                </p>
              ) : null}
            </div>

            <div className="grid gap-1.5">
              <Label>WhatsApp webhook (Meta console)</Label>
              <div className="flex items-start gap-2">
                <pre className="min-w-0 flex-1 overflow-x-auto rounded-md bg-muted p-3 text-xs">
                  <code>{whatsappWebhookUrl}</code>
                </pre>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void copy(whatsappWebhookUrl, "Webhook URL")}
                >
                  <Copy className="h-3.5 w-3.5" /> Copy
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                Set the verify token to the gateway&apos;s{" "}
                <span className="font-mono">WHATSAPP_VERIFY_TOKEN</span> value
                when Meta asks to verify the callback URL.
              </p>
            </div>

            <div className="grid gap-1.5">
              <Label>Telegram webhook (@BotFather / Bot API)</Label>
              <div className="flex items-start gap-2">
                <pre className="min-w-0 flex-1 overflow-x-auto rounded-md bg-muted p-3 text-xs">
                  <code>{telegramWebhookUrl}</code>
                </pre>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void copy(telegramWebhookUrl, "Webhook URL")}
                >
                  <Copy className="h-3.5 w-3.5" /> Copy
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                Register it with{" "}
                <span className="font-mono">
                  @BotFather /setWebhook → {telegramWebhookUrl}
                </span>
                . If the gateway has{" "}
                <span className="font-mono">TELEGRAM_WEBHOOK_SECRET</span> set,
                pass the same value as secret_token.
              </p>
            </div>
          </CardContent>
        </Card>

        <p className="text-xs text-muted-foreground">
          Channel settings are stored in this browser (localStorage, per
          tenant) as a development convenience — the generated snippet is what
          actually configures the gateway.
        </p>
      </div>
    </div>
  );
}
