"use client";

import * as React from "react";
import { Code2, Copy, ExternalLink, Globe, RefreshCw, Save } from "lucide-react";
import Link from "next/link";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import type { PublicSite } from "@/lib/types";

const TEMPLATES = [
  { value: "classic", label: "Classic — header + single column" },
  { value: "modern", label: "Modern — hero block, card grid" },
  { value: "compact", label: "Compact — minimal, embed-friendly" },
];

const DEFAULT_COLOR = "#7c5b3e";

export function PublicSiteClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [site, setSite] = React.useState<PublicSite | null>(null);
  const [form, setForm] = React.useState({
    site_slug: orgSlug,
    primaryColor: DEFAULT_COLOR,
    logoUrl: "",
    heroTitle: "",
    heroSubtitle: "",
    template: "classic",
    tagline: "",
    published: false,
    brandName: "",
    customDomain: "",
  });
  const [error, setError] = React.useState<string | null>(null);
  const [notFound, setNotFound] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  // Bumped after each save so the live preview iframe reloads the published page.
  const [previewKey, setPreviewKey] = React.useState(0);

  React.useEffect(() => {
    (async () => {
      try {
        // booking-service: GET /v1/site (tenant-scoped via X-Tenant-Slug)
        const s = await api.get<PublicSite>("/api/bookings/v1/site", {
          tenant: orgSlug,
        });
        setSite(s);
        setForm({
          site_slug: s.site_slug,
          primaryColor: s.theme?.primaryColor ?? s.theme?.accent ?? DEFAULT_COLOR,
          logoUrl: s.theme?.logoUrl ?? s.theme?.logo_url ?? "",
          heroTitle: s.theme?.heroTitle ?? "",
          heroSubtitle: s.theme?.heroSubtitle ?? s.theme?.hero_blurb ?? "",
          template: s.theme?.template ?? "classic",
          tagline: s.tagline ?? "",
          published: s.published,
          brandName: s.theme?.brandName ?? s.theme?.brand_name ?? "",
          customDomain: s.theme?.customDomain ?? "",
        });
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) {
          setNotFound(true);
        } else {
          setError(e instanceof ApiError ? e.message : "Failed to load site config.");
        }
      }
    })();
  }, [orgSlug]);

  const save = async () => {
    setSaving(true);
    try {
      // booking-service: PUT /v1/site — theme jsonb per the Wave-3 contract
      // {primaryColor, logoUrl, heroTitle, heroSubtitle, template}; the legacy
      // snake_case keys are sent along so older readers keep working.
      const updated = await api.put<PublicSite>(
        "/api/bookings/v1/site",
        {
          site_slug: form.site_slug.trim(),
          tagline: form.tagline.trim(),
          published: form.published,
          theme: {
            primaryColor: form.primaryColor,
            logoUrl: form.logoUrl.trim() || undefined,
            heroTitle: form.heroTitle.trim() || undefined,
            heroSubtitle: form.heroSubtitle.trim() || undefined,
            template: form.template,
            accent: form.primaryColor,
            hero_blurb: form.heroSubtitle.trim() || undefined,
            logo_url: form.logoUrl.trim() || undefined,
            // White-label branding (SPEC-W7 Part C) — same theme jsonb.
            brandName: form.brandName.trim() || undefined,
            brand_name: form.brandName.trim() || undefined,
            customDomain: form.customDomain.trim() || undefined,
          },
        },
        { tenant: orgSlug },
      );
      setSite(updated);
      setNotFound(false);
      setPreviewKey((k) => k + 1);
      toast({ title: "Site settings saved", variant: "success" });
    } catch (e) {
      toast({
        title: "Save failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setSaving(false);
    }
  };

  const slugValid = /^[a-z0-9][a-z0-9-]{2,62}$/.test(form.site_slug.trim());
  const embedSnippet = `<script src="${typeof window !== "undefined" ? window.location.origin : ""}/embed.js" data-site="${form.site_slug.trim() || orgSlug}" async></script>`;

  const copySnippet = async () => {
    try {
      await navigator.clipboard.writeText(embedSnippet);
      toast({ title: "Embed snippet copied", variant: "success" });
    } catch {
      toast({
        title: "Copy failed",
        description: "Select the snippet and copy it manually.",
        variant: "destructive",
      });
    }
  };

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Public booking site"
        description="Your customer-facing booking page, chat widget and voice button."
        actions={
          site?.published ? (
            <Link href={`/p/${form.site_slug}`} target="_blank" rel="noreferrer noopener">
              <Button variant="outline" size="sm">
                <ExternalLink className="h-3.5 w-3.5" /> Open /p/{form.site_slug}
              </Button>
            </Link>
          ) : null
        }
      />
      {error ? <ErrorNote message={error} /> : null}
      {notFound ? (
        <ErrorNote message="No site configured yet — saving this form will provision one." />
      ) : null}

      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,26rem)]">
        <div className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Globe className="h-4 w-4" /> Address
              </CardTitle>
              <CardDescription>
                Customers book at <span className="font-mono">/p/{"{slug}"}</span>.
              </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
              <div className="grid gap-1.5">
                <Label htmlFor="site-slug">Site slug</Label>
                <div className="flex items-center gap-2">
                  <span className="text-sm text-muted-foreground">/p/</span>
                  <Input
                    id="site-slug"
                    value={form.site_slug}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, site_slug: e.target.value }))
                    }
                  />
                </div>
                {!slugValid ? (
                  <p className="text-xs text-destructive">
                    3–63 chars, lowercase letters, digits and dashes.
                  </p>
                ) : null}
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="site-tagline">Tagline</Label>
                <Input
                  id="site-tagline"
                  value={form.tagline}
                  onChange={(e) => setForm((f) => ({ ...f, tagline: e.target.value }))}
                  placeholder="Neighbourhood physiotherapy, since 2012"
                />
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Theme</CardTitle>
              <CardDescription>
                Colours, logo and hero copy shown on the booking page and the
                embeddable widget.
              </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
              <div className="grid gap-1.5">
                <Label htmlFor="site-primary">Primary colour</Label>
                <div className="flex items-center gap-3">
                  <input
                    id="site-primary"
                    type="color"
                    value={form.primaryColor}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, primaryColor: e.target.value }))
                    }
                    className="h-9 w-14 cursor-pointer rounded-md border border-input bg-card p-1"
                  />
                  <Input
                    value={form.primaryColor}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, primaryColor: e.target.value }))
                    }
                    className="w-28 font-mono"
                  />
                  <span
                    className="rounded-md px-3 py-1.5 text-sm text-white"
                    style={{ backgroundColor: form.primaryColor }}
                  >
                    Preview
                  </span>
                </div>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="site-logo">Logo URL</Label>
                <Input
                  id="site-logo"
                  type="url"
                  value={form.logoUrl}
                  onChange={(e) => setForm((f) => ({ ...f, logoUrl: e.target.value }))}
                  placeholder="https://example.com/logo.svg"
                />
                {form.logoUrl ? (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img
                    src={form.logoUrl}
                    alt="Logo preview"
                    className="mt-1 h-10 w-auto rounded-md border border-border bg-card object-contain p-1"
                    onError={(e) => (e.currentTarget.style.display = "none")}
                    onLoad={(e) => (e.currentTarget.style.display = "")}
                  />
                ) : null}
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="site-hero-title">Hero title</Label>
                <Input
                  id="site-hero-title"
                  value={form.heroTitle}
                  onChange={(e) =>
                    setForm((f) => ({ ...f, heroTitle: e.target.value }))
                  }
                  placeholder="Book your visit in seconds"
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="site-hero-subtitle">Hero subtitle</Label>
                <Textarea
                  id="site-hero-subtitle"
                  value={form.heroSubtitle}
                  onChange={(e) =>
                    setForm((f) => ({ ...f, heroSubtitle: e.target.value }))
                  }
                  placeholder="A sentence or two shown at the top of your booking page."
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="site-template">Template</Label>
                <Select
                  id="site-template"
                  value={form.template}
                  onChange={(e) =>
                    setForm((f) => ({ ...f, template: e.target.value }))
                  }
                >
                  {TEMPLATES.map((t) => (
                    <option key={t.value} value={t.value}>
                      {t.label}
                    </option>
                  ))}
                </Select>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>White label</CardTitle>
              <CardDescription>
                Branding shown on the public booking page header and footer
                instead of the default tenant name.
              </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
              <div className="grid gap-1.5">
                <Label htmlFor="site-brand-name">Brand display name</Label>
                <Input
                  id="site-brand-name"
                  value={form.brandName}
                  onChange={(e) =>
                    setForm((f) => ({ ...f, brandName: e.target.value }))
                  }
                  placeholder="Acme Wellness"
                />
                <p className="text-xs text-muted-foreground">
                  Overrides the business name in the public header/footer.
                  Combined with the logo and primary colour above this gives a
                  fully white-labelled booking page.
                </p>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="site-custom-domain">Custom domain</Label>
                <Input
                  id="site-custom-domain"
                  value={form.customDomain}
                  onChange={(e) =>
                    setForm((f) => ({ ...f, customDomain: e.target.value }))
                  }
                  placeholder="book.acme-wellness.com"
                />
                <p className="text-xs text-muted-foreground">
                  Note only — point the DNS record at your gateway and map the
                  host to <span className="font-mono">/p/{form.site_slug || orgSlug}</span>{" "}
                  in APISIX (see docs/security/roles.md → white-label setup).
                </p>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Visibility</CardTitle>
            </CardHeader>
            <CardContent className="flex items-center justify-between">
              <div>
                <p className="text-sm font-medium">Published</p>
                <p className="text-xs text-muted-foreground">
                  While unpublished, /p/{form.site_slug || orgSlug} returns 404.
                </p>
              </div>
              <button
                role="switch"
                aria-checked={form.published}
                onClick={() => setForm((f) => ({ ...f, published: !f.published }))}
                className={`relative h-6 w-11 rounded-full transition-colors cursor-pointer ${form.published ? "bg-success" : "bg-input"}`}
              >
                <span
                  className={`absolute top-0.5 h-5 w-5 rounded-full bg-card shadow transition-transform ${form.published ? "translate-x-5.5 left-0.5" : "left-0.5"}`}
                />
              </button>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Code2 className="h-4 w-4" /> Embed on your website
              </CardTitle>
              <CardDescription>
                Paste this snippet into any page to load the booking + chat
                widget in an iframe. See docs/embedding.md for options.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="flex items-start gap-2">
                <pre className="min-w-0 flex-1 overflow-x-auto rounded-md bg-muted p-3 text-xs">
                  <code>{embedSnippet}</code>
                </pre>
                <Button variant="outline" size="sm" onClick={() => void copySnippet()}>
                  <Copy className="h-3.5 w-3.5" /> Copy
                </Button>
              </div>
            </CardContent>
          </Card>

          <div className="flex items-center justify-between">
            <Badge variant={form.published ? "success" : "secondary"}>
              {form.published ? "Live" : "Draft"}
            </Badge>
            <Button onClick={() => void save()} disabled={saving || !slugValid}>
              <Save className="h-4 w-4" />
              {saving ? "Saving…" : "Save site"}
            </Button>
          </div>
        </div>

        {/* Live preview of the published page */}
        <Card className="sticky top-6 h-fit overflow-hidden">
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <div>
              <CardTitle>Live preview</CardTitle>
              <CardDescription>/p/{form.site_slug || orgSlug}</CardDescription>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setPreviewKey((k) => k + 1)}
            >
              <RefreshCw className="h-3.5 w-3.5" /> Reload
            </Button>
          </CardHeader>
          <CardContent>
            {site?.published ? (
              <iframe
                key={previewKey}
                src={`/p/${form.site_slug}`}
                title="Public site preview"
                className="h-[36rem] w-full rounded-md border border-border bg-background"
              />
            ) : (
              <p className="rounded-md border border-dashed border-border p-6 text-center text-sm text-muted-foreground">
                Publish the site to see the live preview here. The preview
                reloads automatically after each save.
              </p>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
