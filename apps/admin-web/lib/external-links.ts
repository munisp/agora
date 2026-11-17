/** Links from the dashboard menu to tools outside it: the self-hosted Twenty CRM UI (SPEC-CRM §A) and Grafana
 * (SPEC-W3 §1). In a deployment the addresses come from the server env CRM_URL / GRAFANA_URL; set one to "" to hide
 * that entry. Unset, they fall back to the docker-compose dev stack's localhost ports — in a deployment those pointed
 * at the viewer's own computer. */
export type ExternalLinks = { crm?: string; grafana?: string };

export const DEV_EXTERNAL_LINKS: ExternalLinks = { crm: "http://localhost:3100", grafana: "http://localhost:3002" };

/** Server-side: the addresses for this deployment. */
export function externalLinks(): ExternalLinks {
  const pick = (value: string | undefined, fallback?: string) => (value === undefined ? fallback : value.trim());
  return { crm: pick(process.env.CRM_URL, DEV_EXTERNAL_LINKS.crm), grafana: pick(process.env.GRAFANA_URL, DEV_EXTERNAL_LINKS.grafana) };
}
