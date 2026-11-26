"""The Segment Builder previews UNSAVED filters in its own flat shape (no name; has_consent is the purpose).
Same gates as the full create body; a preview without any consent purpose is still refused."""

from __future__ import annotations

from conftest import HDR_A

PREVIEW_URL = "/v1/graph/segments/count"


def test_builder_flat_shape_counts_like_the_full_body(client):
    flat = client.post(PREVIEW_URL, json={"has_consent": "marketing", "include_quarantined": False}, headers=HDR_A)
    full = client.post(PREVIEW_URL, json={"name": "x", "purpose": "marketing", "filter": {}}, headers=HDR_A)
    assert flat.status_code == 200, flat.text
    assert flat.json() == full.json() == {"count": 3}  # quarantined pa4 still excluded


def test_builder_shape_without_a_consent_purpose_is_refused(client):
    assert client.post(PREVIEW_URL, json={"include_quarantined": False}, headers=HDR_A).status_code == 422
