"use client";

/**
 * SPEC-W45 K13 waitlist claim (public, token-authorized).
 *
 * Backend contract (booking-service, public waitlist endpoints):
 *   GET  /api/bookings/v1/waitlist/claim-info?token=<uuid>
 *        (X-Tenant-Slug: <slug>) → slot details for the offered backfill
 *   POST /api/bookings/v1/waitlist/claim { token }
 *        (X-Tenant-Slug: <slug>) → { booking, entry } on success
 *
 * The page renders the offered slot, a confirm button that completes the
 * claim, and a decline path (client-side — declining simply does not claim;
 * the offer expires per the tenant's backfill policy).
 */
import * as React from "react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { formatDateTime } from "@/lib/utils";

/** Shapes decoded defensively — see CONTRACT NOTE in the W45 report. */
interface ClaimInfoEntry {
  id?: string;
  contact_name?: string;
  window_start?: string;
  window_end?: string;
  status?: string;
  offering_id?: string;
}

interface ClaimInfoOffering {
  id?: string;
  name?: string;
  duration_minutes?: number;
}

interface ClaimInfo {
  entry?: ClaimInfoEntry;
  offering?: ClaimInfoOffering;
  tenant?: { name?: string; slug?: string };
  claimable?: boolean;
  reason?: string;
}

interface ClaimResponse {
  booking?: { id?: string; starts_at?: string; status?: string };
  entry?: ClaimInfoEntry;
}

type State =
  | { kind: "loading" }
  | { kind: "ready"; info: ClaimInfo }
  | { kind: "claiming"; info: ClaimInfo }
  | { kind: "claimed"; booking?: ClaimResponse["booking"] }
  | { kind: "declined"; info: ClaimInfo }
  | { kind: "error"; message: string };

export function ClaimClient({
  tenantSlug,
  token,
}: {
  tenantSlug: string;
  token: string;
}) {
  const [state, setState] = React.useState<State>({ kind: "loading" });

  React.useEffect(() => {
    if (!token) {
      setState({
        kind: "error",
        message:
          "This claim link is missing its token. Open the full link from your notification.",
      });
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const info = await api.get<ClaimInfo>(
          "/api/bookings/v1/waitlist/claim-info",
          { tenant: tenantSlug, token },
        );
        if (!cancelled) setState({ kind: "ready", info });
      } catch (e) {
        if (cancelled) return;
        setState({
          kind: "error",
          message:
            e instanceof ApiError && (e.status === 404 || e.status === 410 || e.status === 409)
              ? "This offer is no longer available — it may have expired or already been claimed."
              : e instanceof ApiError
                ? e.message
                : "We could not load this offer. Please try again later.",
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [tenantSlug, token]);

  const confirm = async (info: ClaimInfo) => {
    setState({ kind: "claiming", info });
    try {
      const res = await api.post<ClaimResponse>(
        "/api/bookings/v1/waitlist/claim",
        { token },
        { tenant: tenantSlug },
      );
      setState({ kind: "claimed", booking: res.booking });
    } catch (e) {
      setState({
        kind: "error",
        message:
          e instanceof ApiError && (e.status === 409 || e.status === 410)
            ? "Someone else claimed this slot first, or the offer expired. The business may offer you another slot soon."
            : e instanceof ApiError
              ? e.message
              : "The claim could not be completed. Please try again.",
      });
    }
  };

  const entry = "info" in state ? state.info.entry : undefined;
  const offering = "info" in state ? state.info.offering : undefined;
  const businessName =
    ("info" in state && state.info.tenant?.name) || "the business";

  return (
    <div className="flex min-h-screen items-center justify-center bg-muted/40 px-4 py-10">
      <Card className="w-full max-w-md">
        <CardHeader className="text-center">
          <CardTitle className="text-xl">A slot opened up</CardTitle>
          <CardDescription>
            {state.kind === "claimed"
              ? "Your booking is confirmed."
              : state.kind === "declined"
                ? "No problem — nothing was booked."
                : `${businessName} offered you an earlier slot from the waitlist.`}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {state.kind === "loading" ? (
            <p className="text-center text-sm text-muted-foreground">
              Loading your offer…
            </p>
          ) : null}

          {state.kind === "error" ? (
            <p className="text-center text-sm text-destructive">
              {state.message}
            </p>
          ) : null}

          {state.kind === "ready" || state.kind === "claiming" || state.kind === "declined" ? (
            <>
              <dl className="space-y-2 rounded-lg border border-border bg-card p-4 text-sm">
                {offering?.name ? (
                  <div className="flex justify-between gap-4">
                    <dt className="text-muted-foreground">Service</dt>
                    <dd className="font-medium">{offering.name}</dd>
                  </div>
                ) : null}
                {entry?.contact_name ? (
                  <div className="flex justify-between gap-4">
                    <dt className="text-muted-foreground">Name</dt>
                    <dd className="font-medium">{entry.contact_name}</dd>
                  </div>
                ) : null}
                {entry?.window_start ? (
                  <div className="flex justify-between gap-4">
                    <dt className="text-muted-foreground">Earliest</dt>
                    <dd className="font-medium">
                      {formatDateTime(entry.window_start)}
                    </dd>
                  </div>
                ) : null}
                {entry?.window_end ? (
                  <div className="flex justify-between gap-4">
                    <dt className="text-muted-foreground">Latest</dt>
                    <dd className="font-medium">
                      {formatDateTime(entry.window_end)}
                    </dd>
                  </div>
                ) : null}
              </dl>

              {state.kind === "declined" ? (
                <p className="text-center text-sm text-muted-foreground">
                  You declined this slot. Your original booking (if any) is
                  unchanged.
                </p>
              ) : (
                <div className="flex flex-col gap-2">
                  <Button
                    size="lg"
                    disabled={state.kind === "claiming"}
                    onClick={() => void confirm(state.info)}
                  >
                    {state.kind === "claiming"
                      ? "Confirming…"
                      : "Confirm this slot"}
                  </Button>
                  <Button
                    variant="outline"
                    disabled={state.kind === "claiming"}
                    onClick={() => setState({ kind: "declined", info: state.info })}
                  >
                    No thanks
                  </Button>
                  <p className="text-center text-xs text-muted-foreground">
                    Confirming books the slot immediately. If it was already
                    taken, nothing changes.
                  </p>
                </div>
              )}
            </>
          ) : null}

          {state.kind === "claimed" ? (
            <div className="space-y-3 text-center">
              {state.booking?.starts_at ? (
                <p className="text-sm">
                  See you on{" "}
                  <span className="font-medium">
                    {formatDateTime(state.booking.starts_at)}
                  </span>
                  .
                </p>
              ) : (
                <p className="text-sm">
                  Your waitlist offer was claimed successfully.
                </p>
              )}
              <p className="text-xs text-muted-foreground">
                You can close this page now.
              </p>
            </div>
          ) : null}
        </CardContent>
      </Card>
    </div>
  );
}
