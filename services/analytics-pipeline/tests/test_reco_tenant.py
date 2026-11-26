"""2026-11-21: recommendations accept the business slug (header or ?tenant=) and resolve it to the UUID."""
from analytics_pipeline.server import _is_uuid


def test_is_uuid():
    assert _is_uuid("bb69e5fd-aa86-4d4b-9d86-4cfe5c33ae52")
    assert not _is_uuid("leadway-pensure")
    assert not _is_uuid("")
