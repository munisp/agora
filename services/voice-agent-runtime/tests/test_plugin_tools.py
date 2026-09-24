"""Plugin tools MVP tests (SPEC-W3 §4, innovation 15): template substitution,
SSRF guard, execution via an injected httpx client."""

from __future__ import annotations

import httpx
import pytest

from app import plugin_tools
from app.plugin_tools import (
    PluginTool,
    PluginToolError,
    build_plugin_tools,
    parse_allowed_hosts,
    render_template,
    template_variables,
)

ALLOWED = parse_allowed_hosts("booking,knowledge,identity")


def _mock_client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


def test_render_template_substitution():
    assert render_template("http://booking:7002/x/{{offering_id}}", {"offering_id": "42"}) == \
        "http://booking:7002/x/42"
    # unknown vars render empty
    assert render_template("a-{{nope}}-b", {}) == "a--b"


def test_template_variables():
    assert template_variables("{{a}}/{{ b }}/{{a}}") == ["a", "b"]


def test_parse_allowed_hosts():
    assert parse_allowed_hosts(" booking , identity,") == {"booking", "identity"}


async def test_get_tool_executes_with_query_params():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        return httpx.Response(200, json={"slots": ["10:00"]})

    tool = PluginTool(
        {
            "name": "check_calendar_availability",
            "description": "Check open slots",
            "method": "GET",
            "url": "http://booking:7002/public/sites/{{site_slug}}/availability",
        },
        allowed_hosts=ALLOWED,
        context={"site_slug": "demo"},
        client=_mock_client(handler),
    )
    result = await tool.execute({"offering_id": "o1", "team_member_id": "m1"})
    assert result["status"] == "ok"
    assert result["body"] == {"slots": ["10:00"]}
    assert seen["url"].startswith("http://booking:7002/public/sites/demo/availability")
    assert "offering_id=o1" in seen["url"] and "team_member_id=m1" in seen["url"]


async def test_post_tool_renders_body_template():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = request.content.decode()
        seen["content_type"] = request.headers.get("content-type")
        return httpx.Response(200, json={"ok": True})

    tool = PluginTool(
        {
            "name": "create_ticket",
            "description": "Create a support ticket",
            "method": "POST",
            "url": "http://identity:7001/v1/tickets",
            "bodyTemplate": '{"title": "{{title}}", "tenant": "{{tenant_slug}}"}',
        },
        allowed_hosts=ALLOWED,
        context={"tenant_slug": "acme"},
        client=_mock_client(handler),
    )
    result = await tool.execute({"title": "Broken login"})
    assert result["status"] == "ok"
    assert seen["body"] == '{"title": "Broken login", "tenant": "acme"}'
    assert seen["content_type"] == "application/json"


def test_ssrf_guard_blocks_at_registration():
    with pytest.raises(PluginToolError):
        PluginTool(
            {"name": "evil", "description": "", "method": "GET", "url": "http://169.254.169.254/latest"},
            allowed_hosts=ALLOWED,
        )


async def test_ssrf_guard_blocks_at_execution():
    # host looks static-safe at registration but an argument rewrites it
    tool = PluginTool(
        {"name": "fetch", "description": "", "method": "GET", "url": "http://{{host}}:7002/x"},
        allowed_hosts=ALLOWED,
    )

    def handler(request: httpx.Request) -> httpx.Response:  # pragma: no cover
        return httpx.Response(200, json={})

    tool._client = _mock_client(handler)
    result = await tool.execute({"host": "evil.example.com"})
    assert result["status"] == "error"
    assert "SSRF" in result["message"]


def test_schema_derives_properties_from_template_vars():
    tool = PluginTool(
        {
            "name": "check",
            "description": "Check slots",
            "method": "GET",
            "url": "http://booking:7002/public/sites/{{site_slug}}/availability",
        },
        allowed_hosts=ALLOWED,
        context={"site_slug": "demo"},
    )
    schema = tool.schema()
    assert schema["function"]["name"] == "check"
    # context vars are not exposed as LLM-fillable params
    assert schema["function"]["parameters"]["properties"] == {}


def test_invalid_name_rejected():
    with pytest.raises(PluginToolError):
        PluginTool(
            {"name": "bad name!", "description": "", "method": "GET", "url": "http://booking:7002/x"},
            allowed_hosts=ALLOWED,
        )


def test_invalid_method_rejected():
    with pytest.raises(PluginToolError):
        PluginTool(
            {"name": "t", "description": "", "method": "TRACE", "url": "http://booking:7002/x"},
            allowed_hosts=ALLOWED,
        )


def test_build_plugin_tools_skips_invalid():
    tools = build_plugin_tools(
        [
            {"name": "good", "description": "", "method": "GET", "url": "http://booking:7002/x"},
            {"name": "bad", "description": "", "method": "GET", "url": "http://evil.com/x"},
            {"name": "", "description": "", "method": "GET", "url": "http://booking:7002/x"},
        ],
        allowed_hosts_raw="booking,knowledge,identity",
    )
    assert [t.name for t in tools] == ["good"]


async def test_http_error_status_surfaced():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(404, json={"error": "not found"})

    tool = PluginTool(
        {"name": "t", "description": "", "method": "GET", "url": "http://booking:7002/nope"},
        allowed_hosts=ALLOWED,
        client=_mock_client(handler),
    )
    result = await tool.execute({})
    assert result["status"] == "error"
    assert "404" in result["message"]


async def test_network_failure_is_graceful():
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused")

    tool = PluginTool(
        {"name": "t", "description": "", "method": "GET", "url": "http://booking:7002/x"},
        allowed_hosts=ALLOWED,
        client=_mock_client(handler),
    )
    result = await tool.execute({})
    assert result["status"] == "error"
    assert "request failed" in result["message"]


async def test_suffix_host_allowed():
    tool = PluginTool(
        {"name": "t", "description": "", "method": "GET", "url": "http://eu.booking:7002/x"},
        allowed_hosts=parse_allowed_hosts("booking"),
    )

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"ok": 1})

    tool._client = _mock_client(handler)
    result = await tool.execute({})
    assert result["status"] == "ok"


# ------------------------------------------- SPEC-W46 P11: shared client
async def test_plugin_tools_share_process_client_when_not_injected(monkeypatch):
    """Without an injected client, executions go through ONE shared
    AsyncClient (no per-call connect/close)."""
    requests: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(str(request.url))
        return httpx.Response(200, json={"ok": True})

    shared = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    monkeypatch.setattr(plugin_tools, "_shared_client", shared)
    tool_a = PluginTool(
        {"name": "tool_a", "method": "GET", "url": "http://booking:7002/a"},
        allowed_hosts={"booking"},
    )
    tool_b = PluginTool(
        {"name": "tool_b", "method": "GET", "url": "http://booking:7002/b"},
        allowed_hosts={"booking"},
    )
    ra = await tool_a.execute({})
    rb = await tool_b.execute({})
    assert ra["status"] == rb["status"] == "ok"
    assert requests == ["http://booking:7002/a", "http://booking:7002/b"]
    # Neither tool owns a client; both rode the shared one.
    assert tool_a._client is None and tool_b._client is None


def test_shared_http_client_is_singleton():
    from app.plugin_tools import _shared_http_client

    assert _shared_http_client() is _shared_http_client()
