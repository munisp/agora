"""Multilingual receptionist tests (Wave 5 #3): whisper language detection ->
per-turn locale switch, piper voice map fallback, pack `languages` validation
at the voice runtime's pack consumption point."""

from __future__ import annotations

from types import SimpleNamespace

from app import multilang as ml
from app.prompts import build_system_prompt
from app.tenant_context import TenantContext, _apply_pack


def _ctx(**kw) -> TenantContext:
    base = dict(site_slug="acme", tenant_id="t1", tenant_slug="acme")
    base.update(kw)
    return TenantContext(**base)


# --------------------------------------------------------------------------
# Detection from whisper STT results
# --------------------------------------------------------------------------
class TestDetection:
    def test_segments_dict_language_majority(self):
        segs = [{"language": "es"}, {"language": "es"}, {"language": "en"}]
        assert ml.detect_language_from_segments(segs) == "es"

    def test_segments_object_language(self):
        segs = [SimpleNamespace(language="fr-CA"), SimpleNamespace(language="fr")]
        assert ml.detect_language_from_segments(segs) == "fr"

    def test_segments_without_language(self):
        assert ml.detect_language_from_segments([{"text": "hola"}]) == ""
        assert ml.detect_language_from_segments([]) == ""
        assert ml.detect_language_from_segments(None) == ""

    def test_info_language(self):
        assert ml.detect_language_from_info(SimpleNamespace(language="de")) == "de"
        assert ml.detect_language_from_info(SimpleNamespace()) == ""

    def test_normalize_language(self):
        assert ml.normalize_language("es-ES") == "es"
        assert ml.normalize_language("EN_us") == "en"
        assert ml.normalize_language("english") == ""
        assert ml.normalize_language(None) == ""
        assert ml.default_language_from_locale("fr-FR") == "fr"
        assert ml.default_language_from_locale("") == "en"


# --------------------------------------------------------------------------
# Detection -> per-turn locale switch
# --------------------------------------------------------------------------
class TestLocaleSwitch:
    def test_tenant_locale_sets_default(self):
        state = ml.MultilangState.from_context(_ctx(locale="es-ES"))
        assert state.default_language == "es"
        assert state.active_language == "es"
        # Default language => no extra instruction needed.
        assert state.instruction() == ""

    def test_detection_switches_language(self):
        state = ml.MultilangState.from_context(_ctx(locale="en-US"))
        assert state.observe("es") is True
        assert state.active_language == "es"
        instruction = state.instruction()
        assert "Respond in Spanish" in instruction
        # Detecting the same language again is not a switch.
        assert state.observe("es-ES") is False
        # Switching back to the default clears the instruction.
        assert state.observe("en") is True
        assert state.instruction() == ""

    def test_empty_detection_keeps_current(self):
        state = ml.MultilangState.from_context(_ctx(locale="en-US"))
        state.observe("es")
        assert state.observe("") is False
        assert state.active_language == "es"

    def test_pack_languages_bound_the_switch(self):
        state = ml.MultilangState(
            default_language="en", supported=["en", "es"]
        )
        assert state.observe("es") is True
        # German is not in the pack's declared languages -> keep default es->en?
        state.observe("en")
        assert state.observe("de") is False
        assert state.active_language == "en"

    def test_prompt_carries_locale_instruction(self):
        ctx = _ctx(locale="en-US")
        prompt = build_system_prompt(ctx, conversation_id="c1", language="es")
        assert "LANGUAGE (this turn)" in prompt
        assert "Respond in Spanish" in prompt

    def test_prompt_skips_instruction_for_default_language(self):
        ctx = _ctx(locale="en-US")
        assert "LANGUAGE (this turn)" not in build_system_prompt(
            ctx, conversation_id="c1", language="en"
        )
        assert "LANGUAGE (this turn)" not in build_system_prompt(
            ctx, conversation_id="c1"
        )


# --------------------------------------------------------------------------
# Piper voice map
# --------------------------------------------------------------------------
class TestVoiceMap:
    def test_parse_voice_map(self):
        m = ml.parse_voice_map(
            '{"en": "en_US-lessac-medium", "es": "es_ES-sharvard-medium", "fr": "fr_FR-siwis-medium"}'
        )
        assert m["es"] == "es_ES-sharvard-medium"
        assert ml.parse_voice_map("{bad") == {}
        assert ml.parse_voice_map("") == {}
        # Invalid entries drop out.
        assert ml.parse_voice_map('{"english": "x", "de": ""}') == {}

    def test_voice_for_language_hit_and_fallback(self):
        m = {"es": "es_ES-sharvard-medium"}
        assert ml.voice_for_language("es", m, "en_US-lessac-medium") == "es_ES-sharvard-medium"
        # Graceful fallback: unmapped language / empty language -> default voice.
        assert ml.voice_for_language("fr", m, "en_US-lessac-medium") == "en_US-lessac-medium"
        assert ml.voice_for_language("", m, "en_US-lessac-medium") == "en_US-lessac-medium"


# --------------------------------------------------------------------------
# Pack `languages` validation at pack consumption (identity passes through)
# --------------------------------------------------------------------------
class TestPackLanguages:
    def test_valid_pack_languages_passthrough(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"languages": ["en", "es", "ES", "fr-FR"]}})
        # Normalized + de-duplicated.
        assert ctx.languages == ["en", "es", "fr"]

    def test_invalid_entries_dropped(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"languages": ["klingon", 42, None, "es"]}})
        assert ctx.languages == ["es"]

    def test_non_list_ignored(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"languages": "es"}})
        assert ctx.languages == []

    def test_absent_field_keeps_default(self):
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"agentPersona": "hi"}})
        assert ctx.languages == []

    def test_validate_directly(self):
        assert ml.validate_pack_languages(["EN", "es-MX"]) == ["en", "es"]
        assert ml.validate_pack_languages("nope") == []


# --------------------------------------------------------------------------
# Nigerian Pidgin (pcm) — pcm->en proxy for STT/voice resolution; SPEC-W70
# A4 gives pcm an explicit Pidgin locale instruction (see TestPidgin).
# --------------------------------------------------------------------------
class TestPidgin:
    def test_pcm_in_language_names(self):
        assert ml.LANGUAGE_NAMES["pcm"] == "Nigerian Pidgin"

    def test_pidgin_proxy(self):
        assert ml.pidgin_proxy("pcm") == "en"
        assert ml.pidgin_proxy("en") == "en"
        assert ml.pidgin_proxy("es") == "es"
        assert ml.pidgin_proxy("") == ""

    def test_pcm_detection_proxies_to_english(self):
        # whisper never reports pcm, but if a detection/manual override ever
        # carries it, the next turn stays English (no locale switch to pcm).
        state = ml.MultilangState(default_language="en", supported=["en", "pcm"])
        assert state.observe("pcm") is False
        assert state.active_language == "en"
        assert state.instruction() == ""

    def test_pcm_bounded_by_pack_languages(self):
        # pcm detection on an [en, pcm] pack resolves to en, which is in the
        # supported set; pcm is NOT itself activated.
        assert ml.resolve_turn_language("pcm", default="en", supported=["en", "pcm"]) == "en"
        assert ml.resolve_turn_language("pcm-NG", default="en", supported=["en", "pcm"]) == "en"
        # On a pack without pcm/en the proxy result still bounds correctly.
        assert ml.resolve_turn_language("pcm", default="fr", supported=["fr", "pcm"]) == "fr"

    def test_pcm_locale_instruction_is_pidgin(self):
        # SPEC-W70 A4 (adjudicated ruling, supersedes the old pcm->en proxy
        # for the locale instruction ONLY): pcm now emits an explicit
        # natural-Nigerian-Pidgin instruction. The proxy still stands for
        # the STT/voice resolution paths (tests above/below unchanged).
        instruction = ml.locale_instruction("pcm")
        assert "Nigerian Pidgin" in instruction
        assert "Respond in natural, conversational Nigerian Pidgin" in instruction
        assert "Respond in English" not in instruction
        # Non-pcm languages are unaffected.
        assert "Respond in Spanish" in ml.locale_instruction("es")

    def test_pcm_voice_falls_back_to_english_voice(self):
        # No Pidgin Piper voice exists: pcm speaks with the en mapping when
        # present, else the default voice.
        m = {"en": "en_US-lessac-medium"}
        assert ml.voice_for_language("pcm", m, "en_GB-alan-medium") == "en_US-lessac-medium"
        assert ml.voice_for_language("pcm", {}, "en_US-lessac-medium") == "en_US-lessac-medium"

    def test_pack_pcm_languages_passthrough(self):
        # The nigeria-sme pack declares [en, pcm]; both are valid entries and
        # survive pack-consumption validation.
        ctx = _ctx()
        _apply_pack(ctx, {"pack": {"languages": ["en", "pcm"]}})
        assert ctx.languages == ["en", "pcm"]
