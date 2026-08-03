/**
 * Growth — referrals + leaderboard (+ payout totals), mirroring the
 * admin-web Growth dashboard (SPEC-W14):
 *   - GET /v1/referrals  (list + create)
 *   - GET /v1/payouts    (paid totals; may be unavailable → honest "—")
 *   - leaderboard ranked by verified+converted (src/api/growth.ts mirrors
 *     apps/admin-web/components/growth/types.ts line for line)
 */
import React from "react";
import { ScrollView, RefreshControl, View, Text, StyleSheet } from "react-native";
import { useFocusEffect } from "expo-router";
import {
  listReferrals,
  listPayouts,
  createReferral,
  ApiError,
  NotAuthenticatedError,
} from "../../src/api/client";
import type { Payout, Referral } from "../../src/api/types";
import { buildLeaderboard, formatNgn, shortId } from "../../src/api/growth";
import { Screen } from "../../components/Screen";
import { Card } from "../../components/Card";
import { ListItem } from "../../components/ListItem";
import { StatTile, StatTileRow } from "../../components/StatTile";
import { Badge, Button, EmptyState, ErrorBox, Field } from "../../components/ui";
import { colors, radius, spacing } from "../../src/theme";

function referralTone(status: string): "success" | "warning" | "info" | "secondary" | "destructive" {
  switch (status) {
    case "converted":
    case "paid":
      return "success";
    case "verified":
      return "info";
    case "pending":
      return "warning";
    case "rejected":
      return "destructive";
    default:
      return "secondary";
  }
}

export default function GrowthScreen() {
  const [referrals, setReferrals] = React.useState<Referral[]>([]);
  const [payouts, setPayouts] = React.useState<Payout[] | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [refreshing, setRefreshing] = React.useState(false);

  const [refType, setRefType] = React.useState("contact");
  const [refId, setRefId] = React.useState("");
  const [refPhone, setRefPhone] = React.useState("");
  const [createMsg, setCreateMsg] = React.useState<string | null>(null);
  const [creating, setCreating] = React.useState(false);

  const load = React.useCallback(async () => {
    setError(null);
    try {
      const rows = await listReferrals();
      setReferrals(rows);
    } catch (e) {
      if (!(e instanceof NotAuthenticatedError)) {
        setError(e instanceof Error ? e.message : String(e));
      }
    }
    // Payouts are optional for the leaderboard — never fabricate zeros.
    try {
      const rows = await listPayouts();
      setPayouts(rows);
    } catch {
      setPayouts(null);
    }
  }, []);

  useFocusEffect(
    React.useCallback(() => {
      void load();
    }, [load]),
  );

  const onRefresh = async () => {
    setRefreshing(true);
    await load();
    setRefreshing(false);
  };

  const onCreate = async () => {
    if (!refId.trim() || !refPhone.trim()) {
      setCreateMsg("Referrer id and referee phone are required.");
      return;
    }
    setCreating(true);
    setCreateMsg(null);
    try {
      const res = await createReferral({
        referrer_type: refType,
        referrer_id: refId.trim(),
        referee_phone: refPhone.trim(),
      });
      setCreateMsg(
        res.created
          ? "Referral recorded (pending verification)."
          : "An open referral already exists for this pair (dedupe).",
      );
      setRefId("");
      setRefPhone("");
      await load();
    } catch (e) {
      if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
        setCreateMsg("Not permitted — your role needs manage_bookings on this tenant.");
      } else {
        setCreateMsg(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setCreating(false);
    }
  };

  const board = buildLeaderboard(referrals, payouts);
  const pending = referrals.filter((r) => r.status === "pending").length;
  const converted = referrals.filter(
    (r) => r.status === "converted" || r.status === "paid",
  ).length;

  return (
    <Screen title="Growth" subtitle="Referrals, leaderboard and payouts">
      <ScrollView
        keyboardShouldPersistTaps="handled"
        refreshControl={
          <RefreshControl refreshing={refreshing} onRefresh={onRefresh} tintColor={colors.primary} />
        }
      >
        {error ? <ErrorBox message={error} /> : null}

        <StatTileRow>
          <StatTile value={referrals.length} label="Referrals" />
          <StatTile value={pending} label="Pending" />
          <StatTile value={converted} label="Converted" />
        </StatTileRow>

        <Card
          title="Referrer leaderboard"
          description={
            payouts === null
              ? "Top referrers by verified + converted. Payout totals unavailable right now — counts are still accurate."
              : "Top referrers by verified + converted, with commission paid."
          }
        >
          {board.length === 0 ? (
            <EmptyState title="No verified referrals yet" />
          ) : (
            board.slice(0, 10).map((row, i) => (
              <ListItem
                key={row.key}
                title={`#${i + 1} · ${row.referrer_type} ${shortId(row.referrer_id)}`}
                subtitle={`${row.verified} verified · ${row.converted} converted`}
                right={
                  <Text style={styles.paidTotal}>
                    {row.paidTotalKobo === null ? "—" : formatNgn(row.paidTotalKobo)}
                  </Text>
                }
              />
            ))
          )}
        </Card>

        <Card title="Record a referral" description="POST /v1/referrals — deduped server-side.">
          <View style={styles.typeRow}>
            {["contact", "agent", "staff"].map((t) => (
              <Button
                key={t}
                title={t}
                variant={refType === t ? "primary" : "secondary"}
                onPress={() => setRefType(t)}
              />
            ))}
          </View>
          <Field
            label="Referrer id (contact/agent/staff id)"
            value={refId}
            onChangeText={setRefId}
            placeholder="uuid or staff id"
          />
          <Field
            label="Referee phone (E.164)"
            value={refPhone}
            onChangeText={setRefPhone}
            placeholder="+2348012345678"
            keyboardType="phone-pad"
          />
          {createMsg ? <Text style={styles.createMsg}>{createMsg}</Text> : null}
          <Button title="Record referral" onPress={onCreate} loading={creating} />
        </Card>

        <Card title="Recent referrals">
          {referrals.length === 0 ? (
            <EmptyState title="No referrals yet" />
          ) : (
            referrals.slice(0, 15).map((r) => (
              <ListItem
                key={r.referral_id}
                title={r.referee_phone}
                subtitle={`${r.referrer_type} ${shortId(r.referrer_id)}`}
                meta={r.created_at ? new Date(r.created_at).toLocaleString("en-NG") : undefined}
                right={<Badge label={r.status} tone={referralTone(r.status)} />}
              />
            ))
          )}
        </Card>
        <View style={{ height: spacing.xl }} />
      </ScrollView>
    </Screen>
  );
}

const styles = StyleSheet.create({
  paidTotal: { fontSize: 13, fontWeight: "700", color: colors.primary },
  typeRow: {
    flexDirection: "row",
    marginBottom: spacing.sm,
    gap: spacing.sm,
  },
  createMsg: {
    fontSize: 12,
    color: colors.mutedForeground,
    marginBottom: spacing.sm,
  },
});
