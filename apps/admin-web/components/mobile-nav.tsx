"use client";

/**
 * Mobile nav (SPEC-W82 B1.2): hamburger button + slide-over drawer shown
 * below the md: breakpoint. Reuses the hand-written Dialog pattern
 * (fixed backdrop, Esc to close, body scroll lock) — no new primitives.
 */
import * as React from "react";
import { usePathname } from "next/navigation";
import { Menu, X } from "lucide-react";
import { OrgNav } from "@/components/org-nav";
import { Button } from "@/components/ui/button";

export function MobileNav({
  orgSlug,
  roles = [],
  tenantName,
}: {
  orgSlug: string;
  /** caller's Keycloak realm roles — forwarded to OrgNav gating */
  roles?: string[];
  tenantName: string;
}) {
  const [open, setOpen] = React.useState(false);
  const pathname = usePathname();

  // Close the drawer on navigation.
  React.useEffect(() => {
    setOpen(false);
  }, [pathname]);

  // Dialog pattern: Esc closes + body scroll lock while open.
  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [open]);

  return (
    <>
      <Button
        variant="ghost"
        size="icon"
        aria-label="Open navigation"
        aria-expanded={open}
        onClick={() => setOpen(true)}
      >
        <Menu className="h-5 w-5" />
      </Button>

      {open ? (
        <div className="fixed inset-0 z-50 md:hidden">
          <div
            className="absolute inset-0 bg-foreground/40"
            aria-hidden
            onClick={() => setOpen(false)}
          />
          <div
            role="dialog"
            aria-modal="true"
            aria-label="Navigation"
            className="absolute inset-y-0 left-0 flex w-72 max-w-[85vw] flex-col border-r border-border bg-card shadow-lg"
          >
            <div className="flex items-center justify-between border-b border-border px-5 py-4">
              <p className="truncate text-sm font-semibold">{tenantName}</p>
              <button
                onClick={() => setOpen(false)}
                aria-label="Close navigation"
                className="rounded-sm text-muted-foreground transition-opacity hover:opacity-70 cursor-pointer"
              >
                <X className="h-4 w-4" />
              </button>
            </div>
            <div className="flex-1 overflow-y-auto py-3">
              <OrgNav orgSlug={orgSlug} roles={roles} />
            </div>
          </div>
        </div>
      ) : null}
    </>
  );
}
