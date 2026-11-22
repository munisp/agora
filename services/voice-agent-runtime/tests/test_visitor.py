"""2026-11-21: pre-chat/pre-call visitor form (app/visitor.py)."""
import json

import pytest

from app import visitor as v
from app.prompts import build_system_prompt  # noqa: F401  (import check)
from app.session_state import SessionState


def test_phone_forms_converge():
    assert v.canonical_phone("0806 555 3124") == "+2348065553124"
    assert v.canonical_phone("2348065553124") == "+2348065553124"
    assert v.canonical_phone("+234 806 555 3124") == "+2348065553124"
    assert v.canonical_phone("12") is None


def test_model_validates():
    ok = v.Visitor(name="  Grace   Okon ", phone="08065553124", email="Grace.Okon@Gmail.com")
    assert (ok.name, ok.phone, ok.email) == ("Grace Okon", "+2348065553124", "grace.okon@gmail.com")
    assert v.Visitor(name="A", phone="08065553124", email="").email is None
    for bad in [dict(name="", phone="08065553124"), dict(name="A", phone="abc"), dict(name="A", phone="08065553124", email="nope")]:
        with pytest.raises(ValueError):
            v.Visitor(**bad)


def test_apply_and_prompt_block():
    s = SessionState(conversation_id="c1", site_slug="s")
    v.apply(s, v.Visitor(name="Grace Okon", phone="08065553124", email="g@x.co"))
    assert s.claimed_phone == "+2348065553124" and s.caller_name == "Grace Okon" and s.visitor_form
    assert s.verified_phone is None and s.confirmed_phone is None  # self-reported: never authorizes
    v.apply(s, v.Visitor(name="Other", phone="08000000000"))  # first form wins
    assert s.caller_name == "Grace Okon"
    block = v.prompt_block(v.from_session(s))
    assert "Grace Okon" in block and "+2348065553124" in block and "Do NOT ask" in block
    restored = SessionState.from_dict(s.to_dict())
    assert restored.caller_email == "g@x.co" and restored.visitor_form


def test_no_form_no_block():
    s = SessionState(conversation_id="c2", site_slug="s")
    assert v.from_session(s) is None and v.prompt_block(None) == ""


def test_token_metadata_roundtrip():
    raw = v.token_metadata("yo", v.Visitor(name="Kemi Ojo", phone="08025554417"))
    assert json.loads(raw)["language"] == "yo"
    got = v.from_metadata(raw)
    assert got and got.name == "Kemi Ojo" and got.phone == "+2348025554417"
    assert v.from_metadata('{"language":"yo"}') is None and v.from_metadata("not json") is None
    assert v.token_metadata("", None) == ""


# 2026-11-21: with no form, the opening line asks for name, email and phone first (English and the Nigerian languages).
def test_greeting_asks_for_details_when_no_form():
    from types import SimpleNamespace
    from app import call_language
    from app.prompts import build_greeting
    ctx = SimpleNamespace(display_name="Leadway Pensure PFA", site_slug="leadway-pensure", disclosure=None)
    plain = build_greeting(ctx)
    ask = build_greeting(ctx, ask_details=True)
    assert "How can I help you today?" in plain and "full name" not in plain
    assert "Before we start, may I have your full name, your email and your phone number?" in ask
    assert "How can I help" not in ask
    assert "Kafin mu fara" in call_language.greeting("ha", "Leadway", ask, ask_details=True)
    assert "Tupu anyị amalite" in call_language.greeting("ig", "Leadway", ask, ask_details=True)
    assert "Kí a tó bẹ̀rẹ̀" in call_language.greeting("yo", "Leadway", ask, ask_details=True)
    assert "Before we start" in call_language.greeting("pcm", "Leadway", ask, ask_details=True)
    # a form-filled caller keeps the usual greeting
    assert "How I fit help you today?" in call_language.greeting("pcm", "Leadway", plain)
    assert call_language.greeting("", "Leadway", ask, ask_details=True) == ask
