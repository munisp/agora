"""MCP client tests (SPEC-W9 Part C): initialize handshake, tools/list ->
ToolDef shape, tools/call round-trip, server-down graceful skip, namespacing,
env/pack server parsing, and the sync bridge used by build_plugin_tools.

Fake MCP servers run on httpx.MockTransport — no network, no new deps.
"""

from __future__ import annotations

import asyncio
import json

import httpx
import pytest

from app import mcp_client
from app.async_tools import AsyncToolRunner
from app.mcp_client import (
    MCPClient,
    MCPServerSpec,
    MCPTool,
    build_mcp_tools,
    build_mcp_tools_sync,
    clear_mcp_cache,
    merged_servers,
    parse_env_servers,
    parse_sse_body,
    parse_server_spec,
    tenant_mcp_servers,
)
from app.plugin_tools import build_plugin_tools

N8N_TOOLS = [
    {
        "name": "create_record",
        "description": "Create a CRM record",
        "inputSchema": {
            "type": "object",
            "properties": {"email": {"type": "string"}},
            "required": ["email"],
        },
    },
    {
        "name": "lookup_order",
        "description": "Look up an order",
        "inputSchema": {"type": "object", "properties": {"id": {"type": "string"}}},
    },
]

CALL_RESULT = {
    "content": [{"type": "text", "text": '{"record_id": "r-1", "created": true}'}]
}


def _mock_client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


# ------------------------------------------------- fake streamable-HTTP server
class FakeStreamableServer:
    """2025-03-26 streamable HTTP: POST per message, JSON (or SSE) replies."""

    def __init__(self, *, sse: bool = False, fail: bool = False) -> None:
        self.sse = sse
        self.fail = fail
        self.messages: list[dict] = []
        self.session_header_seen: list[str] = []

    def _reply(self, message: dict) -> httpx.Response:
        body = {"jsonrpc": "2.0", "id": message["id"]}
        if message["method"] == "initialize":
            body["result"] = {
                "protocolVersion": "2025-03-26",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "fake", "version": "0.1"},
            }
        elif message["method"] == "tools/list":
            body["result"] = {"tools": N8N_TOOLS}
        elif message["method"] == "tools/call":
            body["result"] = CALL_RESULT
        else:  # pragma: no cover - defensive
            body["error"] = {"code": -32601, "message": "no such method"}
        headers = {"mcp-session-id": "sess-1"}
        if self.sse:
            payload = f"event: message\ndata: {json.dumps(body)}\n\n"
            return httpx.Response(
                200,
                headers={**headers, "content-type": "text/event-stream"},
                text=payload,
            )
        return httpx.Response(200, json=body, headers=headers)

    def handler(self, request: httpx.Request) -> httpx.Response:
        if self.fail:
            return httpx.Response(503, text="down")
        self.session_header_seen.append(request.headers.get("mcp-session-id", ""))
        message = json.loads(request.content)
        self.messages.append(message)
        if message["method"] == "notifications/initialized":
            assert "id" not in message  # notification, not a request
            return httpx.Response(202)
        return self._reply(message)


def _streamable_client(server: FakeStreamableServer) -> MCPClient:
    spec = MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp")
    return MCPClient(spec, timeout_s=2.0, client=_mock_client(server.handler))


# ------------------------------------------------------------------ handshake
async def test_initialize_handshake_sequence_and_session_header():
    server = FakeStreamableServer()
    client = _streamable_client(server)
    await client.connect()
    await client.list_tools()
    await client.aclose()

    methods = [m["method"] for m in server.messages]
    assert methods == ["initialize", "notifications/initialized", "tools/list"]
    init = server.messages[0]
    assert init["params"]["protocolVersion"] == "2025-03-26"
    assert init["params"]["clientInfo"] == {"name": "opendesk-voice", "version": "1.0"}
    # The mcp-session-id from the initialize response is echoed afterwards.
    assert server.session_header_seen[0] == ""
    assert server.session_header_seen[1:] == ["sess-1", "sess-1"]


async def test_streamable_sse_response_body_parsed():
    server = FakeStreamableServer(sse=True)
    client = _streamable_client(server)
    await client.connect()
    tools = await client.list_tools()
    await client.aclose()
    assert [t["name"] for t in tools] == ["create_record", "lookup_order"]


# ------------------------------------------------------ tools/list -> ToolDef
async def test_build_mcp_tools_namespacing_and_schema_passthrough():
    server = FakeStreamableServer()
    spec = MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp")
    tools = await build_mcp_tools(
        servers=[spec],
        connect_timeout_s=2.0,
        client_factory=lambda s: MCPClient(s, timeout_s=2.0, client=_mock_client(server.handler)),
    )
    assert [t.name for t in tools] == ["mcp__n8n__create_record", "mcp__n8n__lookup_order"]
    schema = tools[0].schema()
    assert schema["type"] == "function"
    assert schema["function"]["name"] == "mcp__n8n__create_record"
    assert schema["function"]["description"] == "Create a CRM record"
    # JSON-schema passthrough, verbatim.
    assert schema["function"]["parameters"] == N8N_TOOLS[0]["inputSchema"]


async def test_tool_name_truncated_to_64_chars():
    spec = MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp")
    tool = MCPTool(spec, {"name": "x" * 100, "description": "d"})
    assert len(tool.name) <= 64
    assert tool.name.startswith("mcp__n8n__")


async def test_missing_input_schema_falls_back_to_open_object():
    spec = MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp")
    tool = MCPTool(spec, {"name": "ping"})
    params = tool.schema()["function"]["parameters"]
    assert params["type"] == "object"
    assert tool.description == "MCP tool ping (server n8n)"


# ------------------------------------------------------------ tools/call path
async def test_tools_call_round_trip_through_runner():
    server = FakeStreamableServer()
    spec = MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp")
    tool = MCPTool(
        spec,
        N8N_TOOLS[0],
        runner=AsyncToolRunner(timeout_s=2.0),
        client_factory=lambda s: MCPClient(s, timeout_s=2.0, client=_mock_client(server.handler)),
    )
    result = await tool.execute({"email": "ada@example.com"})
    assert result["status"] == "ok"
    assert result["body"] == {"record_id": "r-1", "created": True}
    calls = [m for m in server.messages if m["method"] == "tools/call"]
    assert calls[0]["params"] == {"name": "create_record", "arguments": {"email": "ada@example.com"}}


async def test_tools_call_iserror_maps_to_error_status():
    def handler(request: httpx.Request) -> httpx.Response:
        message = json.loads(request.content)
        if "id" not in message:
            return httpx.Response(202)
        if message["method"] == "tools/call":
            return httpx.Response(200, json={
                "jsonrpc": "2.0",
                "id": message["id"],
                "result": {
                    "isError": True,
                    "content": [{"type": "text", "text": "order not found"}],
                },
            })
        return httpx.Response(200, json={"jsonrpc": "2.0", "id": message["id"], "result": {}})

    spec = MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp")
    tool = MCPTool(
        spec,
        N8N_TOOLS[1],
        runner=AsyncToolRunner(timeout_s=2.0),
        client_factory=lambda s: MCPClient(s, timeout_s=2.0, client=_mock_client(handler)),
    )
    result = await tool.execute({"id": "o-9"})
    assert result["status"] == "error"
    assert "order not found" in result["message"]


async def test_tools_call_failure_resolves_to_apology_never_raises():
    spec = MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp")
    tool = MCPTool(
        spec,
        N8N_TOOLS[0],
        runner=AsyncToolRunner(timeout_s=1.0),
        client_factory=lambda s: MCPClient(
            s, timeout_s=1.0, client=_mock_client(FakeStreamableServer(fail=True).handler)
        ),
    )
    result = await tool.execute({"email": "x"})
    assert result["status"] == "error"
    assert "spoken" in result  # AsyncToolRunner apology payload


# ------------------------------------------------------- server-down skipping
async def test_build_mcp_tools_skips_down_server():
    down = FakeStreamableServer(fail=True)
    up = FakeStreamableServer()

    def factory(spec: MCPServerSpec) -> MCPClient:
        server = down if spec.name == "down" else up
        return MCPClient(spec, timeout_s=1.0, client=_mock_client(server.handler))

    tools = await build_mcp_tools(
        servers=[
            MCPServerSpec(name="down", url="https://mcp.example.com/mcp"),
            MCPServerSpec(name="n8n", url="https://mcp.example.com/mcp"),
        ],
        connect_timeout_s=1.0,
        client_factory=factory,
    )
    assert [t.name for t in tools] == ["mcp__n8n__create_record", "mcp__n8n__lookup_order"]


async def test_build_mcp_tools_no_servers_is_noop():
    assert await build_mcp_tools(servers=[]) == []


# --------------------------------------------------------------- legacy SSE
class FakeSSEServer:
    """2024-11-05 HTTP+SSE (n8n .../sse): GET streams endpoint + message
    events; JSON-RPC messages are POSTed to the endpoint (202)."""

    def __init__(self) -> None:
        self.events: asyncio.Queue[bytes | None] = asyncio.Queue()
        self.posts: list[dict] = []

    async def _byte_stream(self):
        while True:
            chunk = await self.events.get()
            if chunk is None:
                return
            yield chunk

    def _respond(self, message: dict) -> None:
        body = {"jsonrpc": "2.0", "id": message["id"]}
        if message["method"] == "initialize":
            body["result"] = {"protocolVersion": "2024-11-05", "capabilities": {}}
        elif message["method"] == "tools/list":
            body["result"] = {"tools": N8N_TOOLS}
        elif message["method"] == "tools/call":
            body["result"] = CALL_RESULT
        self.events.put_nowait(
            f"event: message\ndata: {json.dumps(body)}\n\n".encode()
        )

    async def handler(self, request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            self.events.put_nowait(
                b"event: endpoint\ndata: /messages/?session_id=abc\n\n"
            )
            return httpx.Response(
                200,
                headers={"content-type": "text/event-stream"},
                content=self._byte_stream(),
            )
        message = json.loads(request.content)
        self.posts.append(message)
        assert request.url.path == "/messages/"
        if "id" in message:
            self._respond(message)
        return httpx.Response(202)


async def test_legacy_sse_transport_full_flow():
    server = FakeSSEServer()
    spec = MCPServerSpec(name="n8n", url="https://n8n.example.com/mcp/front-desk/sse")
    client = MCPClient(spec, timeout_s=2.0, client=_mock_client(server.handler))
    await client.connect()
    tools = await client.list_tools()
    result = await client.call_tool("create_record", {"email": "ada@example.com"})
    await client.aclose()

    assert [t["name"] for t in tools] == ["create_record", "lookup_order"]
    assert result == CALL_RESULT
    methods = [m["method"] for m in server.posts]
    assert methods == [
        "initialize",
        "notifications/initialized",
        "tools/list",
        "tools/call",
    ]
    init = server.posts[0]
    assert init["params"]["protocolVersion"] == "2025-03-26"
    assert init["params"]["clientInfo"]["name"] == "opendesk-voice"


async def test_auto_transport_picks_sse_for_sse_suffix_url():
    spec = MCPServerSpec(name="n8n", url="https://n8n.example.com/mcp/x/sse")
    client = MCPClient(spec, timeout_s=1.0, client=_mock_client(FakeSSEServer().handler))
    assert isinstance(client._transport, mcp_client._LegacySSETransport)
    spec2 = MCPServerSpec(name="n8n", url="https://n8n.example.com/mcp/x")
    client2 = MCPClient(spec2, timeout_s=1.0)
    assert isinstance(client2._transport, mcp_client._StreamableHTTPTransport)


def test_parse_sse_body_multiline_and_comments():
    text = ": keepalive\n\nevent: endpoint\ndata: /messages/?s=1\n\nevent: message\ndata: {\"a\":\ndata: 1}\n\n"
    assert parse_sse_body(text) == [
        ("endpoint", "/messages/?s=1"),
        ("message", '{"a":\n1}'),
    ]


# ------------------------------------------------------------- server config
def test_parse_env_servers_valid_and_invalid(monkeypatch):
    raw = json.dumps([
        {"name": "n8n", "url": "https://n8n.example.com/sse",
         "headers": {"authorization": "Bearer x"}},
        {"name": "Bad Name", "url": "https://x.example.com/"},   # bad slug
        {"name": "plain", "url": "http://x.example.com/"},       # not https
        {"name": "crm", "url": "https://crm.example.com/", "transport": "http"},
    ])
    specs = parse_env_servers(raw)
    assert [s.name for s in specs] == ["n8n", "crm"]
    assert specs[0].headers == {"authorization": "Bearer x"}
    assert specs[1].transport == "http"


def test_parse_env_servers_bad_json_and_non_list():
    assert parse_env_servers("{not json") == []
    assert parse_env_servers('{"name": "x"}') == []
    assert parse_env_servers("") == []


def test_parse_server_spec_https_only_and_slug():
    with pytest.raises(mcp_client.MCPError):
        parse_server_spec({"name": "n8n", "url": "http://insecure.example.com/"}, allow_headers=True)
    with pytest.raises(mcp_client.MCPError):
        parse_server_spec({"name": "N8N", "url": "https://x.example.com/"}, allow_headers=True)
    # Packs never carry credentials.
    with pytest.raises(mcp_client.MCPError):
        parse_server_spec(
            {"name": "n8n", "url": "https://x.example.com/", "headers": {"a": "b"}},
            allow_headers=False,
        )


def test_tenant_mcp_servers_from_dict_and_object():
    from_dict = tenant_mcp_servers(
        {"pack": {"mcpServers": [{"name": "n8n", "url": "https://x.example.com/sse"}]}}
    )
    assert [s.name for s in from_dict] == ["n8n"]

    class Ctx:
        mcp_servers = [{"name": "crm", "url": "https://crm.example.com/"}]

    assert [s.name for s in tenant_mcp_servers(Ctx())] == ["crm"]
    assert tenant_mcp_servers(None) == []
    assert tenant_mcp_servers(object()) == []
    # Invalid entries drop out, never raise.
    bad = tenant_mcp_servers({"mcpServers": [{"name": "x", "url": "http://no/"}, "junk"]})
    assert bad == []


def test_merged_servers_env_wins_name_clash(monkeypatch):
    monkeypatch.setenv(
        "MCP_SERVERS",
        json.dumps([{"name": "n8n", "url": "https://env.example.com/sse"}]),
    )
    merged = merged_servers({"mcpServers": [{"name": "n8n", "url": "https://pack.example.com/sse"},
                                            {"name": "crm", "url": "https://crm.example.com/"}]})
    by_name = {s.name: s.url for s in merged}
    assert by_name == {"n8n": "https://env.example.com/sse", "crm": "https://crm.example.com/"}


# ------------------------------------------------- sync bridge + plugin wiring
@pytest.fixture(autouse=True)
def _clean_mcp_state(monkeypatch):
    monkeypatch.delenv("MCP_SERVERS", raising=False)
    clear_mcp_cache()
    yield
    clear_mcp_cache()


def test_build_mcp_tools_sync_no_config_is_noop():
    assert build_mcp_tools_sync() == []


def test_build_plugin_tools_without_mcp_is_byte_compatible():
    tools = build_plugin_tools(
        [
            {
                "name": "check_calendar_availability",
                "description": "Check open slots",
                "method": "GET",
                "url": "http://booking:7002/availability",
            }
        ],
        allowed_hosts_raw="booking,knowledge,identity",
    )
    assert [t.name for t in tools] == ["check_calendar_availability"]


def test_build_mcp_tools_sync_caches_results(monkeypatch):
    calls = []

    async def fake_build(servers, connect_timeout_s):
        calls.append(servers[0].name)
        spec = servers[0]
        return [MCPTool(spec, N8N_TOOLS[0], runner=AsyncToolRunner(timeout_s=1.0))]

    monkeypatch.setattr(mcp_client, "build_mcp_tools", fake_build)
    monkeypatch.setenv(
        "MCP_SERVERS", json.dumps([{"name": "n8n", "url": "https://x.example.com/sse"}])
    )
    first = build_mcp_tools_sync()
    second = build_mcp_tools_sync()
    assert [t.name for t in first] == ["mcp__n8n__create_record"]
    assert [t.name for t in second] == ["mcp__n8n__create_record"]
    assert calls == ["n8n"]  # second call served from the TTL cache


def test_build_mcp_tools_sync_failure_skips_and_short_caches(monkeypatch):
    calls = []

    async def failing_build(servers, connect_timeout_s):
        calls.append(servers[0].name)
        raise ConnectionError("unreachable")

    monkeypatch.setattr(mcp_client, "build_mcp_tools", failing_build)
    monkeypatch.setenv(
        "MCP_SERVERS", json.dumps([{"name": "n8n", "url": "https://x.example.com/sse"}])
    )
    assert build_mcp_tools_sync() == []  # never raises into the session
    assert build_mcp_tools_sync() == []  # failure cached (no second handshake)
    assert calls == ["n8n"]


async def test_build_mcp_tools_sync_inside_running_loop(monkeypatch):
    """The chat path calls build_plugin_tools from inside an event loop — the
    handshake must run on the background loop thread, not block/error."""

    async def fake_build(servers, connect_timeout_s):
        await asyncio.sleep(0)  # proves the coroutine actually runs on a loop
        return [MCPTool(servers[0], N8N_TOOLS[1], runner=AsyncToolRunner(timeout_s=1.0))]

    monkeypatch.setattr(mcp_client, "build_mcp_tools", fake_build)
    monkeypatch.setenv(
        "MCP_SERVERS", json.dumps([{"name": "crm", "url": "https://crm.example.com/"}])
    )
    tools = build_plugin_tools([], allowed_hosts_raw="booking")
    assert [t.name for t in tools] == ["mcp__crm__lookup_order"]

