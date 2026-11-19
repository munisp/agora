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
