import Link from "next/link";
import { notFound } from "next/navigation";
import { ExternalLink, LogOut } from "lucide-react";
import { auth, signOut } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import { OrgNav } from "@/components/org-nav";
import { AgoraMark } from "@/components/brand";
import { Button } from "@/components/ui/button";
import type { Tenant } from "@/lib/types";

export default async function OrgLayout({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  // Middleware already guarantees a session; this is defence in depth.
  if (!session) return notFound();

  // Tenant isolation: the dashboard is only reachable for tenants present in
  // the Keycloak `tenant_slugs` claim.
  if (!(session.tenantSlugs ?? []).includes(orgSlug)) {
    return (
      <div className="flex min-h-screen items-center justify-center px-4">
        <div className="max-w-md text-center">
          <h1 className="text-xl font-semibold">No access to “{orgSlug}”</h1>
          <p className="mt-2 text-sm text-muted-foreground">
            Your account is not a member of this organisation. Switch to one of
            your tenants:
          </p>
          <div className="mt-4 flex flex-wrap justify-center gap-2">
            {(session.tenantSlugs ?? []).map((slug) => (
              <Link key={slug} href={`/app/${slug}`}>
                <Button variant="outline" size="sm">
                  {slug}
                </Button>
              </Link>
            ))}
          </div>
        </div>
      </div>
    );
  }

  let tenant: Tenant | null = null;
  try {
    tenant = await serverApi<Tenant>(`/api/identity/v1/tenants/${orgSlug}`);
  } catch {
    // Gateway or identity service unreachable: render the shell anyway with
    // the slug as display name; individual pages surface their own errors.
    tenant = null;
  }

  return (
    <div className="flex min-h-screen">
      <aside className="flex w-64 shrink-0 flex-col bg-umber-deep text-paper">
        <div className="flex items-center gap-3 px-5 py-5">
          <AgoraMark tone="light" />
          <div className="min-w-0">
            <p className="truncate font-display text-lg leading-tight">
              {tenant?.name ?? orgSlug}
            </p>
            <p className="truncate text-xs text-paper/60">
              {tenant?.plan ? `${tenant.plan} plan` : orgSlug}
            </p>
          </div>
        </div>
        <div className="woven-band opacity-80" />
        <div className="flex-1 overflow-y-auto py-3">
          <OrgNav orgSlug={orgSlug} roles={session.realmRoles ?? []} />
        </div>
        <div className="space-y-1 border-t border-paper/10 p-3">
          <Link
            href={`/p/${orgSlug}`}
            target="_blank"
            rel="noreferrer noopener"
            className="flex items-center gap-2 rounded-md px-3 py-2 text-sm text-paper/65 hover:bg-paper/5 hover:text-paper"
          >
            <ExternalLink className="h-4 w-4" />
            View public site
          </Link>
          <form
            action={async () => {
              "use server";
              await signOut({ redirectTo: "/" });
            }}
          >
            <button
              type="submit"
              className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-sm text-paper/65 hover:bg-paper/5 hover:text-paper cursor-pointer"
            >
              <LogOut className="h-4 w-4" />
              Sign out
            </button>
          </form>
          <p className="truncate px-3 pt-1 text-xs text-paper/50">
            {session.user?.email ?? session.user?.name ?? ""}
          </p>
        </div>
      </aside>
      <main className="paper-pattern min-w-0 flex-1 px-8 py-8">{children}</main>
    </div>
  );
}
