"use client";

/**
 * Command palette (SPEC-W82 B1.1): Ctrl/⌘K (or "/") opens a hand-rolled
 * palette on the existing ui/dialog idiom — no cmdk/Radix dependency.
 *
 *  - Lists every nav destination, INCLUDING the five in-app CRM consoles
 *    that are orphaned from the sidebar nav (apps/crm-360, apps/helpdesk,
 *    apps/loyalty-wallet, apps/surveys-voc, apps/campaign-studio), plus the
 *    external Twenty CRM entry (renamed "Twenty CRM (external)" everywhere
 *    to kill the naming collision with the in-app CRM 360 console).
 *  - Live contact quick-find via the existing
 *    GET /v1/crm/contacts/search?q= endpoint (debounced 250 ms,
 *    AbortController, limit ≤ 20) — Enter opens the contact profile.
 *  - Ranking/filtering is the exported PURE function filterPaletteItems.
 *
 * Role guards stay server-side on each page: the palette only deep-links,
 * exactly like typing the URL, and unauthorized roles are bounced by the
 * pages themselves.
 */
import * as React from "react";
import { useRouter } from "next/navigation";
import { ArrowRight, ContactRound, Search } from "lucide-react";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import { Dialog } from "@/components/ui/dialog";
import { unwrap } from "@/components/apps/types";
import type { ContactSearchResult } from "@/components/apps/crm-360/types";

export interface PaletteItem {
  id: string;
  label: string;
  /** short right-hand hint, e.g. "CRM console" or "External" */
  hint?: string;
  href: string;
  external?: boolean;
  /** extra lowercase match text (not rendered) */
  keywords?: string;
  kind: "nav" | "contact";
}

/**
 * PURE ranking: case-insensitive match against label (prefix > word-prefix
 * > substring) then keywords. Returns a new array, best matches first,
 * stable within the same rank. An empty/blank query returns the input
 * order unchanged.
 */
export function filterPaletteItems(
  items: PaletteItem[],
  query: string,
): PaletteItem[] {
  const q = query.trim().toLowerCase();
  if (!q) return items.slice();
  const ranked: { item: PaletteItem; score: number; index: number }[] = [];
  items.forEach((item, index) => {
    const label = item.label.toLowerCase();
    let score = -1;
    if (label.startsWith(q)) score = 0;
    else if (label.split(/[\s/()/&-]+/).some((w) => w.startsWith(q))) score = 1;
    else if (label.includes(q)) score = 2;
    else if (item.keywords?.toLowerCase().includes(q)) score = 3;
    if (score >= 0) ranked.push({ item, score, index });
  });
  ranked.sort((a, b) => a.score - b.score || a.index - b.index);
  return ranked.map((r) => r.item);
}

/** Sidebar destinations (mirrors components/org-nav.tsx) + CRM consoles. */
const NAV_ENTRIES: { segment: string; label: string; keywords?: string }[] = [
  { segment: "", label: "Overview", keywords: "dashboard home" },
  { segment: "bookings", label: "Bookings", keywords: "appointments" },
  { segment: "waitlist", label: "Waitlist" },
  { segment: "schedule", label: "My Schedule", keywords: "calendar" },
  { segment: "offerings", label: "Offerings", keywords: "services catalog" },
  { segment: "team", label: "Team", keywords: "staff" },
  { segment: "availability", label: "Availability" },
  { segment: "leads", label: "Leads", keywords: "pipeline crm" },
  { segment: "contacts", label: "Contacts", keywords: "directory crm customers" },
  { segment: "knowledge", label: "Knowledge" },
  { segment: "voice-agent", label: "Voice Agent" },
  { segment: "agents", label: "Agents" },
  { segment: "public-site", label: "Public Site" },
  { segment: "billing", label: "Billing", keywords: "invoices payments" },
  { segment: "analytics", label: "Analytics" },
  { segment: "cac", label: "CAC", keywords: "acquisition cost marketing" },
  { segment: "growth", label: "Growth", keywords: "referrals commissions loyalty" },
  { segment: "webhooks", label: "Webhooks" },
  { segment: "channels", label: "Channels", keywords: "whatsapp telegram sms" },
  { segment: "locations", label: "Locations" },
  { segment: "geo-campaigns", label: "Geo campaigns", keywords: "marketing" },
  { segment: "segments", label: "Segments", keywords: "audiences" },
  { segment: "alerts", label: "Alerts", keywords: "fraud trust" },
  { segment: "kyc", label: "KYC review" },
  { segment: "media-inbox", label: "Media inbox" },
  { segment: "bookings/reconciliation", label: "Reconciliation" },
  { segment: "incidents", label: "Incidents", keywords: "iot civic" },
  { segment: "ops-alerts", label: "Ops alerts" },
  { segment: "ops-registry", label: "Registry", keywords: "fraud registry" },
  { segment: "cases", label: "Cases", keywords: "civic triage" },
  { segment: "voices", label: "Voices", keywords: "tts studio" },
  { segment: "apps", label: "Apps", keywords: "catalog" },
  { segment: "settings", label: "Settings" },
];

/** The five in-app CRM consoles that are orphaned from the sidebar nav. */
const CRM_CONSOLES: { segment: string; label: string; keywords?: string }[] = [
  {
    segment: "apps/crm-360",
    label: "CRM 360",
    keywords: "customer profile unified view timeline notes tags",
  },
  {
    segment: "apps/helpdesk",
    label: "Helpdesk",
    keywords: "tickets support sla",
  },
  {
    segment: "apps/loyalty-wallet",
    label: "Loyalty & Wallet",
    keywords: "points tier wallet rewards",
  },
  {
    segment: "apps/surveys-voc",
    label: "Surveys & VoC",
    keywords: "voice of customer feedback nps",
  },
  {
    segment: "apps/campaign-studio",
    label: "Campaign Studio",
    keywords: "journeys marketing segments",
  },
];

const EXTERNAL_ENTRIES: { url: string; label: string; keywords?: string }[] = [
  {
    url: "http://localhost:3100",
    label: "Twenty CRM (external)",
    keywords: "crm twenty self-hosted",
  },
  {
    url: "http://localhost:3002",
    label: "Grafana (external)",
    keywords: "dashboards observability metrics",
  },
];

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  return (
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT" ||
    target.isContentEditable
  );
}

export function CommandPalette({ orgSlug }: { orgSlug: string }) {
  const router = useRouter();
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [activeIndex, setActiveIndex] = React.useState(0);
  const [contacts, setContacts] = React.useState<PaletteItem[]>([]);
  const [searching, setSearching] = React.useState(false);
  const listRef = React.useRef<HTMLDivElement>(null);

  const navItems = React.useMemo<PaletteItem[]>(() => {
    const base = `/app/${orgSlug}`;
    const toItem = (
      e: { segment: string; label: string; keywords?: string },
      hint?: string,
    ): PaletteItem => ({
      id: `nav:${e.segment || "overview"}`,
      label: e.label,
      hint,
      href: e.segment ? `${base}/${e.segment}` : base,
      keywords: e.keywords,
      kind: "nav",
    });
    return [
      ...CRM_CONSOLES.map((e) => toItem(e, "CRM console")),
      ...NAV_ENTRIES.map((e) => toItem(e)),
      ...EXTERNAL_ENTRIES.map(
        (e): PaletteItem => ({
          id: `ext:${e.url}`,
          label: e.label,
          hint: "External",
          href: e.url,
          external: true,
          keywords: e.keywords,
          kind: "nav",
        }),
      ),
    ];
  }, [orgSlug]);

  // Global shortcut: Ctrl/⌘K toggles; "/" opens (unless typing in a field).
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((v) => !v);
        return;
      }
      if (e.key === "/" && !isEditableTarget(e.target)) {
        e.preventDefault();
        setOpen(true);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // Reset transient state whenever the palette opens.
  React.useEffect(() => {
    if (open) {
      setQuery("");
      setActiveIndex(0);
      setContacts([]);
    }
  }, [open]);

  // Debounced (250 ms) contact quick-find with AbortController — the same
  // idiom as the crm-360 search client. Fail-open: errors just yield no
  // contact rows, the nav destinations keep working offline.
  React.useEffect(() => {
    const q = query.trim();
    if (!open || q.length < 2) {
      setContacts([]);
      setSearching(false);
      return;
    }
    const controller = new AbortController();
    setSearching(true);
    const timer = setTimeout(() => {
      api
        .get<unknown>("/api/bookings/v1/crm/contacts/search", {
          tenant: orgSlug,
          q,
          limit: 20,
        }, controller.signal)
        .then((data) => {
          if (controller.signal.aborted) return;
          setContacts(
            unwrap<ContactSearchResult>(data).map(
              (c): PaletteItem => ({
                id: `contact:${c.id}`,
                label: c.name || c.phone || c.email || c.id,
                hint: [c.phone, c.email].filter(Boolean).join(" · ") || "Contact",
                href: `/app/${orgSlug}/apps/crm-360/contacts/${c.id}`,
                keywords: `${c.phone} ${c.email}`,
                kind: "contact",
              }),
            ),
          );
          setSearching(false);
        })
        .catch(() => {
          if (controller.signal.aborted) return;
          setContacts([]);
          setSearching(false);
        });
    }, 250);
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [open, query, orgSlug]);

  const visible = React.useMemo(
    () => [...filterPaletteItems(navItems, query), ...contacts],
    [navItems, query, contacts],
  );

  // Keep the active row inside the visible range as the list changes.
  React.useEffect(() => {
    setActiveIndex((i) => (visible.length === 0 ? 0 : Math.min(i, visible.length - 1)));
  }, [visible.length]);

  React.useEffect(() => {
    setActiveIndex(0);
  }, [query]);

  // Keep the active option scrolled into view.
  React.useEffect(() => {
    const el = listRef.current?.querySelector<HTMLElement>(
      `#cmdk-opt-${activeIndex}`,
    );
    el?.scrollIntoView({ block: "nearest" });
  }, [activeIndex]);

  const activate = React.useCallback(
    (item: PaletteItem | undefined) => {
      if (!item) return;
      setOpen(false);
      if (item.external) {
        window.open(item.href, "_blank", "noreferrer,noopener");
      } else {
        router.push(item.href);
      }
    },
    [router],
  );

  const onInputKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActiveIndex((i) => (visible.length ? (i + 1) % visible.length : 0));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveIndex((i) =>
        visible.length ? (i - 1 + visible.length) % visible.length : 0,
      );
    } else if (e.key === "Enter") {
      e.preventDefault();
      activate(visible[activeIndex]);
    }
  };

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Command palette"
        className="relative z-10 w-full max-w-lg overflow-hidden rounded-lg border border-border bg-card shadow-lg"
      >
        <div className="flex items-center gap-2 border-b border-border px-4">
          <Search className="h-4 w-4 shrink-0 text-muted-foreground" />
          <input
            autoFocus
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onInputKeyDown}
            role="combobox"
            aria-expanded="true"
            aria-controls="cmdk-list"
            aria-activedescendant={
              visible.length ? `cmdk-opt-${activeIndex}` : undefined
            }
            aria-label="Search pages and contacts"
            placeholder="Search pages, CRM consoles, contacts…"
            className="h-12 w-full bg-transparent text-sm outline-none placeholder:text-muted-foreground"
          />
          <kbd className="shrink-0 rounded border border-border bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
            Esc
          </kbd>
        </div>

        <div
          ref={listRef}
          id="cmdk-list"
          role="listbox"
          aria-label="Results"
          className="max-h-80 overflow-y-auto p-2"
        >
          {visible.length === 0 ? (
            <p className="px-3 py-6 text-center text-sm text-muted-foreground">
              {searching
                ? "Searching contacts…"
                : "No matches — try a page name, console, or contact."}
            </p>
          ) : (
            visible.map((item, i) => (
              <button
                key={item.id}
                id={`cmdk-opt-${i}`}
                type="button"
                role="option"
                aria-selected={i === activeIndex}
                onMouseEnter={() => setActiveIndex(i)}
                onClick={() => activate(item)}
                className={cn(
                  "flex w-full items-center gap-3 rounded-md px-3 py-2 text-left text-sm cursor-pointer",
                  i === activeIndex
                    ? "bg-accent text-foreground"
                    : "text-foreground",
                )}
              >
                {item.kind === "contact" ? (
                  <ContactRound className="h-4 w-4 shrink-0 text-muted-foreground" />
                ) : (
                  <ArrowRight className="h-4 w-4 shrink-0 text-muted-foreground" />
                )}
                <span className="min-w-0 flex-1 truncate">{item.label}</span>
                {item.hint ? (
                  <span className="shrink-0 truncate text-xs text-muted-foreground">
                    {item.hint}
                  </span>
                ) : null}
              </button>
            ))
          )}
        </div>

        <div className="flex items-center gap-3 border-t border-border px-4 py-2 text-xs text-muted-foreground">
          <span>↑↓ navigate</span>
          <span>Enter open</span>
          <span>Esc close</span>
          <span className="ml-auto">Ctrl/⌘K</span>
        </div>
      </div>
    </Dialog>
  );
}
