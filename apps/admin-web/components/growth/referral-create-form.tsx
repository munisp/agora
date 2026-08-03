"use client";

/**
 * Create-referral form (SPEC-W14 Agent C, contract §1). Pure presentational
 * form — the parent client owns the POST /v1/referrals call so the list can
 * refresh after a successful create.
 */
import * as React from "react";
import { UserPlus } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";

export interface NewReferral {
  referrer_type: string;
  referrer_id: string;
  referee_phone: string;
  campaign_id?: string;
}

export function ReferralCreateForm({
  busy,
  onCreate,
}: {
  busy: boolean;
  onCreate: (input: NewReferral) => Promise<boolean>;
}) {
  const [referrerType, setReferrerType] = React.useState("contact");
  const [referrerId, setReferrerId] = React.useState("");
  const [refereePhone, setRefereePhone] = React.useState("");
  const [campaignId, setCampaignId] = React.useState("");

  const valid =
    referrerId.trim().length > 0 && refereePhone.trim().length > 0;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || busy) return;
    const ok = await onCreate({
      referrer_type: referrerType,
      referrer_id: referrerId.trim(),
      referee_phone: refereePhone.trim(),
      ...(campaignId.trim() ? { campaign_id: campaignId.trim() } : {}),
    });
    if (ok) {
      setReferrerId("");
      setRefereePhone("");
      setCampaignId("");
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Record a referral</CardTitle>
        <CardDescription>
          Register who referred a new customer. Duplicate open referrals for
          the same referee phone are rejected server-side.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          onSubmit={submit}
          className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5"
        >
          <div className="space-y-1.5">
            <Label htmlFor="ref-type">Referrer type</Label>
            <Select
              id="ref-type"
              value={referrerType}
              onChange={(e) => setReferrerType(e.target.value)}
            >
              <option value="contact">Contact</option>
              <option value="agent">Agent</option>
              <option value="staff">Staff</option>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ref-id">Referrer ID</Label>
            <Input
              id="ref-id"
              value={referrerId}
              onChange={(e) => setReferrerId(e.target.value)}
              placeholder="contact / agent / staff id"
              required
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ref-phone">Referee phone</Label>
            <Input
              id="ref-phone"
              value={refereePhone}
              onChange={(e) => setRefereePhone(e.target.value)}
              placeholder="+234…"
              required
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ref-campaign">Campaign ID (optional)</Label>
            <Input
              id="ref-campaign"
              value={campaignId}
              onChange={(e) => setCampaignId(e.target.value)}
              placeholder="uuid"
            />
          </div>
          <div className="flex items-end">
            <Button type="submit" disabled={!valid || busy} className="w-full">
              <UserPlus className="h-3.5 w-3.5" />
              {busy ? "Saving…" : "Add referral"}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}
