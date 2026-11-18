"use client";

/**
 * CRM-360 contact profile client (SPEC-W20 Agent A; SPEC-W82 B1.3/B1.4):
 * 360 aggregation, merged timeline, notes editor and tag chips for one
 * contact, all through the BFF with the x-tenant-slug header:
 *   - GET   /api/bookings/v1/crm/contacts/{id}/360
 *   - GET   /api/bookings/v1/crm/contacts/{id}/timeline?limit=
 *   - GET   /api/bookings/v1/crm/contacts/{id}/notes
 *   - POST  /api/bookings/v1/crm/contacts/{id}/notes
 *   - PATCH /api/bookings/v1/crm/notes/{noteId}
 *   - POST  /api/bookings/v1/crm/contacts/{id}/tags
 *   - DELETE /api/bookings/v1/crm/contacts/{id}/tags/{tag}
 *
 * W82 additions:
 *  - B1.4: 360 + timeline + notes arrive server-rendered as initialData;
 *    the mount effect below is now a background refresh, not a waterfall.
 *  - B1.3: successful loads update a per-browser localStorage last-good
 *    bundle keyed by tenant + contact id; on network failure the cached
 *    bundle renders with a "Cached as of HH:MM — reconnecting…" warning
 *    Badge, and the browser `online` event re-fetches.
 */
import * as React from "react";
import Link from "next/link";
import { ArrowLeft, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { unwrap } from "@/components/apps/types";
import {
  cacheKey,
  formatCachedAt,
  readCache,
  writeCache,
} from "@/lib/local-cache";
import { ProfileSections } from "@/components/apps/crm-360/profile-sections";
import { TimelineFeed } from "@/components/apps/crm-360/timeline-feed";
import { NotesEditor } from "@/components/apps/crm-360/notes-editor";
import { TagChips } from "@/components/apps/crm-360/tag-chips";
import type {
  CrmNote,
  Profile360,
  TimelineItem,
} from "@/components/apps/crm-360/types";

type Tab = "profile" | "timeline" | "notes";

/** B1.3 last-good bundle cached per tenant + contact. */
interface ProfileBundle {
  profile?: Profile360 | null;
  timeline?: TimelineItem[];
  notes?: CrmNote[];
}

export function ContactProfileClient({
  orgSlug,
  contactId,
  canWork,
  initialProfile,
  initialTimeline,
  initialNotes,
  initialError,
}: {
  orgSlug: string;
  contactId: string;
  /** owner/admin/staff — may write notes/tags */
  canWork: boolean;
  /** SPEC-W82 B1.4: parallel server fetch (null when it failed). */
  initialProfile: Profile360 | null;
  initialTimeline: TimelineItem[];
  initialNotes: CrmNote[];
  initialError: string | null;
}) {
  const [tab, setTab] = React.useState<Tab>("profile");
  const [profile, setProfile] = React.useState<Profile360 | null>(initialProfile);
  const [timeline, setTimeline] = React.useState<TimelineItem[]>(initialTimeline);
  const [notes, setNotes] = React.useState<CrmNote[]>(initialNotes);
  const [loading, setLoading] = React.useState(
    initialProfile === null && initialError === null,
  );
  const [timelineLoading, setTimelineLoading] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(initialError);
  const [notFound, setNotFound] = React.useState(false);
  /** savedAt of the cached bundle being shown, if any (B1.3). */
  const [cachedAt, setCachedAt] = React.useState<number | null>(null);

  const base = `/api/bookings/v1/crm/contacts/${contactId}`;
  const bundleKey = cacheKey(orgSlug, "crm-profile", contactId);

  /** B1.3: merge one slice into the last-good bundle for this contact. */
  const mergeCache = React.useCallback(
    (patch: ProfileBundle) => {
      const prev = readCache<ProfileBundle>(bundleKey)?.data ?? {};
      writeCache<ProfileBundle>(bundleKey, { ...prev, ...patch });
    },
    [bundleKey],
  );

  /** B1.3: fall back to the cached bundle after a network failure. */
  const restoreCache = React.useCallback((): boolean => {
    const cached = readCache<ProfileBundle>(bundleKey);
    if (!cached || !cached.data.profile) return false;
    setProfile(cached.data.profile);
    if (cached.data.timeline) setTimeline(cached.data.timeline);
    if (cached.data.notes) setNotes(cached.data.notes);
    setCachedAt(cached.savedAt);
    return true;
  }, [bundleKey]);

  const loadProfile = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<{ profile: Profile360 }>(`${base}/360`, {
          tenant: orgSlug,
        });
        if (signal?.aborted) return;
        setProfile(data.profile ?? null);
        setCachedAt(null);
        mergeCache({ profile: data.profile ?? null });
      } catch (e) {
        if (signal?.aborted) return;
        if (e instanceof ApiError && e.status === 404) {
          setNotFound(true);
        } else {
          // B1.3: show the last-good bundle (flagged stale) if we have one.
          restoreCache();
          setError(e instanceof Error ? e.message : "Failed to load the profile.");
        }
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [base, orgSlug, mergeCache, restoreCache],
  );

  const loadTimeline = React.useCallback(
    async (signal?: AbortSignal) => {
      setTimelineLoading(true);
      try {
        const data = await api.get<unknown>(`${base}/timeline`, {
          tenant: orgSlug,
          limit: 100,
        });
        if (!signal?.aborted) {
          const items = unwrap<TimelineItem>(data);
          setTimeline(items);
          mergeCache({ timeline: items });
        }
      } catch {
        // Timeline is additive context — degrade to empty, the profile
        // sections still carry the page.
        if (!signal?.aborted) setTimeline([]);
      } finally {
        if (!signal?.aborted) setTimelineLoading(false);
      }
    },
    [base, orgSlug, mergeCache],
  );

  const loadNotes = React.useCallback(
    async (signal?: AbortSignal) => {
      try {
        const data = await api.get<unknown>(`${base}/notes`, { tenant: orgSlug });
        if (!signal?.aborted) {
          const rows = unwrap<CrmNote>(data);
          setNotes(rows);
          mergeCache({ notes: rows });
        }
      } catch {
        if (!signal?.aborted) setNotes([]);
      }
    },
    [base, orgSlug, mergeCache],
  );

  // B1.4: initial data is server-rendered — this mount pass is a
  // background refresh so the page never shows stale data for long.
  React.useEffect(() => {
    const controller = new AbortController();
    void loadProfile(controller.signal);
    void loadTimeline(controller.signal);
    void loadNotes(controller.signal);
    return () => controller.abort();
  }, [loadProfile, loadTimeline, loadNotes]);

  const refresh = React.useCallback(() => {
    void loadProfile();
    void loadTimeline();
    void loadNotes();
  }, [loadProfile, loadTimeline, loadNotes]);

  // B1.3: reconnect → refresh automatically.
  React.useEffect(() => {
    const onOnline = () => refresh();
    window.addEventListener("online", onOnline);
    return () => window.removeEventListener("online", onOnline);
  }, [refresh]);

  // --- tag mutations (response carries the full tag set) ---
  const mutateTags = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try {
      const data = await fn();
      const tags = (data as { tags?: string[] })?.tags;
      if (Array.isArray(tags)) {
        setProfile((p) => (p ? { ...p, tags } : p));
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Tag change failed.");
    } finally {
      setBusy(false);
    }
  };

  const addTag = (tag: string) =>
    mutateTags(() =>
      api.post(`${base}/tags`, { tag }, { tenant: orgSlug }),
    );
  const removeTag = (tag: string) =>
    mutateTags(() =>
      api.delete(`${base}/tags/${encodeURIComponent(tag)}`, { tenant: orgSlug }),
    );

  // --- note mutations ---
  const createNote = async (body: string, pinned: boolean) => {
    setBusy(true);
    setError(null);
    try {
      await api.post(`${base}/notes`, { body, pinned }, { tenant: orgSlug });
      await loadNotes();
      void loadTimeline(); // the new note joins the merged feed
    } catch (e) {
      setError(e instanceof Error ? e.message : "Adding the note failed.");
    } finally {
      setBusy(false);
    }
  };

  const updateNote = async (id: string, patch: { body?: string; pinned?: boolean }) => {
    setBusy(true);
    setError(null);
    try {
      await api.patch(`/api/bookings/v1/crm/notes/${id}`, patch, { tenant: orgSlug });
      await loadNotes();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Updating the note failed.");
    } finally {
      setBusy(false);
    }
  };

  if (notFound) {
    return (
      <div>
        <PageHeader title="Contact not found" />
        <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
          This contact does not exist in the current organisation (or the id
          is wrong).{" "}
          <Link href={`/app/${orgSlug}/apps/crm-360`} className="text-primary underline">
            Back to search
          </Link>
        </p>
      </div>
    );
  }

  const c = profile?.contact;
  const subtitle = c
    ? [c.phone, c.email].filter(Boolean).join(" · ") || "No phone or email on record"
    : "Loading…";

  return (
    <div>
      <PageHeader
        title={c ? c.name : "Contact 360"}
        description={subtitle}
        actions={
          <div className="flex items-center gap-2">
            <Link href={`/app/${orgSlug}/apps/crm-360`}>
              <Button variant="outline" size="sm">
                <ArrowLeft className="mr-1 h-4 w-4" /> Search
              </Button>
            </Link>
            <Button variant="outline" size="sm" onClick={refresh} disabled={loading}>
              <RefreshCw className={`mr-1 h-4 w-4 ${loading ? "animate-spin" : ""}`} />
              Refresh
            </Button>
          </div>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

      {cachedAt !== null ? (
        <div className="mb-3">
          <Badge variant="warning">
            Cached as of {formatCachedAt(cachedAt)} — reconnecting…
          </Badge>
        </div>
      ) : null}

      {/* Tags always visible above the tabs (chips idiom). */}
      <div className="mb-4 rounded-md border border-border bg-card px-4 py-3">
        <TagChips
          tags={profile?.tags ?? []}
          canWork={canWork}
          busy={busy}
          onAdd={addTag}
          onRemove={removeTag}
        />
      </div>

      {/* Tab switch (same segmented-control idiom as the W18/W19 pages). */}
      <div className="mb-4 inline-flex rounded-md border border-border bg-muted/40 p-0.5">
        {(
          [
            ["profile", "360 profile"],
            ["timeline", "Timeline"],
            ["notes", `Notes${notes.length ? ` (${notes.length})` : ""}`],
          ] as [Tab, string][]
        ).map(([key, label]) => (
          <button
            key={key}
            type="button"
            onClick={() => setTab(key)}
            className={`rounded px-3 py-1.5 text-sm font-medium transition-colors ${
              tab === key
                ? "bg-card text-foreground shadow-sm"
                : "text-muted-foreground hover:text-foreground"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "profile" ? (
        loading && !profile ? (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            {Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className="h-28 animate-pulse rounded-lg border border-border bg-muted" />
            ))}
          </div>
        ) : profile ? (
          <ProfileSections profile={profile} />
        ) : null
      ) : tab === "timeline" ? (
        <TimelineFeed items={timeline} loading={timelineLoading} />
      ) : (
        <NotesEditor
          notes={notes}
          canWork={canWork}
          busy={busy}
          onCreate={createNote}
          onUpdate={updateNote}
        />
      )}
    </div>
  );
}
