from app.tts_providers.spoken import number_words, ordinal_words, spoken


def test_number_words():
    assert number_words(0) == "zero"
    assert number_words(25) == "twenty-five"
    assert number_words(105) == "one hundred and five"
    assert number_words(2026) == "two thousand and twenty-six"
    assert number_words(15000) == "fifteen thousand"
    assert ordinal_words(25) == "twenty-fifth"
    assert ordinal_words(1) == "first"
    assert ordinal_words(30) == "thirtieth"


def test_times_as_spoken():
    assert spoken("We have 9:00 AM, 9:30 AM or 10:00 AM.") == "We have nine a m, nine thirty a m or ten a m."
    assert spoken("Is 9:05 fine?") == "Is nine oh five fine?"
    assert spoken("Come at 10:00.") == "Come at ten o'clock."
    assert spoken("Open 9am to 5pm") == "Open nine a m to five p m"


def test_phone_numbers_digit_by_digit():
    assert spoken("Your number is 0806-555-3108, is that correct?") == (
        "Your number is zero eight zero six, five five five, three one zero eight, is that correct?")
    assert spoken("I have 0803 555 2214.") == "I have zero eight zero three, five five five, two two one four."


def test_dates_money_and_plain_text():
    assert spoken("Wednesday, November 25th") == "Wednesday, November twenty-fifth"
    assert spoken("The fee is ₦15,000.") == "The fee is fifteen thousand naira."
    assert spoken("Thank you for calling.") == "Thank you for calling."
