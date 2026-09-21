"""Docs/scripts consistency guards (SPEC-W45 CODER-J).

Self-contained (no conftest): asserts the W45 doc/script rewrites don't
regress — seed-demo.sh must not reference retired dev-bypass flags,
channels-ussd.md must document the authenticated callback path, and the new
contract/roadmap docs must exist with their declared content.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def _read(rel: str) -> str:
    return (ROOT / rel).read_text(encoding="utf-8")


# --- OOS-22: seed-demo.sh retired-flag guards --------------------------------

def test_seed_demo_has_no_authz_disabled() -> None:
    """The retired dev bypass must not appear in any executable line
    (header comments may explain that it was removed)."""
    for line in _read("scripts/seed-demo.sh").splitlines():
        if line.lstrip().startswith("#"):
            continue
        assert "AUTHZ_DISABLED" not in line, line


def test_seed_demo_has_no_direct_service_posts() -> None:
    """No curl calls against host-direct service ports (W34 GF4: the ports
    are not host-published; the gateway is the only entry)."""
    src = _read("scripts/seed-demo.sh")
    for line in src.splitlines():
        if line.lstrip().startswith("#"):
            continue  # comments may explain the retired posture
        assert not re.search(r"localhost:700[128]", line), line
        assert "http://identity:" not in line and "http://booking:" not in line


def test_seed_demo_goes_through_gateway_with_jwt() -> None:
    src = _read("scripts/seed-demo.sh")
    assert 'GATEWAY:-http://localhost:9080' in src
    assert "$GATEWAY/api/identity/v1/tenants" in src
    assert "$GATEWAY/api/bookings/v1/offerings" in src
    assert "$GATEWAY/api/knowledge/v1/documents" in src
    assert "Authorization: Bearer" in src
    assert "openid-connect/token" in src


def test_seed_demo_does_not_set_paid_plan() -> None:
    """Plan is server-forced to free for non-platform-admins (SPEC-W44
    W-I-1); the seed script must not request a plan."""
    src = _read("scripts/seed-demo.sh")
    assert '"plan":' not in src
    assert "plan=pro" not in src and '"pro"' not in src


def test_local_dev_runbook_matches_seed_reality() -> None:
    doc = _read("docs/runbooks/local-dev.md")
    assert "Hits services DIRECTLY" not in doc
    assert "AUTHZ_DISABLED" in doc  # only as the removed-bypass note
    assert "was\nremoved" in doc or "removed in W34" in doc


# --- K14: USSD callback path --------------------------------------------------

def test_channels_ussd_documents_authenticated_callback() -> None:
    doc = _read("docs/channels-ussd.md")
    assert "/ussd/callback/" in doc
    assert "AT_CALLBACK_SECRET" in doc
    # The retired route may only appear as an explicitly-retired mention.
    for line in doc.splitlines():
        if "webhooks/ussd" in line:
            assert "retired" in line, line


# --- ORPH O7-O10 / roadmap: new docs exist with declared content --------------

def test_event_contracts_doc_declares_streams() -> None:
    doc = _read("docs/event-contracts.md")
    for topic in (
        "opendesk.helpdesk.events.v1",
        "opendesk.fsm.events.v1",
        "opendesk.loyalty.events.v1",
        "opendesk.studio.events.v1",
        "opendesk.crm.events.v1",
        "opendesk.surveys.events.v1",
        "opendesk.workforce.events.v1",
        "opendesk.social.events.v1",
        "opendesk.billing.events",
        "opendesk.apps.lifecycle.v1",
        "opendesk.graph.erasure.done.v1",
        "opendesk.identity.events",
    ):
        assert topic in doc, topic
    # The create-topics.sh comment drift note must be present (O9).
    assert "AppStatusChanged" in doc


def test_deferred_roadmap_register_exists() -> None:
    doc = _read("docs/roadmap/W45-deferred.md")
    for item in ("Franchise", "Multi-currency", "FIRS", "SSO", "2FA",
                 "Cross-tenant", "Outbound voice", "signing", "ACLs",
                 "cost quotas", "Recurring bookings"):
        assert item in doc, item
