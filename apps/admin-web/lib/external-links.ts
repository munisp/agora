/** Links from the dashboard menu to tools outside it: the self-hosted Twenty CRM UI (SPEC-CRM §A) and Grafana
 * (SPEC-W3 §1). In a deployment the addresses come from the server env CRM_URL / GRAFANA_URL; set one to "" to hide
 * that entry. Unset, they fall back to the docker-compose dev stack's localhost ports — in a deployment those pointed
 * at the viewer's own computer. */
export type ExternalLinks = { crm?: string; inbox?: string; grafana?: string };

export const DEV_EXTERNAL_LINKS: ExternalLinks = { crm: "http://localhost:3100", grafana: "http://localhost:3002" };

/** 2026-11-23: a business with its own CRM gets its own link — CRM_URLS is a JSON map {"<org slug>": "<url>"}
 * (e.g. {"leadway-pensure":"https://leadway-crm.newfire.app"}); other businesses keep CRM_URL. */
function crmFor(orgSlug?: string): string | undefined {
  return perOrg(process.env.CRM_URLS, orgSlug);
}

/** 2026-11-23: the human agents' inbox (Chatwoot) — INBOX_URLS {"<org slug>": "<url>"}, else INBOX_URL; no dev
 * fallback, so the entry stays hidden until a deployment configures one. */
function inboxFor(orgSlug?: string): string | undefined {
  const own = perOrg(process.env.INBOX_URLS, orgSlug);
  const shared = process.env.INBOX_URL?.trim();
  return own ?? (shared ? shared : undefined);
}

function perOrg(raw: string | undefined, orgSlug?: string): string | undefined {
  if (!orgSlug || !raw) return undefined;
  try {
    const map = JSON.parse(raw) as Record<string, string>;
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
    inbox: inboxFor(orgSlug),
    grafana: pick(process.env.GRAFANA_URL, DEV_EXTERNAL_LINKS.grafana),
  };
}
