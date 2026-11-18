"""SPEC-W82 A4.5: per-tenant pronunciation lexicon — unit tests."""

from __future__ import annotations

from app.pronunciation import (
    PronSpec,
    apply_pronunciation,
    merge_lexicons,
    parse_lexicon,
    parse_tenant_lexicons,
)


def test_parse_lexicon_string_and_dict_forms():
    lex = parse_lexicon(
        {
            "Adaeze": "ah-DAH-ay-zeh",
            "Chukwuemeka": {"respell": "choo-kwoo-eh-MEH-kah", "ipa": "tʃukwuɛmɛkɑ"},
        }
    )
    assert lex["Adaeze"].respell == "ah-DAH-ay-zeh"
    assert lex["Chukwuemeka"].ipa == "tʃukwuɛmɛkɑ"


def test_parse_lexicon_drops_malformed():
    lex = parse_lexicon({"ok": "fine", "empty": {}, "bad": 42, "": "x"})
    assert list(lex) == ["ok"]


def test_parse_tenant_lexicons_json_env():
    raw = '{"acme": {"Adaeze": "ah-DAH-ay-zeh"}, "*": {"Naija": "NY-jah"}}'
    parsed = parse_tenant_lexicons(raw)
    assert parsed["acme"]["Adaeze"].respell == "ah-DAH-ay-zeh"
    assert parsed["*"]["Naija"].respell == "NY-jah"
    assert parse_tenant_lexicons("not json") == {}
    assert parse_tenant_lexicons("") == {}
    assert parse_tenant_lexicons(None) == {}


def test_merge_precedence_global_then_tenant_then_pack():
    env = {
        "*": {"Naija": PronSpec(respell="NY-jah"), "Musa": PronSpec(respell="MOO-sah")},
        "acme": {"Musa": PronSpec(respell="moo-SAH")},
    }
    pack = {"Naija": PronSpec(respell="NYE-jah")}
    merged = merge_lexicons(env, "acme", pack)
    # Pack wins over env-*; tenant-slug wins over env-*; pack wins overall.
    assert merged["Naija"].respell == "NYE-jah"
    assert merged["Musa"].respell == "moo-SAH"
    # Unknown tenant still gets the global entries.
    other = merge_lexicons(env, "other-tenant", None)
    assert other["Musa"].respell == "MOO-sah"
    assert other["Naija"].respell == "NY-jah"


def test_respell_substitution_default_providers():
    lex = parse_lexicon({"Adaeze": "ah-DAH-ay-zeh"})
    out = apply_pronunciation("Dr. Adaeze will see you now.", lex, provider="mms")
    assert out == "Dr. ah-DAH-ay-zeh will see you now."
    # Provider omitted / piper / spitch: same respell behavior.
    assert apply_pronunciation("Adaeze", lex) == "ah-DAH-ay-zeh"


def test_azure_ssml_phoneme_wrapping():
    lex = parse_lexicon({"Adaeze": {"respell": "ah-DAH-ay-zeh", "ipa": "ɑːdɑːˈɛzɛ"}})
    out = apply_pronunciation("Adaeze is here.", lex, provider="azure")
    assert out == '<phoneme alphabet="ipa" ph="ɑːdɑːˈɛzɛ">Adaeze</phoneme> is here.'
    # Non-azure providers get the respell, never SSML markup.
    out_mms = apply_pronunciation("Adaeze is here.", lex, provider="mms")
    assert out_mms == "ah-DAH-ay-zeh is here."
    assert "<phoneme" not in out_mms


def test_ipa_only_entry_skipped_for_non_azure():
    lex = parse_lexicon({"Adaeze": {"ipa": "ɑːdɑːˈɛzɛ"}})
    out = apply_pronunciation("Adaeze is here.", lex, provider="piper")
    assert out == "Adaeze is here."  # no respell available -> no change
    # Azure still wraps it.
    out_az = apply_pronunciation("Adaeze is here.", lex, provider="azure")
    assert "<phoneme" in out_az


def test_longest_match_first():
    lex = parse_lexicon(
        {"Chukwu": "CHOO-kwoo", "Chukwuemeka": "choo-kwoo-eh-MEH-kah"}
    )
    out = apply_pronunciation("Chukwuemeka and Chukwu arrived.", lex, provider="mms")
    assert out == "choo-kwoo-eh-MEH-kah and CHOO-kwoo arrived."


def test_word_boundary_safe():
    lex = parse_lexicon({"Ada": "AH-dah"})
    # "Adaeze" must NOT be partially rewritten; standalone "Ada" must be.
    out = apply_pronunciation("Adaeze and Ada", lex, provider="mms")
    assert out == "Adaeze and AH-dah"


def test_case_insensitive_matching():
    lex = parse_lexicon({"Adaeze": "ah-DAH-ay-zeh"})
    out = apply_pronunciation("ADAEZE said it", lex, provider="mms")
    assert out == "ah-DAH-ay-zeh said it"


def test_empty_lexicon_and_text_passthrough():
    assert apply_pronunciation("hello", {}, provider="mms") == "hello"
    assert apply_pronunciation("hello", None, provider="mms") == "hello"
    assert apply_pronunciation("", {"A": PronSpec(respell="B")}, provider="mms") == ""
