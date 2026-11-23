/** Links from the dashboard menu to tools outside it: the self-hosted Twenty CRM UI (SPEC-CRM §A) and Grafana
 * (SPEC-W3 §1). In a deployment the addresses come from the server env CRM_URL / GRAFANA_URL; set one to "" to hide
 * that entry. Unset, they fall back to the docker-compose dev stack's localhost ports — in a deployment those pointed
 * at the viewer's own computer. */
export type ExternalLinks = { crm?: string; grafana?: string };

export const DEV_EXTERNAL_LINKS: ExternalLinks = { crm: "http://localhost:3100", grafana: "http://localhost:3002" };

/** 2026-11-23: a business with its own CRM gets its own link — CRM_URLS is a JSON map {"<org slug>": "<url>"}
 * (e.g. {"leadway-pensure":"https://leadway-crm.newfire.app"}); other businesses keep CRM_URL. */
function crmFor(orgSlug?: string): string | undefined {
  if (!orgSlug || !process.env.CRM_URLS) return undefined;
  try {
    const map = JSON.parse(process.env.CRM_URLS) as Record<string, string>;
    const url = map[orgSlug];
    return typeof url === "string" && url.trim() ? url.trim() : undefined;
  } catch {
    return undefined;
  }
}

/** Server-side: the addresses for this deployment (and this business, when it has its own CRM). */
export function externalLinks(orgSlug?: string): ExternalLinks {
  const pick = (value: string | undefined, fallback?: string) => (value === undefined ? fallback : value.trim());
  return {
    crm: crmFor(orgSlug) ?? pick(process.env.CRM_URL, DEV_EXTERNAL_LINKS.crm),
    grafana: pick(process.env.GRAFANA_URL, DEV_EXTERNAL_LINKS.grafana),
  };
}
