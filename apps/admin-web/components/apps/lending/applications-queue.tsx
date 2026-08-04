"use client";

/**
 * Lending applications queue (SPEC-W20 Agent C): status filter chips, a
 * table with the naive-score badge, and the row actions that walk the
 * state machine — submit, start review, approve/decline (dialog with the
 * KYC gate: kyc-service identifiers when the integrator wired
 * LENDING_KYC_URL, otherwise an explicit override + reason), disburse,
 * and operator-driven default marking.
 *
 * Data: GET /api/bookings/v1/lending/applications?status=
 *       PATCH /api/bookings/v1/lending/applications/{id}
 *       POST /api/bookings/v1/lending/applications/{id}/disburse
 */
import * as React from "react";
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
import { Input, Label, Select } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  APPLICATION_STATUSES,
  APPLICATION_STATUS_META,
  formatKobo,
  formatTs,
  scoreVariant,
  shortId,
  type LoanApplication,
  type Product,
} from "@/components/apps/lending/types";

/** Body of PATCH /v1/lending/applications/{id}. */
export interface DecisionInput {
  status: string;
  decline_reason?: string;
  decided_by?: string;
  kyc_override?: boolean;
  kyc_reason?: string;
  kyc?: { subject_phone: string; id_type: string; id_value: string };
}

export function StatusFilterBar({
  value,
  onChange,
}: {
  value: string;
  onChange: (status: string) => void;
}) {
  return (
    <div className="mb-3 flex flex-wrap gap-1.5">
      <Button
        size="sm"
        variant={value === "" ? "default" : "outline"}
        onClick={() => onChange("")}
      >
        All
      </Button>
      {APPLICATION_STATUSES.map((s) => (
        <Button
          key={s}
          size="sm"
          variant={value === s ? "default" : "outline"}
          onClick={() => onChange(value === s ? "" : s)}
        >
          {APPLICATION_STATUS_META[s]?.label ?? s}
        </Button>
      ))}
    </div>
  );
}

export function ScoreBadge({ score }: { score: number | null }) {
  if (score === null || score === undefined) {
    return <Badge variant="outline">—</Badge>;
  }
  return (
    <Badge variant={scoreVariant(score)} title="Naive rule-based score — not a credit bureau score">
      {score}/100
    </Badge>
  );
}

export function DecisionDialog({
  mode,
  application,
  busy,
  onSubmit,
  onCancel,
}: {
  mode: "approve" | "decline";
  application: LoanApplication;
  busy: boolean;
  onSubmit: (input: DecisionInput) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [reason, setReason] = React.useState("");
  const [decidedBy, setDecidedBy] = React.useState("");
  const [kycOverride, setKycOverride] = React.useState(false);
  const [kycReason, setKycReason] = React.useState("");
  const [phone, setPhone] = React.useState("");
  const [idType, setIdType] = React.useState("bvn");
  const [idValue, setIdValue] = React.useState("");

  const approve = mode === "approve";
  const hasServiceKyc = phone.trim() !== "" && idValue.trim() !== "";
  const canSubmit = approve
    ? hasServiceKyc || (kycOverride && kycReason.trim() !== "")
    : reason.trim() !== "";

  const submit = () => {
    const input: DecisionInput = {
      status: approve ? "approved" : "declined",
    };
    if (decidedBy.trim() !== "") input.decided_by = decidedBy.trim();
    if (approve) {
      if (hasServiceKyc) {
        input.kyc = {
          subject_phone: phone.trim(),
          id_type: idType,
          id_value: idValue.trim(),
        };
      }
      if (kycOverride) {
        input.kyc_override = true;
        input.kyc_reason = kycReason.trim();
      }
    } else {
      input.decline_reason = reason.trim();
    }
    void onSubmit(input);
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onCancel()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {approve ? "Approve application" : "Decline application"}
          </DialogTitle>
          <DialogDescription>
            {shortId(application.id)} ·{" "}
            {formatKobo(application.principal_kobo)} · score{" "}
            {application.score ?? "—"}
          </DialogDescription>
        </DialogHeader>

        {approve ? (
          <div className="space-y-3">
            <div className="rounded-md border border-border bg-muted/40 p-3 text-sm">
              <p className="font-medium">KYC gate</p>
              <p className="mt-1 text-muted-foreground">
                When the KYC service is wired (LENDING_KYC_URL), approval
                calls it with the subject identifiers below and requires a
                &quot;verified&quot; result. When it is not wired, an
                explicit override with a reason is required — the override is
                recorded in the decision event.
              </p>
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <div>
                <Label htmlFor="kyc-phone">Subject phone</Label>
                <Input
                  id="kyc-phone"
                  value={phone}
                  onChange={(e) => setPhone(e.target.value)}
                  placeholder="+234…"
                />
              </div>
              <div>
                <Label htmlFor="kyc-type">ID type</Label>
                <Select
                  id="kyc-type"
                  value={idType}
                  onChange={(e) => setIdType(e.target.value)}
                >
                  <option value="bvn">BVN</option>
                  <option value="nin">NIN</option>
                </Select>
              </div>
              <div>
                <Label htmlFor="kyc-value">ID value</Label>
                <Input
                  id="kyc-value"
                  value={idValue}
                  onChange={(e) => setIdValue(e.target.value)}
                  placeholder="BVN / NIN"
                />
              </div>
            </div>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                className="h-4 w-4"
                checked={kycOverride}
                onChange={(e) => setKycOverride(e.target.checked)}
              />
              KYC override (no KYC service configured / verified manually)
            </label>
            {kycOverride ? (
              <div>
                <Label htmlFor="kyc-reason">Override reason</Label>
                <Input
                  id="kyc-reason"
                  value={kycReason}
                  onChange={(e) => setKycReason(e.target.value)}
                  placeholder="e.g. branch-verified ID card"
                />
              </div>
            ) : null}
            <div>
              <Label htmlFor="decided-by">Decided by (optional)</Label>
              <Input
                id="decided-by"
                value={decidedBy}
                onChange={(e) => setDecidedBy(e.target.value)}
                placeholder="operator handle"
              />
            </div>
          </div>
        ) : (
          <div className="space-y-3">
            <div>
              <Label htmlFor="decline-reason">Decline reason (required)</Label>
              <Input
                id="decline-reason"
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="e.g. thin file, insufficient history"
              />
            </div>
            <div>
              <Label htmlFor="decided-by-d">Decided by (optional)</Label>
              <Input
                id="decided-by-d"
                value={decidedBy}
                onChange={(e) => setDecidedBy(e.target.value)}
                placeholder="operator handle"
              />
            </div>
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant={approve ? "default" : "destructive"}
            disabled={busy || !canSubmit}
            onClick={submit}
          >
            {busy ? "Working…" : approve ? "Approve" : "Decline"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function ApplicationsTable({
  applications,
  products,
  canManage,
  loading,
  onAction,
  onViewLoan,
}: {
  applications: LoanApplication[];
  products: Product[];
  canManage: boolean;
  loading: boolean;
  /** action: submit | review | approve | decline | disburse | default */
  onAction: (action: string, a: LoanApplication) => void;
  onViewLoan: (a: LoanApplication) => void;
}) {
  const productName = React.useMemo(() => {
    const map = new Map(products.map((p) => [p.id, p.name]));
    return (id: string) => map.get(id) ?? shortId(id);
  }, [products]);

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Application</TableHead>
          <TableHead>Product</TableHead>
          <TableHead>Principal</TableHead>
          <TableHead>Score</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Created</TableHead>
          {canManage ? <TableHead className="text-right">Actions</TableHead> : null}
        </TableRow>
      </TableHeader>
      <TableBody>
        {applications.map((a) => (
          <TableRow key={a.id}>
            <TableCell>
              <span className="font-mono text-xs" title={a.id}>
                {shortId(a.id)}
              </span>
              <span
                className="ml-2 text-xs text-muted-foreground"
                title={`contact ${a.contact_id}`}
              >
                → {shortId(a.contact_id)}
              </span>
            </TableCell>
            <TableCell>{productName(a.product_id)}</TableCell>
            <TableCell>{formatKobo(a.principal_kobo)}</TableCell>
            <TableCell>
              <ScoreBadge score={a.score} />
            </TableCell>
            <TableCell>
              <Badge variant={APPLICATION_STATUS_META[a.status]?.variant ?? "outline"}>
                {APPLICATION_STATUS_META[a.status]?.label ?? a.status}
              </Badge>
              {a.status === "declined" && a.decline_reason ? (
                <span className="ml-1 text-xs text-muted-foreground">
                  ({a.decline_reason})
                </span>
              ) : null}
            </TableCell>
            <TableCell className="text-sm text-muted-foreground">
              {formatTs(a.created_at)}
            </TableCell>
            {canManage ? (
              <TableCell className="space-x-1 text-right">
                {a.status === "draft" ? (
                  <Button size="sm" variant="outline" onClick={() => onAction("submit", a)}>
                    Submit
                  </Button>
                ) : null}
                {a.status === "submitted" ? (
                  <Button size="sm" variant="outline" onClick={() => onAction("review", a)}>
                    Start review
                  </Button>
                ) : null}
                {a.status === "under_review" ? (
                  <>
                    <Button size="sm" onClick={() => onAction("approve", a)}>
                      Approve…
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => onAction("decline", a)}
                    >
                      Decline…
                    </Button>
                  </>
                ) : null}
                {a.status === "approved" ? (
                  <Button size="sm" onClick={() => onAction("disburse", a)}>
                    Disburse
                  </Button>
                ) : null}
                {a.status === "disbursed" || a.status === "repaid" ? (
                  <Button size="sm" variant="outline" onClick={() => onViewLoan(a)}>
                    View loan
                  </Button>
                ) : null}
                {["submitted", "under_review", "approved", "disbursed"].includes(a.status) ? (
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => onAction("default", a)}
                  >
                    Mark defaulted
                  </Button>
                ) : null}
              </TableCell>
            ) : null}
          </TableRow>
        ))}
        {applications.length === 0 && !loading ? (
          <TableEmpty colSpan={canManage ? 7 : 6}>
            No applications match this filter.
          </TableEmpty>
        ) : null}
        {loading ? (
          <TableEmpty colSpan={canManage ? 7 : 6}>Loading applications…</TableEmpty>
        ) : null}
      </TableBody>
    </Table>
  );
}
