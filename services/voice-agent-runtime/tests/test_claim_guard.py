from types import SimpleNamespace as NS

from app.lk_tool_loop import _booked_in, booking_claim


def test_booking_claims_are_recognised():
    for t in ("I have booked your micro-pension enrollment clinic with Aisha Bello for Tuesday at 9.",
              "Your appointment is confirmed for Wednesday, 25 November at 11:00 AM.",
              "Great, you're all set for the registration call on Tuesday.",
              "Your claim assessment visit has been booked.",
              "You dey welcome! I don book the appointment for you with Aisha Bello on Tuesday.",
              "E don book: your registration call na Tuesday ten o'clock.",
              "I go book this RSA Registration and Onboarding call for you now. One moment abeg.",
              "Let me book that registration call with Aisha for you."):
        assert booking_claim(t), t


def test_ordinary_lines_are_not_claims():
    for t in ("Thank you. I have confirmed your phone number as 0806 555 3108.",
              "I can book a registration call for you. Which time works?",
              "Let me check the availability for next Tuesday.",
              "Is there anything else I can help you with?",
              "I don confirm your number: zero eight zero six.",
              "Would you like me to book a registration call?"):
        assert not booking_claim(t), t


def test_booked_in_sees_a_booking_tool_in_the_history():
    tool_msg = NS(role="tool", name="book_appointment", tool_calls=None)
    other = NS(role="tool", name="get_availability", tool_calls=None)
    assert _booked_in(NS(messages=[other, tool_msg]))
    assert not _booked_in(NS(messages=[other]))
    call = NS(function_info=NS(name="book_appointment"))
    assert _booked_in(NS(messages=[NS(role="assistant", name="", tool_calls=[call])]))


def test_upload_link_claims_are_held_until_the_tool_runs():
    """2026-11-26: a live call promised "a WhatsApp upload link ... right now" and never called the tool."""
    from app.lk_tool_loop import link_claim, unbacked_claim

    said = ("Please hold on while I get the link ready for you. I will send a WhatsApp upload link for your pay stub "
            "right now.")
    assert link_claim(said) and link_claim("The upload link is on its way to your WhatsApp.")
    assert link_claim("Please hold on a moment while I prepare a secure link for you to upload the document via WhatsApp.")
    assert not link_claim("You can upload it on our website.") and not link_claim("Would you like me to help?")
    empty = NS(messages=[])
    tools = NS(ai_functions={"send_document_upload_link": object()})
    assert unbacked_claim(said, empty, tools) == "link"
    ran = NS(messages=[NS(role="tool", name="send_document_upload_link", tool_calls=None)])
    assert unbacked_claim(said, ran, tools) is None
    assert unbacked_claim(said, empty, NS(ai_functions={"book_appointment": object()})) is None
    assert unbacked_claim("I have booked your registration call for Tuesday.", empty, tools) == "booking"


def test_nudge_quotes_what_the_caller_already_heard():
    from app.lk_tool_loop import _nudge

    n = _nudge("link", "Good evening, Funmi. Yes, you can send it. ")
    assert "BEFORE saying anything" in n and 'ALREADY heard you say: "Good evening, Funmi. Yes, you can send it."' in n
    assert "ALREADY" not in _nudge("booking", "")


def test_nothing_to_say_is_not_spoken():
    from app.livekit_worker import has_words

    assert not has_words('"""') and not has_words(" ... ") and not has_words("")
    assert has_words("Goodbye.") and has_words("Ẹ ṣé") and has_words("9:00")


def test_cjk_text_is_never_spoken():
    from app.livekit_worker import has_words, speakable

    assert not has_words(speakable("探寻更多信息，请随时联系我们。再见。"))
    assert speakable("Goodbye, Funmi. 再见。") == "Goodbye, Funmi."
    assert speakable("Ẹ ṣé, ẹ kú àṣálẹ́.") == "Ẹ ṣé, ẹ kú àṣálẹ́."
