"use client";

/**
 * Surveys / VoC app client (SPEC-W20 Agent B): survey list, editor
 * (create/edit), send-invites dialog, and the results dashboard with VoC
 * themes.
 *
 * Data sources (all through the BFF with the x-tenant-slug header):
 *   - GET   /api/bookings/v1/surveys/surveys
 *   - POST  /api/bookings/v1/surveys/surveys
 *   - GET   /api/bookings/v1/surveys/surveys/{id}        (survey + stats)
 *   - PATCH /api/bookings/v1/surveys/surveys/{id}
 *   - POST  /api/bookings/v1/surveys/surveys/{id}/send
 *   - GET   /api/bookings/v1/surveys/surveys/{id}/results
 *   - GET   /api/bookings/v1/surveys/voc/themes?survey_id=
 *
 * The PUBLIC customer-facing submit path (POST /v1/surveys/respond) is
 * deliberately NOT called from this admin UI — it is token-gated, not
 * tenant-gated (see docs/apps/surveys-voc.md).
 */
import * as React from "react";
import { Plus, RefreshCw, Send } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { unwrap } from "@/components/apps/types";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { useToast } from "@/components/ui/toast";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { SurveyEditor, type SurveyInput } from "@/components/apps/surveys-voc/survey-editor";
import { SendDialog } from "@/components/apps/surveys-voc/send-dialog";
import { ResultsDashboard } from "@/components/apps/surveys-voc/results-dashboard";
import {
  NEXT_STATUS,
  STATUS_LABEL,
  statusVariant,
  shortId,
  type SendResult,
  type Survey,
  type SurveyResults,
  type SurveyStats,
  type SurveyStatus,
  type Theme,
} from "@/components/apps/surveys-voc/types";

const BASE = "/api/bookings/v1/surveys";

const ROLLOUT_NOTE =
  "Surveys is not available yet — the booking-service surveys API may still be rolling out.";

export function SurveysVocClient({
  orgSlug,
  canWrite,
}: {
  orgSlug: string;
  /** manage_bookings (owner/admin/staff) — enables every write control */
  canWrite: boolean;
}) {
  const { toast } = useToast();

  const [surveys, setSurveys] = React.useState<Survey[]>([]);
  const [listLoading, setListLoading] = React.useState(true);
  const [listError, setListError] = React.useState<string | null>(null);

  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  const [detail, setDetail] = React.useState<Survey | null>(null);
  const [stats, setStats] = React.useState<SurveyStats | null>(null);
  const [detailLoading, setDetailLoading] = React.useState(false);
  const [detailError, setDetailError] = React.useState<string | null>(null);

  const [results, setResults] = React.useState<SurveyResults | null>(null);
  const [themes, setThemes] = React.useState<Theme[]>([]);
  const [resultsLoading, setResultsLoading] = React.useState(false);
  const [resultsError, setResultsError] = React.useState<string | null>(null);

  const [creating, setCreating] = React.useState(false);
  const [editing, setEditing] = React.useState(false);
  const [sendOpen, setSendOpen] = React.useState(false);
  const [sendResult, setSendResult] = React.useState<SendResult | null>(null);
  const [busy, setBusy] = React.useState(false);

  const loadList = React.useCallback(
    async (signal?: AbortSignal) => {
      setListLoading(true);
      setListError(null);
      try {
        const data = await api.get<unknown>(`${BASE}/surveys`, { tenant: orgSlug }, signal);
        if (signal?.aborted) return;
        setSurveys(unwrap<Survey>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setSurveys([]);
        setListError(e instanceof ApiError && e.status !== 404 ? e.message : ROLLOUT_NOTE);
      } finally {
        if (!signal?.aborted) setListLoading(false);
      }
    },
    [orgSlug],
  );

  const loadDetail = React.useCallback(
    async (id: string, signal?: AbortSignal) => {
      setDetailLoading(true);
      setDetailError(null);
      try {
        const data = await api.get<{ survey?: Survey; stats?: SurveyStats }>(
          `${BASE}/surveys/${id}`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setDetail(data?.survey ?? null);
        setStats(data?.stats ?? null);
      } catch (e) {
        if (signal?.aborted) return;
        setDetail(null);
        setStats(null);
        setDetailError(e instanceof ApiError ? e.message : "Failed to load the survey.");
      } finally {
        if (!signal?.aborted) setDetailLoading(false);
      }
    },
    [orgSlug],
  );

  const loadResults = React.useCallback(
    async (id: string, signal?: AbortSignal) => {
      setResultsLoading(true);
      setResultsError(null);
      try {
        const [resData, themeData] = await Promise.all([
          api.get<{ results?: SurveyResults }>(`${BASE}/surveys/${id}/results`, { tenant: orgSlug }, signal),
          api.get<{ themes?: Theme[] }>(`${BASE}/voc/themes`, { tenant: orgSlug, survey_id: id }, signal),
        ]);
        if (signal?.aborted) return;
        setResults(resData?.results ?? null);
        setThemes(themeData?.themes ?? []);
      } catch (e) {
        if (signal?.aborted) return;
        setResults(null);
        setThemes([]);
        setResultsError(e instanceof ApiError ? e.message : "Failed to load results.");
      } finally {
        if (!signal?.aborted) setResultsLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void loadList(controller.signal);
    return () => controller.abort();
  }, [loadList]);

  React.useEffect(() => {
    if (!selectedId) return;
    const controller = new AbortController();
    void loadDetail(selectedId, controller.signal);
    void loadResults(selectedId, controller.signal);
    return () => controller.abort();
  }, [selectedId, loadDetail, loadResults]);

  const select = (id: string) => {
    setSelectedId(id);
    setEditing(false);
    setCreating(false);
    setSendResult(null);
  };

  const refreshAll = async () => {
    await loadList();
    if (selectedId) {
      await Promise.all([loadDetail(selectedId), loadResults(selectedId)]);
    }
  };

  const createSurvey = async (input: SurveyInput): Promise<boolean> => {
    setBusy(true);
    try {
      const data = await api.post<{ survey?: Survey }>(`${BASE}/surveys`, input, { tenant: orgSlug });
      toast({ title: "Survey created (draft)" });
      await loadList();
      setCreating(false);
      if (data?.survey?.id) select(data.survey.id);
      return true;
    } catch (e) {
      toast({
        title: "Create failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const saveSurvey = async (input: SurveyInput): Promise<boolean> => {
    if (!detail) return false;
    setBusy(true);
    try {
      await api.patch(`${BASE}/surveys/${detail.id}`, input, { tenant: orgSlug });
      toast({ title: "Survey saved" });
      setEditing(false);
      await Promise.all([loadList(), loadDetail(detail.id)]);
      return true;
    } catch (e) {
      toast({
        title: "Save failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const transition = async (status: SurveyStatus) => {
    if (!detail) return;
    setBusy(true);
    try {
      await api.patch(`${BASE}/surveys/${detail.id}`, { status }, { tenant: orgSlug });
      toast({ title: `Survey ${STATUS_LABEL[status].toLowerCase()}` });
      await Promise.all([loadList(), loadDetail(detail.id)]);
    } catch (e) {
      toast({
        title: "Status change failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const sendInvites = async (contactIds: string[]) => {
    if (!detail) return;
    setBusy(true);
    try {
      const data = await api.post<SendResult>(
        `${BASE}/surveys/${detail.id}/send`,
        { contact_ids: contactIds },
        { tenant: orgSlug },
      );
      setSendResult(data);
      toast({ title: `Invites created: ${data?.invites_created ?? 0} (${data?.sent ?? 0} sent)` });
      await Promise.all([loadDetail(detail.id), loadList()]);
    } catch (e) {
      toast({
        title: "Send failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Surveys & VoC"
        description="NPS / CSAT / CES surveys — build, send, and read the voice of the customer."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refreshAll()}>
              <RefreshCw className="mr-1 h-4 w-4" /> Refresh
            </Button>
            {canWrite ? (
              <Button
                size="sm"
                onClick={() => {
                  setCreating(true);
                  setEditing(false);
                }}
              >
                <Plus className="mr-1 h-4 w-4" /> New survey
              </Button>
            ) : null}
          </>
        }
      />

      {creating ? (
        <SurveyEditor busy={busy} onSave={createSurvey} onCancel={() => setCreating(false)} />
      ) : null}

      <div className="grid gap-6 lg:grid-cols-[20rem_1fr]">
        <Card className="self-start">
          <CardHeader>
            <CardTitle className="text-base">Surveys</CardTitle>
            <CardDescription>{surveys.length} total</CardDescription>
          </CardHeader>
          <CardContent>
            {listLoading ? (
              <p className="text-sm text-muted-foreground">Loading surveys…</p>
            ) : listError ? (
              <ErrorNote message={listError} />
            ) : surveys.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No surveys yet{canWrite ? " — create your first NPS or CSAT survey." : "."}
              </p>
            ) : (
              <ul className="space-y-1.5">
                {surveys.map((sv) => (
                  <li key={sv.id}>
                    <button
                      type="button"
                      onClick={() => select(sv.id)}
                      className={`w-full rounded-md border px-3 py-2 text-left text-sm transition-colors ${
                        sv.id === selectedId
                          ? "border-primary bg-primary/5"
                          : "border-border hover:bg-muted"
                      }`}
                    >
                      <span className="flex items-center justify-between gap-2">
                        <span className="truncate font-medium">{sv.name}</span>
                        <Badge variant={statusVariant(sv.status)}>
                          {STATUS_LABEL[sv.status]}
                        </Badge>
                      </span>
                      <span className="mt-0.5 block text-xs text-muted-foreground">
                        {sv.kind.toUpperCase()} · {sv.questions.length}q · {shortId(sv.id)}
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </CardContent>
        </Card>

        <div className="space-y-4">
          {!selectedId ? (
            <Card>
              <CardContent className="py-10 text-center text-sm text-muted-foreground">
                Select a survey to see results, edit questions, or send invites.
              </CardContent>
            </Card>
          ) : detailLoading && !detail ? (
            <Card>
              <CardContent className="py-10 text-center text-sm text-muted-foreground">
                Loading survey…
              </CardContent>
            </Card>
          ) : detailError ? (
            <ErrorNote message={detailError} />
          ) : detail ? (
            <>
              <Card>
                <CardHeader className="flex-row items-start justify-between gap-3 space-y-0">
                  <div>
                    <CardTitle className="text-lg">{detail.name}</CardTitle>
                    <CardDescription>
                      {detail.kind.toUpperCase()} · {detail.questions.length} questions ·
                      invites via {detail.channel === "sms" ? "SMS" : "marketing push"}
                      {detail.trigger_kind !== "manual"
                        ? ` · trigger ${detail.trigger_kind.replace("_", " ")} (automation coming)`
                        : ""}
                    </CardDescription>
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant={statusVariant(detail.status)}>
                      {STATUS_LABEL[detail.status]}
                    </Badge>
                    {canWrite
                      ? NEXT_STATUS[detail.status].map((st) => (
                          <Button
                            key={st}
                            variant="outline"
                            size="sm"
                            disabled={busy}
                            onClick={() => void transition(st)}
                          >
                            {st === "active" && detail.status === "paused" ? "Resume" : STATUS_LABEL[st]}
                          </Button>
                        ))
                      : null}
                    {canWrite && detail.status === "active" ? (
                      <Button size="sm" disabled={busy} onClick={() => setSendOpen(true)}>
                        <Send className="mr-1 h-4 w-4" /> Send invites
                      </Button>
                    ) : null}
                  </div>
                </CardHeader>
                {stats ? (
                  <CardContent className="grid grid-cols-2 gap-2 text-sm sm:grid-cols-5">
                    <span>Queued: <strong>{stats.invites_queued}</strong></span>
                    <span>Sent: <strong>{stats.invites_sent}</strong></span>
                    <span>Answered: <strong>{stats.invites_answered}</strong></span>
                    <span>Expired: <strong>{stats.invites_expired}</strong></span>
                    <span>Responses: <strong>{stats.responses}</strong></span>
                  </CardContent>
                ) : null}
              </Card>

              <Tabs defaultValue="results">
                <TabsList>
                  <TabsTrigger value="results">Results & themes</TabsTrigger>
                  {canWrite && detail.status !== "archived" ? (
                    <TabsTrigger value="edit">Edit</TabsTrigger>
                  ) : null}
                </TabsList>
                <TabsContent value="results" className="pt-4">
                  {resultsLoading && !results ? (
                    <p className="text-sm text-muted-foreground">Loading results…</p>
                  ) : resultsError ? (
                    <ErrorNote message={resultsError} />
                  ) : results ? (
                    <ResultsDashboard
                      survey={detail}
                      results={results}
                      themes={themes}
                    />
                  ) : null}
                </TabsContent>
                <TabsContent value="edit" className="pt-4">
                  {editing ? (
                    <SurveyEditor
                      survey={detail}
                      busy={busy}
                      onSave={saveSurvey}
                      onCancel={() => setEditing(false)}
                    />
                  ) : (
                    <Card>
                      <CardContent className="space-y-3 py-6">
                        <ul className="space-y-1 text-sm">
                          {detail.questions.map((q) => (
                            <li key={q.id}>
                              <span className="font-medium">{q.label}</span>{" "}
                              <span className="text-xs text-muted-foreground">
                                [{q.type}
                                {q.options?.length ? `: ${q.options.join(" / ")}` : ""}
                                {q.required ? ", required" : ""}]
                              </span>
                            </li>
                          ))}
                        </ul>
                        <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
                          Edit questions
                        </Button>
                      </CardContent>
                    </Card>
                  )}
                </TabsContent>
              </Tabs>
            </>
          ) : null}
        </div>
      </div>

      <SendDialog
        survey={detail}
        open={sendOpen}
        busy={busy}
        result={sendResult}
        onOpenChange={setSendOpen}
        onSend={sendInvites}
      />
    </div>
  );
}
