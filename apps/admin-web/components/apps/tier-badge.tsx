import { Badge } from "@/components/ui/badge";
import { titleCase } from "@/lib/utils";
import type { AppPlanTier } from "@/lib/types";

/**
 * Plan-tier badge for a catalog app (SPEC-W18 Agent B, contract §3 field
 * default_plan_tier). The contract names the tiers starter | growth | scale;
 * Agent C's catalog.yaml ships the REAL billing-engine plan_presets tiers
 * free | standard | pro (mapping starter→free, growth→standard, scale→pro)
 * — both spellings are mapped. Unknown tiers fall back to a title-cased
 * outline badge.
 */
const TIER_VARIANT: Record<string, "outline" | "info" | "secondary"> = {
  starter: "outline",
  free: "outline",
  growth: "info",
  standard: "info",
  scale: "secondary",
  pro: "secondary",
};

export function TierBadge({ tier }: { tier: AppPlanTier }) {
  if (!tier) return null;
  return (
    <Badge variant={TIER_VARIANT[tier] ?? "outline"}>{titleCase(tier)}</Badge>
  );
}
