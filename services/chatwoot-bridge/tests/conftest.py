"""Shared fixtures + fakes for chatwoot-bridge tests (SPEC-W118).

OFFLINE contract: no network, no Kafka, no Postgres (except the pgserver
RLS tests). All external systems are in-memory fakes defined HERE —
production code has no mock paths.
"""

from __future__ import annotations

import sys
import uuid

sys.path.insert(0, ".")

from app.conversations import ConversationNotFound, ConversationServiceError  # noqa: E402

TENANT_A = "11111111-2222-3333-4444-555555555555"
TENANT_B = "99999999-8888-7777-6666-555555555555"
CONV_ID = str(uuid.uuid4())
BRIDGE_TOKEN = "test-bridge-internal-token"
CONV_TOKEN = "test-conversation-internal-token"


def make_event(
    *,
    role: str = "user",
    text: str = "hello from customer",
    tenant_id: str = TENANT_A,
    conversation_id: str = CONV_ID,
    event_id: str = "evt-0001",
    origin: str | None = None,
) -> dict:
    data = {
        "conversationId": conversation_id,
        "tenantId": tenant_id,
        "role": role,
        "text": text,
        "ts": "2026-11-24T10:00:00+00:00",
        "redacted": True,
    }
    if origin:
        data["origin"] = origin
    return {
        "specversion": "1.0",
        "id": event_id,
        "source": "//opendesk/conversation-service",
        "type": "com.opendesk.conversation.ConversationTurn",
        "subject": "acme",
        "time": "2026-11-24T10:00:00+00:00",
        "tenantid": tenant_id,
        "data": data,
    }


def make_mapping(tenant_id: str = TENANT_A, **overrides) -> dict:
    m = {
        "tenant_id": tenant_id,
        "account_id": 7,
        "inbox_id": 12,
        "api_access_token": "cw-test-token",
        "webhook_token": "whsec-test-token",
        "webhook_id": 55,
        "hmac_secret": None,
        "enabled": True,
    }
    m.update(overrides)
    return m


class FakeStore:
    """In-memory Database stand-in (same method surface)."""

    def __init__(self) -> None:
        self.tenants: dict[str, dict] = {}
        self.links: dict[tuple[str, str], dict] = {}
        self.sent: set[tuple[str, str]] = set()
        self.pinged = False

    async def ping(self) -> bool:
        self.pinged = True
        return True

    async def upsert_tenant(self, tenant_id, *, account_id, inbox_id,
                            api_access_token, webhook_token, webhook_id,
                            hmac_secret):
        existing = self.tenants.get(tenant_id, {})
        row = {
            "tenant_id": tenant_id,
            "account_id": account_id,
            "inbox_id": inbox_id,
            "api_access_token": api_access_token,
            "webhook_token": existing.get("webhook_token", webhook_token),
            "webhook_id": webhook_id,
            "hmac_secret": hmac_secret,
            "enabled": existing.get("enabled", True),
        }
        self.tenants[tenant_id] = row
        return row

    async def get_tenant(self, tenant_id):
        return self.tenants.get(tenant_id)

    async def set_webhook_id(self, tenant_id, webhook_id):
        self.tenants[tenant_id]["webhook_id"] = webhook_id

    async def delete_tenant(self, tenant_id):
        self.tenants.pop(tenant_id, None)
        self.links = {k: v for k, v in self.links.items() if k[0] != tenant_id}
        self.sent = {s for s in self.sent if s[0] != tenant_id}

    async def get_link(self, tenant_id, conversation_id):
        return self.links.get((tenant_id, conversation_id))

    async def insert_link(self, tenant_id, conversation_id, *,
                          chatwoot_conversation_id, chatwoot_contact_id,
                          contact_source_id):
        row = {
            "tenant_id": tenant_id,
            "conversation_id": conversation_id,
            "chatwoot_conversation_id": chatwoot_conversation_id,
            "chatwoot_contact_id": chatwoot_contact_id,
            "contact_source_id": contact_source_id,
        }
        self.links.setdefault((tenant_id, conversation_id), row)
        return self.links[(tenant_id, conversation_id)]

    async def get_link_by_chatwoot_conversation(self, tenant_id, cw_id):
        for (t, _c), row in self.links.items():
            if t == tenant_id and row["chatwoot_conversation_id"] == cw_id:
                return row
        return None

    async def sent_lookup(self, tenant_id, source_id) -> bool:
        return (tenant_id, source_id) in self.sent

    async def sent_insert(self, tenant_id, source_id, chatwoot_message_id):
        self.sent.add((tenant_id, source_id))


class FakeChatwoot:
    """In-memory ChatwootClient stand-in (same method surface)."""

    def __init__(self, *, inbox_valid: bool = True) -> None:
        self.inbox_valid = inbox_valid
        self.fail_next = False
        self.webhooks: list[dict] = []
        self.contacts: list[dict] = []
        self.conversations: list[dict] = []
        self.messages: list[dict] = []
        self._next_id = 1000

    def _id(self) -> int:
        self._next_id += 1
        return self._next_id

    def _maybe_fail(self):
        if self.fail_next:
            from app.client import ChatwootError
            self.fail_next = False
            raise ChatwootError(500, "fake chatwoot outage")

    async def validate_inbox(self, token, account_id, inbox_id) -> bool:
        self._maybe_fail()
        return self.inbox_valid

    async def list_webhooks(self, token, account_id):
        self._maybe_fail()
        return list(self.webhooks)

    async def create_webhook(self, token, account_id, *, url, subscriptions,
                             name="OpenDesk bridge"):
        self._maybe_fail()
        hook = {"id": self._id(), "url": url, "subscriptions": subscriptions,
                "name": name}
        self.webhooks.append(hook)
        return hook["id"]

    async def create_contact(self, token, account_id, *, inbox_id, name,
                             identifier, phone_number=None):
        self._maybe_fail()
        cid = self._id()
        self.contacts.append({
            "id": cid, "inbox_id": inbox_id, "name": name,
            "identifier": identifier, "phone_number": phone_number,
        })
        return cid, f"src-{cid}"

    async def create_conversation(self, token, account_id, *, source_id,
                                  inbox_id, opendesk_conversation_id):
        self._maybe_fail()
        cid = self._id()
        self.conversations.append({
            "id": cid, "source_id": source_id, "inbox_id": inbox_id,
            "opendesk_conversation_id": opendesk_conversation_id,
        })
        return cid

    async def create_message(self, token, account_id, conversation_id, *,
                             content, message_type, source_id):
        self._maybe_fail()
        mid = self._id()
        self.messages.append({
            "id": mid, "conversation_id": conversation_id,
            "content": content, "message_type": message_type,
            "source_id": source_id,
        })
        return {"id": mid}


_UNSET = object()


class FakeConvClient:
    """In-memory ConversationServiceClient stand-in."""

    def __init__(self, *, conversation: dict | None | object = _UNSET) -> None:
        if conversation is _UNSET:
            conversation = {
                "id": CONV_ID,
                "tenant_id": TENANT_A,
                "site_slug": "acme",
                "channel": "whatsapp",
                "contact_phone": "+2348012345678",
                "started_at": "2026-11-24T09:00:00+00:00",
                "ended_at": None,
            }
        self.conversation = conversation
        self.fail_append = False
        self.appended: list[dict] = []
        self.seen_idem: set[str] = set()

    async def get_conversation(self, conversation_id, tenant_id):
        if self.conversation is None:
            raise ConversationNotFound(conversation_id)
        return self.conversation

    async def append_agent_turn(self, conversation_id, tenant_id, *, text,
                                idempotency_key):
        if self.fail_append:
            raise ConversationServiceError(503, "fake conversation outage")
        self.appended.append({
            "conversation_id": conversation_id,
            "tenant_id": tenant_id,
            "text": text,
            "idempotency_key": idempotency_key,
        })
        created = idempotency_key not in self.seen_idem
        self.seen_idem.add(idempotency_key)
        return {"created": created}
