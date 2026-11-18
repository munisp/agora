"use client";

/**
 * Sign-out submit button (SPEC-W82 B1.3): clears the per-browser last-good
 * CRM localStorage cache before the surrounding server-action form signs
 * the user out, so no tenant data lingers on a shared device.
 */
import { LogOut } from "lucide-react";
import { clearLocalCache } from "@/lib/local-cache";

export function SignOutButton({ className }: { className?: string } = {}) {
  return (
    <button
      type="submit"
      onClick={() => clearLocalCache()}
      className={
        className ??
        "flex w-full items-center gap-2 rounded-md px-3 py-2 text-sm text-muted-foreground hover:bg-accent hover:text-foreground cursor-pointer"
      }
    >
      <LogOut className="h-4 w-4" />
      Sign out
    </button>
  );
}
