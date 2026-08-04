"use client";

/**
 * Lending app client (SPEC-W20 Agent C): applications queue (status
 * filters, score badge, approve/decline dialog with the KYC gate,
 * disburse), products editor, loan book + loan detail (schedule + repay
 * form), and portfolio tiles (outstanding, PAR30).
 *
 * Data sources (all through the BFF with the x-tenant-slug header
 * attached, mirroring the handlers in internal/lending):
 *   - GET/POST   /api/bookings/v1/lending/products
 *   - PATCH      /api/bookings/v1/lending/products/{id}
 *   - GET/POST   /api/bookings/v1/lending/applications?status=
 *   - PATCH      /api/bookings/v1/lending/applications/{id}
 *   - POST       /api/bookings/v1/lending/applications/{id}/disburse
 *   - GET        /api/bookings/v1/lending/loans?status=&application_id=
 *   - GET        /api/bookings/v1/lending/loans/{id}
 *   - POST       /api/bookings/v1/lending/loans/{id}/repay
 *   - GET        /api/bookings/v1/lending/portfolio
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  unwrap,
  formatKobo,
  type LoanAccount,
  type LoanApplication,
  type LoanView,
  type Portfolio,
  type Product,
  type RepayResponse,
} from "@/components/apps/lending/types";
import {
  draftFromProduct,
  emptyDraft,
  ProductCard,
  ProductEditorDialog,
  type ProductDraft,
} from "@/components/apps/lending/products-panel";
import {
  ApplicationsTable,
  DecisionDialog,
  StatusFilterBar,
  type DecisionInput,
} from "@/components/apps/lending/applications-queue";
import {
  ApplicationCreateDialog,
  type CreateApplicationInput,
} from "@/components/apps/lending/application-create";
import { LoanDetail, LoansTable } from "@/components/apps/lending/loan-detail";
import { PortfolioTiles } from "@/components/apps/lending/portfolio-tiles";

const ROLLOUT_NOTE =
  "Lending is not available yet — the booking-service lending API may still be rolling out.";

function errMsg(e: unknown): string {
  return e instanceof ApiError ? e.message : "Unexpected error";
}

export function LendingClient({
  orgSlug,
  canManage,
}: {
  orgSlug: string;
  canManage: boolean;
}) {
  const { toast } = useToast();

  // Products
  const [products, setProducts] = React.useState<Product[]>([]);
  const [productsLoading, setProductsLoading] = React.useState(true);
  const [productsError, setProductsError] = React.useState<string | null>(null);
  const [editingProduct, setEditingProduct] = React.useState<ProductDraft | null>(null);
  const [savingProduct, setSavingProduct] = React.useState(false);

  // Applications
  const [applications, setApplications] = React.useState<LoanApplication[]>([]);
  const [appsLoading, setAppsLoading] = React.useState(true);
  const [appsError, setAppsError] = React.useState<string | null>(null);
  const [statusFilter, setStatusFilter] = React.useState("");
  const [creating, setCreating] = React.useState(false);
  const [decision, setDecision] = React.useState<{
    mode: "approve" | "decline";
    application: LoanApplication;
  } | null>(null);
  const [actionBusy, setActionBusy] = React.useState(false);

  // Loans
  const [loans, setLoans] = React.useState<LoanAccount[]>([]);
  const [loansLoading, setLoansLoading] = React.useState(true);
  const [loanStatusFilter, setLoanStatusFilter] = React.useState("");
  const [loanView, setLoanView] = React.useState<LoanView | null>(null);

  // Portfolio
  const [portfolio, setPortfolio] = React.useState<Portfolio | null>(null);
  const [portfolioLoading, setPortfolioLoading] = React.useState(true);

  const loadProducts = React.useCallback(
    async (signal?: AbortSignal) => {
      setProductsLoading(true);
      setProductsError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/lending/products", {
          tenant: orgSlug,
          all: "true",
        });
        if (signal?.aborted) return;
        setProducts(unwrap<Product>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setProducts([]);
        setProductsError(
          e instanceof ApiError && e.status !== 404 ? e.message : ROLLOUT_NOTE,
        );
      } finally {
        if (!signal?.aborted) setProductsLoading(false);
      }
    },
    [orgSlug],
  );

  const loadApplications = React.useCallback(
    async (status: string, signal?: AbortSignal) => {
      setAppsLoading(true);
      setAppsError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/lending/applications", {
          tenant: orgSlug,
          status,
        });
        if (signal?.aborted) return;
        setApplications(unwrap<LoanApplication>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setApplications([]);
        setAppsError(
          e instanceof ApiError && e.status !== 404 ? e.message : ROLLOUT_NOTE,
        );
      } finally {
        if (!signal?.aborted) setAppsLoading(false);
      }
    },
    [orgSlug],
  );

  const loadLoans = React.useCallback(
    async (status: string, signal?: AbortSignal) => {
      setLoansLoading(true);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/lending/loans", {
          tenant: orgSlug,
          status,
        });
        if (signal?.aborted) return;
        setLoans(unwrap<LoanAccount>(data));
      } catch {
        if (!signal?.aborted) setLoans([]);
      } finally {
        if (!signal?.aborted) setLoansLoading(false);
      }
    },
    [orgSlug],
  );

  const loadPortfolio = React.useCallback(
    async (signal?: AbortSignal) => {
      setPortfolioLoading(true);
      try {
        const data = await api.get<{ portfolio: Portfolio }>(
          "/api/bookings/v1/lending/portfolio",
          { tenant: orgSlug },
        );
        if (signal?.aborted) return;
        setPortfolio(data.portfolio ?? null);
      } catch {
        if (!signal?.aborted) setPortfolio(null);
      } finally {
        if (!signal?.aborted) setPortfolioLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const c = new AbortController();
    void loadProducts(c.signal);
    return () => c.abort();
  }, [loadProducts]);
  React.useEffect(() => {
    const c = new AbortController();
    void loadApplications(statusFilter, c.signal);
    return () => c.abort();
  }, [statusFilter, loadApplications]);
  React.useEffect(() => {
    const c = new AbortController();
    void loadLoans(loanStatusFilter, c.signal);
    return () => c.abort();
  }, [loanStatusFilter, loadLoans]);
  React.useEffect(() => {
    const c = new AbortController();
    void loadPortfolio(c.signal);
    return () => c.abort();
  }, [loadPortfolio]);

  const reloadAll = async () => {
    await Promise.all([
      loadApplications(statusFilter),
      loadLoans(loanStatusFilter),
      loadPortfolio(),
    ]);
  };

  // ---------------------------------------------------------------------
  // Product mutations
  // ---------------------------------------------------------------------

  const saveProduct = async (draft: ProductDraft): Promise<boolean> => {
    setSavingProduct(true);
    try {
      const body = {
        name: draft.name,
        active: draft.active,
        principal_min_kobo: draft.principal_min_kobo,
        principal_max_kobo: draft.principal_max_kobo,
        term_days: draft.term_days,
        interest_bps: draft.interest_bps,
        fee_flat_kobo: draft.fee_flat_kobo,
      };
      if (draft.id) {
        await api.patch(`/api/bookings/v1/lending/products/${draft.id}`, body, {
          tenant: orgSlug,
        });
      } else {
        await api.post("/api/bookings/v1/lending/products", body, { tenant: orgSlug });
      }
      toast({ title: draft.id ? "Product updated" : "Product created", variant: "success" });
      setEditingProduct(null);
      await loadProducts();
      return true;
    } catch (e) {
      toast({ title: "Save failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setSavingProduct(false);
    }
  };

  const toggleProduct = async (p: Product, active: boolean) => {
    try {
      await api.patch(
        `/api/bookings/v1/lending/products/${p.id}`,
        { active },
        { tenant: orgSlug },
      );
      await loadProducts();
    } catch (e) {
      toast({ title: "Update failed", description: errMsg(e), variant: "destructive" });
    }
  };

  // ---------------------------------------------------------------------
  // Application mutations
  // ---------------------------------------------------------------------

  const createApplication = async (input: CreateApplicationInput): Promise<boolean> => {
    setActionBusy(true);
    try {
      await api.post("/api/bookings/v1/lending/applications", input, { tenant: orgSlug });
      toast({
        title: input.status === "submitted" ? "Application submitted & scored" : "Draft saved",
        variant: "success",
      });
      setCreating(false);
      await loadApplications(statusFilter);
      return true;
    } catch (e) {
      toast({ title: "Create failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setActionBusy(false);
    }
  };

  const patchStatus = async (a: LoanApplication, body: Record<string, unknown>, ok: string) => {
    setActionBusy(true);
    try {
      await api.patch(`/api/bookings/v1/lending/applications/${a.id}`, body, {
        tenant: orgSlug,
      });
      toast({ title: ok, variant: "success" });
      await reloadAll();
      return true;
    } catch (e) {
      toast({ title: "Action failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setActionBusy(false);
    }
  };

  const submitDecision = async (input: DecisionInput): Promise<boolean> => {
    if (!decision) return false;
    const ok = await patchStatus(
      decision.application,
      input as unknown as Record<string, unknown>,
      input.status === "approved" ? "Application approved" : "Application declined",
    );
    if (ok) setDecision(null);
    return ok;
  };

  const disburse = async (a: LoanApplication) => {
    setActionBusy(true);
    try {
      const res = await api.post<{ loan: LoanAccount; replayed: boolean }>(
        `/api/bookings/v1/lending/applications/${a.id}/disburse`,
        {},
        { tenant: orgSlug },
      );
      toast({
        title: res.replayed
          ? "Already disbursed — returning the existing loan"
          : `Disbursed ${formatKobo(res.loan.principal_kobo)} (payout intent emitted for the payments rail)`,
        variant: "success",
      });
      await reloadAll();
      await openLoan(res.loan.id);
    } catch (e) {
      toast({ title: "Disburse failed", description: errMsg(e), variant: "destructive" });
    } finally {
      setActionBusy(false);
    }
  };

  const onAppAction = (action: string, a: LoanApplication) => {
    switch (action) {
      case "submit":
        void patchStatus(a, { status: "submitted" }, "Application submitted & scored");
        break;
      case "review":
        void patchStatus(a, { status: "under_review" }, "Application under review");
        break;
      case "approve":
      case "decline":
        setDecision({ mode: action, application: a });
        break;
      case "disburse":
        void disburse(a);
        break;
      case "default":
        void patchStatus(a, { status: "defaulted" }, "Marked defaulted");
        break;
    }
  };

  // ---------------------------------------------------------------------
  // Loans
  // ---------------------------------------------------------------------

  const openLoan = React.useCallback(
    async (loanId: string) => {
      try {
        const view = await api.get<LoanView>(`/api/bookings/v1/lending/loans/${loanId}`, {
          tenant: orgSlug,
        });
        setLoanView(view);
      } catch (e) {
        toast({ title: "Loan load failed", description: errMsg(e), variant: "destructive" });
      }
    },
    [orgSlug, toast],
  );

  const viewLoanForApplication = async (a: LoanApplication) => {
    try {
      const data = await api.get<unknown>("/api/bookings/v1/lending/loans", {
        tenant: orgSlug,
        application_id: a.id,
      });
      const found = unwrap<LoanAccount>(data);
      if (found.length === 0) {
        toast({ title: "No loan for this application yet", variant: "destructive" });
        return;
      }
      await openLoan(found[0].id);
    } catch (e) {
      toast({ title: "Loan lookup failed", description: errMsg(e), variant: "destructive" });
    }
  };

  const repay = async (amountKobo: number, refId: string): Promise<boolean> => {
    if (!loanView) return false;
    setActionBusy(true);
    try {
      const res = await api.post<RepayResponse>(
        `/api/bookings/v1/lending/loans/${loanView.loan.id}/repay`,
        { amount_kobo: amountKobo, ref_id: refId },
        { tenant: orgSlug },
      );
      toast({
        title: res.replayed
          ? "Already recorded — this reference was posted before"
          : res.clamped
            ? `Repayment clamped to outstanding (${formatKobo(res.repayment.amount_kobo)} applied)`
            : res.loan_repaid
              ? "Loan fully repaid"
              : `Repayment of ${formatKobo(res.repayment.amount_kobo)} posted`,
        variant: "success",
      });
      await openLoan(loanView.loan.id);
      await Promise.all([loadLoans(loanStatusFilter), loadPortfolio()]);
      return true;
    } catch (e) {
      toast({ title: "Repayment failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setActionBusy(false);
    }
  };

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Lending"
        description="Micro-loans: products, applications (naive 0–100 score — not a credit bureau score), KYC-gated decisions, disbursement intents for the payments rail, idempotent repayments and the PAR30 portfolio view."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void loadProducts();
              void reloadAll();
            }}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            Refresh
          </Button>
        }
      />

      <Tabs defaultValue="applications">
        <TabsList className="mb-4">
          <TabsTrigger value="applications">Applications</TabsTrigger>
          <TabsTrigger value="loans">Loans</TabsTrigger>
          <TabsTrigger value="products">Products</TabsTrigger>
          <TabsTrigger value="portfolio">Portfolio</TabsTrigger>
        </TabsList>

        <TabsContent value="applications">
          {appsError ? <ErrorNote message={appsError} /> : null}
          <div className="mb-3 flex items-center justify-between">
            <StatusFilterBar value={statusFilter} onChange={setStatusFilter} />
            {canManage ? (
              <Button size="sm" onClick={() => setCreating(true)}>
                New application
              </Button>
            ) : null}
          </div>
          <ApplicationsTable
            applications={applications}
            products={products}
            canManage={canManage}
            loading={appsLoading}
            onAction={onAppAction}
            onViewLoan={(a) => void viewLoanForApplication(a)}
          />
        </TabsContent>

        <TabsContent value="loans">
          <LoansTable
            loans={loans}
            statusFilter={loanStatusFilter}
            onStatusFilter={setLoanStatusFilter}
            onOpen={(l) => void openLoan(l.id)}
            loading={loansLoading}
          />
          {loanView ? (
            <LoanDetail
              view={loanView}
              canManage={canManage}
              busy={actionBusy}
              onRepay={repay}
              onClose={() => setLoanView(null)}
            />
          ) : null}
        </TabsContent>

        <TabsContent value="products">
          {productsError ? <ErrorNote message={productsError} /> : null}
          <div className="space-y-3">
            {canManage ? (
              <div>
                <Button size="sm" onClick={() => setEditingProduct(emptyDraft())}>
                  New product
                </Button>
              </div>
            ) : null}
            {products.map((p) => (
              <ProductCard
                key={p.id}
                product={p}
                canManage={canManage}
                onEdit={() => setEditingProduct(draftFromProduct(p))}
                onToggle={(active) => void toggleProduct(p, active)}
              />
            ))}
            {!productsLoading && products.length === 0 && !productsError ? (
              <p className="text-sm text-muted-foreground">
                No loan products yet
                {canManage ? " — create one to start accepting applications." : "."}
              </p>
            ) : null}
            {productsLoading ? (
              <p className="text-sm text-muted-foreground">Loading products…</p>
            ) : null}
          </div>
        </TabsContent>

        <TabsContent value="portfolio">
          <PortfolioTiles portfolio={portfolio} loading={portfolioLoading} />
          <p className="mt-3 text-xs text-muted-foreground">
            PAR30 = outstanding of loans &gt;30 days past due ÷ total
            outstanding. Default marking is operator-driven (no automatic
            cron yet).
          </p>
        </TabsContent>
      </Tabs>

      {editingProduct ? (
        <ProductEditorDialog
          draft={editingProduct}
          busy={savingProduct}
          onSave={saveProduct}
          onCancel={() => setEditingProduct(null)}
        />
      ) : null}
      {creating ? (
        <ApplicationCreateDialog
          products={products}
          busy={actionBusy}
          onCreate={createApplication}
          onCancel={() => setCreating(false)}
        />
      ) : null}
      {decision ? (
        <DecisionDialog
          mode={decision.mode}
          application={decision.application}
          busy={actionBusy}
          onSubmit={submitDecision}
          onCancel={() => setDecision(null)}
        />
      ) : null}
    </div>
  );
}
