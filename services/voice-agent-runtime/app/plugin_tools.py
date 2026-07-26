"""Plugin tools MVP (SPEC-W3 §4, innovation 15).

Industry packs may declare ``customTools: [{name, description, method, url,
bodyTemplate}]`` — declarative HTTP tools the voice runtime registers in the
tool layer alongside the built-in receptionist tools. Execution:

- ``{{var}}`` placeholders in ``url`` and ``bodyTemplate`` are substituted
  from the tool-call arguments (plus the session context vars ``site_slug``,
  ``tenant_slug``, ``tenant_id``).
- GET/DELETE send remaining args as query params; other methods send the
  rendered ``bodyTemplate`` (or the raw args as JSON when no template).
- SSRF guard: the URL hostname must be in the ``PLUGIN_ALLOWED_HOSTS``
  allowlist (default ``booking,knowledge,identity``) — pack-declared tools
  can only reach platform services, never arbitrary hosts.

WASM-sandboxed plugins are the documented phase-2 design (docs/plugins.md).
"""

from __future__ import annotations

import json
import re
from string import Template
from typing import Any
from urllib.parse import urlparse

import httpx

from .logging import get_logger
from .mcp_client import build_mcp_tools_sync

log = get_logger("plugin-tools")

_VAR_RE = re.compile(r"\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}")
_ALLOWED_METHODS = {"GET", "POST", "PUT", "PATCH", "DELETE"}


class PluginToolError(ValueError):
    """Invalid plugin tool declaration or blocked execution."""


def parse_allowed_hosts(raw: str) -> set[str]:
    return {h.strip().lower() for h in raw.split(",") if h.strip()}


def render_template(template: str, variables: dict[str, Any]) -> str:
    """Substitute {{var}} placeholders; unknown vars render as empty."""

    def _sub(match: re.Match[str]) -> str:
        value = variables.get(match.group(1), "")
        return str(value) if value is not None else ""

    return _VAR_RE.sub(_sub, template)


def template_variables(template: str) -> list[str]:
    """Variable names referenced by a template (used to derive the schema)."""
    return sorted(set(_VAR_RE.findall(template)))


class PluginTool:
    """One declarative HTTP tool from a pack ``customTools`` entry."""

    def __init__(
        self,
        spec: dict[str, Any],
        *,
        allowed_hosts: set[str],
        context: dict[str, Any] | None = None,
        client: httpx.AsyncClient | None = None,
        timeout_s: float = 10.0,
    ) -> None:
        self.name = str(spec.get("name") or "").strip()
        self.description = str(spec.get("description") or "").strip()
        self.method = str(spec.get("method") or "GET").strip().upper()
        self.url = str(spec.get("url") or "").strip()
        self.body_template = spec.get("bodyTemplate")
        if not self.name or not re.fullmatch(r"[a-zA-Z_][a-zA-Z0-9_]*", self.name):
            raise PluginToolError(f"invalid plugin tool name {self.name!r}")
        if self.method not in _ALLOWED_METHODS:
            raise PluginToolError(f"{self.name}: unsupported method {self.method!r}")
        if not self.url:
            raise PluginToolError(f"{self.name}: url is required")
        self._allowed_hosts = allowed_hosts
        self._context = context or {}
        self._client = client
        self._timeout_s = timeout_s
        # Host check runs at registration too (fail fast on static hosts).
        host = urlparse(render_template(self.url, self._context)).hostname
        if host and not self._host_allowed(host):
            raise PluginToolError(
                f"{self.name}: host {host!r} not in PLUGIN_ALLOWED_HOSTS"
            )

    def _host_allowed(self, host: str) -> bool:
        host = host.lower()
        return any(host == h or host.endswith("." + h) for h in self._allowed_hosts)

    def schema(self) -> dict[str, Any]:
        """OpenAI-format tool schema; properties derived from template vars."""
        variables = set(template_variables(self.url)) - set(self._context)
        if isinstance(self.body_template, str):
            variables |= set(template_variables(self.body_template)) - set(self._context)
        properties = {v: {"type": "string"} for v in sorted(variables)}
        return {
            "type": "function",
            "function": {
                "name": self.name,
                "description": self.description or f"Pack tool {self.name}",
                "parameters": {
                    "type": "object",
                    "properties": properties,
                    "required": [],
                },
            },
        }

    async def execute(self, arguments: dict[str, Any]) -> dict[str, Any]:
        variables = {**self._context, **{k: str(v) for k, v in arguments.items()}}
        url = render_template(self.url, variables)
        host = urlparse(url).hostname or ""
        if not self._host_allowed(host):
            log.warning("plugin tool blocked by SSRF guard", tool=self.name, host=host)
            return {
                "status": "error",
                "message": f"{self.name}: host {host!r} not allowed (SSRF guard)",
            }

        request_kwargs: dict[str, Any] = {}
        if self.method in ("GET", "DELETE"):
            # Args not consumed by the URL template become query params.
            used = set(template_variables(self.url))
            params = {k: v for k, v in variables.items() if k not in used and k not in self._context}
            if params:
                request_kwargs["params"] = params
        else:
            if isinstance(self.body_template, str) and self.body_template.strip():
                rendered = render_template(self.body_template, variables)
                request_kwargs["content"] = rendered
                request_kwargs["headers"] = {"content-type": "application/json"}
            else:
                request_kwargs["json"] = {
                    k: v for k, v in arguments.items()
                }

        owns_client = self._client is None
        client = self._client or httpx.AsyncClient(timeout=httpx.Timeout(self._timeout_s))
        try:
            resp = await client.request(self.method, url, **request_kwargs)
        except Exception as exc:  # noqa: BLE001 - surfaced to the model
            log.warning("plugin tool request failed", tool=self.name, error=str(exc))
            return {"status": "error", "message": f"{self.name} request failed: {exc}"}
        finally:
            if owns_client:
                await client.aclose()

        try:
            body: Any = resp.json()
        except (json.JSONDecodeError, ValueError):
            body = resp.text[:2000]
        if resp.status_code >= 400:
            return {
                "status": "error",
                "message": f"{self.name} returned HTTP {resp.status_code}",
                "body": body,
            }
        return {"status": "ok", "http_status": resp.status_code, "body": body}


def build_plugin_tools(
    specs: list[dict[str, Any]],
    *,
    allowed_hosts_raw: str,
    context: dict[str, Any] | None = None,
    client: httpx.AsyncClient | None = None,
    tenant_ctx: Any = None,
) -> list[Any]:
    """Validate pack customTools and build executable PluginTools.

    Invalid entries are skipped with a warning (a broken pack tool must not
    take down the session); validation in identity's pack loader is the
    fail-fast gate at provisioning time.

    SPEC-W9 Part C: MCP tools (env MCP_SERVERS + per-tenant pack mcpServers,
    namespaced ``mcp__{server}__{tool}``) are appended AFTER the customTools.
    When no MCP servers are configured the MCP step is a no-op and the
    returned list is exactly the customTools result — existing behavior is
    unchanged. A down/unreachable MCP server is skipped with a warning and
    never breaks the session. ``tenant_ctx`` is optional: the chat path does
    not pass it yet, so per-tenant pack servers activate here as soon as the
    tenant context exposes them (see docs/mcp.md).
    """
    allowed = parse_allowed_hosts(allowed_hosts_raw)
    tools: list[Any] = []
    for spec in specs:
        try:
            tools.append(
                PluginTool(spec, allowed_hosts=allowed, context=context, client=client)
            )
        except PluginToolError as exc:
            log.warning("skipping invalid plugin tool", error=str(exc))
    try:
        tools.extend(build_mcp_tools_sync(tenant_ctx))
    except Exception as exc:  # noqa: BLE001 - MCP must never break the session
        log.warning("mcp tool build failed", error=str(exc)[:200])
    return tools
