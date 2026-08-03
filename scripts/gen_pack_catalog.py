#!/usr/bin/env python3
"""Generate the pack-catalog snapshot consumed by the admin-web packs page.

Reads industries/*.yaml + industries/index.json and writes a typed TS module.
Run from the repo root:

    python3 scripts/gen_pack_catalog.py > "apps/admin-web/app/app/[orgSlug]/settings/packs/catalog-data.ts"

identity-service does not (yet) expose GET /v1/packs, so the dashboard ships
this build-time snapshot of the registry directory instead of a live fetch.
"""

import json
import sys
import pathlib

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "industries")
index = json.loads((root / "index.json").read_text())
versions = {p["id"]: p.get("version", "1.0.0") for p in index.get("packs", [])}

import yaml  # noqa: E402

entries = []
for path in sorted(root.glob("*.yaml")):
    doc = yaml.safe_load(path.read_text())
    if not isinstance(doc, dict) or not doc.get("id"):
        continue
    entry = {
        "id": doc["id"],
        "displayName": doc.get("displayName", doc["id"]),
        "version": versions.get(doc["id"], "1.0.0"),
        "indexed": doc["id"] in versions,
        "languages": doc.get("languages") or [],
        "terminology": doc.get("terminology") or {},
    }
    if doc.get("temporalWorkflow"):
        entry["temporalWorkflow"] = doc["temporalWorkflow"]
    persona = (doc.get("agentPersona") or "").strip()
    if persona:
        # first paragraph, capped — preview only
        first = persona.split("\n\n")[0].strip()
        entry["personaExcerpt"] = first[:400]
    bp = doc.get("bookingPolicy")
    if isinstance(bp, dict):
        entry["bookingPolicy"] = {
            k: v
            for k, v in bp.items()
            if k
            in (
                "depositPercent",
                "noShowFeeCents",
                "phoneConfirmation",
                "intakeRequired",
                "cancellationWindowHours",
            )
        }
    d = doc.get("disclosure")
    if isinstance(d, dict):
        entry["disclosure"] = {
            "spokenAiDisclosure": bool(d.get("spokenAiDisclosure")),
            "recordingConsent": bool(d.get("recordingConsent")),
            "text": d.get("text") or "",
        }
    u = doc.get("ussd")
    if isinstance(u, dict) and isinstance(u.get("menu"), list):
        entry["ussdMenu"] = [
            {"key": str(i.get("key", "")), "label": str(i.get("label", "")), "action": str(i.get("action", "") or "")}
            for i in u["menu"]
            if isinstance(i, dict)
        ]
    ct = (doc.get("consentText") or "").strip()
    if ct:
        entry["consentTextExcerpt"] = ct[:400]
    # SPEC-W15 §2 growth block: {referral_bounty_ngn, primary_channels[],
    # cac_target_ngn} — free-form map, only known keys are snapshotted.
    g = doc.get("growth")
    if isinstance(g, dict):
        growth = {}
        if isinstance(g.get("referral_bounty_ngn"), (int, float)):
            growth["referralBountyNgn"] = g["referral_bounty_ngn"]
        if isinstance(g.get("cac_target_ngn"), (int, float)):
            growth["cacTargetNgn"] = g["cac_target_ngn"]
        if isinstance(g.get("primary_channels"), list):
            growth["primaryChannels"] = [str(c) for c in g["primary_channels"]]
        if growth:
            entry["growth"] = growth
    # SPEC-W15 §3 i18n block: {locale: {greeting, referralLine, ussdPrompt,
    # ...}} — snapshot the greeting/ussdPrompt preview strings per locale.
    i18n = doc.get("i18n")
    if isinstance(i18n, dict):
        locales = {}
        for locale, strings in i18n.items():
            if not isinstance(strings, dict):
                continue
            preview = {}
            for key in ("greeting", "ussdPrompt", "referralLine"):
                v = strings.get(key)
                if isinstance(v, str) and v.strip():
                    preview[key] = v.strip()[:400]
            if preview:
                locales[str(locale)] = preview
        if locales:
            entry["i18n"] = locales
    entries.append(entry)

ts = """/**
 * GENERATED FILE — build-time snapshot of the industries/ pack registry
 * (industries/*.yaml + industries/index.json). Regenerate from the repo root
 * with scripts/gen_pack_catalog.py (see the generator header) whenever packs
 * change. identity-service does not yet expose GET /v1/packs, so the packs
 * settings page renders this snapshot instead of a live catalog fetch; the
 * tenant's *active* pack still comes from the real
 * GET /api/identity/v1/tenants/{slug} endpoint.
 */

export interface PackCatalogDisclosure {
  spokenAiDisclosure: boolean;
  recordingConsent: boolean;
  text: string;
}

export interface PackCatalogUssdItem {
  key: string;
  label: string;
  action: string;
}

export interface PackCatalogEntry {
  id: string;
  displayName: string;
  version: string;
  /** true when the pack is listed in industries/index.json */
  indexed: boolean;
  languages: string[];
  terminology: Record<string, string>;
  temporalWorkflow?: string;
  personaExcerpt?: string;
  bookingPolicy?: {
    depositPercent?: number;
    noShowFeeCents?: number;
    phoneConfirmation?: boolean;
    intakeRequired?: boolean;
    cancellationWindowHours?: number;
  };
  disclosure?: PackCatalogDisclosure;
  ussdMenu?: PackCatalogUssdItem[];
  consentTextExcerpt?: string;
  /** SPEC-W15 §2 growth block (camelCased snapshot). */
  growth?: {
    referralBountyNgn?: number;
    cacTargetNgn?: number;
    primaryChannels?: string[];
  };
  /** SPEC-W15 §3 i18n preview strings per locale (greeting/ussdPrompt/referralLine). */
  i18n?: Record<
    string,
    { greeting?: string; ussdPrompt?: string; referralLine?: string }
  >;
}

export const PACK_CATALOG: PackCatalogEntry[] = %s;
""" % json.dumps(entries, indent=2, ensure_ascii=False)

sys.stdout.write(ts)
print(f"// {len(entries)} packs", file=sys.stderr)
