"""Tolerant structured parse (SPEC-W53: nulls allowed, never raises)."""

from __future__ import annotations

from app.llm import parse_model_answer


def test_fenced_json_receipt():
    text, fields = parse_model_answer(
        "# R\n\nSome Store\n\n```json\n{\"vendor\": \"Some Store\", \"amount\": 10.5,"
        " \"date\": \"2024-02-01\", \"items\": []}\n```",
        "receipt",
    )
    assert "Some Store" in text
    assert "```" not in text
    assert fields == {
        "vendor": "Some Store",
        "amount": 10.5,
        "currency": None,
        "date": "2024-02-01",
        "items": [],
    }


def test_bare_json_tail():
    text, fields = parse_model_answer(
        'markdown body\n{"vendor": "V", "amount": "1,234.50", "date": null, "items": null}',
        "receipt",
    )
    assert fields["vendor"] == "V"
    assert fields["amount"] == 1234.50  # string-with-commas tolerated
    assert fields["date"] is None
    assert fields["items"] == []  # non-list -> empty list


def test_junk_answer_degrades_to_nulls():
    text, fields = parse_model_answer("sorry, I cannot read this image", "receipt")
    assert text == "sorry, I cannot read this image"
    assert fields == {
        "vendor": None,
        "amount": None,
        "currency": None,
        "date": None,
        "items": [],
    }


def test_broken_json_degrades_to_nulls():
    text, fields = parse_model_answer('notes\n{"vendor": "unterminated', "receipt")
    assert fields["vendor"] is None


def test_document_kind_shape():
    _, fields = parse_model_answer(
        '{"title": "Lease", "date": "2023-11-30", "summary": "Tenancy agreement."}',
        "knowledge_scan",
    )
    assert fields == {
        "title": "Lease",
        "date": "2023-11-30",
        "summary": "Tenancy agreement.",
    }


def test_document_kind_nulls():
    _, fields = parse_model_answer("illegible", "document")
    assert fields == {"title": None, "date": None, "summary": None}
