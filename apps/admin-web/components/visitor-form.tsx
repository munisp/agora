"use client";

/**
 * Visitor details asked once before the public chat opens or a call starts (2026-11-21): name, phone and email.
 * They go to the voice runtime as `visitor` (app/visitor.py), so the receptionist greets the visitor by name and
 * does not ask for these again, and the conversation is filed under them. Remembered in this browser per business
 * (localStorage, best-effort) so a returning visitor is not asked twice.
 */
import * as React from "react";

export interface VisitorDetails {
  name: string;
  phone: string;
  email?: string;
}

const key = (tenant: string) => `agora-visitor:${tenant}`;

export function loadVisitor(tenant: string): VisitorDetails | null {
  try {
    const raw = localStorage.getItem(key(tenant));
    const v = raw ? (JSON.parse(raw) as Partial<VisitorDetails>) : null;
    return v && typeof v.name === "string" && typeof v.phone === "string" && v.name && v.phone
      ? { name: v.name, phone: v.phone, ...(v.email ? { email: v.email } : {}) }
      : null;
  } catch {
    return null;
  }
}

function saveVisitor(tenant: string, v: VisitorDetails) {
  try {
    localStorage.setItem(key(tenant), JSON.stringify(v));
  } catch {
    /* private mode / blocked storage: asked again next time */
  }
}

/** The visitor details for this business, and a setter that also remembers them. */
export function useVisitor(tenant: string): [VisitorDetails | null, (v: VisitorDetails) => void] {
  const [visitor, setVisitor] = React.useState<VisitorDetails | null>(null);
  React.useEffect(() => setVisitor(loadVisitor(tenant)), [tenant]);
  const set = React.useCallback(
    (v: VisitorDetails) => {
      saveVisitor(tenant, v);
      setVisitor(v);
    },
    [tenant],
  );
  return [visitor, set];
}

const PHONE = /^\+?[\d\s()-]{7,20}$/;
const EMAIL = /^[^@\s]+@[^@\s]+\.[A-Za-z]{2,}$/;

export function VisitorForm({
  onSubmit,
  submitLabel = "Start chat",
  intro = "Before we start, tell us who you are so we can help you faster.",
  className,
}: {
  onSubmit: (v: VisitorDetails) => void;
  submitLabel?: string;
  intro?: string;
  className?: string;
}) {
  const [name, setName] = React.useState("");
  const [phone, setPhone] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const n = name.trim().replace(/\s+/g, " ");
    const p = phone.trim();
    const m = email.trim();
    if (!n) return setError("Please enter your name.");
    if (!PHONE.test(p) || p.replace(/\D/g, "").length < 10) return setError("Please enter a valid phone number.");
    if (m && !EMAIL.test(m)) return setError("Please enter a valid email, or leave it empty.");
    setError(null);
    onSubmit({ name: n, phone: p, ...(m ? { email: m } : {}) });
  };

  const field =
    "w-full rounded-md border border-border bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring";
  return (
    <form onSubmit={submit} className={className ?? "space-y-3 p-4"} aria-label="Your details">
      <p className="text-sm text-muted-foreground">{intro}</p>
      <label className="block text-xs font-medium">
        Full name
        <input className={`${field} mt-1`} value={name} onChange={(e) => setName(e.target.value)} autoComplete="name" placeholder="e.g. Grace Okon" />
      </label>
      <label className="block text-xs font-medium">
        Phone number
        <input className={`${field} mt-1`} value={phone} onChange={(e) => setPhone(e.target.value)} autoComplete="tel" inputMode="tel" placeholder="e.g. 0806 555 3124" />
      </label>
      <label className="block text-xs font-medium">
        Email <span className="font-normal text-muted-foreground">(optional)</span>
        <input className={`${field} mt-1`} value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="email" inputMode="email" placeholder="you@example.com" />
      </label>
      {error ? <p className="text-xs text-destructive" role="alert">{error}</p> : null}
      <button type="submit" className="w-full rounded-full bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:opacity-90">
        {submitLabel}
      </button>
      <p className="text-[11px] leading-snug text-muted-foreground">
        We use these only to answer you and to book on your behalf. We never ask for your BVN, NIN, PIN or OTP.
      </p>
    </form>
  );
}
