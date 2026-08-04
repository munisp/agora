"use client";

/**
 * Lending products editor (SPEC-W20 Agent C): product cards with the
 * principal band / term / interest / fee, an activate toggle and a dialog
 * editor (kobo + bps fields with naira hints).
 *
 * Data: GET/POST /api/bookings/v1/lending/products,
 *       PATCH /api/bookings/v1/lending/products/{id}
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label } from "@/components/ui/input";
import { formatBps, formatKobo, type Product } from "@/components/apps/lending/types";

export interface ProductDraft {
  id?: string;
  name: string;
  active: boolean;
  principal_min_kobo: number;
  principal_max_kobo: number;
  term_days: number;
  interest_bps: number;
  fee_flat_kobo: number;
}

export function emptyDraft(): ProductDraft {
  return {
    name: "",
    active: true,
    principal_min_kobo: 100000,
    principal_max_kobo: 5000000,
    term_days: 30,
    interest_bps: 1500,
    fee_flat_kobo: 0,
  };
}

export function draftFromProduct(p: Product): ProductDraft {
  return {
    id: p.id,
    name: p.name,
    active: p.active,
    principal_min_kobo: p.principal_min_kobo,
    principal_max_kobo: p.principal_max_kobo,
    term_days: p.term_days,
    interest_bps: p.interest_bps,
    fee_flat_kobo: p.fee_flat_kobo,
  };
}

function nairaHint(kobo: number): string {
  if (!Number.isFinite(kobo) || kobo <= 0) return "";
  return `≈ ${formatKobo(kobo)}`;
}

export function ProductEditorDialog({
  draft,
  busy,
  onSave,
  onCancel,
}: {
  draft: ProductDraft;
  busy: boolean;
  onSave: (d: ProductDraft) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [d, setD] = React.useState<ProductDraft>(draft);
  const num =
    (key: keyof ProductDraft) =>
    (e: React.ChangeEvent<HTMLInputElement>) =>
      setD({ ...d, [key]: Number(e.target.value) });

  return (
    <Dialog open onOpenChange={(open) => !open && onCancel()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{d.id ? "Edit product" : "New product"}</DialogTitle>
          <DialogDescription>
            Amounts are kobo (₦1 = 100 kobo); interest is basis points
            (1500 = 15% flat over the term).
          </DialogDescription>
        </DialogHeader>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="sm:col-span-2">
            <Label htmlFor="lp-name">Name</Label>
            <Input
              id="lp-name"
              value={d.name}
              onChange={(e) => setD({ ...d, name: e.target.value })}
              placeholder="Trader Cash"
            />
          </div>
          <div>
            <Label htmlFor="lp-min">Min principal (kobo)</Label>
            <Input
              id="lp-min"
              type="number"
              min={1}
              value={d.principal_min_kobo}
              onChange={num("principal_min_kobo")}
            />
            <p className="mt-1 text-xs text-muted-foreground">
              {nairaHint(d.principal_min_kobo)}
            </p>
          </div>
          <div>
            <Label htmlFor="lp-max">Max principal (kobo)</Label>
            <Input
              id="lp-max"
              type="number"
              min={1}
              value={d.principal_max_kobo}
              onChange={num("principal_max_kobo")}
            />
            <p className="mt-1 text-xs text-muted-foreground">
              {nairaHint(d.principal_max_kobo)}
            </p>
          </div>
          <div>
            <Label htmlFor="lp-term">Term (days)</Label>
            <Input
              id="lp-term"
              type="number"
              min={1}
              value={d.term_days}
              onChange={num("term_days")}
            />
          </div>
          <div>
            <Label htmlFor="lp-bps">Interest (bps)</Label>
            <Input
              id="lp-bps"
              type="number"
              min={0}
              max={10000}
              value={d.interest_bps}
              onChange={num("interest_bps")}
            />
            <p className="mt-1 text-xs text-muted-foreground">
              = {formatBps(d.interest_bps)} flat
            </p>
          </div>
          <div>
            <Label htmlFor="lp-fee">Flat fee (kobo)</Label>
            <Input
              id="lp-fee"
              type="number"
              min={0}
              value={d.fee_flat_kobo}
              onChange={num("fee_flat_kobo")}
            />
            <p className="mt-1 text-xs text-muted-foreground">
              {nairaHint(d.fee_flat_kobo)}
            </p>
          </div>
          <label className="flex items-end gap-2 pb-2 text-sm">
            <input
              type="checkbox"
              className="h-4 w-4"
              checked={d.active}
              onChange={(e) => setD({ ...d, active: e.target.checked })}
            />
            Active (accepts new applications)
          </label>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            disabled={busy || d.name.trim() === ""}
            onClick={() => void onSave(d)}
          >
            {busy ? "Saving…" : "Save product"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function ProductCard({
  product,
  canManage,
  onEdit,
  onToggle,
}: {
  product: Product;
  canManage: boolean;
  onEdit: () => void;
  onToggle: (active: boolean) => void;
}) {
  return (
    <Card>
      <CardContent className="flex flex-wrap items-center justify-between gap-3 py-4">
        <div>
          <div className="flex items-center gap-2">
            <span className="font-medium">{product.name}</span>
            <Badge variant={product.active ? "success" : "secondary"}>
              {product.active ? "Active" : "Inactive"}
            </Badge>
          </div>
          <p className="mt-1 text-sm text-muted-foreground">
            {formatKobo(product.principal_min_kobo)} –{" "}
            {formatKobo(product.principal_max_kobo)} · {product.term_days} days
            · {formatBps(product.interest_bps)} interest
            {product.fee_flat_kobo > 0
              ? ` · ${formatKobo(product.fee_flat_kobo)} fee`
              : " · no fee"}
          </p>
        </div>
        {canManage ? (
          <div className="flex items-center gap-2">
            <Button variant="outline" size="sm" onClick={onEdit}>
              Edit
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => onToggle(!product.active)}
            >
              {product.active ? "Deactivate" : "Activate"}
            </Button>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
