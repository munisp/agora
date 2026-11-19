from types import SimpleNamespace as NS

from app.livekit_worker import caller_phone, plausible_ng_phone, spoken_phone


def test_spoken_phone_reads_english_and_pidgin_digits():
    assert spoken_phone("my fon nomba nor zero eight zero six five-five-five three one zero eight") == "08065553108"
    assert spoken_phone("zero eight zero six, double five five, three one zero eight") == "08065553108"
    assert spoken_phone("It's 0806 555 3108") == "08065553108"


def test_spoken_phone_reads_nigerian_language_digits():
    assert spoken_phone("Nọ́mbà fóònù mi ni òdo mẹ́jọ òdo méjì, márùn-ún márùn-ún márùn-ún, mẹ́rin mẹ́rin ọ̀kan méje") == "08025554417"
    assert spoken_phone("Lambar wayata ita ce sifiri takwas sifiri biyar, biyar biyar biyar, shida ɗaya biyu sifiri") == "08055556120"


def test_spoken_phone_ignores_lines_without_a_number():
    assert spoken_phone("Ten o'clock in the morning is fine for me.") is None


def test_caller_phone_prefers_the_latest_caller_line_and_validity():
    msgs = [NS(role="user", content="my number na zero eight zero six five five five three one zero eight"),
            NS(role="assistant", content="Na zero eight zero, zero six five…")]
    assert caller_phone(msgs) == "08065553108"
    assert not plausible_ng_phone("080065553108") and plausible_ng_phone("08065553108")
