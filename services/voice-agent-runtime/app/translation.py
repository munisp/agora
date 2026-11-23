"""SPEC-W82 A1-A3: Nigerian-language translation layer.

Greenfield module (no translation infrastructure existed pre-W82). Three
cooperating pieces:

1. :func:`detect_language` — PURE STDLIB heuristic detector over
   {en, pcm, yo, ig, ha}. Signals: Yoruba/Igbo diacritics (e-dot, o-dot,
   i-dot, u-dot, n-tilde — matched in precomposed AND NFD combining forms),
   Hausa hooked letters (b-hook, d-hook, k-hook, y-hook), a Nigerian-Pidgin
   lexicon (wetin, dey, wahala, abeg, una, how far, no dey, na so, ...) plus
   small function-word lexicons for Yoruba/Igbo/Hausa so short texts with no
   special characters still resolve. English is the fallback when no signal
   fires. Deterministic, < 1 ms on 500 chars, no dependencies, no network.
2. Providers behind :class:`TranslatorInterface`:
   - :class:`HTTPTranslator` — POSTs {text, source_lang, target_lang} to
     TRANSLATE_URL with an optional Bearer key and a hard timeout
     (TRANSLATE_TIMEOUT_S, default 3.0s). FAIL-OPEN on ANY error (timeout,
     connection failure, 5xx, bad JSON, missing key): the original text is
     returned and ``voice_translation_errors_total{provider="http"}`` is
     incremented — a turn is NEVER blocked or failed by translation.
   - :class:`MockTranslator` — echoes the text, provider="mock", any lang
     pair. Default via TRANSLATE_MOCK=1 (the sanctioned XTTS_MOCK /
     COSYVOICE_MOCK / MMS_MOCK dev pattern).
3. Turn helpers :func:`process_inbound_text` / :func:`process_outbound_text`
   — the fail-open seams consumed by the chat path (app/chat.py
   ChatService._prepare_turn / reply path) and the voice path
   (app/livekit_worker.py WhisperSTTNode post-STT hook + PiperTTSNode
   pre-TTS transform).

Only {pcm, yo, ig, ha} are translated inbound -> en and outbound en -> lang
(:data:`TRANSLATABLE_LANGUAGES`); every other detection passes through
untouched. Code-switched pcm/en turns (app/codeswitch.py) are NOT
force-translated wholesale — they pass through with a code-switch locale
instruction instead.
"""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass, field
from typing import Any, Protocol

from . import metrics
from .logging import get_logger
from .multilang import normalize_language

log = get_logger("translation")

#: Languages the W82 translation layer handles (Nigerian Pidgin + the three
#: major Nigerian languages). Everything else passes through.
TRANSLATABLE_LANGUAGES = frozenset({"pcm", "yo", "ig", "ha"})


@dataclass
class TranslationResult:
    text: str
    source_lang: str
    target_lang: str
    provider: str
    cached: bool = False


class TranslatorInterface(Protocol):
    async def translate(
        self, text: str, source_lang: str, target_lang: str
    ) -> TranslationResult:
        """Translate ``text`` from ``source_lang`` to ``target_lang``."""
        ...


# ---------------------------------------------------------------------------
# A1: pure-stdlib heuristic language detector (en / pcm / yo / ig / ha)
# ---------------------------------------------------------------------------
# Nigerian Pidgin lexicon. Strong words are unambiguous Pidgin markers
# (weight 2); weak words are common-but-nameable (weight 1). English words
# that also exist in Pidgin (chop, dash, yarn, don) are deliberately absent —
# false positives on English text are worse than a missed detection.
PIDGIN_WORDS_STRONG = frozenset(
    {
        "wetin",
        "dey",
        "wahala",
        "abeg",
        "una",
        "pikin",
        "sabi",
        "oya",
        "shebi",
        "naija",
        "waka",
        "kuku",
        "yarn",
    }
)
PIDGIN_WORDS_WEAK = frozenset({"abi"})
PIDGIN_WORDS = PIDGIN_WORDS_STRONG | PIDGIN_WORDS_WEAK

#: Multi-word Pidgin markers (weight 3 when present as a substring phrase).
PIDGIN_PHRASES = (
    "how far",
    "no dey",
    "na so",
    "i dey",
    "e don",
    "dey go",
    "no wahala",
    "una dey",
)

# Small function-word lexicons so short texts without diacritics/hooked
# letters still resolve ("Bawo ni?", "Kedu?", "Sannu!"). Weight 2 each.
YORUBA_WORDS = frozenset(
    {
        "bawo",
        "kini",
        "jowo",
        "ekaaro",
        "ekasan",
        "ekale",
        "nlo",
        "wipe",
        "odabo",
        "dabo",
        "nko",
        "kosi",
        "rere",
        "sugbon",
        "nitori",
        "emi",
    }
)
IGBO_WORDS = frozenset(
    {
        "kedu",
        "ndewo",
        "biko",
        "dalu",
        "imela",
        "kwenu",
        "gini",
        "ole",
        "kwanu",
        "udo",
        "nnoo",
        "olee",
    }
)
HAUSA_WORDS = frozenset(
    {
        "sannu",
        "lafiya",
        "nagode",
        "yaya",
        "gobe",
        "yau",
        "kudin",
        "kudi",
        "barka",
        "gida",
        "aboki",
        "yarinya",
    }
)
HAUSA_PHRASES = ("don allah", "ina kwana", "yau da gobe", "sai gobe")

# Deterministic tie-break order when two languages score identically
# (documented: Pidgin first, then Yoruba, Igbo, Hausa — Nigerian Pidgin is
# the most common mixed register on Naija telephony audio/text).
_DETECTOR_PRIORITY = ("pcm", "yo", "ig", "ha")

_WORD_RE = re.compile(r"\w+")


def detect_language(text: str) -> str:
    """Heuristically detect en/pcm/yo/ig/ha for a caller utterance.

    Pure stdlib, deterministic, no network. Scoring:
    - Yoruba diacritics (e/s + dot below) weight 2; o-dot weight 1 for BOTH
      Yoruba and Igbo (shared letter); Igbo letters (i/u/n + dot below,
      n-tilde) weight 2; Hausa hooked letters (b/d/k/y-hook) weight 2.
      Precomposed and NFD combining forms are both matched.
    - Lexicon hits weight 2 (weak Pidgin markers weight 1), multi-word
      phrases weight 3.
    - Highest score wins; ties break by _DETECTOR_PRIORITY; zero score is
      English (the detector NEVER invents a language for unmarked text).
    """
    raw = (text or "").strip()
    if not raw:
        return "en"
    lower = raw.lower()
    scores = {"pcm": 0, "yo": 0, "ig": 0, "ha": 0}

    # Signal 1+2: diacritics and hooked letters (precomposed form).
    for ch in lower:
        if ch in "ẹṣ":
            scores["yo"] += 2
        elif ch == "ọ":
            scores["yo"] += 1
            scores["ig"] += 1
        elif ch in "ịụñ":
            scores["ig"] += 2
        elif ch in "ɓɗƙƴ":
            scores["ha"] += 2
    # Same signals in NFD (decomposed) form: base letter + U+0323 dot below.
    nfd = unicodedata.normalize("NFD", lower)
    prev = ""
    for ch in nfd:
        if ch == "\u0323":  # combining dot below (e.g. o + U+0323 = o-dot)
            if prev in "es":
                scores["yo"] += 2
            elif prev == "o":
                scores["yo"] += 1
                scores["ig"] += 1
            elif prev in "iun":
                scores["ig"] += 2
        prev = ch

    # Signal 3: lexicons.
    words = _WORD_RE.findall(lower)
    for word in words:
        if word in PIDGIN_WORDS_STRONG:
            scores["pcm"] += 2
        elif word in PIDGIN_WORDS_WEAK:
            scores["pcm"] += 1
        if word in YORUBA_WORDS:
            scores["yo"] += 2
        if word in IGBO_WORDS:
            scores["ig"] += 2
        if word in HAUSA_WORDS:
            scores["ha"] += 2
    for phrase in PIDGIN_PHRASES:
        if phrase in lower:
            scores["pcm"] += 3
    for phrase in HAUSA_PHRASES:
        if phrase in lower:
            scores["ha"] += 3

    best = "en"
    best_score = 0
    for lang in _DETECTOR_PRIORITY:
        if scores[lang] > best_score:
            best = lang
            best_score = scores[lang]
    return best


# ---------------------------------------------------------------------------
# A1: providers
# ---------------------------------------------------------------------------
class MockTranslator:
    """Dev default (TRANSLATE_MOCK=1): echoes the text for any lang pair."""

    provider = "mock"

    async def translate(
        self, text: str, source_lang: str, target_lang: str
    ) -> TranslationResult:
        return TranslationResult(
            text=text,
            source_lang=source_lang,
            target_lang=target_lang,
            provider=self.provider,
        )


class HTTPTranslator:
    """POST {text, source_lang, target_lang} to a translation HTTP endpoint.

    FAIL-OPEN on any error/timeout/5xx/bad JSON/missing key: returns the
    original text, increments ``voice_translation_errors_total`` and logs a
    single warning per call. The response body is JSON; the translated string
    is read from ``text`` (falling back to ``translated_text`` /
    ``translation`` keys).
    """

    provider = "http"

    def __init__(
        self,
        url: str,
        *,
        api_key: str = "",
        timeout_s: float = 3.0,
        client: Any = None,
    ) -> None:
        self._url = (url or "").strip()
        self._api_key = api_key or ""
        self._timeout_s = timeout_s
        self._client = client  # injected in tests; lazily created otherwise
        if not self._url:
            log.warning("HTTPTranslator configured without TRANSLATE_URL; fail-open")

    async def _get_client(self) -> Any:
        if self._client is None:
            import httpx

            self._client = httpx.AsyncClient(
                timeout=httpx.Timeout(self._timeout_s or 3.0)
            )
        return self._client

    async def translate(
        self, text: str, source_lang: str, target_lang: str
    ) -> TranslationResult:
        try:
            if not self._url:
                raise RuntimeError("TRANSLATE_URL not configured")
            headers = (
                {"Authorization": f"Bearer {self._api_key}"} if self._api_key else None
            )
            client = await self._get_client()
            resp = await client.post(
                self._url,
                json={
                    "text": text,
                    "source_lang": source_lang,
                    "target_lang": target_lang,
                },
                headers=headers,
            )
            if resp.status_code != 200:
                raise RuntimeError(f"translator HTTP {resp.status_code}")
            data = resp.json()
            translated = (
                data.get("text") or data.get("translated_text") or data.get("translation")
                if isinstance(data, dict)
                else None
            )
            if not isinstance(translated, str) or not translated.strip():
                raise RuntimeError("translator returned no text")
            return TranslationResult(
                text=translated,
                source_lang=source_lang,
                target_lang=target_lang,
                provider=self.provider,
            )
        except Exception as exc:  # noqa: BLE001 - fail-open by contract
            metrics.translation_error(self.provider)
            log.warning(
                "translation failed; passing through original text",
                provider=self.provider,
                source_lang=source_lang,
                target_lang=target_lang,
                error=str(exc)[:200],
            )
            return TranslationResult(
                text=text,
                source_lang=source_lang,
                target_lang=target_lang,
                provider=self.provider,
            )

    async def aclose(self) -> None:
        if self._client is not None:
            try:
                await self._client.aclose()
            except Exception:  # noqa: BLE001 - shutdown must not raise
                pass


def _truthy(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    return str(value).strip().lower() in ("1", "true", "yes", "on")


_LLM_LANGUAGE_NAMES = {"yo": "Yoruba", "ha": "Hausa", "ig": "Igbo", "pcm": "Nigerian Pidgin", "en": "English"}


class LLMTranslator:
    """Translate through an OpenAI-compatible chat model (the receptionist's own Ollama).

    Used by the "think in English, speak the caller's language" mode: the receptionist reasons and calls its tools
    in English, and each sentence group is translated just before it is spoken. Dates, times, phone numbers, names,
    emails and service names stay as written — Nigerian callers say those in English, and the speech voices read
    English words fine. Fail-open: any error returns the original text. Results are cached per (text, target).
    """

    provider = "llm"

    def __init__(self, base_url: str, model: str, *, api_key: str = "", timeout_s: float = 20.0,
                 extra_body: dict | None = None, client: Any = None,
                 context: Any = None, review: bool = False) -> None:
        self._url = (base_url or "").rstrip("/")
        self._model = model
        self._api_key = api_key or "ollama"
        self._timeout_s = timeout_s
        self._extra = dict(extra_body or {})
        self._client = client
        self._cache: dict[tuple[str, str], str] = {}
        # Inbound (caller -> English) only: a callable returning {"business", "industry", "services", "last_line"}
        # for the current call, read at each turn (the receptionist's last line changes every turn).
        self._context = context
        self._review = review

    def _inbound_prompt(self, source_lang: str) -> str:
        """What the caller said, from rough speech recognition, into plain English.

        The Yoruba/Hausa/Igbo recogniser writes what it hears phonetically (tone marks missing or wrong, words split
        or run together, some misheard): read word for word it means nothing ("The price of your goods is too low"
        for "I just started a new job and want to open a pension account"). Told who was called, what they offer and
        what the receptionist just asked, the model recovers what the caller most likely said.
        """
        name = _LLM_LANGUAGE_NAMES.get(source_lang, source_lang)
        ctx: dict = {}
        try:
            ctx = dict(self._context() or {}) if callable(self._context) else {}
        except Exception:  # noqa: BLE001 - context is a help, never a requirement
            ctx = {}
        business = str(ctx.get("business") or "a Nigerian business").strip()
        industry = str(ctx.get("industry") or "").strip()
        services = [str(s).strip() for s in (ctx.get("services") or []) if str(s).strip()][:12]
        last = str(ctx.get("last_line") or "").strip()
        about = f"{business}" + (f" ({industry.replace('-', ' ')})" if industry else "")
        return (
            f"You are listening to a phone call to {about}."
            + (f" Its services: {'; '.join(services)}." if services else "")
            + f" The caller speaks {name}. Their words below come from speech recognition, so they are rough: tone "
            "marks missing or wrong, words split or run together, some words misheard. Work out what the caller most "
            "likely said, using the business, its services and the receptionist's last line, and write it as plain "
            "English in the caller's own voice (\"I ...\"), keeping every detail you can make out (who, what, which "
            "day, what time). Keep names, numbers, emails, days and times exactly as given. If English words are mixed "
            "in, keep them. Short answers stay short (\"Yes.\", \"Thank you, goodbye.\"). Reply with the English only. "
            "Always give your best reading of what they most likely said - never reply that it is unclear: the "
            "receptionist confirms what it understood, so the caller can correct a wrong guess."
            + (f"\nThe receptionist last said: \"{last}\"" if last else "")
        )

    # A few exact lines per language, all in the language itself. The Yoruba/Hausa/Igbo voices mumble or garble
    # English words and English numbers inside a sentence (measured 2026-11-20 by recognising the voice's own audio:
    # a phone read-back mixed with English scored 0.06-0.29, the same line all in Yoruba 0.56-0.71), so only names,
    # the business name and email addresses stay as written. Digit strings stay digits here and are spoken with the
    # language's own number words afterwards (native_digits), so the model never has to spell a phone number.
    _EXAMPLES = {
        "yo": [("Please tell me your first name and last name.", "Ẹ jọ̀wọ́, ẹ sọ orúkọ àkọ́kọ́ yín àti orúkọ ìdílé yín fún mi."),
               ("You are booked with Aisha Bello for Tuesday, 24 November, at 12 noon.",
                "Mo ti ṣètò ìpàdé yín pẹ̀lú Aisha Bello ní ọjọ́ Ìṣẹ́gun, ọjọ́ kẹrìnlélógún oṣù kọkànlá, ní aago méjìlá ọ̀sán."),
               ("I have your number as 0802 555 4417. Is that correct?",
                "Nọ́mbà fóònù yín tí mo gbà ni 0802 555 4417. Ṣé ó tọ̀nà?"),
               ("Would you like me to book a registration call with a pension advisor?",
                "Ṣé ẹ fẹ́ kí n ṣètò ìpè ìforúkọsílẹ̀ pẹ̀lú olùdámọ̀ràn ìfẹ̀yìntì?"),
               ("One moment while I check.", "Ẹ dúró díẹ̀, mo ń wò ó.")],
        "ha": [("Please tell me your first name and last name.", "Don Allah, gaya mini sunanka na farko da sunan mahaifinka."),
               ("You are booked with Aisha Bello for Tuesday, 24 November, at 12 noon.",
                "Na shirya muku ganawa da Aisha Bello ranar Talata, ashirin da huɗu ga watan Nuwamba, da ƙarfe goma sha biyu na rana."),
               ("I have your number as 0802 555 4417. Is that correct?",
                "Lambar wayarku da na rubuta ita ce 0802 555 4417. Haka ne?"),
               ("Would you like me to book a registration call with a pension advisor?",
                "Kuna so in shirya muku kiran rajista da mai ba da shawara kan fansho?"),
               ("One moment while I check.", "Ku dakata kaɗan, ina dubawa.")],
        "ig": [("Please tell me your first name and last name.", "Biko, gwa m aha mbụ gị na aha nna gị."),
               ("You are booked with Aisha Bello for Tuesday, 24 November, at 12 noon.",
                "Edebela m gị oge gị na Aisha Bello na Tuzdee, ụbọchị iri abụọ na anọ nke ọnwa Nọvemba, n'elekere iri na abụọ nke ehihie."),
               ("I have your number as 0802 555 4417. Is that correct?",
                "Nọmba ekwentị gị m dere bụ 0802 555 4417. Ọ ziri ezi?"),
               ("Would you like me to book a registration call with a pension advisor?",
                "Ị chọrọ ka m deba gị oku ndebanye aha gị na onye ndụmọdụ ezumike nká?"),
               ("One moment while I check.", "Chere ntakịrị, ana m elele.")],
    }

    async def _chat(self, system: str, user: str) -> str:
        if not self._url or not self._model:
            raise RuntimeError("translator model not configured")
        if self._client is None:
            import httpx

            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout_s))
        body = {"model": self._model, "temperature": 0.0,
                "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}], **self._extra}
        resp = await self._client.post(f"{self._url}/chat/completions", json=body,
                                       headers={"Authorization": f"Bearer {self._api_key}"})
        if resp.status_code != 200:
            raise RuntimeError(f"translator HTTP {resp.status_code}")
        out = (resp.json()["choices"][0]["message"].get("content") or "").strip().strip('"').strip()
        return re.sub(r"<0x[0-9A-Fa-f]{2}>", "", out).strip()  # stray byte tokens from the model

    def _review_prompt(self, target_lang: str) -> str:
        name = _LLM_LANGUAGE_NAMES.get(target_lang, target_lang)
        return (
            f"You check a {name} translation of a phone receptionist's English line before it is spoken to a caller. "
            "It must say exactly what the English says. Check in particular: every negative (do not, never, no) is "
            "kept; who does what to whom is the same; numbers, days, dates and times match; nothing is added or left "
            f"out; and only {name} words are used, except names of people, the business name and email addresses. If "
            f"the translation is right, reply with it unchanged. If not, reply with the corrected {name} translation. "
            "Reply with the translation only - no notes, no quotes."
        )

    def _prompt(self, target_lang: str) -> str:
        name = _LLM_LANGUAGE_NAMES.get(target_lang, target_lang)
        examples = "".join(f"\nEnglish: {e}\n{name}: {t}" for e, t in self._EXAMPLES.get(target_lang, []))
        return (
            f"You translate a phone receptionist's line from English into natural, polite spoken {name}, as a "
            f"Nigerian receptionist would say it. Translate faithfully: keep the exact meaning (something done stays "
            f"done, a question stays a question). Reply with the {name} translation only - no notes, no quotes. "
            f"Use {name} words only. Keep in English ONLY: names of people, the name of the business, and email "
            f"addresses. Say services and products in plain {name} words (a short known abbreviation such as RSA may "
            f"stay). Say days, months, dates, times and amounts of money in {name} words. Copy phone numbers and other "
            "digit strings exactly as digits. Keep it short and easy to say aloud; no lists, symbols or emojis."
            + (f"\nExamples:{examples}" if examples else "")
        )

    async def translate(self, text: str, source_lang: str, target_lang: str) -> TranslationResult:
        try:  # the fixed switch agreement: said exactly, never machine-translated
            from . import call_language

            if text.strip() == call_language.agreement_en(target_lang) and target_lang in call_language.AGREEMENTS:
                return TranslationResult(call_language.AGREEMENTS[target_lang], source_lang, target_lang, self.provider)
        except Exception:  # noqa: BLE001
            pass
        inbound = target_lang == "en" and source_lang in ("yo", "ha", "ig", "pcm")
        system = self._inbound_prompt(source_lang) if inbound else self._prompt(target_lang)
        # Inbound depends on the turn's context (the receptionist's last line), so it is cached on the whole prompt.
        key = (text, target_lang) if not inbound else (text, system)
        if key in self._cache:
            return TranslationResult(self._cache[key], source_lang, target_lang, self.provider, cached=True)
        try:
            if inbound:
                out = await self._chat(system, text)
            else:
                out = clean_spoken(await self._chat(system, emails_spoken(text)))
                # Review pass: the model sometimes changed the meaning (an Igbo "do not share your BVN" came out as
                # "share your BVN with anyone") or left an English word in. A second look against the English, told
                # what to check, catches those before anything is said.
                if self._review and out:
                    try:
                        reviewed = clean_spoken(await self._chat(self._review_prompt(target_lang),
                                                                 f"English: {text}\nTranslation: {out}"))
                        if reviewed and reviewed != out:
                            log.info("translation corrected by review", lang=target_lang, before=out[:160],
                                     after=reviewed[:160])
                            out = reviewed
                    except Exception as exc:  # noqa: BLE001 - keep the first translation
                        log.warning("translation review skipped", error=str(exc)[:200])
            if not out:
                raise RuntimeError("empty translation")
            self._cache[key] = out
            return TranslationResult(out, source_lang, target_lang, self.provider)
        except Exception as exc:  # noqa: BLE001 - fail-open: the English line is still spoken
            metrics.translation_error(self.provider)
            log.warning("llm translator failed; speaking the original", error=str(exc)[:200])
            return TranslationResult(text, source_lang, target_lang, self.provider)


# Each digit in the language's own counting word, for phone numbers and codes read back to a caller.
_NATIVE_DIGITS = {
    "yo": ["òdo", "ọ̀kan", "èjì", "ẹ̀ta", "ẹ̀rin", "àrún", "ẹ̀fà", "èje", "ẹ̀jọ", "ẹ̀sán"],
    "ha": ["sifili", "ɗaya", "biyu", "uku", "huɗu", "biyar", "shida", "bakwai", "takwas", "tara"],
    "ig": ["efu", "otu", "abụọ", "atọ", "anọ", "ise", "isii", "asaa", "asatọ", "itoolu"],
}


def native_digits(text: str, lang: str) -> str:
    """Phone numbers and codes (4+ digits) in a translated line, spoken digit by digit with the language's own words
    ("0802" -> "òdo, ẹ̀jọ, òdo, èjì"); a pause between groups. Shorter numbers are left to the translation."""
    words = _NATIVE_DIGITS.get(lang)
    if not words:
        return text

    def group(m: "re.Match[str]") -> str:
        if sum(c.isdigit() for c in m.group(0)) < 4:  # a short number is a time, day or amount: the model words those
            return m.group(0)
        parts = re.split(r"[\s-]+", m.group(0).strip())
        return "; ".join(", ".join(words[int(d)] for d in p if d.isdigit()) for p in parts if p)

    return re.sub(r"\d+(?:[\s-]\d+)*", group, text)


_EMAIL = re.compile(r"\b([A-Za-z0-9._%+-]+)@([A-Za-z0-9.-]+\.[A-Za-z]{2,})\b")


def emails_spoken(text: str) -> str:
    """kemi.ojo@gmail.com -> kemi dot ojo at gmail dot com: the voices garble the symbols."""
    return _EMAIL.sub(lambda m: f"{m.group(1).replace('.', ' dot ')} at {m.group(2).replace('.', ' dot ')}", text)


# Letters a Yoruba/Hausa/Igbo line may contain: Latin with its tone marks and dots, plus the Hausa hooked letters.
# A stray character from another script (a Hebrew letter once appeared inside a Yoruba word) is dropped.
_NOT_SPOKEN = re.compile(r"[^\u0000-\u024F\u0300-\u036F\u1E00-\u1EFF\u0253\u0257\u0199\u01B4\u2018\u2019\u201C\u201D\u2013\u2014]")


def clean_spoken(text: str) -> str:
    return re.sub(r"\s{2,}", " ", _NOT_SPOKEN.sub("", text or "")).strip()


_DIGIT_WORDS = {"zero": "0", "oh": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6",
                "seven": "7", "eight": "8", "nine": "9"}
_DIGIT_WORD_RUN = re.compile(r"\b(?:zero|oh|one|two|three|four|five|six|seven|eight|nine)(?:[\s,]+(?:zero|oh|one|two|three|"
                             r"four|five|six|seven|eight|nine)\b){3,}", re.IGNORECASE)


def digit_words_to_digits(text: str) -> str:
    """A read-back in English digit words ("zero eight zero two, five five five") back to digits ("0802 555"), so it
    reaches the native-number step instead of being spoken as English inside a Yoruba/Hausa/Igbo sentence. Commas
    keep the caller's grouping. Runs of 4+ digit words only: "one moment" and "two people" are left alone."""
    def run(m: "re.Match[str]") -> str:
        groups = re.split(r"\s*,\s*", m.group(0).strip())
        return " ".join("".join(_DIGIT_WORDS[w.lower()] for w in g.split()) for g in groups if g.strip())

    return _DIGIT_WORD_RUN.sub(run, text)


def build_translator(settings: Any) -> TranslatorInterface:
    """Provider selection, mirroring the existing chain idioms.

    TRANSLATE_MOCK=1 (default) forces the MockTranslator regardless of
    TRANSLATE_PROVIDER — the sanctioned mock-default dev pattern
    (XTTS_MOCK/COSYVOICE_MOCK/MMS_MOCK/WHATSAPP_MOCK all default "1").
    """
    if _truthy(getattr(settings, "translate_mock", True)):
        return MockTranslator()
    provider = (getattr(settings, "translate_provider", "mock") or "mock").strip().lower()
    if provider == "llm":
        return LLMTranslator(
            getattr(settings, "translate_llm_base_url", "") or getattr(settings, "llm_base_url", ""),
            getattr(settings, "translate_llm_model", "") or getattr(settings, "llm_model", ""),
            api_key=getattr(settings, "llm_api_key", ""),
            timeout_s=max(getattr(settings, "translate_timeout_s", 3.0), 20.0),
            extra_body=getattr(settings, "llm_extra_body", None) or None,
        )
    if provider == "http":
        return HTTPTranslator(
            getattr(settings, "translate_url", ""),
            api_key=getattr(settings, "translate_api_key", ""),
            timeout_s=getattr(settings, "translate_timeout_s", 3.0),
        )
    if provider != "mock":
        log.warning(
            "unknown TRANSLATE_PROVIDER; falling back to mock translator",
            provider=provider,
        )
    return MockTranslator()


# ---------------------------------------------------------------------------
# A2/A3: fail-open turn helpers
# ---------------------------------------------------------------------------
def _provider_name(translator: Any) -> str:
    return str(getattr(translator, "provider", "unknown"))


async def _safe_translate(
    translator: TranslatorInterface,
    text: str,
    source_lang: str,
    target_lang: str,
    *,
    direction: str,
) -> TranslationResult:
    """One guarded translation call: metrics on every attempt, fail-open to
    the original text on ANY error (a turn never blocks on translation)."""
    provider = _provider_name(translator)
    metrics.translation(
        direction=direction,
        source_lang=source_lang,
        target_lang=target_lang,
        provider=provider,
    )
    try:
        return await translator.translate(text, source_lang, target_lang)
    except Exception as exc:  # noqa: BLE001 - belt-and-braces guard; the
        # built-in providers already fail open internally.
        metrics.translation_error(provider)
        log.warning(
            "translator raised; passing through original text",
            provider=provider,
            direction=direction,
            error=str(exc)[:200],
        )
        return TranslationResult(
            text=text,
            source_lang=source_lang,
            target_lang=target_lang,
            provider=provider,
        )


@dataclass
class InboundResult:
    """Outcome of the inbound (caller-language -> English) turn seam."""

    text: str  # effective text for routing/LLM (translated, or original)
    detected_language: str = ""
    original_text: str = ""
    translated: bool = False
    code_switch: bool = False
    segments: list[dict[str, str]] = field(default_factory=list)


async def process_inbound_text(
    text: str,
    translator: TranslatorInterface,
    *,
    enabled: bool = True,
    path: str = "chat",
    language_hint: str | None = None,
) -> InboundResult:
    """Inbound seam (A2): detect the caller language, translate pure
    pcm/yo/ig/ha turns to English for routing/LLM.

    Code-switched pcm/en turns (A4.1) are NOT force-translated wholesale —
    they pass through annotated (``code_switch=True`` + segments); only pure
    non-English turns go through full translation. Fail-open: detection and
    translation errors never alter or block the turn.
    """
    original = text or ""
    if not enabled or not original.strip():
        return InboundResult(text=original, original_text=original)
    try:
        # The call already knows the caller's language (they chose it, or the recogniser that heard them was for
        # it): a rough Hausa/Igbo/Yoruba transcript without its marked letters reads as English to the heuristic.
        hint = (language_hint or "").strip().lower()
        detected = hint if hint in ("yo", "ha", "ig") else detect_language(original)
    except Exception as exc:  # noqa: BLE001 - detection must never break a turn
        log.warning("language detection failed; passing through", error=str(exc)[:200])
        return InboundResult(text=original, original_text=original)
    metrics.language_detected(detected, path)
    if detected not in TRANSLATABLE_LANGUAGES:
        return InboundResult(
            text=original, detected_language=detected, original_text=original
        )
    # A4.1: pcm/en code-switch guard (mixed turns keep their mixed text;
    # the locale instruction tells the LLM to answer in kind).
    if detected == "pcm":
        from .codeswitch import analyze

        try:
            switch = analyze(original)
        except Exception as exc:  # noqa: BLE001 - annotation must never break a turn
            log.warning("code-switch analysis failed; treating as pure pcm", error=str(exc)[:200])
            switch = None
        if switch is not None and switch.code_switch:
            return InboundResult(
                text=original,
                detected_language=detected,
                original_text=original,
                code_switch=True,
                segments=[
                    {"text": s.text, "lang": s.lang} for s in switch.segments
                ],
            )
    result = await _safe_translate(
        translator, original, detected, "en", direction="inbound"
    )
    text_en = result.text
    if text_en.strip().lower().strip(".") == "(unclear)":
        # Nothing could be made out: say so plainly, so the receptionist asks again instead of guessing.
        text_en = "(The caller said something that could not be made out.)"
    return InboundResult(
        text=text_en,
        detected_language=detected,
        original_text=original,
        translated=result.text != original,
    )


async def process_outbound_text(
    text: str,
    target_lang: str | None,
    translator: TranslatorInterface,
    *,
    enabled: bool = True,
) -> str:
    """Outbound seam (A3): English reply -> caller language before TTS (voice)
    or before the chat reply payload.

    Fail-open to the English reply on any error. Skipped when the reply is
    already substantially non-English (detector sees the caller's language
    or a code-switch rather than English — the A4.1 pass-through).
    """
    if not enabled or not text or not text.strip():
        return text
    lang = normalize_language(target_lang)
    if not lang or lang == "en" or lang not in TRANSLATABLE_LANGUAGES:
        return text
    try:
        if detect_language(text) != "en":
            return text  # already (substantially) non-English
    except Exception:  # noqa: BLE001 - guard only; translate below fails open
        pass
    result = await _safe_translate(
        translator, text, "en", lang, direction="outbound"
    )
    return result.text
