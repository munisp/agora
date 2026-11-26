"use client";

import * as React from "react";
import { Eraser, FileDown } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Input, Label } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";
import type { PrivacyWorkflowStarted } from "@/lib/types";

interface SubmittedRequest {
  kind: string;
  workflow_id: string;
  status: string;
  subject: string;
  at: string;
}

/**
 * SPEC-W47 UIWEB-17: privacy export/erase console.
 *   POST /v1/privacy/export { phone | email } → 202 { status, kind,
 *     workflow_id } — gathers the subject's data across services into a JSON
 *     bundle in the MinIO `exports` bucket.
 *   POST /v1/privacy/erase  { phone | email } → 202 — publishes the
 *     PrivacyEraseRequested tombstone consumed by booking/conversation/
 *     crm-sync to anonymize the subject's PII.
 * Both start a Temporal workflow; the response's workflow_id is the tracking
 * handle (there is no status read-back endpoint — status is "started" and
 * completion lands as the export object / tombstone events).
 */
export function PrivacyClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [phone, setPhone] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [confirmErase, setConfirmErase] = React.useState(false);
  const [requests, setRequests] = React.useState<SubmittedRequest[]>([]);

  const subject = phone.trim() || email.trim();
  const valid = subject !== "";

  const start = async (kind: "export" | "erase") => {
    setBusy(true);
    try {
      const res = await api.post<PrivacyWorkflowStarted>(
        `/api/bookings/v1/privacy/${kind}`,
        {
          phone: phone.trim() || undefined,
          email: email.trim() || undefined,
        },
        { tenant: orgSlug },
      );
      setRequests((r) => [
        {
          kind: res.kind,
          workflow_id: res.workflow_id,
          status: res.status,
          subject,
          at: new Date().toISOString(),
        },
        ...r,
      ]);
      toast({
        title: kind === "export" ? "Export started" : "Erasure started",
        description:
          kind === "export"
            ? `Workflow ${res.workflow_id}. The bundle lands in the exports bucket.`
            : `Workflow ${res.workflow_id}. PII is anonymized across services.`,
        variant: "success",
      });
      setConfirmErase(false);
    } catch (e) {
      toast({
        title: kind === "export" ? "Export failed" : "Erasure failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="max-w-2xl">
      <PageHeader
        title="Privacy"
        description="NDPA data-subject requests: export a person's data or erase (anonymize) it across all services. Requests run as tracked workflows and are irreversible once completed."
      />

      <Card>
        <CardHeader>
          <CardTitle>Data subject</CardTitle>
          <CardDescription>
            Identify the subject by phone and/or email — at least one is
            required.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="privacy-phone">Phone</Label>
            <Input
              id="privacy-phone"
              type="tel"
              placeholder="+234…"
              value={phone}
              onChange={(e) => setPhone(e.target.value)}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="privacy-email">Email</Label>
            <Input
              id="privacy-email"
              type="email"
              placeholder="person@example.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </div>
          <div className="flex flex-wrap gap-2 pt-2">
            <Button
              variant="secondary"
              disabled={busy || !valid}
              onClick={() => void start("export")}
            >
              <FileDown className="h-4 w-4" />
              Export data
            </Button>
            <Button
              variant="destructive"
              disabled={busy || !valid}
              onClick={() => setConfirmErase(true)}
            >
              <Eraser className="h-4 w-4" />
              Erase data…
            </Button>
          </div>
        </CardContent>
      </Card>

      {requests.length > 0 ? (
        <Card className="mt-6">
          <CardHeader>
            <CardTitle>Requests this session</CardTitle>
            <CardDescription>
              Workflow handles returned by the service. Exports land as JSON
              bundles in the exports bucket; erasures complete when every
              service has applied the tombstone.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <ul className="space-y-2 text-sm">
              {requests.map((r) => (
                <li
                  key={r.workflow_id}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border px-3 py-2"
                >
                  <div className="min-w-0">
                    <p className="text-sm font-medium">
                      {r.kind === "erase" ? "Erasure" : "Export"} · {r.subject}
                    </p>
                    <p className="truncate font-mono text-xs text-muted-foreground">
                      {r.workflow_id}
                    </p>
                  </div>
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">
                      {formatDateTime(r.at)}
                    </span>
                    <Badge variant="warning">{r.status}</Badge>
                  </div>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      ) : null}

      <ConfirmDialog
        open={confirmErase}
        onOpenChange={(open) => !open && setConfirmErase(false)}
        title="Erase this person's data?"
        description={`${subject} will be anonymized across bookings, conversations and CRM records. This is a data-protection request and cannot be undone.`}
        confirmLabel="Erase data"
        destructive
        busy={busy}
        onConfirm={() => void start("erase")}
      />
    </div>
  );
}
