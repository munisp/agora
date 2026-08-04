"use client";

/**
 * New lending application dialog (SPEC-W20 Agent C): contact id, product
 * picker (active products, principal band shown), principal in kobo with
 * live naira + schedule preview, and the draft/submitted choice —
 * submitting computes the naive score immediately.
 *
 * Data: POST /api/bookings/v1/lending/applications
 */
import * as React from "react";
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
  formatBps,
  formatKobo,
  type Product,
} from "@/components/apps/lending/types";

export interface CreateApplicationInput {
  contact_id: string;
  product_id: string;
  principal_kobo: number;
  status: "draft" | "submitted";
}

const UUID_RE =
  /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

export function ApplicationCreateDialog({
  products,
  busy,
  onCreate,
  onCancel,
}: {
  products: Product[];
  busy: boolean;
  onCreate: (input: CreateApplicationInput) => Promise<boolean>;
  onCancel: () => void;
}) {
  const active = products.filter((p) => p.active);
  const [contactId, setContactId] = React.useState("");
  const [productId, setProductId] = React.useState(active[0]?.id ?? "");
  const [principal, setPrincipal] = React.useState(
    active[0]?.principal_min_kobo ?? 0,
  );
  const [submitNow, setSubmitNow] = React.useState(true);

  const product = active.find((p) => p.id === productId);
  const inBand =
    product !== undefined &&
    principal >= product.principal_min_kobo &&
    principal <= product.principal_max_kobo;
  const interest = product
    ? Math.floor((principal * product.interest_bps) / 10000)
    : 0;
  const canCreate =
    UUID_RE.test(contactId.trim()) && product !== undefined && inBand;

  return (
    <Dialog open onOpenChange={(open) => !open && onCancel()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New loan application</DialogTitle>
          <DialogDescription>
            The principal is validated against the product band; submitting
            computes the naive 0–100 score (not a credit bureau score).
          </DialogDescription>
        </DialogHeader>
        {active.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No active products — create and activate a product first.
          </p>
        ) : (
          <div className="space-y-3">
            <div>
              <Label htmlFor="la-contact">Contact ID (uuid)</Label>
              <Input
                id="la-contact"
                value={contactId}
                onChange={(e) => setContactId(e.target.value)}
                placeholder="00000000-0000-0000-0000-000000000000"
              />
              {contactId.trim() !== "" && !UUID_RE.test(contactId.trim()) ? (
                <p className="mt-1 text-xs text-destructive">
                  Must be a uuid (copy it from the contacts/CRM view).
                </p>
              ) : null}
            </div>
            <div>
              <Label htmlFor="la-product">Product</Label>
              <Select
                id="la-product"
                value={productId}
                onChange={(e) => {
                  const p = active.find((x) => x.id === e.target.value);
                  setProductId(e.target.value);
                  if (p) setPrincipal(p.principal_min_kobo);
                }}
              >
                {active.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name} ({formatKobo(p.principal_min_kobo)}–
                    {formatKobo(p.principal_max_kobo)})
                  </option>
                ))}
              </Select>
            </div>
            <div>
              <Label htmlFor="la-principal">Principal (kobo)</Label>
              <Input
                id="la-principal"
                type="number"
                min={1}
                value={principal}
                onChange={(e) => setPrincipal(Number(e.target.value))}
              />
              <p className="mt-1 text-xs text-muted-foreground">
                {formatKobo(principal)}
                {product
                  ? ` · interest ${formatKobo(interest)} (${formatBps(
                      product.interest_bps,
                    )}) · fee ${formatKobo(product.fee_flat_kobo)} · total ${formatKobo(
                      principal + interest + product.fee_flat_kobo,
                    )} over ${product.term_days} days`
                  : ""}
              </p>
              {!inBand && product ? (
                <p className="mt-1 text-xs text-destructive">
                  Outside the product band {formatKobo(product.principal_min_kobo)}–
                  {formatKobo(product.principal_max_kobo)}.
                </p>
              ) : null}
            </div>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                className="h-4 w-4"
                checked={submitNow}
                onChange={(e) => setSubmitNow(e.target.checked)}
              />
              Submit immediately (computes the score; unchecked saves a draft)
            </label>
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            disabled={busy || !canCreate}
            onClick={() =>
              void onCreate({
                contact_id: contactId.trim(),
                product_id: productId,
                principal_kobo: principal,
                status: submitNow ? "submitted" : "draft",
              })
            }
          >
            {busy ? "Creating…" : submitNow ? "Create & submit" : "Save draft"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
