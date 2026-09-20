"use client";

import * as React from "react";
import { CheckCircle2, AlertTriangle, Info, XCircle, X } from "lucide-react";
import { cn } from "@/lib/utils";

/** Tiny toast system: <ToastProvider> at the root, useToast() anywhere. */

export type ToastVariant = "default" | "success" | "warning" | "destructive";

export interface ToastItem {
  id: number;
  title: string;
  description?: string;
  variant: ToastVariant;
  /** optional call-to-action link rendered under the description */
  href?: string;
  hrefLabel?: string;
  /**
   * Optional call-to-action button rendered under the description (used
   * when the target URL is not known up-front, e.g. SPEC-W45 K15(e) staff
   * escalation tokens minted on click). Takes precedence over `href`.
   */
  actionLabel?: string;
  onAction?: () => void;
}

interface ToastInput {
  title: string;
  description?: string;
  variant?: ToastVariant;
  href?: string;
  hrefLabel?: string;
  actionLabel?: string;
  onAction?: () => void;
}

const ToastContext = React.createContext<{
  toast: (t: ToastInput) => void;
} | null>(null);

const icons: Record<ToastVariant, React.ReactNode> = {
  default: <Info className="h-4 w-4 text-info" />,
  success: <CheckCircle2 className="h-4 w-4 text-success" />,
  warning: <AlertTriangle className="h-4 w-4 text-warning" />,
  destructive: <XCircle className="h-4 w-4 text-destructive" />,
};

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = React.useState<ToastItem[]>([]);
  const nextId = React.useRef(1);

  const dismiss = React.useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const toast = React.useCallback(
    (input: ToastInput) => {
      const id = nextId.current++;
      const item: ToastItem = {
        id,
        title: input.title,
        description: input.description,
        variant: input.variant ?? "default",
        href: input.href,
        hrefLabel: input.hrefLabel,
        actionLabel: input.actionLabel,
        onAction: input.onAction,
      };
      setToasts((prev) => [...prev.slice(-4), item]);
      setTimeout(() => dismiss(id), 5000);
    },
    [dismiss],
  );

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <div className="pointer-events-none fixed bottom-4 right-4 z-[100] flex w-80 flex-col gap-2">
        {toasts.map((t) => (
          <div
            key={t.id}
            className={cn(
              "pointer-events-auto flex items-start gap-2 rounded-lg border border-border bg-card p-3 shadow-md",
            )}
            role="status"
          >
            <span className="mt-0.5 shrink-0">{icons[t.variant]}</span>
            <div className="min-w-0 flex-1">
              <p className="text-sm font-medium">{t.title}</p>
              {t.description ? (
                <p className="mt-0.5 truncate text-xs text-muted-foreground">
                  {t.description}
                </p>
              ) : null}
              {t.onAction ? (
                <button
                  type="button"
                  onClick={t.onAction}
                  className="mt-1 inline-block cursor-pointer text-xs font-medium text-primary underline underline-offset-2"
                >
                  {t.actionLabel ?? "Open"}
                </button>
              ) : t.href ? (
                <a
                  href={t.href}
                  className="mt-1 inline-block text-xs font-medium text-primary underline underline-offset-2"
                >
                  {t.hrefLabel ?? t.href}
                </a>
              ) : null}
            </div>
            <button
              onClick={() => dismiss(t.id)}
              aria-label="Dismiss"
              className="shrink-0 text-muted-foreground hover:opacity-70 cursor-pointer"
            >
              <X className="h-3.5 w-3.5" />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast() {
  const ctx = React.useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used inside <ToastProvider>");
  return ctx;
}
