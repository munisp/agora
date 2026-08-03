import { Badge } from "@/components/ui/badge";
import { titleCase } from "@/lib/utils";
import type { TenantAppStatus } from "@/lib/types";

/**
 * Status pill for a tenant app installation (SPEC-W18 Agent B, contract §2):
 * Enabled / Disabled / Suspended / Not provisioned. Unknown future statuses
 * fall back to a title-cased outline pill so nothing renders blank.
 */
const STATUS_META: Record<
  string,
  { label: string; variant: "success" | "secondary" | "warning" | "outline" }
> = {
  enabled: { label: "Enabled", variant: "success" },
  disabled: { label: "Disabled", variant: "secondary" },
  suspended: { label: "Suspended", variant: "warning" },
  not_provisioned: { label: "Not provisioned", variant: "outline" },
};

export function StatusPill({ status }: { status: TenantAppStatus }) {
  const meta = STATUS_META[status] ?? {
    label: titleCase(status || "unknown"),
    variant: "outline" as const,
  };
  return <Badge variant={meta.variant}>{meta.label}</Badge>;
}
