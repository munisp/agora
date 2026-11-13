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


# ---------------------------------------------------------------------------
# SPEC-W58 A3/P9: per-kind KYC whitelist parse + ICAO 9303 MRZ vectors (P7)
# ---------------------------------------------------------------------------
from app.llm import MOCK_KYC_PASSPORT_MRZ, parse_model_answer, validate_mrz  # noqa: E402

# ICAO 9303 Doc 9303 part 3 worked example (Utopia specimen — published
# standard test vector, not a real person).
ICAO_UTO_MRZ = (
    "P<UTOERIKSSON<<ANNA<MARIA<<<<<<<<<<<<<<<<<<<\n"
    "L898902C36UTO7408122F1204159ZE184226B<<<<<10"
)


def test_kyc_nin_slip_whitelist_drops_extras():
    _, fields = parse_model_answer(
        '{"full_name": "ADAEZE OKAFOR", "dob": "1990-04-12", "nin": "12345678901",'
        ' "address": "12 Marina Rd", "bvn": "should-be-dropped", "photo": "x"}',
        "kyc_nin_slip",
    )
    assert fields == {
        "full_name": "ADAEZE OKAFOR",
        "dob": "1990-04-12",
        "nin": "12345678901",
        "address": "12 Marina Rd",
    }


def test_kyc_whitelist_null_fallback_never_raises():
    _, fields = parse_model_answer("completely illegible", "kyc_drivers_license")
    assert fields == {
        "full_name": None,
        "dob": None,
        "license_no": None,
        "expiry": None,
    }


def test_kyc_voters_card_optional_dob():
    _, fields = parse_model_answer(
        '{"full_name": "A", "vin": "V1"}', "kyc_voters_card"
    )
    assert fields == {"full_name": "A", "vin": "V1", "dob": None}


def test_kyc_cac_document_shape():
    _, fields = parse_model_answer(
        '{"company_name": "ACME LTD", "rc_number": 1234567,'
        ' "registration_date": "2015-03-20", "address": null, "extra": 1}',
        "kyc_cac_document",
    )
    assert fields == {
        "company_name": "ACME LTD",
        "rc_number": "1234567",  # numbers tolerated, coerced to str
        "registration_date": "2015-03-20",
        "address": None,
    }


def test_mrz_icao_reference_vector_valid():
    assert validate_mrz(ICAO_UTO_MRZ) is True


def test_mrz_synthetic_fixture_valid():
    assert validate_mrz(MOCK_KYC_PASSPORT_MRZ) is True


def test_mrz_check_digit_tamper_invalid():
    # flip the composite check digit (last char)
    tampered = ICAO_UTO_MRZ[:-1] + "1"
    assert validate_mrz(tampered) is False
    # flip the passport-number check digit (char 10 of line 2)
    line1, line2 = ICAO_UTO_MRZ.splitlines()
    bad = line1 + "\n" + line2[:9] + ("7" if line2[9] != "7" else "8") + line2[10:]
    assert validate_mrz(bad) is False


def test_mrz_absent_is_none_not_false():
    assert validate_mrz(None) is None
    assert validate_mrz("") is None
    _, fields = parse_model_answer('{"full_name": "J DOE"}', "kyc_passport")
    assert fields["mrz_valid"] is None


def test_mrz_malformed_is_false():
    assert validate_mrz("NOT AN MRZ") is False
    assert validate_mrz("P<UTO" + "X" * 39) is False  # one line only


def test_mrz_transliteration_vector():
    """Transliterated names (ICAO 9303 §6, e.g. Ü -> U, Å -> AA) do not
    affect check digits: line 1 is not check-summed, so a transliterated
    surname keeps a VALID mrz_valid verdict."""
    line1 = "P<NGAMUELLER<<ANNA<MARIA<<<<<<<<<<<<<<<<<<<"  # MUELLER <- MÜLLER
    line1 = line1.ljust(44, "<")[:44]
    _, line2 = MOCK_KYC_PASSPORT_MRZ.splitlines()
    assert validate_mrz(line1 + "\n" + line2) is True


def test_kyc_passport_mrz_from_model_is_never_trusted():
    """P7: a model-claimed mrz_valid field is ignored; the code verdict
    (here: False for a tampered MRZ) wins."""
    line1, line2 = MOCK_KYC_PASSPORT_MRZ.splitlines()
    bad = line1 + "\n" + line2[:-1] + ("1" if line2[-1] != "1" else "2")
    import json as _json

    answer = _json.dumps(
        {"full_name": "J", "passport_no": "A01234569", "mrz": bad, "mrz_valid": True}
    )
    _, fields = parse_model_answer(answer, "kyc_passport")
    assert fields["mrz_valid"] is False
    assert "mrz" not in fields  # raw MRZ not retained in the fields payload
