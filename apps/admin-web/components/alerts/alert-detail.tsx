"use client";

/**
 * SPEC-W30 WS-D: alert detail drawer + resolve dialog.
 *
 * The drawer fetches the full alert (GET /v1/graph/alerts/{id}) and renders
 * the evidence JSON readably, per detector type (SPEC-W30 §0):
 *   - referral_cycle      → the cycle path as an ordered list
 *   - geo_impossibility   → both coordinates + the computed travel speed
 *   - consent_backdating  → the two timestamps (consent granted vs first
 *                           message) with the backdating delta
 *   - anything else       → key/value rows + the raw JSON behind a
 *                           disclosure, so every alert stays replayable
 *                           (SPEC-W30 §5 gate 3)
 *
 * Resolution posts {decision, reason} to /v1/graph/alerts/{id}/resolve;
 * the reason is mandatory (≥10 chars — mirrored client-side from the
 * server's 422 rule) because it becomes part of the audit trail
 * (SPEC-W30 §5 gate 4). Detection ≠ punishment (§0): the copy makes clear
 * that "confirmed" keeps any quarantine and "dismissed" lifts it when no
 * other open high-severity alert remains on that person.
 */
import * as React from "react";
import Link from "next/link";
import { Loader2, ShieldCheck, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label, Textarea } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";
import { SeverityChip } from "./alerts-table";
import {
  ALERTS_API,
  alertTypeLabel,
  normalizeAlert,
  validateResolveReason,
  type FraudAlert,
  type ResolveDecision,
} from "./types";

/* ------------------------------------------------------------- helpers */

function asRecords(v: unknown): Record<string, unknown>[] {
  if (!Array.isArray(v)) return [];
  return v.map((x) =>
    typeof x === "object" && x !== null ? (x as Record<string, unknown>) : { value: x },
  );
}

function pick(ev: Record<string, unknown>, ...keys: string[]): unknown {
  for (const k of keys) {
    if (ev[k] !== undefined && ev[k] !== null) return ev[k];
  }
  return undefined;
}

function ts(v: unknown): string {
  return typeof v === "string" && v ? formatDateTime(v) : "—";
}

interface GeoPoint {
  lat: number;
  lon: number;
  at?: string;
  label?: string;
}

function parseGeoPoint(raw: unknown, label?: string): GeoPoint | null {
  if (typeof raw !== "object" || raw === null) return null;
  const o = raw as Record<string, unknown>;
  const lat = Number(o.lat ?? o.latitude);
  const lon = Number(o.lon ?? o.lng ?? o.longitude);
  if (!Number.isFinite(lat) || !Number.isFinite(lon)) return null;
  return {
    lat,
    lon,
    at: typeof o.at === "string" ? o.at : typeof o.captured_at === "string" ? o.captured_at : undefined,
    label,
  };
}

/** Great-circle distance in km (WGS84 mean radius). */
function haversineKm(a: GeoPoint, b: GeoPoint): number {
  const rad = Math.PI / 180;
  const dLat = (b.lat - a.lat) * rad;
  const dLon = (b.lon - a.lon) * rad;
  const h =
    Math.sin(dLat / 2) ** 2 +
    Math.cos(a.lat * rad) * Math.cos(b.lat * rad) * Math.sin(dLon / 2) ** 2;
  return 6371 * 2 * Math.asin(Math.min(1, Math.sqrt(h)));
}

/* -------------------------------------------------- evidence renderers */

function EvidenceSection({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1.5">
      <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
        {title}
      </p>
      {children}
    </div>
  );
}

/** D1: referral ring — cycle path as an ordered list. */
function ReferralCycleEvidence({ evidence }: { evidence: Record<string, unknown> }) {
  const cycle = asRecords(
    pick(evidence, "cycle", "cycle_path", "path", "persons", "ring"),
  );
  const label = (r: Record<string, unknown>, i: number) =>
    String(r.name ?? r.person_id ?? r.id ?? r.value ?? `person ${i + 1}`);
  return (
    <EvidenceSection title="Referral cycle (in order)">
      {cycle.length === 0 ? (
        <GenericEvidence evidence={evidence} />
      ) : (
        <ol className="list-decimal space-y-1 pl-5 text-sm">
          {cycle.map((r, i) => (
            <li key={i} className="font-mono text-xs">
              {label(r, i)}
            </li>
          ))}
          <li className="font-mono text-xs text-muted-foreground">
            …back to {label(cycle[0], 0)} (cycle closes)
          </li>
        </ol>
      )}
    </EvidenceSection>
  );
}

/** D4: geo impossibility — coordinates + computed travel speed. */
function GeoEvidence({ evidence }: { evidence: Record<string, unknown> }) {
  const from =
    parseGeoPoint(pick(evidence, "from", "previous", "a"), "Previous capture") ??
    parseGeoPoint(asRecords(evidence.points)[0], "Capture 1");
  const to =
    parseGeoPoint(pick(evidence, "to", "next", "b"), "Next capture") ??
    parseGeoPoint(asRecords(evidence.points)[1], "Capture 2");

  const givenSpeed = Number(pick(evidence, "speed_kmh", "speed"));
  const givenDist = Number(pick(evidence, "distance_km", "distance"));

  let dist: number | null = Number.isFinite(givenDist) ? givenDist : null;
  let speed: number | null = Number.isFinite(givenSpeed) ? givenSpeed : null;
  if (from && to) {
    if (dist === null) dist = haversineKm(from, to);
    if (speed === null && from.at && to.at) {
      const hours = (new Date(to.at).getTime() - new Date(from.at).getTime()) / 3_600_000;
      if (hours > 0) speed = dist / hours;
    }
  }

  if (!from || !to) return <GenericEvidence evidence={evidence} />;

  return (
    <EvidenceSection title="Impossible travel">
      <div className="space-y-2 text-sm">
        {[from, to].map((p, i) => (
          <div key={i} className="rounded-md border border-border px-3 py-2">
            <p className="text-xs text-muted-foreground">{p.label}</p>
            <p className="font-mono text-xs">
              {p.lat.toFixed(5)}, {p.lon.toFixed(5)}
            </p>
            <p className="text-xs">{ts(p.at)}</p>
          </div>
        ))}
        <p className="text-sm">
          {dist !== null ? (
            <>
              Distance <span className="font-semibold">{dist.toFixed(1)} km</span>
            </>
          ) : null}
          {speed !== null ? (
            <>
              {dist !== null ? " · implied speed " : "Implied speed "}
              <span className="font-semibold">{speed.toFixed(0)} km/h</span>
            </>
          ) : null}
        </p>
      </div>
    </EvidenceSection>
  );
}

/** D5: consent backdating — granted_at vs first-messaged timestamps. */
function ConsentBackdatingEvidence({ evidence }: { evidence: Record<string, unknown> }) {
  const granted = pick(evidence, "granted_at", "consent_granted_at", "consent_at");
  const messaged = pick(evidence, "first_messaged_at", "messaged_at", "first_message_at");
  const purpose = pick(evidence, "purpose", "consent_purpose");

  let deltaHours: number | null = null;
  if (typeof granted === "string" && typeof messaged === "string") {
    const ms = new Date(granted).getTime() - new Date(messaged).getTime();
    if (Number.isFinite(ms)) deltaHours = ms / 3_600_000;
  }

  return (
    <EvidenceSection title="Consent recorded after first contact">
      <dl className="space-y-1.5 text-sm">
        {typeof purpose === "string" ? (
          <div className="flex justify-between gap-3">
            <dt className="text-muted-foreground">Purpose</dt>
            <dd className="font-medium">{purpose}</dd>
          </div>
        ) : null}
        <div className="flex justify-between gap-3">
          <dt className="text-muted-foreground">First message sent</dt>
          <dd>{ts(messaged)}</dd>
        </div>
        <div className="flex justify-between gap-3">
          <dt className="text-muted-foreground">Consent granted</dt>
          <dd>{ts(granted)}</dd>
        </div>
        {deltaHours !== null ? (
          <div className="flex justify-between gap-3">
            <dt className="text-muted-foreground">Backdated by</dt>
            <dd className="font-medium">
              {deltaHours >= 24
                ? `${(deltaHours / 24).toFixed(1)} days`
                : `${deltaHours.toFixed(1)} hours`}
            </dd>
          </div>
        ) : null}
      </dl>
    </EvidenceSection>
  );
}

/** Fallback: key/value rows so any evidence stays human-readable. */
function GenericEvidence({ evidence }: { evidence: Record<string, unknown> }) {
  const entries = Object.entries(evidence);
  if (entries.length === 0) {
    return <p className="text-sm text-muted-foreground">No evidence payload recorded.</p>;
  }
  return (
    <dl className="space-y-1.5 text-sm">
      {entries.map(([k, v]) => (
        <div key={k} className="flex items-start justify-between gap-3">
          <dt className="shrink-0 text-muted-foreground">{k.replace(/_/g, " ")}</dt>
          <dd className="break-all text-right font-mono text-xs">
            {typeof v === "object" && v !== null ? JSON.stringify(v) : String(v)}
          </dd>
        </div>
      ))}
    </dl>
  );
}

function EvidenceView({ alert }: { alert: FraudAlert }) {
  const body =
    alert.type === "referral_cycle" ? (
      <ReferralCycleEvidence evidence={alert.evidence} />
    ) : alert.type === "geo_impossibility" ? (
      <GeoEvidence evidence={alert.evidence} />
    ) : alert.type === "consent_backdating" ? (
      <ConsentBackdatingEvidence evidence={alert.evidence} />
    ) : (
      <GenericEvidence evidence={alert.evidence} />
    );
  return (
    <div className="space-y-3">
      {body}
      {Object.keys(alert.evidence).length > 0 ? (
        <details className="rounded-md border border-border px-3 py-2 text-xs">
          <summary className="cursor-pointer font-medium text-muted-foreground">
            Raw evidence JSON (audit replay)
          </summary>
          <pre className="mt-2 overflow-x-auto rounded bg-muted p-2 font-mono text-[11px] leading-relaxed">
            {JSON.stringify(alert.evidence, null, 2)}
          </pre>
        </details>
      ) : null}
    </div>
  );
}

/* --------------------------------------------------------- the drawer */

export function AlertDetail({
  orgSlug,
  alert,
  open,
  onOpenChange,
  onResolved,
}: {
  orgSlug: string;
  /** the queue row that was clicked; null while closed */
  alert: FraudAlert | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** called after a successful resolve so the queue refreshes */
  onResolved: () => void;
}) {
  const { toast } = useToast();
  const [detail, setDetail] = React.useState<FraudAlert | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [resolveOpen, setResolveOpen] = React.useState(false);
  const [decision, setDecision] = React.useState<ResolveDecision>("confirmed");
  const [reason, setReason] = React.useState("");
  const [resolving, setResolving] = React.useState(false);

  // Load the full alert whenever a row is opened (the list row may carry a
  // truncated evidence payload).
  React.useEffect(() => {
    if (!open || !alert) return;
    setDetail(alert);
    setLoadError(null);
    setResolveOpen(false);
    setDecision("confirmed");
    setReason("");
    const controller = new AbortController();
    (async () => {
      setLoading(true);
      try {
        const data = await api.get<unknown>(
          `${ALERTS_API}/${encodeURIComponent(alert.alert_id)}`,
          { tenant: orgSlug },
          controller.signal,
        );
        if (controller.signal.aborted) return;
        setDetail(
          normalizeAlert(
            (typeof data === "object" && data !== null ? data : {}) as Record<
              string,
              unknown
            >,
          ),
        );
      } catch (e) {
        if (controller.signal.aborted) return;
        // Keep showing the list row's data; just note the refresh failure.
        if (!(e instanceof ApiError && e.status === 404)) {
          setLoadError("Full detail unavailable — showing the queue row data.");
        }
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    })();
    return () => controller.abort();
  }, [open, alert, orgSlug]);

  // Escape closes — same idiom as the helpdesk ticket drawer.
  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChange(false);
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [open, onOpenChange]);

  if (!open || !alert) return null;

  const a = detail ?? alert;
  const reasonError = validateResolveReason(reason);
  const isOpen = a.status === "open";

  async function resolve() {
    const err = validateResolveReason(reason);
    if (err) return;
    setResolving(true);
    try {
      const data = await api.post<unknown>(
        `${ALERTS_API}/${encodeURIComponent(a.alert_id)}/resolve`,
        { decision, reason: reason.trim() },
        { tenant: orgSlug },
      );
      if (typeof data === "object" && data !== null) {
        setDetail(normalizeAlert(data as Record<string, unknown>));
      } else {
        setDetail({ ...a, status: decision, resolve_reason: reason.trim() });
      }
      toast({
        title: decision === "confirmed" ? "Alert confirmed" : "Alert dismissed",
        description:
          decision === "dismissed"
            ? "Any fraud quarantine lifts once no other open high-severity alerts remain for this person."
            : "The person stays quarantined while this alert stands.",
        variant: "success",
      });
      setResolveOpen(false);
      onResolved();
    } catch (e) {
      toast({
        title: "Could not resolve alert",
        description:
          e instanceof ApiError ? e.message : "The graph service may be offline.",
        variant: "destructive",
      });
    } finally {
      setResolving(false);
    }
  }

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/40"
        aria-hidden
        onClick={() => onOpenChange(false)}
      />
      <div className="absolute inset-y-0 right-0 flex w-full max-w-lg flex-col border-l border-border bg-background shadow-xl">
        <div className="flex items-start justify-between gap-3 border-b border-border px-5 py-4">
          <div className="space-y-1.5">
            <div className="flex flex-wrap items-center gap-2">
              <h2 className="text-lg font-semibold">{alertTypeLabel(a.type)}</h2>
              <SeverityChip severity={a.severity} />
              <Badge variant={isOpen ? "info" : a.status === "confirmed" ? "warning" : "secondary"}>
                {a.status}
              </Badge>
            </div>
            <p className="font-mono text-xs text-muted-foreground">{a.alert_id}</p>
          </div>
          <button
            onClick={() => onOpenChange(false)}
            aria-label="Close"
            className="rounded-sm text-muted-foreground transition-opacity hover:opacity-70 cursor-pointer"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="flex-1 space-y-5 overflow-y-auto px-5 py-4">
          {loadError ? (
            <p className="rounded-md border border-border bg-muted px-3 py-2 text-xs text-muted-foreground">
              {loadError}
            </p>
          ) : null}
          {loading ? (
            <p className="flex items-center gap-2 text-xs text-muted-foreground">
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
              Refreshing detail…
            </p>
          ) : null}

          <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">Raised</dt>
            <dd>{ts(a.created_at)}</dd>
            <dt className="text-muted-foreground">Person</dt>
            <dd>
              {a.person_id ? (
                <Link
                  href={`/app/${orgSlug}/segments/persons/${encodeURIComponent(a.person_id)}`}
                  className="font-mono text-xs text-primary underline-offset-4 hover:underline"
                >
                  {a.person_id}
                </Link>
              ) : (
                "—"
              )}
            </dd>
            <dt className="text-muted-foreground">Field agent</dt>
            <dd className="font-mono text-xs">{a.agent_id ?? "—"}</dd>
            {a.resolved_at ? (
              <>
                <dt className="text-muted-foreground">Resolved</dt>
                <dd>
                  {ts(a.resolved_at)}
                  {a.resolved_by ? ` by ${a.resolved_by}` : ""}
                </dd>
              </>
            ) : null}
          </dl>

          {a.resolve_reason ? (
            <div className="rounded-md border border-border bg-muted px-3 py-2 text-sm">
              <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                Resolution reason
              </p>
              <p className="mt-1">{a.resolve_reason}</p>
            </div>
          ) : null}

          <div className="space-y-2">
            <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Evidence — why this fired
            </p>
            <EvidenceView alert={a} />
          </div>

          <p className="text-xs text-muted-foreground">
            Detection is not punishment: a person flagged here stays visible in
            the graph, and fraud-suspect records are never auto-erased (audit
            rights, SPEC-W30 §0).
          </p>
        </div>

        {isOpen ? (
          <div className="border-t border-border px-5 py-4">
            <Button className="w-full" onClick={() => setResolveOpen(true)}>
              <ShieldCheck className="h-4 w-4" />
              Resolve alert
            </Button>
          </div>
        ) : null}
      </div>

      <Dialog open={resolveOpen} onOpenChange={setResolveOpen}>
        <DialogContent onClose={() => setResolveOpen(false)}>
          <DialogHeader>
            <DialogTitle>Resolve alert</DialogTitle>
            <DialogDescription>
              Your decision and reason are written to the audit trail
              (who / when / why — SPEC-W30 §5 gate 4).
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-2" role="radiogroup" aria-label="Resolution decision">
            {(
              [
                {
                  value: "confirmed",
                  label: "Confirmed fraud",
                  hint: "The flag stands; any quarantine on the person is kept.",
                },
                {
                  value: "dismissed",
                  label: "Dismissed (false positive)",
                  hint: "The quarantine lifts once no other open high-severity alerts remain for this person.",
                },
              ] as const
            ).map((opt) => (
              <label
                key={opt.value}
                className="flex cursor-pointer items-start gap-3 rounded-md border border-border px-3 py-2.5 has-checked:border-primary has-checked:bg-accent"
              >
                <input
                  type="radio"
                  name="resolve-decision"
                  value={opt.value}
                  checked={decision === opt.value}
                  onChange={() => setDecision(opt.value)}
                  className="mt-1 h-4 w-4 accent-primary"
                />
                <span>
                  <span className="block text-sm font-medium">{opt.label}</span>
                  <span className="block text-xs text-muted-foreground">{opt.hint}</span>
                </span>
              </label>
            ))}
          </div>

          <div className="mt-4 space-y-1.5">
            <Label htmlFor="resolve-reason">Reason (required, at least 10 characters)</Label>
            <Textarea
              id="resolve-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              maxLength={1000}
              aria-invalid={reason !== "" && reasonError ? true : undefined}
              placeholder="e.g. Called the customer — they confirmed referring their three siblings during the promo."
            />
            {reason !== "" && reasonError ? (
              <p className="text-xs text-destructive">{reasonError}</p>
            ) : null}
          </div>

          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setResolveOpen(false)}
              disabled={resolving}
            >
              Cancel
            </Button>
            <Button
              onClick={() => void resolve()}
              disabled={resolving || reasonError !== null}
              title={reasonError ?? undefined}
            >
              {resolving ? "Resolving…" : "Submit resolution"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
