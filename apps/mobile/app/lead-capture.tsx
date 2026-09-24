/**
 * Lead capture (modal, SPEC-W16 §5 + SPEC-W46 MB-6 offline queue).
 *
 * Online path: POST /v1/leads with channel "field" — the real
 * httpapi.createLeadRequest contract (phone_e164 + channel_of_first_touch;
 * promo_code / ref / utm optional). The server dedupes first-touch leads
 * (201 created / 200 dedupe hit) and we surface which happened.
 *
 * Offline path (MB-6): when the capture can't reach the server (network
 * failure or 15s timeout — NOT a server rejection), the capture is queued
 * locally (src/field/queue.ts, expo-secure-store) and flushed through the
 * same batched, idempotent contract as the field PWA — POST
 * /v1/field/capture, deduped server-side on client_id (contract §4), so
 * re-flushes are exactly-once. The UI is HONEST about the queued state:
 * queued captures are reported as queued (never as synced), the queue
 * depth is shown, and sync results report delivered vs still-queued.
 */
import React from "react";
import { View, Text, ScrollView, StyleSheet } from "react-native";
import { useFocusEffect, useRouter } from "expo-router";
import { createLead, ApiError, TimeoutError } from "../src/api/client";
import {
  enqueueCapture,
  flushQueue,
  queueLength,
  QueueFullError,
} from "../src/field/queue";
import { Screen } from "../components/Screen";
import { Card } from "../components/Card";
import { Button, Field, ErrorBox } from "../components/ui";
import { colors, spacing } from "../src/theme";

/** Loose E.164 check — the server is the authoritative validator. */
function looksLikeE164(phone: string): boolean {
  return /^\+[1-9]\d{6,14}$/.test(phone.trim());
}

/** True when the request never got a server response (offline/dead
 * network/timeout) — the only failures safe to queue: anything the server
 * DID answer (validation, auth) must be shown, not queued. RN fetch
 * rejects with TypeError on network failure. */
function isOfflineFailure(e: unknown): boolean {
  return e instanceof TimeoutError || e instanceof TypeError;
}

export default function LeadCaptureModal() {
  const router = useRouter();
  const [phone, setPhone] = React.useState("");
  const [promoCode, setPromoCode] = React.useState("");
  const [ref, setRef] = React.useState("");
  const [notes, setNotes] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [result, setResult] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [queued, setQueued] = React.useState(0);
  const [syncing, setSyncing] = React.useState(false);
  const [syncMsg, setSyncMsg] = React.useState<string | null>(null);

  const refreshQueueCount = React.useCallback(async () => {
    try {
      setQueued(await queueLength());
    } catch {
      // queue unreadable — leave the count as-is; capture still works online
    }
  }, []);

  const syncQueue = React.useCallback(async () => {
    setSyncing(true);
    setSyncMsg(null);
    try {
      const report = await flushQueue();
      setQueued(report.remaining);
      setSyncMsg(
        report.synced
          ? `Queue synced — ${report.delivered} capture${report.delivered === 1 ? "" : "s"} delivered.`
          : `${report.delivered} delivered, ${report.remaining} still queued (server rejected or unreached — they are NOT lost).`,
      );
    } catch (e) {
      await refreshQueueCount();
      setSyncMsg(
        `Sync failed (${e instanceof Error ? e.message : String(e)}) — captures stay queued and will retry.`,
      );
    } finally {
      setSyncing(false);
    }
  }, [refreshQueueCount]);

  // On open: show the honest queue depth and best-effort drain it (a
  // failure just leaves items queued — the banner stays truthful).
  useFocusEffect(
    React.useCallback(() => {
      void (async () => {
        await refreshQueueCount();
        const n = await queueLength().catch(() => 0);
        if (n > 0) await syncQueue();
      })();
    }, [refreshQueueCount, syncQueue]),
  );

  const onSubmit = async () => {
    if (!looksLikeE164(phone)) {
      setError("Phone must be E.164, e.g. +2348012345678");
      return;
    }
    setError(null);
    setResult(null);
    setBusy(true);
    const trimmedPhone = phone.trim();
    const trimmedNotes = notes.trim();
    try {
      const res = await createLead({
        phone_e164: trimmedPhone,
        channel: "field",
        promo_code: promoCode.trim() || undefined,
        ref: ref.trim() || undefined,
        utm: trimmedNotes ? { field_notes: trimmedNotes } : undefined,
      });
      setResult(
        res.created
          ? `Lead captured (${res.lead.status}).`
          : "Already a first-touch lead for this phone — existing lead returned (dedupe).",
      );
      setPhone("");
      setPromoCode("");
      setRef("");
      setNotes("");
      // Online again — best-effort drain anything queued earlier.
      if (queued > 0) void syncQueue();
    } catch (e) {
      if (isOfflineFailure(e)) {
        // MB-6: never lose a field capture to a dead network. Queue it
        // with a client_id idempotency anchor and say so honestly.
        try {
          await enqueueCapture("lead_capture", {
            phone_e164: trimmedPhone,
            notes: trimmedNotes || undefined,
            // promo_code/ref are preserved verbatim on the server's
            // field_captures anchor row; the batch endpoint structures
            // only phone_e164 + utm into the lead (see src/field/queue.ts).
            promo_code: promoCode.trim() || undefined,
            ref: ref.trim() || undefined,
            utm: trimmedNotes ? { field_notes: trimmedNotes } : undefined,
          });
          setQueued((n) => n + 1);
          setResult(
            "Offline — lead saved to the on-device queue. It will sync automatically when you're back online (not yet on the server).",
          );
          setPhone("");
          setPromoCode("");
          setRef("");
          setNotes("");
        } catch (qerr) {
          setError(qerr instanceof QueueFullError ? qerr.message : String(qerr));
        }
      } else if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
        setError("Not permitted — your role needs manage_bookings on this tenant.");
      } else {
        setError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <Screen title="Capture lead" subtitle="Field capture — posts straight to the tenant BFF">
      <ScrollView keyboardShouldPersistTaps="handled">
        {error ? <ErrorBox message={error} /> : null}
        {result ? (
          <View style={styles.okBox}>
            <Text style={styles.okText}>{result}</Text>
          </View>
        ) : null}

        {queued > 0 ? (
          <Card
            title={`Offline queue (${queued})`}
            description="Captured on this device, not yet on the server. They sync automatically on connectivity and can never apply twice (server dedupes on client_id)."
          >
            {syncMsg ? <Text style={styles.syncMsg}>{syncMsg}</Text> : null}
            <Button
              title={syncing ? "Syncing…" : `Sync ${queued} queued capture${queued === 1 ? "" : "s"} now`}
              variant="secondary"
              onPress={() => void syncQueue()}
              loading={syncing}
            />
          </Card>
        ) : null}

        <Card title="Lead" description="Only the phone number is required.">
          <Field
            label="Phone (E.164)"
            value={phone}
            onChangeText={setPhone}
            placeholder="+2348012345678"
            keyboardType="phone-pad"
          />
          <Field
            label="Promo code (optional)"
            value={promoCode}
            onChangeText={setPromoCode}
            placeholder="e.g. RAINYDAY10"
          />
          <Field
            label="QR ref slug (optional)"
            value={ref}
            onChangeText={setRef}
            placeholder="e.g. counter-qr-3"
          />
          <Field
            label="Notes (optional)"
            value={notes}
            onChangeText={setNotes}
            placeholder="Context from the conversation"
            multiline
          />
        </Card>

        <Button title="Save lead" onPress={onSubmit} loading={busy} />
        <View style={{ height: spacing.sm }} />
        <Button title="Close" variant="secondary" onPress={() => router.back()} />

        <Text style={styles.consent}>
          Capture leads only with the person's verbal consent — NDPA consent
          records are attached server-side via consent_id where required.
        </Text>
        <View style={{ height: spacing.xl }} />
      </ScrollView>
    </Screen>
  );
}

const styles = StyleSheet.create({
  okBox: {
    backgroundColor: colors.successSoft,
    borderRadius: 8,
    padding: spacing.md,
    marginBottom: spacing.md,
  },
  okText: { fontSize: 13, color: colors.success },
  syncMsg: {
    fontSize: 12,
    color: colors.mutedForeground,
    marginBottom: spacing.sm,
  },
  consent: {
    marginTop: spacing.md,
    fontSize: 11,
    color: colors.mutedForeground,
    textAlign: "center",
  },
});
