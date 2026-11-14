"use client";

import * as React from "react";
import { ChevronDown, ChevronRight, FilePlus2, QrCode, Receipt } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";
import { ConfirmDialog } from "@/components/ui/dialog";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatMoney } from "@/lib/utils";
import type { Invoice, Tenant } from "@/lib/types";

/**
 * Invoice panel (SPEC-W7 Part C) — talks to the billing-engine contract
 * (SPEC-W7 Part B) through the gateway at /api/billing:
 *   GET  /v1/invoices?tenant_id=&status=&limit=&cursor=
 *   POST /v1/invoices/generate { tenant_id, period: "YYYY-MM" }
 *   GET  /v1/invoices/{id}
 *   GET  /v1/invoices/{id}/qr          (SVG, 404 until a payment_ref exists)
 *   POST /v1/invoices/{id}/issue       (draft → issued; SPEC-W47 UIWEB-21)
 *   POST /v1/invoices/{id}/void        (issued/past_due → void; reverses AR)
 * The billing-engine authenticates service-to-service calls with an
 * X-Tenant-ID header matching tenant_id, so every request carries it.
 *
 * SPEC-W46 R10 keyset pagination (UIWEB-06): the list is bounded at 50 rows
 * by default; the cursor is the previous page's last item encoded as
 * `{created_at_unix_micros}:{id}` (billing-engine InvoiceCursor). "Load
 * more" appends the next page instead of silently capping the list.
 */

/** Page size for the keyset-paginated invoice list (backend default 50, max 200). */
const PAGE_SIZE = 50;

/** Encode the keyset cursor for the row AFTER `inv` (see InvoiceCursor::encode). */
function invoiceCursor(inv: Invoice): string {
  return `${Date.parse(inv.created_at) * 1000}:${inv.id}`;
}

/** YYYY-MM of the previous calendar month (last full billing period). */
function defaultPeriod(): string {
  const d = new Date();
  d.setUTCDate(1);
  d.setUTCMonth(d.getUTCMonth() - 1);
  return d.toISOString().slice(0, 7);
}

function statusVariant(
  status: string,
): "success" | "warning" | "destructive" | "secondary" {
  switch (status) {
    case "paid":
      return "success";
    case "issued":
      return "warning";
    case "past_due":
    case "void":
      return "destructive";
    default:
      return "secondary";
  }
}

export function InvoicesPanel({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [tenant, setTenant] = React.useState<Tenant | null>(null);
  const [invoices, setInvoices] = React.useState<Invoice[] | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [period, setPeriod] = React.useState(defaultPeriod);
  const [generating, setGenerating] = React.useState(false);
  const [openId, setOpenId] = React.useState<string | null>(null);
  // Keyset pagination state (UIWEB-06): the cursor of the last loaded row;
  // a full page means there may be more rows beyond it.
  const [cursor, setCursor] = React.useState<string | null>(null);
  const [hasMore, setHasMore] = React.useState(false);
  const [loadingMore, setLoadingMore] = React.useState(false);

  const tenantHeaders = React.useCallback(
    () => (tenant ? { "x-tenant-id": tenant.id } : undefined),
    [tenant],
  );

  const fetchPage = React.useCallback(
    async (tenantId: string, after: string | null) => {
      const data = await api.get<Invoice[] | { items: Invoice[] }>(
        "/api/billing/v1/invoices",
        { tenant_id: tenantId, limit: PAGE_SIZE, cursor: after ?? undefined },
        undefined,
        { "x-tenant-id": tenantId },
      );
      return Array.isArray(data) ? data : (data.items ?? []);
    },
    [],
  );

  const load = React.useCallback(async () => {
    setError(null);
    try {
      const t = await api.get<Tenant>(
        `/api/identity/v1/tenants/${orgSlug}`,
      );
      setTenant(t);
      const page = await fetchPage(t.id, null);
      setInvoices(page);
      setCursor(page.length > 0 ? invoiceCursor(page[page.length - 1]) : null);
      setHasMore(page.length >= PAGE_SIZE);
    } catch (e) {
      setError(
        e instanceof ApiError
          ? e.message
          : "Failed to load invoices — the billing engine may be offline.",
      );
      setInvoices([]);
      setHasMore(false);
    }
  }, [orgSlug, fetchPage]);

  const loadMore = async () => {
    if (!tenant || !cursor) return;
    setLoadingMore(true);
    setError(null);
    try {
      const page = await fetchPage(tenant.id, cursor);
      setInvoices((prev) => [...(prev ?? []), ...page]);
      setCursor(page.length > 0 ? invoiceCursor(page[page.length - 1]) : null);
      setHasMore(page.length >= PAGE_SIZE);
    } catch (e) {
      setError(
        e instanceof ApiError ? e.message : "Failed to load more invoices.",
      );
    } finally {
      setLoadingMore(false);
    }
  };

  React.useEffect(() => {
    void load();
  }, [load]);

  const generate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!tenant || !/^\d{4}-\d{2}$/.test(period)) return;
    setGenerating(true);
    try {
      const inv = await api.post<Invoice>(
        "/api/billing/v1/invoices/generate",
        { tenant_id: tenant.id, period },
        undefined,
        tenantHeaders(),
      );
      toast({
        title: `Invoice ${inv.period} generated`,
        description: `${formatMoney(inv.subtotal_cents, inv.currency)} · status ${inv.status}. Regenerating replaces a draft only.`,
        variant: "success",
      });
      await load();
    } catch (err) {
      toast({
        title: "Invoice generation failed",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setGenerating(false);
    }
  };

  const totals = React.useMemo(() => {
    const list = invoices ?? [];
    const currency = list[0]?.currency ?? tenant?.currency ?? "USD";
    const sum = (pred: (i: Invoice) => boolean) =>
      list.filter(pred).reduce((s, i) => s + i.subtotal_cents, 0);
    return {
      currency,
      outstanding: sum((i) => i.status === "issued" || i.status === "past_due"),
      paid: sum((i) => i.status === "paid"),
    };
  }, [invoices, tenant]);

  return (
    <Card className="mt-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Receipt className="h-4 w-4" /> Invoices
        </CardTitle>
        <CardDescription>
          Usage-based invoices rated by the billing engine from metered
          platform usage. Pay by scanning the QR code on an issued invoice.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {error ? <ErrorNote message={error} /> : null}

        <form
          onSubmit={generate}
          className="mb-4 flex flex-wrap items-end gap-3"
        >
          <div className="grid gap-1.5">
            <Label htmlFor="invoice-period">Billing period</Label>
            <Input
              id="invoice-period"
              type="month"
              value={period}
              onChange={(e) => setPeriod(e.target.value)}
            />
          </div>
          <Button type="submit" disabled={generating || !tenant}>
            <FilePlus2 className="h-4 w-4" />
            {generating ? "Generating…" : "Generate invoice"}
          </Button>
        </form>

        {invoices && invoices.length > 0 ? (
          <div className="mb-4 flex flex-wrap gap-6 rounded-md border border-border bg-muted/40 px-4 py-3 text-sm">
            <span>
              <span className="text-muted-foreground">Outstanding: </span>
              <span className="font-semibold">
                {formatMoney(totals.outstanding, totals.currency)}
              </span>
            </span>
            <span>
              <span className="text-muted-foreground">Collected: </span>
              <span className="font-semibold">
                {formatMoney(totals.paid, totals.currency)}
              </span>
            </span>
          </div>
        ) : null}

        {invoices === null ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-8" />
                <TableHead>Period</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Total</TableHead>
                <TableHead className="pr-2 text-right">Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {invoices.length === 0 ? (
                <TableEmpty colSpan={5}>
                  No invoices yet — generate one for a billing period above.
                </TableEmpty>
              ) : (
                invoices.map((inv) => (
                  <InvoiceRow
                    key={inv.id}
                    invoice={inv}
                    open={openId === inv.id}
                    onToggle={() =>
                      setOpenId((id) => (id === inv.id ? null : inv.id))
                    }
                    headers={tenantHeaders}
                    onChanged={() => void load()}
                  />
                ))
              )}
            </TableBody>
          </Table>
        )}

        {hasMore ? (
          <div className="mt-3 flex justify-center">
            <Button
              variant="outline"
              size="sm"
              onClick={() => void loadMore()}
              disabled={loadingMore || !cursor}
            >
              {loadingMore ? "Loading…" : "Load more invoices"}
            </Button>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

function InvoiceRow({
  invoice,
  open,
  onToggle,
  headers,
  onChanged,
}: {
  invoice: Invoice;
  open: boolean;
  onToggle: () => void;
  headers: () => Record<string, string> | undefined;
  onChanged: () => void;
}) {
  const { toast } = useToast();
  const [detail, setDetail] = React.useState<Invoice | null>(null);
  const [qrUrl, setQrUrl] = React.useState<string | null>(null);
  const [qrNote, setQrNote] = React.useState<string | null>(null);
  const [linkBusy, setLinkBusy] = React.useState(false);
  const [actionBusy, setActionBusy] = React.useState<"issue" | "void" | null>(
    null,
  );
  const [confirmVoid, setConfirmVoid] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    let cancelled = false;
    let objectUrl: string | null = null;
    (async () => {
      try {
        const d = await api.get<Invoice>(
          `/api/billing/v1/invoices/${invoice.id}`,
          undefined,
          undefined,
          headers(),
        );
        if (!cancelled) setDetail(d);
      } catch {
        if (!cancelled) setDetail(invoice);
      }
      // QR: 404 until the invoice has a payment_ref. Fetched (not <img src>)
      // so the X-Tenant-ID header rides along; rendered via object URL.
      try {
        const res = await fetch(`/api/billing/v1/invoices/${invoice.id}/qr`, {
          headers: { accept: "image/svg+xml", ...(headers() ?? {}) },
          cache: "no-store",
        });
        if (!res.ok) throw new ApiError(res.status, await res.text());
        const svg = await res.text();
        if (!cancelled) {
          objectUrl = URL.createObjectURL(
            new Blob([svg], { type: "image/svg+xml" }),
          );
          setQrUrl(objectUrl);
          setQrNote(null);
        }
      } catch {
        if (!cancelled) {
          setQrUrl(null);
          setQrNote(
            "No payment QR yet — create a payment link first (or wait until the invoice is issued).",
          );
        }
      }
    })();
    return () => {
      cancelled = true;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, invoice.id]);

  const createPaymentLink = async () => {
    setLinkBusy(true);
    try {
      await api.post<{ authorization_url?: string; reference?: string }>(
        `/api/billing/v1/invoices/${invoice.id}/payment-link`,
        {},
        undefined,
        headers(),
      );
      toast({
        title: "Payment link created",
        description: "Reload the invoice to see the QR code.",
        variant: "success",
      });
      onChanged();
    } catch (e) {
      toast({
        title: "Payment link failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setLinkBusy(false);
    }
  };

  // SPEC-W47 UIWEB-21: invoice lifecycle actions. Issue flips draft →
  // issued (posts DR AR / CR revenue in the same transaction); void reverses
  // the receivable for issued/past_due invoices. Both are money-role-gated
  // server-side (X-User-Roles injected by the gateway).
  const lifecycle = async (action: "issue" | "void") => {
    setActionBusy(action);
    try {
      await api.post<Invoice>(
        `/api/billing/v1/invoices/${invoice.id}/${action}`,
        {},
        undefined,
        headers(),
      );
      toast({
        title: action === "issue" ? "Invoice issued" : "Invoice voided",
        description:
          action === "issue"
            ? "The receivable was posted to the ledger."
            : "The receivable was reversed.",
        variant: "success",
      });
      setConfirmVoid(false);
      onChanged();
    } catch (e) {
      toast({
        title: action === "issue" ? "Issue failed" : "Void failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setActionBusy(null);
    }
  };

  const lineItems = detail?.line_items ?? invoice.line_items ?? [];

  return (
    <>
      <TableRow
        className="cursor-pointer"
        onClick={onToggle}
        aria-expanded={open}
      >
        <TableCell className="w-8">
          {open ? (
            <ChevronDown className="h-4 w-4 text-muted-foreground" />
          ) : (
            <ChevronRight className="h-4 w-4 text-muted-foreground" />
          )}
        </TableCell>
        <TableCell className="font-medium">{invoice.period}</TableCell>
        <TableCell>
          <Badge variant={statusVariant(invoice.status)}>
            {invoice.status.replace("_", " ")}
          </Badge>
        </TableCell>
        <TableCell>
          {formatMoney(invoice.subtotal_cents, invoice.currency)}
        </TableCell>
        <TableCell className="pr-2 text-right text-xs text-muted-foreground">
          {invoice.created_at
            ? new Date(invoice.created_at).toLocaleDateString()
            : "—"}
        </TableCell>
      </TableRow>
      {open ? (
        <TableRow>
          <TableCell colSpan={5} className="bg-muted/30">
            <div className="grid gap-4 px-2 py-3 md:grid-cols-[minmax(0,1fr)_auto]">
              <div>
                <p className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  Line items
                </p>
                {lineItems.length === 0 ? (
                  <p className="text-sm text-muted-foreground">
                    No billable usage in this period.
                  </p>
                ) : (
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="text-left text-xs text-muted-foreground">
                        <th className="pb-1 font-medium">Metric</th>
                        <th className="pb-1 font-medium">Usage</th>
                        <th className="pb-1 font-medium">Included</th>
                        <th className="pb-1 font-medium">Billed</th>
                        <th className="pb-1 text-right font-medium">Amount</th>
                      </tr>
                    </thead>
                    <tbody>
                      {lineItems.map((li) => (
                        <tr key={li.metric} className="border-t border-border">
                          <td className="py-1.5 font-medium">{li.metric}</td>
                          <td className="py-1.5">{li.quantity}</td>
                          <td className="py-1.5">{li.included}</td>
                          <td className="py-1.5">{li.billable}</td>
                          <td className="py-1.5 text-right">
                            {formatMoney(li.amount_cents, invoice.currency)}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
                {detail?.paid_at ? (
                  <p className="mt-2 text-xs text-muted-foreground">
                    Paid {new Date(detail.paid_at).toLocaleString()}
                  </p>
                ) : null}
              </div>
              <div className="flex min-w-40 flex-col items-center gap-2">
                {qrUrl ? (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img
                    src={qrUrl}
                    alt={`Payment QR for invoice ${invoice.period}`}
                    className="h-36 w-36 rounded-md border border-border bg-white p-2"
                  />
                ) : (
                  <div className="flex h-36 w-36 flex-col items-center justify-center gap-2 rounded-md border border-dashed border-border p-3 text-center">
                    <QrCode className="h-6 w-6 text-muted-foreground" />
                    <p className="text-[11px] text-muted-foreground">
                      {qrNote ?? "Loading QR…"}
                    </p>
                  </div>
                )}
                {invoice.status !== "paid" && invoice.status !== "void" ? (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={linkBusy || actionBusy !== null}
                    onClick={(e) => {
                      e.stopPropagation();
                      void createPaymentLink();
                    }}
                  >
                    {linkBusy ? "Creating…" : "Create payment link"}
                  </Button>
                ) : null}
                {invoice.status === "draft" ? (
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={actionBusy !== null}
                    onClick={(e) => {
                      e.stopPropagation();
                      void lifecycle("issue");
                    }}
                  >
                    {actionBusy === "issue" ? "Issuing…" : "Issue invoice"}
                  </Button>
                ) : null}
                {invoice.status !== "paid" && invoice.status !== "void" ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="text-destructive"
                    disabled={actionBusy !== null}
                    onClick={(e) => {
                      e.stopPropagation();
                      setConfirmVoid(true);
                    }}
                  >
                    Void
                  </Button>
                ) : null}
              </div>
            </div>
          </TableCell>
        </TableRow>
      ) : null}
      <ConfirmDialog
        open={confirmVoid}
        onOpenChange={(open) => !open && setConfirmVoid(false)}
        title="Void this invoice?"
        description={`Invoice ${invoice.period} · ${formatMoney(invoice.subtotal_cents, invoice.currency)}. Voiding an issued invoice reverses the receivable in the ledger; voided invoices can no longer be paid.`}
        confirmLabel="Void invoice"
        destructive
        busy={actionBusy === "void"}
        onConfirm={() => void lifecycle("void")}
      />
    </>
  );
}
