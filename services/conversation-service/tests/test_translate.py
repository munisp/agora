from app.intel import parse_translation


def test_english_lines_are_marked_not_translated():
    assert parse_translation("ENGLISH", "Good afternoon") == ""
    assert parse_translation("english.", "Hello") == ""
    assert parse_translation("Good afternoon", "Good afternoon") == ""  # an echo is not a translation
    assert parse_translation("", "x") is None  # no reply: not stored, retried later


def test_translation_is_kept_clean():
    assert parse_translation('"Good evening, I want to open a pension account."', "Ẹ kú ìrọ̀lẹ́ …") == \
        "Good evening, I want to open a pension account."
