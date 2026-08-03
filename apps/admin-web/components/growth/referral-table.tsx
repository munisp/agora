"use client";

/**
 * Referrals table (SPEC-W14 Agent C, contract §1): referee, referrer,
 * status, campaign and timestamps, with a per-row Verify action for pending
 * referrals (POST /v1/referrals/{id}/verify — fires the rules engine and
 * balanced commission postings server-side; idempotent).
 *
 * Verify is a two-step inline confirm: the server requires a `trigger`
 * (signup_verified | first_booking | first_txn | sale) in the body, and
 * percent rules on the revenue triggers (first_txn | sale) are computed
 * against `base_amount_ngn` (integer kobo) — so the operator picks the
 * trigger and, for revenue triggers, may enter the base amount in whole
 * naira (converted with Math.round(x * 100)).
 */
import * as React from "react";
import { BadgeCheck, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime, titleCase } from "@/lib/utils";
import {
  referralStatusVariant,
  shortId,
  type Referral,
} from "@/components/growth/types";

export function ReferralTable({
  rows,
  loading,
  verifyingId,
  onVerify,
}: {
  rows: Referral[];
  loading: boolean;
  /** referral_id currently being verified (disables its button) */
  verifyingId: string | null;
  onVerify: (
    referral: Referral,
    trigger: string,
    baseAmountKobo: number,
  ) => void;
}) {
  const sorted = [...rows].sort((a, b) =>
    (b.created_at ?? "").localeCompare(a.created_at ?? ""),
  );

  // Inline verify confirm: which row is being confirmed, the chosen
  // trigger, and the optional revenue base in whole naira.
  const [confirmingId, setConfirmingId] = React.useState<string | null>(null);
  const [trigger, setTrigger] = React.useState("signup_verified");
  const [baseNaira, setBaseNaira] = React.useState("");

  const openConfirm = (r: Referral) => {
    setConfirmingId(r.referral_id);
    setTrigger("signup_verified");
    setBaseNaira("");
  };

  const revenueBase = trigger === "first_txn" || trigger === "sale";
  const parsedBase = baseNaira.trim() ? parseFloat(baseNaira) : 0;
  const baseValid =
    !revenueBase ||
    (baseNaira.trim() === "" ||
      (Number.isFinite(parsedBase) && parsedBase >= 0));
  const baseKobo = revenueBase ? Math.round((parsedBase || 0) * 100) : 0;
  return (
    <Card>
      <CardHeader>
        <CardTitle>Referrals</CardTitle>
        <CardDescription>
          One open referral per referee phone number. Verifying a pending
          referral evaluates the active commission rules and posts the bounty
          to the ledger.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Referee phone</TableHead>
              <TableHead>Referrer</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Campaign</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Verified</TableHead>
              <TableHead>Paid</TableHead>
              <TableHead className="text-right">Action</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableEmpty colSpan={8}>Loading referrals…</TableEmpty>
            ) : sorted.length === 0 ? (
              <TableEmpty colSpan={8}>
                No referrals yet — create the first one with the form above.
              </TableEmpty>
            ) : (
              sorted.map((r) => (
                <React.Fragment key={r.referral_id}>
                  <TableRow>
                  <TableCell className="font-medium">
                    {r.referee_phone || "—"}
                  </TableCell>
                  <TableCell>
                    <span className="text-muted-foreground">
                      {titleCase(r.referrer_type)} ·{" "}
                    </span>
                    {r.referrer_id}
                  </TableCell>
                  <TableCell>
                    <Badge variant={referralStatusVariant(r.status)}>
                      {titleCase(r.status)}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {r.campaign_id ? shortId(r.campaign_id) : "—"}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {r.created_at ? formatDateTime(r.created_at) : "—"}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {r.verified_at ? formatDateTime(r.verified_at) : "—"}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {r.paid_at ? formatDateTime(r.paid_at) : "—"}
                  </TableCell>
                    <TableCell className="text-right">
                      {r.status === "pending" ? (
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={verifyingId !== null}
                          onClick={() =>
                            confirmingId === r.referral_id
                              ? setConfirmingId(null)
                              : openConfirm(r)
                          }
                        >
                          <BadgeCheck className="h-3.5 w-3.5" />
                          {verifyingId === r.referral_id
                            ? "Verifying…"
                            : confirmingId === r.referral_id
                              ? "Close"
                              : "Verify"}
                        </Button>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                  </TableRow>
                  {confirmingId === r.referral_id ? (
                    <TableRow className="bg-muted/40 hover:bg-muted/40">
                      <TableCell colSpan={8}>
                        <div className="flex flex-wrap items-end gap-3 py-1">
                          <div className="space-y-1.5">
                            <Label htmlFor={`verify-trigger-${r.referral_id}`}>
                              Trigger
                            </Label>
                            <Select
                              id={`verify-trigger-${r.referral_id}`}
                              value={trigger}
                              onChange={(e) => setTrigger(e.target.value)}
                            >
                              <option value="signup_verified">
                                Signup verified
                              </option>
                              <option value="first_booking">
                                First booking
                              </option>
                              <option value="first_txn">
                                First transaction
                              </option>
                              <option value="sale">Sale</option>
                            </Select>
                          </div>
                          {revenueBase ? (
                            <div className="space-y-1.5">
                              <Label htmlFor={`verify-base-${r.referral_id}`}>
                                Base amount (₦, optional)
                              </Label>
                              <Input
                                id={`verify-base-${r.referral_id}`}
                                type="number"
                                min="0"
                                step="0.01"
                                inputMode="decimal"
                                value={baseNaira}
                                onChange={(e) => setBaseNaira(e.target.value)}
                                placeholder="0.00"
                                className="w-36"
                              />
                            </div>
                          ) : null}
                          <Button
                            size="sm"
                            disabled={!baseValid || verifyingId !== null}
                            onClick={() => {
                              setConfirmingId(null);
                              onVerify(r, trigger, baseKobo);
                            }}
                          >
                            <BadgeCheck className="h-3.5 w-3.5" />
                            {verifyingId === r.referral_id
                              ? "Verifying…"
                              : "Confirm verify"}
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() => setConfirmingId(null)}
                          >
                            <X className="h-3.5 w-3.5" />
                            Cancel
                          </Button>
                          <p className="w-full text-xs text-muted-foreground">
                            {revenueBase
                              ? "Percent rules are computed against the base amount (whole naira, sent as integer kobo). Leave empty for a zero base."
                              : "Verifying evaluates the active commission rules for this trigger and posts the bounty to the ledger."}
                          </p>
                        </div>
                      </TableCell>
                    </TableRow>
                  ) : null}
                </React.Fragment>
              ))
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
