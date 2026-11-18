"""SPEC-W82 A4.1: code-switch-aware turns — pure-function unit tests."""

from __future__ import annotations

from app.codeswitch import analyze, instruction


def test_mixed_utterance_is_segmented_and_flagged():
    result = analyze("Wetin be the earliest appointment for tomorrow morning?")
    assert result.code_switch is True
    assert result.pcm_tokens >= 1
    assert result.en_tokens >= 2
    langs = {seg.lang for seg in result.segments}
    assert langs == {"pcm", "en"}
    # The Pidgin opener is its own segment.
    assert result.segments[0].lang == "pcm"
    assert result.segments[0].text.startswith("Wetin")
    # The English tail is a separate segment.
    assert result.segments[-1].lang == "en"
    assert "appointment" in result.segments[-1].text


def test_pure_english_passthrough():
    result = analyze("I would like to book an appointment please.")
    assert result.code_switch is False
    assert result.pcm_tokens == 0
    assert all(seg.lang == "en" for seg in result.segments)


def test_pure_pidgin_passthrough():
    result = analyze("Wetin dey?")
    assert result.code_switch is False
    assert result.en_tokens == 0
    assert all(seg.lang == "pcm" for seg in result.segments)


def test_phrase_markers_tag_pidgin_tokens():
    result = analyze("How far, I dey come tomorrow morning?")
    assert result.code_switch is True
    pcm_text = " ".join(
        seg.text for seg in result.segments if seg.lang == "pcm"
    ).lower()
    assert "how far" in pcm_text
    assert "dey" in pcm_text


def test_two_english_token_floor():
    # A single English borrowing inside Pidgin speech is NOT a switch.
    result = analyze("Wetin dey price?")
    assert result.code_switch is False
    assert result.en_tokens == 1


def test_empty_and_blank():
    assert analyze("").code_switch is False
    assert analyze("   ").code_switch is False
    assert analyze("").segments == []


def test_locale_instruction_content():
    text = instruction()
    assert "code-switch" in text.lower()
    assert "Nigerian Pidgin" in text
    assert "English" in text
    # Same natural mixed register, tool arguments preserved.
    assert "mixed" in text.lower()
    assert "tool arguments" in text
