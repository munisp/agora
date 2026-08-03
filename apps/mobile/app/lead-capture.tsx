/**
 * Lead capture (modal, SPEC-W16 §5).
 *
 * Online path: POST /v1/leads with channel "field" — the real
 * httpapi.createLeadRequest contract (phone_e164 + channel_of_first_touch;
 * promo_code / ref / utm optional). The server dedupes first-touch leads
 * (201 created / 200 dedupe hit) and we surface which happened.
 *
 * Offline batch path: src/api/client.submitFieldCapture posts the
 * SPEC-W16 §4 queue shape ({client_id, kind, payload, captured_at, gps})
 * to POST /v1/field/capture — used by connectivity-aware callers; this
 * modal captures online and reports the honest error when offline (the
 * standalone field PWA owns the IndexedDB offline queue — contract §4).
 */
import React from "react";
import { View, Text, ScrollView, StyleSheet } from "react-native";
import { useRouter } from "expo-router";
import { createLead, ApiError } from "../src/api/client";
import { Screen } from "../components/Screen";
import { Card } from "../components/Card";
import { Button, Field, ErrorBox } from "../components/ui";
import { colors, spacing } from "../src/theme";

/** Loose E.164 check — the server is the authoritative validator. */
function looksLikeE164(phone: string): boolean {
  return /^\+[1-9]\d{6,14}$/.test(phone.trim());
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

  const onSubmit = async () => {
    if (!looksLikeE164(phone)) {
      setError("Phone must be E.164, e.g. +2348012345678");
      return;
    }
    setError(null);
    setBusy(true);
    try {
      const res = await createLead({
        phone_e164: phone.trim(),
        channel: "field",
        promo_code: promoCode.trim() || undefined,
        ref: ref.trim() || undefined,
        utm: notes.trim() ? { field_notes: notes.trim() } : undefined,
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
    } catch (e) {
      if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
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
  consent: {
    marginTop: spacing.md,
    fontSize: 11,
    color: colors.mutedForeground,
    textAlign: "center",
  },
});
