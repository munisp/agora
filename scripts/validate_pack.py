#!/usr/bin/env python3
"""OpenDesk industry pack validator + registry helper (STRATEGY §3, Wave 5 #6).

Validates pack YAML against the SPEC-CRM §C schema — the same rules
identity-service enforces in Go (services/identity-service/internal/packs/
packs.go Pack.Validate): required fields, types, value ranges, the
temporalWorkflow enum, and the optional agents / customTools blocks. Keep
the two validators in sync when the schema evolves.

Usage:
    validate_pack.py validate FILE [FILE...]          validate pack YAML files
    validate_pack.py validate-index INDEX.json        validate registry + sha256 of each pack file
    validate_pack.py upsert-index INDEX.json PACK.yaml --version V [--author A]
                                                      validate, then add/update the registry entry

Exit code 0 on success, 1 on any validation/registry error (all errors are
printed, not just the first).
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any

import yaml

# Keep in sync with services/identity-service/internal/packs/packs.go.
KNOWN_WORKFLOWS = (
    "SalonDepositWorkflow",
    "ClinicIntakeWorkflow",
    "ConsultancyFollowupWorkflow",
    "SupportEscalationWorkflow",
)
TERMINOLOGY_KEYS = ("offering", "team_member", "booking", "contact")
DASHBOARD_LABEL_KEYS = ("bookingSingular", "bookingPlural", "customerTerm")
AGENT_ID_RE = re.compile(r"^[a-z][a-z0-9-]*$")
TOOL_NAME_RE = re.compile(r"^[a-zA-Z_][a-zA-Z0-9_]*$")
TOOL_METHODS = ("GET", "POST", "PUT", "PATCH", "DELETE")
MCP_SERVER_NAME_RE = AGENT_ID_RE  # slug rule, kept in sync with packs.go
INDEX_SCHEMA_VERSION = 1


class PackError(Exception):
    """One pack validation error (collected, not raised en-masse)."""


def _is_int(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def _require_str(errs: list[str], doc: dict, key: str, *, where: str = "") -> None:
    v = doc.get(key)
    if not isinstance(v, str) or not v.strip():
        errs.append(f"{where}{key} is required and must be a non-empty string")


def _validate_agents(agents: Any, errs: list[str]) -> None:
    if agents is None:
        return
    if not isinstance(agents, list):
        errs.append("agents must be a list")
        return
    seen: set[str] = set()
    for i, a in enumerate(agents):
        if not isinstance(a, dict):
            errs.append(f"agents[{i}] must be a mapping")
            continue
        aid = a.get("id")
        if not isinstance(aid, str) or not AGENT_ID_RE.match(aid):
            errs.append(f"agents[{i}]: id {aid!r} must match {AGENT_ID_RE.pattern}")
            continue
        if aid in seen:
            errs.append(f"agents[{i}]: duplicate agent id {aid!r}")
        seen.add(aid)
        if not isinstance(a.get("name"), str) or not a["name"].strip():
            errs.append(f"agents[{i}] ({aid}): name is required")
        if not isinstance(a.get("persona"), str) or not a["persona"].strip():
            errs.append(f"agents[{i}] ({aid}): persona is required")
        intents = a.get("intents")
        if not isinstance(intents, list) or not intents:
            errs.append(f"agents[{i}] ({aid}): at least one intent is required")
        else:
            for j, intent in enumerate(intents):
                if not isinstance(intent, str) or not intent.strip():
                    errs.append(f"agents[{i}] ({aid}): intents[{j}] must not be empty")


def _validate_custom_tools(tools: Any, errs: list[str]) -> None:
    if tools is None:
        return
    if not isinstance(tools, list):
        errs.append("customTools must be a list")
        return
    seen: set[str] = set()
    for i, t in enumerate(tools):
        if not isinstance(t, dict):
            errs.append(f"customTools[{i}] must be a mapping")
            continue
        name = t.get("name")
        if not isinstance(name, str) or not TOOL_NAME_RE.match(name):
            errs.append(f"customTools[{i}]: name {name!r} must match {TOOL_NAME_RE.pattern}")
            continue
        if name in seen:
            errs.append(f"customTools[{i}]: duplicate tool name {name!r}")
        seen.add(name)
        if not isinstance(t.get("description"), str) or not t["description"].strip():
            errs.append(f"customTools[{i}] ({name}): description is required")
        method = t.get("method")
        if not isinstance(method, str) or method.upper() not in TOOL_METHODS:
            errs.append(f"customTools[{i}] ({name}): method {method!r} not allowed")
        url = t.get("url")
        if (
            not isinstance(url, str)
            or not re.match(r"^https?://[^/]+", url)
        ):
            errs.append(f"customTools[{i}] ({name}): url must be absolute http(s)")
        if "bodyTemplate" in t and not isinstance(t["bodyTemplate"], str):
            errs.append(f"customTools[{i}] ({name}): bodyTemplate must be a string")


def _validate_mcp_servers(servers: Any, errs: list[str]) -> None:
    """SPEC-W9 Part C2: optional mcpServers passthrough block — slug names,
    https-only URLs, no credentials (headers live in env MCP_SERVERS only)."""
    if servers is None:
        return
    if not isinstance(servers, list):
        errs.append("mcpServers must be a list")
        return
    seen: set[str] = set()
    for i, s in enumerate(servers):
        if not isinstance(s, dict):
            errs.append(f"mcpServers[{i}] must be a mapping")
            continue
        name = s.get("name")
        if not isinstance(name, str) or not MCP_SERVER_NAME_RE.match(name):
            errs.append(
                f"mcpServers[{i}]: name {name!r} must match {MCP_SERVER_NAME_RE.pattern}"
            )
            continue
        if name in seen:
            errs.append(f"mcpServers[{i}]: duplicate server name {name!r}")
        seen.add(name)
        url = s.get("url")
        if not isinstance(url, str) or not re.match(r"^https://[^/]+", url):
            errs.append(f"mcpServers[{i}] ({name}): url must be absolute https")
        if "headers" in s:
            errs.append(
                f"mcpServers[{i}] ({name}): headers are not allowed in packs "
                "(credentials belong in the MCP_SERVERS env var)"
            )


def validate_pack(doc: Any, *, source: str = "<pack>") -> list[str]:
    """Validate one parsed pack document; returns the list of errors
    (empty == valid). Mirrors Pack.Validate() in identity-service."""
    errs: list[str] = []
    if not isinstance(doc, dict):
        return [f"{source}: top level must be a mapping"]

    _require_str(errs, doc, "id")
    _require_str(errs, doc, "displayName")

    terminology = doc.get("terminology")
    if not isinstance(terminology, dict):
        errs.append("terminology is required and must be a mapping")
    else:
        for k in TERMINOLOGY_KEYS:
            if not isinstance(terminology.get(k), str) or not terminology[k].strip():
                errs.append(f"terminology.{k} is required")

    if not isinstance(doc.get("agentPersona"), str) or not doc["agentPersona"].strip():
        errs.append("agentPersona is required")

    policy = doc.get("bookingPolicy")
    if policy is not None:
        if not isinstance(policy, dict):
            errs.append("bookingPolicy must be a mapping")
        else:
            dep = policy.get("depositPercent", 0)
            if not _is_int(dep) or not 0 <= dep <= 100:
                errs.append(f"bookingPolicy.depositPercent must be 0-100, got {dep!r}")
            fee = policy.get("noShowFeeCents", 0)
            if not _is_int(fee) or fee < 0:
                errs.append("bookingPolicy.noShowFeeCents must be >= 0")
            win = policy.get("cancellationWindowHours", 0)
            if not _is_int(win) or win < 0:
                errs.append("bookingPolicy.cancellationWindowHours must be >= 0")
            for flag in ("phoneConfirmation", "intakeRequired"):
                if flag in policy and not isinstance(policy[flag], bool):
                    errs.append(f"bookingPolicy.{flag} must be a boolean")

    wf = doc.get("temporalWorkflow")
    if wf not in KNOWN_WORKFLOWS:
        errs.append(
            f"temporalWorkflow {wf!r} is not a known pack workflow "
            f"(expected one of {', '.join(KNOWN_WORKFLOWS)})"
        )

    offerings = doc.get("offerings")
    if offerings is not None:
        if not isinstance(offerings, list):
            errs.append("offerings must be a list")
        else:
            for i, o in enumerate(offerings):
                if not isinstance(o, dict):
                    errs.append(f"offerings[{i}] must be a mapping")
                    continue
                name = o.get("name")
                if not isinstance(name, str) or not name.strip():
                    errs.append(f"offerings[{i}].name is required")
                    name = "?"
                dur = o.get("duration_min")
                if not _is_int(dur) or dur <= 0:
                    errs.append(f"offerings[{i}] ({name}): duration_min must be > 0")
                for k in ("buffer_min", "price_cents", "capacity"):
                    v = o.get(k, 0)
                    if not _is_int(v) or v < 0:
                        errs.append(f"offerings[{i}] ({name}): {k} must be >= 0")

    seed = doc.get("knowledgeSeed")
    if seed is not None:
        if not isinstance(seed, list):
            errs.append("knowledgeSeed must be a list")
        else:
            for i, d in enumerate(seed):
                if (
                    not isinstance(d, dict)
                    or not isinstance(d.get("title"), str)
                    or not d.get("title", "").strip()
                    or not isinstance(d.get("body"), str)
                    or not d.get("body", "").strip()
                ):
                    errs.append(f"knowledgeSeed[{i}]: title and body are required")

    labels = doc.get("dashboardLabels")
    if not isinstance(labels, dict):
        errs.append("dashboardLabels is required and must be a mapping")
    else:
        for k in DASHBOARD_LABEL_KEYS:
            if not isinstance(labels.get(k), str) or not labels[k].strip():
                errs.append(f"dashboardLabels.{k} is required")

    _validate_agents(doc.get("agents"), errs)
    _validate_custom_tools(doc.get("customTools"), errs)
    _validate_mcp_servers(doc.get("mcpServers"), errs)
    return errs


def load_pack(path: Path) -> tuple[Any, list[str]]:
    """Parse a pack YAML file; returns (doc, errors)."""
    try:
        doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        return None, [f"{path}: YAML parse error: {exc}"]
    except OSError as exc:
        return None, [f"{path}: {exc}"]
    return doc, []


def sha256_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


# --------------------------------------------------------------------- index
def validate_index(index_path: Path) -> list[str]:
    """Validate the pack registry: schema, required entry fields, and the
    sha256 + validity of every referenced pack file (resolved relative to
    the index file's directory)."""
    errs: list[str] = []
    try:
        index = json.loads(index_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return [f"{index_path}: {exc}"]
    if not isinstance(index, dict) or index.get("schemaVersion") != INDEX_SCHEMA_VERSION:
        errs.append(f"{index_path}: schemaVersion must be {INDEX_SCHEMA_VERSION}")
    packs = index.get("packs")
    if not isinstance(packs, list):
        return errs + [f"{index_path}: packs must be a list"]

    seen_ids: set[str] = set()
    base = index_path.parent
    for i, entry in enumerate(packs):
        if not isinstance(entry, dict):
            errs.append(f"packs[{i}] must be a mapping")
            continue
        pid = entry.get("id")
        if not isinstance(pid, str) or not pid:
            errs.append(f"packs[{i}].id is required")
            continue
        where = f"packs[{i}] ({pid})"
        if pid in seen_ids:
            errs.append(f"{where}: duplicate pack id")
        seen_ids.add(pid)
        if not isinstance(entry.get("version"), str) or not entry["version"]:
            errs.append(f"{where}.version is required")
        digest = entry.get("sha256")
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest or ""):
            errs.append(f"{where}.sha256 must be a lowercase hex sha256")
        if not isinstance(entry.get("author"), str) or not entry["author"]:
            errs.append(f"{where}.author is required")
        if "signature" not in entry:
            errs.append(f"{where}.signature key is required (null until sigstore lands)")
        rel = entry.get("path") or f"{pid}.yaml"
        pack_path = base / rel
        if not pack_path.exists():
            errs.append(f"{where}: pack file {rel} not found")
            continue
        doc, load_errs = load_pack(pack_path)
        errs.extend(load_errs)
        if not load_errs:
            errs.extend(validate_pack(doc, source=str(pack_path)))
            if doc.get("id") != pid:
                errs.append(f"{where}: registry id does not match pack id {doc.get('id')!r}")
        if isinstance(digest, str) and re.fullmatch(r"[0-9a-f]{64}", digest or ""):
            actual = sha256_file(pack_path)
            if actual != digest:
                errs.append(f"{where}: sha256 mismatch (file is {actual})")
    return errs


def upsert_index(index_path: Path, pack_path: Path, *, version: str, author: str) -> list[str]:
    """Validate pack_path, then add/update its entry in the registry."""
    doc, errs = load_pack(pack_path)
    if not errs:
        errs = validate_pack(doc, source=str(pack_path))
    if errs:
        return errs
    pid = doc["id"]
    try:
        index = json.loads(index_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        index = {"schemaVersion": INDEX_SCHEMA_VERSION, "packs": []}
    entry = {
        "id": pid,
        "version": version,
        "sha256": sha256_file(pack_path),
        "author": author,
        "signature": None,
        "path": f"{pid}.yaml",
    }
    packs = [p for p in index.get("packs", []) if p.get("id") != pid]
    packs.append(entry)
    packs.sort(key=lambda p: p.get("id", ""))
    index["schemaVersion"] = INDEX_SCHEMA_VERSION
    index["packs"] = packs
    index_path.write_text(json.dumps(index, indent=2) + "\n", encoding="utf-8")
    return []


# ----------------------------------------------------------------------- cli
def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_val = sub.add_parser("validate", help="validate pack YAML files")
    p_val.add_argument("files", nargs="+", type=Path)

    p_idx = sub.add_parser("validate-index", help="validate the pack registry")
    p_idx.add_argument("index", type=Path)

    p_up = sub.add_parser("upsert-index", help="validate + add/update a registry entry")
    p_up.add_argument("index", type=Path)
    p_up.add_argument("pack", type=Path)
    p_up.add_argument("--version", default="0.1.0")
    p_up.add_argument("--author", default="community")

    args = parser.parse_args(argv)

    if args.cmd == "validate":
        errs: list[str] = []
        for f in args.files:
            doc, load_errs = load_pack(f)
            errs.extend(load_errs)
            if not load_errs:
                errs.extend(validate_pack(doc, source=str(f)))
    elif args.cmd == "validate-index":
        errs = validate_index(args.index)
    else:  # upsert-index
        errs = upsert_index(args.index, args.pack, version=args.version, author=args.author)
        if not errs:
            print(f"[validate_pack] registry updated: {args.index}")

    for e in errs:
        print(f"[validate_pack] ERROR: {e}", file=sys.stderr)
    if errs:
        return 1
    if args.cmd == "validate":
        for f in args.files:
            print(f"[validate_pack] OK: {f}")
    elif args.cmd == "validate-index":
        print(f"[validate_pack] OK: {args.index}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
