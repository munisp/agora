"""SPEC-W9 Part C integration: pack-level `mcpServers` must reach
mcp_client.build_mcp_tools through the chat tool-assembly path.

Covers the two additive hooks:
- tenant_context._apply_pack surfaces pack.mcpServers as ctx.mcp_servers
  (mirroring customTools), and
- chat._prepare_turn passes tenant_ctx=ctx into build_plugin_tools.
"""

from __future__ import annotations

from types import SimpleNamespace

from app import chat as chat_module
from app import plugin_tools as plugin_tools_module
from app.chat import ChatService
from app.config import Settings
from app.mcp_client import tenant_mcp_servers
from app.session_state import SessionStore
from app.tenant_context import TenantContext, _apply_pack

from conftest import FakeDapr

PACK_MCP = [{"name": "n8n", "url": "https://n8n.example.com/sse"}]


# ------------------------------------------------------- tenant context
def test_apply_pack_surfaces_mcp_servers():
    ctx = TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")
    _apply_pack(ctx, {"pack": {"mcpServers": PACK_MCP}})
    assert ctx.mcp_servers == PACK_MCP
    # …and the MCP client can extract a validated spec from the attribute.
    specs = tenant_mcp_servers(ctx)
    assert [s.name for s in specs] == ["n8n"]
    assert specs[0].url == "https://n8n.example.com/sse"


def test_apply_pack_mcp_servers_defensive():
    ctx = TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")
    _apply_pack(ctx, {"pack": {"mcpServers": "not-a-list"}})
    assert ctx.mcp_servers == []
    _apply_pack(ctx, {"pack": {"mcpServers": [{"name": "ok", "url": "https://x"}, "junk", 42]}})
    assert ctx.mcp_servers == [{"name": "ok", "url": "https://x"}]

    # No pack / no payload at all: defaults are left untouched.
    fresh = TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")
    _apply_pack(fresh, {})
    assert fresh.mcp_servers == []
    _apply_pack(fresh, None)  # type: ignore[arg-type]
    assert fresh.mcp_servers == []


# ------------------------------------------------------ chat tool assembly
class FakeMcpTool:
    """Stands in for a ToolDef produced by build_mcp_tools."""

    name = "mcp__n8n__lookup_order"

    def schema(self):
        return {
            "type": "function",
            "function": {"name": self.name, "parameters": {"type": "object"}},
        }

    async def execute(self, arguments):
        return {"status": "ok"}


class FakeLLM:
    def __init__(self):
        self.tools_seen: list[list[str]] = []

    async def chat_with_tools(self, messages, tools):
        self.tools_seen.append([t["function"]["name"] for t in tools])
        return SimpleNamespace(content="done", tool_calls=[])


async def test_chat_passes_tenant_ctx_to_plugin_tools(monkeypatch):
    """A pack mcpServers entry on the tenant context flows through
    chat._prepare_turn -> build_plugin_tools -> build_mcp_tools."""
    captured: list = []

    def fake_build_mcp_tools_sync(tenant_ctx=None):
        captured.append(tenant_ctx)
        # Only "connect" when the tenant context actually carries servers.
        if tenant_ctx is not None and getattr(tenant_ctx, "mcp_servers", None):
            return [FakeMcpTool()]
        return []

    monkeypatch.setattr(
        plugin_tools_module, "build_mcp_tools_sync", fake_build_mcp_tools_sync
    )

    ctx = TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")
    ctx.mcp_servers = list(PACK_MCP)

    async def fake_fetch(dapr, settings, site_slug):
        return ctx

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)

    llm = FakeLLM()
    service = ChatService(
        settings=Settings(),
        dapr=FakeDapr(),  # type: ignore[arg-type]
        llm=llm,  # type: ignore[arg-type]
        sessions=SessionStore(),
    )
    resp = await service.handle_message(
        site_slug="demo", message="hi", conversation_id=None
    )

    assert resp["reply"] == "done"
    # build_plugin_tools received THIS tenant context (not None)…
    assert captured and captured[0] is ctx
    # …so the pack MCP tool was registered and offered to the LLM.
    assert "mcp__n8n__lookup_order" in llm.tools_seen[0]

