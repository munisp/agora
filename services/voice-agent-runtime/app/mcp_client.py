"""MCP (Model Context Protocol) client for the voice tool layer (SPEC-W9 Part C).

Hand-rolled JSON-RPC 2.0 client over the MCP HTTP transports — no new
dependencies, httpx only. Two transports are supported:

- **Streamable HTTP** (protocol 2025-03-26): every JSON-RPC message is a POST;
  the server answers with either ``application/json`` or an
  ``text/event-stream`` SSE body; a ``mcp-session-id`` response header is
  echoed back on subsequent requests.
- **Legacy HTTP+SSE** (2024-11-05, what n8n's MCP Server Trigger exposes at
  ``.../sse``): a hanging GET streams events; the first ``endpoint`` event
  carries the URI JSON-RPC messages are POSTed to; responses arrive back on
  the GET stream as ``message`` events.

Handshake: ``initialize`` (protocolVersion "2025-03-26", clientInfo
opendesk-voice/1.0) -> ``notifications/initialized`` -> ``tools/list``;
tool execution is ``tools/call``.

Server configuration (merged, env first — env entries win name collisions):

1. Env ``MCP_SERVERS`` — JSON ``[{"name":"n8n","url":"https://.../sse",
   "headers":{"authorization":"Bearer ..."},"transport":"sse"}]``. Headers
   are allowed here only (env is operator-controlled); packs never carry
   credentials. Read via ``os.environ`` on purpose — app/config.py stays
   untouched.
2. Per-tenant pack ``mcpServers`` (identity passes the block through into
   the tenant context JSON, mirroring customTools) — consumed defensively
   via ``getattr(ctx, "mcp_servers", ...)`` / dict ``pack.mcpServers``.

Security model: **https only**, name slug regex (same as the pack
validator), 5s connect timeout, tool calls routed through the existing
:class:`~app.async_tools.AsyncToolRunner` hard timeout, and ANY server
failure logs a warning and skips that server — the session never breaks.

Tool names are namespaced ``mcp__{server}__{tool}`` and the remote
``inputSchema`` is passed through verbatim as the function parameters.

``build_plugin_tools`` is synchronous (chat.py must not change), so the
async handshake is bridged via :func:`build_mcp_tools_sync`: results are
TTL-cached per server (failures cached for a shorter window), and when the
caller already runs an event loop the handshake is executed on a dedicated
background loop thread and awaited with a bounded timeout.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable
from urllib.parse import urljoin, urlparse

import httpx

from .async_tools import AsyncToolRunner
from .logging import get_logger

log = get_logger("mcp-client")

PROTOCOL_VERSION = "2025-03-26"
CLIENT_INFO = {"name": "opendesk-voice", "version": "1.0"}
# Same slug rule as the pack validator (scripts/validate_pack.py) and the Go
# pack loader (identity-service internal/packs).
SERVER_NAME_RE = re.compile(r"^[a-z][a-z0-9-]*$")
# OpenAI-style function names cap at 64 chars.
MAX_TOOL_NAME = 64

_ACCEPT = "application/json, text/event-stream"


class MCPError(Exception):
    """MCP transport/protocol failure (server is skipped, never fatal)."""


# ---------------------------------------------------------------------- env
def _env_float(key: str, default: float) -> float:
    try:
        return float(os.environ.get(key, str(default)))
    except ValueError:
        return default


def _connect_timeout_s() -> float:
    return _env_float("MCP_CONNECT_TIMEOUT_SECONDS", 5.0)


def _cache_ttl_s() -> float:
    return _env_float("MCP_TOOLS_CACHE_SECONDS", 300.0)


def _failure_ttl_s() -> float:
    return _env_float("MCP_FAILURE_CACHE_SECONDS", 30.0)


def _tool_timeout_s() -> float:
    # Mirror Settings.tool_timeout_s (TOOL_TIMEOUT_SECONDS, default 4s).
    return _env_float("MCP_TOOL_TIMEOUT_SECONDS", _env_float("TOOL_TIMEOUT_SECONDS", 4.0))


# ------------------------------------------------------------ server specs
@dataclass(frozen=True)
class MCPServerSpec:
    """One MCP server entry (env MCP_SERVERS or pack mcpServers)."""

    name: str
    url: str
    headers: dict[str, str] = field(default_factory=dict)
    transport: str = "auto"  # auto|http|sse


def parse_server_spec(raw: Any, *, allow_headers: bool) -> MCPServerSpec:
    """Validate one server entry; raises MCPError on invalid input.

    ``allow_headers`` is True only for env-sourced entries — pack mcpServers
    carry no credentials (docs/mcp.md security model).
    """
    if not isinstance(raw, dict):
        raise MCPError(f"mcp server entry must be a mapping, got {type(raw).__name__}")
    name = str(raw.get("name") or "").strip()
    if not SERVER_NAME_RE.fullmatch(name):
        raise MCPError(f"mcp server name {name!r} must match {SERVER_NAME_RE.pattern}")
    url = str(raw.get("url") or "").strip()
    parsed = urlparse(url)
    if parsed.scheme != "https" or not parsed.hostname:
        raise MCPError(f"mcp server {name!r}: url must be absolute https")
    headers = raw.get("headers")
    if headers:
        if not allow_headers:
            raise MCPError(f"mcp server {name!r}: headers are not allowed in pack mcpServers")
        if not isinstance(headers, dict):
            raise MCPError(f"mcp server {name!r}: headers must be a mapping")
        headers = {str(k): str(v) for k, v in headers.items()}
    transport = str(raw.get("transport") or "auto").strip().lower()
    if transport not in ("auto", "http", "sse"):
        raise MCPError(f"mcp server {name!r}: transport must be auto|http|sse")
    return MCPServerSpec(name=name, url=url, headers=headers or {}, transport=transport)


def parse_env_servers(raw: str) -> list[MCPServerSpec]:
    """Parse the MCP_SERVERS env JSON; invalid entries skip with a warning."""
    raw = (raw or "").strip()
    if not raw:
        return []
    try:
        doc = json.loads(raw)
    except json.JSONDecodeError as exc:
        log.warning("MCP_SERVERS is not valid JSON — no MCP servers configured", error=str(exc))
        return []
    if not isinstance(doc, list):
        log.warning("MCP_SERVERS must be a JSON list — no MCP servers configured")
        return []
    specs: list[MCPServerSpec] = []
    for entry in doc:
        try:
            specs.append(parse_server_spec(entry, allow_headers=True))
        except MCPError as exc:
            log.warning("skipping invalid MCP_SERVERS entry", error=str(exc))
    return specs


def env_servers() -> list[MCPServerSpec]:
    return parse_env_servers(os.environ.get("MCP_SERVERS", ""))


def tenant_mcp_servers(tenant_ctx: Any) -> list[MCPServerSpec]:
    """Defensive extraction of per-tenant pack mcpServers.

    Accepts a TenantContext-like object (``mcp_servers`` attribute — additive,
    mirroring custom_tools), a raw tenant payload dict (``pack.mcpServers``),
    or None. Identity passes packs through unvalidated, so every entry is
    validated here; invalid entries drop out with a warning, never fatal.
    """
    raw: Any = None
    if tenant_ctx is None:
        return []
    if isinstance(tenant_ctx, dict):
        raw = tenant_ctx.get("mcpServers")
        pack = tenant_ctx.get("pack")
        if raw is None and isinstance(pack, dict):
            raw = pack.get("mcpServers")
    else:
        raw = getattr(tenant_ctx, "mcp_servers", None)
        if raw is None:
            raw = getattr(tenant_ctx, "mcpServers", None)
    if not isinstance(raw, list):
        return []
    specs: list[MCPServerSpec] = []
    for entry in raw:
        try:
            specs.append(parse_server_spec(entry, allow_headers=False))
        except MCPError as exc:
            log.warning("skipping invalid pack mcpServers entry", error=str(exc))
    return specs


def merged_servers(tenant_ctx: Any = None) -> list[MCPServerSpec]:
    """Env MCP_SERVERS + per-tenant pack mcpServers; env wins name clashes."""
    specs = env_servers()
    seen = {s.name for s in specs}
    for spec in tenant_mcp_servers(tenant_ctx):
        if spec.name in seen:
            log.warning("pack mcpServers entry shadowed by env server", server=spec.name)
            continue
        seen.add(spec.name)
        specs.append(spec)
    return specs


# -------------------------------------------------------------- SSE parsing
class _SSEParser:
    """Incremental SSE parser (event:/data: lines, blank line dispatches)."""

    def __init__(self) -> None:
        self._event = "message"
        self._data: list[str] = []

    def feed_line(self, line: str) -> tuple[str, str] | None:
        line = line.rstrip("\r")
        if not line:  # dispatch
            if not self._data:
                self._event = "message"
                return None
            event = (self._event, "\n".join(self._data))
            self._event = "message"
            self._data = []
            return event
        if line.startswith(":"):  # comment / keepalive
            return None
        field_name, _, value = line.partition(":")
        if value.startswith(" "):
            value = value[1:]
        if field_name == "event":
            self._event = value
        elif field_name == "data":
            self._data.append(value)
        return None


def parse_sse_body(text: str) -> list[tuple[str, str]]:
    parser = _SSEParser()
    events = []
    for line in text.split("\n"):
        ev = parser.feed_line(line)
        if ev is not None:
            events.append(ev)
    return events


# --------------------------------------------------------------- JSON-RPC
def _rpc_request(rpc_id: int, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    msg: dict[str, Any] = {"jsonrpc": "2.0", "id": rpc_id, "method": method}
    if params is not None:
        msg["params"] = params
    return msg


def _rpc_notification(method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    msg: dict[str, Any] = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        msg["params"] = params
    return msg


def _result_of(message: dict[str, Any], *, want_id: int, what: str) -> dict[str, Any]:
    if message.get("id") != want_id:
        raise MCPError(f"{what}: unmatched response id {message.get('id')!r} (want {want_id})")
    error = message.get("error")
    if isinstance(error, dict):
        raise MCPError(f"{what}: JSON-RPC error {error.get('code')}: {error.get('message')}")
    result = message.get("result")
    return result if isinstance(result, dict) else {}


def _parse_http_response(resp: httpx.Response, *, want_id: int, what: str) -> dict[str, Any]:
    """Parse a streamable-HTTP response body (JSON or SSE) into a result."""
    if resp.status_code >= 400:
        raise MCPError(f"{what}: HTTP {resp.status_code}")
    content_type = resp.headers.get("content-type", "")
    if "text/event-stream" in content_type:
        for event, data in parse_sse_body(resp.text):
            if event != "message":
                continue
            try:
                message = json.loads(data)
            except json.JSONDecodeError:
                continue
            if isinstance(message, dict) and message.get("id") == want_id:
                return _result_of(message, want_id=want_id, what=what)
        raise MCPError(f"{what}: no response in SSE stream")
    try:
        message = resp.json()
    except (json.JSONDecodeError, ValueError) as exc:
        raise MCPError(f"{what}: unreadable response body ({exc})") from exc
    if not isinstance(message, dict):
        raise MCPError(f"{what}: response is not a JSON-RPC object")
    return _result_of(message, want_id=want_id, what=what)


# -------------------------------------------------------- transport: legacy
class _LegacySSETransport:
    """2024-11-05 HTTP+SSE transport (n8n `.../sse` endpoints): a hanging GET
    delivers the ``endpoint`` event then ``message`` responses; JSON-RPC
    messages are POSTed to that endpoint (202 Accepted)."""

    def __init__(
        self,
        spec: MCPServerSpec,
        *,
        timeout_s: float,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self._spec = spec
        self._timeout_s = timeout_s
        self._injected = client
        self._client: httpx.AsyncClient | None = None
        self._stream_ctx: Any = None
        self._resp: httpx.Response | None = None
        self._reader: asyncio.Task | None = None
        self._events: asyncio.Queue[tuple[str, str]] = asyncio.Queue()
        self._post_url = ""

    async def open(self) -> None:
        self._client = self._injected or httpx.AsyncClient(
            timeout=httpx.Timeout(self._timeout_s),
            headers={**self._spec.headers, "accept": "text/event-stream"},
        )
        try:
            self._stream_ctx = self._client.stream("GET", self._spec.url)
            self._resp = await self._stream_ctx.__aenter__()
            if self._resp.status_code >= 400:
                raise MCPError(f"sse connect: HTTP {self._resp.status_code}")
            self._reader = asyncio.create_task(self._read_loop())
            # First event must be `endpoint` with the POST URI.
            deadline = time.monotonic() + self._timeout_s
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise MCPError("sse connect: timed out waiting for endpoint event")
                event, data = await asyncio.wait_for(self._events.get(), timeout=remaining)
                if event == "endpoint":
                    self._post_url = urljoin(self._spec.url, data.strip())
                    if urlparse(self._post_url).scheme != "https":
                        raise MCPError("sse connect: endpoint event is not https")
                    return
        except Exception:
            await self.aclose()
            raise

    async def _read_loop(self) -> None:
        parser = _SSEParser()
        assert self._resp is not None
        try:
            async for line in self._resp.aiter_lines():
                ev = parser.feed_line(line)
                if ev is not None:
                    self._events.put_nowait(ev)
        except Exception as exc:  # noqa: BLE001 - stream drop surfaces via rpc timeout
            log.warning("mcp sse stream ended", server=self._spec.name, error=str(exc)[:200])

    async def request(self, message: dict[str, Any]) -> dict[str, Any]:
        assert self._client is not None
        resp = await self._client.post(self._post_url, json=message)
        if resp.status_code >= 400:
            raise MCPError(f"{message.get('method')}: HTTP {resp.status_code}")
        rpc_id = message.get("id")
        if rpc_id is None:  # notification: 202, no response expected
            return {}
        deadline = time.monotonic() + self._timeout_s
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise MCPError(f"{message.get('method')}: timed out waiting for response")
            event, data = await asyncio.wait_for(self._events.get(), timeout=remaining)
            if event != "message":
                continue
            try:
                incoming = json.loads(data)
            except json.JSONDecodeError:
                continue
            if isinstance(incoming, dict) and incoming.get("id") == rpc_id:
                return _result_of(incoming, want_id=rpc_id, what=str(message.get("method")))

    async def aclose(self) -> None:
        if self._reader is not None:
            self._reader.cancel()
            self._reader = None
        if self._stream_ctx is not None:
            try:
                await self._stream_ctx.__aexit__(None, None, None)
            except Exception:  # noqa: BLE001 - best-effort close
                pass
            self._stream_ctx = None
        if self._client is not None:
            await self._client.aclose()
            self._client = None


# -------------------------------------------------- transport: streamable
class _StreamableHTTPTransport:
    """2025-03-26 streamable HTTP transport: POST per JSON-RPC message, JSON
    or SSE response body, ``mcp-session-id`` header echoed back when set."""

    def __init__(
        self,
        spec: MCPServerSpec,
        *,
        timeout_s: float,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self._spec = spec
        self._timeout_s = timeout_s
        self._injected = client
        self._client: httpx.AsyncClient | None = None
        self._session_id = ""

    async def open(self) -> None:
        self._client = self._injected or httpx.AsyncClient(
            timeout=httpx.Timeout(self._timeout_s),
            headers={**self._spec.headers, "accept": _ACCEPT},
        )

    async def request(self, message: dict[str, Any]) -> dict[str, Any]:
        assert self._client is not None
        headers = {}
        if self._session_id:
            headers["mcp-session-id"] = self._session_id
        resp = await self._client.post(self._spec.url, json=message, headers=headers)
        session = resp.headers.get("mcp-session-id")
        if session:
            self._session_id = session
        if message.get("id") is None:  # notification: 202 + no body
            if resp.status_code >= 400:
                raise MCPError(f"{message.get('method')}: HTTP {resp.status_code}")
            return {}
        return _parse_http_response(resp, want_id=message["id"], what=str(message.get("method")))

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None


# --------------------------------------------------------------- the client
class MCPClient:
    """One MCP session against one server: initialize handshake, tools/list,
    tools/call. Never raises into the session layer — callers catch MCPError
    and skip the server."""

    def __init__(
        self,
        spec: MCPServerSpec,
        *,
        timeout_s: float | None = None,
        transport: Any = None,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self._spec = spec
        self._timeout_s = timeout_s if timeout_s is not None else _connect_timeout_s()
        self._next_id = 0
        if transport is not None:
            self._transport = transport
        elif spec.transport == "sse" or (
            spec.transport == "auto" and urlparse(spec.url).path.rstrip("/").endswith("/sse")
        ):
            self._transport = _LegacySSETransport(
                spec, timeout_s=self._timeout_s, client=client
            )
        else:
            self._transport = _StreamableHTTPTransport(
                spec, timeout_s=self._timeout_s, client=client
            )

    def _id(self) -> int:
        self._next_id += 1
        return self._next_id

    async def connect(self) -> None:
        """open + initialize + notifications/initialized (5s default)."""
        await self._transport.open()
        try:
            init = _rpc_request(
                self._id(),
                "initialize",
                {
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": {},
                    "clientInfo": CLIENT_INFO,
                },
            )
            await self._transport.request(init)
            await self._transport.request(_rpc_notification("notifications/initialized"))
        except Exception:
            await self.aclose()
            raise

    async def list_tools(self) -> list[dict[str, Any]]:
        result = await self._transport.request(_rpc_request(self._id(), "tools/list"))
        tools = result.get("tools")
        if not isinstance(tools, list):
            raise MCPError("tools/list: result.tools is not a list")
        return [t for t in tools if isinstance(t, dict) and t.get("name")]

    async def call_tool(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        return await self._transport.request(
            _rpc_request(self._id(), "tools/call", {"name": name, "arguments": arguments})
        )

    async def aclose(self) -> None:
        await self._transport.aclose()


# ----------------------------------------------------------------- the tool
def _normalise_call_result(result: dict[str, Any]) -> dict[str, Any]:
    """Map an MCP tools/call result to the plugin-tool result shape."""
    is_error = bool(result.get("isError"))
    body: Any = ""
    content = result.get("content")
    if isinstance(content, list):
        texts = [
            str(c.get("text", ""))
            for c in content
            if isinstance(c, dict) and c.get("type") == "text"
        ]
        body = "\n".join(t for t in texts if t)
    structured = result.get("structuredContent")
    if isinstance(structured, (dict, list)):
        body = structured
    elif isinstance(body, str) and body.strip():
        try:
            body = json.loads(body)
        except json.JSONDecodeError:
            body = body[:4000]
    out: dict[str, Any] = {"status": "error" if is_error else "ok", "body": body}
    if is_error:
        out["message"] = (body if isinstance(body, str) else json.dumps(body))[:500] or (
            "MCP tool reported an error"
        )
    return out


class MCPTool:
    """One remote MCP tool exposed as a plugin tool (name/schema/execute)."""

    def __init__(
        self,
        spec: MCPServerSpec,
        tool: dict[str, Any],
        *,
        runner: AsyncToolRunner | None = None,
        connect_timeout_s: float | None = None,
        client_factory: Callable[..., MCPClient] | None = None,
    ) -> None:
        self.server = spec.name
        self.remote_name = str(tool.get("name") or "").strip()
        # Namespaced so a remote tool can never shadow a built-in tool.
        self.name = f"mcp__{spec.name}__{self.remote_name}"[:MAX_TOOL_NAME]
        self.description = str(tool.get("description") or "").strip() or (
            f"MCP tool {self.remote_name} (server {spec.name})"
        )
        input_schema = tool.get("inputSchema")
        # JSON-schema passthrough; fall back to an open object schema.
        self.input_schema = (
            input_schema
            if isinstance(input_schema, dict) and input_schema.get("type") == "object"
            else {"type": "object", "properties": {}, "required": []}
        )
        self._spec = spec
        self._connect_timeout_s = connect_timeout_s
        self._client_factory = client_factory
        self._runner = runner or AsyncToolRunner(timeout_s=_tool_timeout_s())

    def schema(self) -> dict[str, Any]:
        """OpenAI-format tool schema with JSON-schema passthrough."""
        return {
            "type": "function",
            "function": {
                "name": self.name,
                "description": self.description,
                "parameters": self.input_schema,
            },
        }

    async def execute(self, arguments: dict[str, Any]) -> dict[str, Any]:
        """tools/call routed through the AsyncToolRunner hard timeout —
        a timeout/failure resolves to the spoken-apology payload."""

        async def _call() -> dict[str, Any]:
            client = (
                self._client_factory(self._spec)
                if self._client_factory is not None
                else MCPClient(self._spec, timeout_s=self._connect_timeout_s)
            )
            try:
                await client.connect()
                result = await client.call_tool(self.remote_name, dict(arguments))
            finally:
                await client.aclose()
            return _normalise_call_result(result)

        return await self._runner.run(self.name, _call)


# ---------------------------------------------------------------- building
async def build_mcp_tools(
    tenant_ctx: Any = None,
    *,
    servers: list[MCPServerSpec] | None = None,
    connect_timeout_s: float | None = None,
    runner: AsyncToolRunner | None = None,
    client_factory: Callable[..., MCPClient] | None = None,
) -> list[MCPTool]:
    """Handshake each configured server and build namespaced tool defs.

    ANY server failure (connect, handshake, tools/list) logs a warning and
    skips that server — the session never breaks.
    """
    specs = servers if servers is not None else merged_servers(tenant_ctx)
    timeout_s = connect_timeout_s if connect_timeout_s is not None else _connect_timeout_s()
    tools: list[MCPTool] = []
    for spec in specs:
        try:
            client = (
                client_factory(spec)
                if client_factory is not None
                else MCPClient(spec, timeout_s=timeout_s)
            )
            try:
                await asyncio.wait_for(client.connect(), timeout=timeout_s)
                remote_tools = await asyncio.wait_for(client.list_tools(), timeout=timeout_s)
            finally:
                await client.aclose()
        except Exception as exc:  # noqa: BLE001 - skip the server, never fatal
            log.warning(
                "mcp server skipped",
                server=spec.name,
                url=spec.url,
                error=str(exc)[:200],
            )
            continue
        for tool in remote_tools:
            mcp_tool = MCPTool(
                spec,
                tool,
                runner=runner,
                connect_timeout_s=timeout_s,
                client_factory=client_factory,
            )
            log.info(
                "mcp tool registered",
                server=spec.name,
                tool=mcp_tool.name,
            )
            tools.append(mcp_tool)
    return tools


# -------------------------------------------- sync bridge (plugin_tools.py)
_cache_lock = threading.Lock()
_tools_cache: dict[tuple[str, str], tuple[float, list[MCPTool]]] = {}

_bg_lock = threading.Lock()
_bg_loop: asyncio.AbstractEventLoop | None = None


def _background_loop() -> asyncio.AbstractEventLoop:
    """Dedicated loop thread for handshakes triggered from a running loop
    (httpx clients are loop-bound, so the caller's loop cannot be reused)."""
    global _bg_loop
    with _bg_lock:
        if _bg_loop is None or _bg_loop.is_closed():
            loop = asyncio.new_event_loop()
            thread = threading.Thread(
                target=loop.run_forever, name="mcp-handshake", daemon=True
            )
            thread.start()
            _bg_loop = loop
    return _bg_loop


def _run_blocking(coro: Awaitable[list[MCPTool]], timeout_s: float) -> list[MCPTool]:
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(asyncio.wait_for(coro, timeout_s))
    future = asyncio.run_coroutine_threadsafe(coro, _background_loop())
    return future.result(timeout_s)


def clear_mcp_cache() -> None:
    """Test hook: drop all cached tool defs/failures."""
    with _cache_lock:
        _tools_cache.clear()


def build_mcp_tools_sync(tenant_ctx: Any = None) -> list[MCPTool]:
    """Synchronous, TTL-cached wrapper around build_mcp_tools.

    Called from build_plugin_tools (chat.py must not change). With no MCP
    servers configured this is a no-op. Cold-cache handshakes are bounded by
    the connect timeout; failures are cached for a short window so a down
    server cannot stall every turn.
    """
    specs = merged_servers(tenant_ctx)
    if not specs:
        return []
    timeout_s = _connect_timeout_s()
    now = time.monotonic()
    tools: list[MCPTool] = []
    missing: list[MCPServerSpec] = []
    with _cache_lock:
        for spec in specs:
            key = (spec.name, spec.url)
            hit = _tools_cache.get(key)
            if hit is not None and hit[0] > now:
                tools.extend(hit[1])
            else:
                missing.append(spec)
    for spec in missing:
        key = (spec.name, spec.url)
        try:
            fetched = _run_blocking(
                build_mcp_tools(servers=[spec], connect_timeout_s=timeout_s),
                timeout_s=timeout_s * 2 + 2,
            )
        except Exception as exc:  # noqa: BLE001 - skip the server, never fatal
            log.warning(
                "mcp server skipped",
                server=spec.name,
                url=spec.url,
                error=str(exc)[:200],
            )
            fetched = []
        ttl = _cache_ttl_s() if fetched else _failure_ttl_s()
        with _cache_lock:
            _tools_cache[key] = (time.monotonic() + ttl, fetched)
        tools.extend(fetched)
    return tools
