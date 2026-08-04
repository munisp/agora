"use client";

/**
 * Wallet lookup + ledger table + accrue/redeem actions (SPEC-W19 Agent C).
 * Pure presentational — the parent client owns the network calls.
 */
import * as React from "react";
import { Coins, Gift, MinusCircle, PlusCircle, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/utils";
import {
  ACCOUNT_CODES,
  EARN_EVENTS,
  EVENT_LABELS,
  formatPoints,
  tierLabel,
  type WalletView,
} from "./types";

export interface AccrueInput {
  contact_id: string;
  event: string;
  ref_id: string;
}

export interface RedeemInput {
  contact_id: string;
  points: number;
  reason: string;
  ref_id: string;
}

export function WalletLookup({
  loading,
  onLookup,
}: {
  loading: boolean;
  onLookup: (contactID: string) => void;
}) {
  const [contactID, setContactID] = React.useState("");
  return (
    <form
      className="mb-4 flex flex-wrap items-end gap-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (contactID.trim()) onLookup(contactID.trim());
      }}
    >
      <div className="flex-1 space-y-1.5">
        <Label htmlFor="wallet-contact">Contact ID</Label>
        <Input
          id="wallet-contact"
          value={contactID}
          onChange={(e) => setContactID(e.target.value)}
          placeholder="uuid of the contact"
          required
        />
      </div>
      <Button type="submit" disabled={loading || !contactID.trim()}>
        <Search className="h-3.5 w-3.5" />
        {loading ? "Looking up…" : "Look up wallet"}
      </Button>
    </form>
  );
}

export function WalletCards({ view }: { view: WalletView }) {
  const w = view.wallet;
  const mismatch = view.ledger_balance !== w.balance;
  return (
    <div className="mb-4 grid gap-3 sm:grid-cols-4">
      <Card>
        <CardHeader className="pb-2">
          <CardDescription>Balance</CardDescription>
          <CardTitle className="flex items-center gap-2 text-2xl">
            <Coins className="h-5 w-5 text-primary" />
            {formatPoints(w.balance)}
          </CardTitle>
        </CardHeader>
      </Card>
      <Card>
        <CardHeader className="pb-2">
          <CardDescription>Lifetime earned</CardDescription>
          <CardTitle className="text-2xl">
            {formatPoints(w.lifetime_earned)}
          </CardTitle>
        </CardHeader>
      </Card>
      <Card>
        <CardHeader className="pb-2">
          <CardDescription>Lifetime redeemed</CardDescription>
          <CardTitle className="text-2xl">
            {formatPoints(w.lifetime_redeemed)}
          </CardTitle>
        </CardHeader>
      </Card>
      <Card>
        <CardHeader className="pb-2">
          <CardDescription>Tier</CardDescription>
          <CardTitle className="text-2xl">
            <Badge variant="info">{tierLabel(w.tier)}</Badge>
          </CardTitle>
        </CardHeader>
      </Card>
      {mismatch ? (
        <p className="sm:col-span-4 text-sm text-destructive">
          Ledger cross-check mismatch: cached balance {w.balance} vs ledger{" "}
          {view.ledger_balance} — flag to engineering.
        </p>
      ) : null}
    </div>
  );
}

export function WalletActions({
  contactID,
  canManage,
  busy,
  onAccrue,
  onRedeem,
}: {
  contactID: string;
  canManage: boolean;
  busy: boolean;
  onAccrue: (input: AccrueInput) => Promise<void>;
  onRedeem: (input: RedeemInput) => Promise<void>;
}) {
  const [event, setEvent] = React.useState<string>(EARN_EVENTS[0]);
  const [refID, setRefID] = React.useState("");
  const [points, setPoints] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [redeemRef, setRedeemRef] = React.useState("");

  if (!canManage) return null;

  return (
    <div className="mb-4 grid gap-3 lg:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <PlusCircle className="h-4 w-4 text-primary" /> Accrue points
          </CardTitle>
          <CardDescription>
            Idempotent on ref_id + event — use the booking/transaction id so
            retries never double-award.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form
            className="flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (!refID.trim() || busy) return;
              void onAccrue({
                contact_id: contactID,
                event,
                ref_id: refID.trim(),
              });
            }}
          >
            <div className="space-y-1.5">
              <Label>Event</Label>
              <Select value={event} onChange={(e) => setEvent(e.target.value)}>
                {EARN_EVENTS.map((ev) => (
                  <option key={ev} value={ev}>
                    {EVENT_LABELS[ev]}
                  </option>
                ))}
              </Select>
            </div>
            <div className="flex-1 space-y-1.5">
              <Label>Reference id</Label>
              <Input
                value={refID}
                onChange={(e) => setRefID(e.target.value)}
                placeholder="e.g. booking id"
                required
              />
            </div>
            <Button type="submit" disabled={busy || !refID.trim()}>
              Accrue
            </Button>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <MinusCircle className="h-4 w-4 text-destructive" /> Redeem points
          </CardTitle>
          <CardDescription>
            Insufficient balance is rejected (409). ref_id anchors idempotent
            retries.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form
            className="flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              const n = Number(points);
              if (!(n > 0) || !reason.trim() || busy) return;
              void onRedeem({
                contact_id: contactID,
                points: n,
                reason: reason.trim(),
                ref_id:
                  redeemRef.trim() ||
                  `ui-${contactID}-${Date.now().toString(36)}`,
              });
            }}
          >
            <div className="w-28 space-y-1.5">
              <Label>Points</Label>
              <Input
                type="number"
                min={1}
                value={points}
                onChange={(e) => setPoints(e.target.value)}
                required
              />
            </div>
            <div className="flex-1 space-y-1.5">
              <Label>Reason</Label>
              <Input
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="e.g. voucher #123"
                required
              />
            </div>
            <div className="flex-1 space-y-1.5">
              <Label>Reference id (optional)</Label>
              <Input
                value={redeemRef}
                onChange={(e) => setRedeemRef(e.target.value)}
                placeholder="auto-generated if empty"
              />
            </div>
            <Button
              type="submit"
              variant="destructive"
              disabled={busy || !(Number(points) > 0) || !reason.trim()}
            >
              <Gift className="h-3.5 w-3.5" /> Redeem
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}

export function WalletLedgerTable({
  entries,
  loading,
}: {
  entries: WalletView["entries"];
  loading: boolean;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Ledger</CardTitle>
        <CardDescription>
          Double-entry points journal for this contact — accruals credit
          account 400, redemptions debit it (house side is account 401).
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>When</TableHead>
              <TableHead>Account</TableHead>
              <TableHead className="text-right">Debit</TableHead>
              <TableHead className="text-right">Credit</TableHead>
              <TableHead>Reference</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.map((e) => (
              <TableRow key={e.entry_id}>
                <TableCell>
                  {e.created_at ? formatDateTime(e.created_at) : "—"}
                </TableCell>
                <TableCell>
                  <Badge variant={e.account_code === 400 ? "info" : "secondary"}>
                    {ACCOUNT_CODES[e.account_code] ?? e.account_code}
                  </Badge>
                </TableCell>
                <TableCell className="text-right">
                  {e.debit_points > 0 ? formatPoints(e.debit_points) : "—"}
                </TableCell>
                <TableCell className="text-right">
                  {e.credit_points > 0 ? formatPoints(e.credit_points) : "—"}
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {e.ref_type}:{e.ref_id}
                </TableCell>
              </TableRow>
            ))}
            {entries.length === 0 ? (
              <TableEmpty colSpan={5}>
                {loading
                  ? "Loading ledger…"
                  : "No ledger entries yet — accrue points to start the journal."}
              </TableEmpty>
            ) : null}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
