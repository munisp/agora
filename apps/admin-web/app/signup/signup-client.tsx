"use client";

/**
 * SPEC-W45 STK O5 tenant self-signup: POST /api/identity/v1/tenants
 * { name, slug } with the caller's Keycloak JWT (attached by the BFF).
 * The identity service binds the caller as owner and forces plan=free —
 * the client deliberately sends neither. On success the user lands in the
 * new workspace at /app/[slug].
 */
import * as React from "react";
import { useRouter } from "next/navigation";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";

const SLUG_RE = /^[a-z0-9-]{2,63}$/;

function slugify(name: string): string {
  return name
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63);
}

export function SignupClient() {
  const router = useRouter();
  const [name, setName] = React.useState("");
  const [slug, setSlug] = React.useState("");
  const [slugTouched, setSlugTouched] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const effectiveSlug = slugTouched ? slug : slugify(name);
  const valid = name.trim().length > 0 && SLUG_RE.test(effectiveSlug);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.post("/api/identity/v1/tenants", {
        name: name.trim(),
        slug: effectiveSlug,
      });
      router.push(`/app/${effectiveSlug}`);
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 409
          ? "That slug is already taken — pick another one."
          : err instanceof ApiError
            ? err.message
            : "Could not create the organisation. Please try again.",
      );
      setBusy(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-muted/40 px-4">
      <Card className="w-full max-w-md">
        <CardHeader className="items-center text-center">
          <span className="mb-2 flex h-10 w-10 items-center justify-center rounded-md bg-primary text-sm font-bold text-primary-foreground">
            OD
          </span>
          <CardTitle className="text-xl">Create your organisation</CardTitle>
          <CardDescription>
            Spin up a free workspace — you become its owner. Invite your team
            from Settings once you are in.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={(e) => void submit(e)} className="grid gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="su-name">Organisation name</Label>
              <Input
                id="su-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Sunrise Dental"
                autoFocus
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="su-slug">Workspace slug</Label>
              <Input
                id="su-slug"
                value={effectiveSlug}
                onChange={(e) => {
                  setSlugTouched(true);
                  setSlug(e.target.value.toLowerCase());
                }}
                placeholder="sunrise-dental"
              />
              <p className="text-xs text-muted-foreground">
                2–63 lowercase letters, digits and dashes. This is your
                workspace URL: /app/{effectiveSlug || "…"}
              </p>
            </div>
            {error ? (
              <p className="text-sm text-destructive">{error}</p>
            ) : null}
            <Button type="submit" size="lg" disabled={!valid || busy}>
              {busy ? "Creating…" : "Create workspace"}
            </Button>
            <p className="text-center text-xs text-muted-foreground">
              Free plan — up to 3 team members. If the new workspace does not
              appear, sign out and back in to refresh your session.
            </p>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
