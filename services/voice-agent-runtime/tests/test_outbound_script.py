"""Outbound-call script (app/outbound_script.py, 2026-11-22): purpose -> attributes -> greeting + prompt block."""
from types import SimpleNamespace

from app import outbound_script as os_
from app.prompts import RECORDING_NOTICE_LINE, build_system_prompt

CTX = SimpleNamespace(display_name="Leadway Pensure PFA", site_slug="leadway-pensure", disclosure=None)
FACTS = {"offering": "RSA Registration and Onboarding Call", "team_member": "Aisha Bello", "when": "Tuesday 24 November at 9 a.m."}


def brief(purpose="confirm_appointment", name="Ngozi Okafor", context=FACTS):
    return os_.from_attributes(os_.to_attributes(purpose, name, context))


def test_attributes_round_trip_and_reject_unknown_purpose():
    b = brief()
    assert b.purpose == "confirm_appointment" and b.contact_name == "Ngozi Okafor" and b.context == FACTS
    assert os_.from_attributes(os_.to_attributes("sell_insurance", "X", {})).purpose == ""
    assert os_.to_attributes(None, "  ", {"offering": "", "junk": "x"}) == {}


def test_values_are_trimmed_and_capped():
    b = os_.from_attributes(os_.to_attributes("callback", "  Ngozi   Okafor ", {"reason": "x" * 500}))
    assert b.contact_name == "Ngozi Okafor" and len(b.context["reason"]) == os_.MAX_VALUE


def test_greeting_checks_identity_before_any_detail():
    g = os_.greeting(CTX, brief())
    assert g.startswith(RECORDING_NOTICE_LINE)
    assert "virtual assistant at Leadway Pensure PFA" in g and "Am I speaking with Ngozi Okafor?" in g
    assert "thank you for calling" not in g.lower()
    assert "Aisha" not in g and "Tuesday" not in g  # nothing about the appointment before they confirm


def test_greeting_without_a_name_states_the_purpose():
    g = os_.greeting(CTX, brief(purpose="callback", name="", context={"reason": "your RSA statement"}))
    assert "You asked us to call you back about your RSA statement" in g


def test_purpose_lines_read_naturally():
    assert os_.purpose_line(brief()) == ("I am calling to confirm your RSA Registration and Onboarding Call with Aisha Bello "
                                         "on Tuesday 24 November at 9 a.m. Can you still make it?")
    w = os_.purpose_line(brief("waitlist_offer", context={"offering": "Retirement Benefits Consultation", "when": "Thursday at 11 a.m."}))
    assert w == ("You asked to be told when a Retirement Benefits Consultation slot opens up. "
                 "One is free on Thursday at 11 a.m. Would you like it?")
    assert os_.purpose_line(brief("no_show_rebook", context={})).startswith("We missed you at your appointment, and")
    assert os_.purpose_line(brief("callback", context={})) == "You asked us to call you back. How can I help?"
    assert os_.purpose_line(brief("", context={})) == "How can I help you today?"
    assert ".." not in "".join(os_.purpose_line(brief(p)) for p in os_.PURPOSES)
    assert "at your appointment yesterday at 2 p.m.," in os_.purpose_line(brief("no_show_rebook", context={"when": "yesterday at 2 p.m."}))


def test_prompt_block_rules():
    p = os_.prompt_block(brief("no_show_rebook"), "+2348035550000")
    assert "you placed this call" in p and 'Never say "thank you for calling"' in p
    assert "Do NOT ask for their name, phone number or email" in p
    assert "Number we dialed: +2348035550000" in p and "With: Aisha Bello" in p
    assert "never blame them" in p and "share no details" in p
    assert "Do not offer or mention WhatsApp" in p and "send_whatsapp_confirmation" not in p
    assert "send_whatsapp_confirmation once" in os_.prompt_block(brief(), "+2348035550000", whatsapp=True)


def test_system_prompt_uses_outbound_block_instead_of_caller_id():
    from app.tenant_context import TenantContext
    ctx = TenantContext(site_slug="leadway-pensure", tenant_id="00000000-0000-0000-0000-000000000001",
                        tenant_slug="leadway-pensure", display_name="Leadway Pensure PFA", timezone="Africa/Lagos")
    inbound = build_system_prompt(ctx, conversation_id="c1", caller_phone="+2348035550000")
    outbound = build_system_prompt(ctx, conversation_id="c1", caller_phone="+2348035550000",
                                   outbound=os_.prompt_block(brief(), "+2348035550000"))
    assert "CALLER ID (SIP, unverified)" in inbound and "OUTBOUND CALL" not in inbound
    assert "OUTBOUND CALL" in outbound and "CALLER ID (SIP, unverified)" not in outbound
