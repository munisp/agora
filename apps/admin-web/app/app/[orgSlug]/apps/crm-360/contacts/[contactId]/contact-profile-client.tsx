"use client";

/**
 * CRM-360 contact profile client (SPEC-W20 Agent A): 360 aggregation,
 * merged timeline, notes editor and tag chips for one contact, all
 * through the BFF with the x-tenant-slug header:
 *   - GET   /api/bookings/v1/crm/contacts/{id}/360
 *   - GET   /api/bookings/v1/crm/contacts/{id}/timeline?limit=
 *   - GET   /api/bookings/v1/crm/contacts/{id}/notes
 *   - POST  /api/bookings/v1/crm/contacts/{id}/notes
 *   - PATCH /api/bookings/v1/crm/notes/{noteId}
 *   - POST  /api/bookings/v1/crm/contacts/{id}/tags
 *   - DELETE /api/bookings/v1/crm/contacts/{id}/tags/{tag}
 */
import * as React from "react";
import Link from "next/link";
import { ArrowLeft, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { unwrap } from "@/components/apps/types";
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

export function ContactProfileClient({
  orgSlug,
  contactId,
  canWork,
}: {
  orgSlug: string;
  contactId: string;
  /** owner/admin/staff — may write notes/tags */
  canWork: boolean;
}) {
  const [tab, setTab] = React.useState<Tab>("profile");
  const [profile, setProfile] = React.useState<Profile360 | null>(null);
  const [timeline, setTimeline] = React.useState<TimelineItem[]>([]);
  const [notes, setNotes] = React.useState<CrmNote[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [timelineLoading, setTimelineLoading] = React.useState(true);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [notFound, setNotFound] = React.useState(false);

  const base = `/api/bookings/v1/crm/contacts/${contactId}`;

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
      } catch (e) {
        if (signal?.aborted) return;
        if (e instanceof ApiError && e.status === 404) {
          setNotFound(true);
        } else {
          setError(e instanceof Error ? e.message : "Failed to load the profile.");
        }
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [base, orgSlug],
  );

  const loadTimeline = React.useCallback(
    async (signal?: AbortSignal) => {
      setTimelineLoading(true);
      try {
        const data = await api.get<unknown>(`${base}/timeline`, {
          tenant: orgSlug,
          limit: 100,
        });
        if (!signal?.aborted) setTimeline(unwrap<TimelineItem>(data));
      } catch {
        // Timeline is additive context — degrade to empty, the profile
        // sections still carry the page.
        if (!signal?.aborted) setTimeline([]);
      } finally {
        if (!signal?.aborted) setTimelineLoading(false);
      }
    },
    [base, orgSlug],
  );

  const loadNotes = React.useCallback(
    async (signal?: AbortSignal) => {
      try {
        const data = await api.get<unknown>(`${base}/notes`, { tenant: orgSlug });
        if (!signal?.aborted) setNotes(unwrap<CrmNote>(data));
      } catch {
        if (!signal?.aborted) setNotes([]);
      }
    },
    [base, orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void loadProfile(controller.signal);
    void loadTimeline(controller.signal);
    void loadNotes(controller.signal);
    return () => controller.abort();
  }, [loadProfile, loadTimeline, loadNotes]);

  const refresh = () => {
    void loadProfile();
    void loadTimeline();
    void loadNotes();
  };

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
