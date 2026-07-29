"""Hand-rolled Prometheus text exposition for the voice inference plane
(VOICE-SCALING §3): per-call STT/LLM/TTS latency, LLM token usage, tool-call
outcomes and active voice sessions, served on the control plane `/metrics`.

No prometheus-client dependency: the registry is a small thread-safe in-memory
store with a `render()` producing the standard text exposition format, so the
worker job processes and the control plane can both record at low overhead.

Series:
- voice_stt_latency_seconds   (histogram)
- voice_llm_latency_seconds   (histogram)
- voice_llm_tokens_total      (counter, label kind=prompt|completion)
- voice_tts_latency_seconds   (histogram)
- tts_provider_failures_total (counter, label provider) — SPEC-W10 chain
- voice_tool_calls_total      (counter, labels tool,result)
- voice_active_sessions       (gauge)
- voice_emergency_sessions_total (counter) — SPEC-W11 Part C emergency lane
"""

from __future__ import annotations

import contextvars
import threading
import time
from typing import Any

DEFAULT_BUCKETS = (0.05, 0.1, 0.25, 0.5, 1.0, 2.0, 4.0, 8.0)


def _labels_key(labels: dict[str, str]) -> tuple[tuple[str, str], ...]:
    return tuple(sorted(labels.items()))


def _format_labels(labels: tuple[tuple[str, str], ...]) -> str:
    if not labels:
        return ""
    inner = ",".join(f'{k}="{v}"' for k, v in labels)
    return "{" + inner + "}"


class Histogram:
    def __init__(self, name: str, description: str, buckets=DEFAULT_BUCKETS) -> None:
        self.name = name
        self.description = description
        self.buckets = tuple(sorted(buckets))
        self._counts = [0] * len(self.buckets)
        self._sum = 0.0
        self._count = 0
        self._lock = threading.Lock()

    def observe(self, value: float) -> None:
        with self._lock:
            for i, bound in enumerate(self.buckets):
                if value <= bound:
                    self._counts[i] += 1
            self._sum += value
            self._count += 1

    def time(self) -> "_Timer":
        return _Timer(self)

    def render(self) -> list[str]:
        lines = [
            f"# HELP {self.name} {self.description}",
            f"# TYPE {self.name} histogram",
        ]
        with self._lock:
            counts = list(self._counts)
            total = self._count
            sum_ = self._sum
        for bound, count in zip(self.buckets, counts):
            lines.append(f'{self.name}_bucket{{le="{bound:g}"}} {count}')
        lines.append(f'{self.name}_bucket{{le="+Inf"}} {total}')
        lines.append(f"{self.name}_sum {sum_:.6f}")
        lines.append(f"{self.name}_count {total}")
        return lines


class _Timer:
    """Context manager: `with hist.time(): ...` records wall-clock seconds."""

    def __init__(self, hist: Histogram) -> None:
        self._hist = hist
        self._start = 0.0
        self.elapsed = 0.0  # seconds measured by the last `with` block

    def __enter__(self) -> "_Timer":
        self._start = time.perf_counter()
        return self

    def __exit__(self, *exc) -> None:
        self.elapsed = time.perf_counter() - self._start
        self._hist.observe(self.elapsed)


class Counter:
    def __init__(self, name: str, description: str, label_names: tuple[str, ...]) -> None:
        self.name = name
        self.description = description
        self.label_names = label_names
        self._values: dict[tuple[tuple[str, str], ...], float] = {}
        self._lock = threading.Lock()

    def inc(self, amount: float = 1.0, **labels: str) -> None:
        key = _labels_key({k: str(v) for k, v in labels.items()})
        with self._lock:
            self._values[key] = self._values.get(key, 0.0) + amount

    def render(self) -> list[str]:
        lines = [
            f"# HELP {self.name} {self.description}",
            f"# TYPE {self.name} counter",
        ]
        with self._lock:
            items = sorted(self._values.items())
        for key, value in items:
            lines.append(f"{self.name}{_format_labels(key)} {value:g}")
        return lines


class Gauge:
    def __init__(self, name: str, description: str) -> None:
        self.name = name
        self.description = description
        self._value = 0.0
        self._lock = threading.Lock()

    def set(self, value: float) -> None:
        with self._lock:
            self._value = value

    def inc(self, amount: float = 1.0) -> None:
        with self._lock:
            self._value += amount

    def dec(self, amount: float = 1.0) -> None:
        with self._lock:
            self._value -= amount

    def render(self) -> list[str]:
        with self._lock:
            value = self._value
        return [
            f"# HELP {self.name} {self.description}",
            f"# TYPE {self.name} gauge",
            f"{self.name} {value:g}",
        ]


class Registry:
    def __init__(self) -> None:
        self.stt_latency = Histogram(
            "voice_stt_latency_seconds", "STT transcription latency per call."
        )
        self.llm_latency = Histogram(
            "voice_llm_latency_seconds", "LLM chat-completion latency per call."
        )
        self.llm_tokens = Counter(
            "voice_llm_tokens_total",
            "LLM tokens consumed.",
            ("kind",),  # kind=prompt|completion
        )
        self.tts_latency = Histogram(
            "voice_tts_latency_seconds", "TTS synthesis latency per call."
        )
        # SPEC-W10 Part A: per-provider failures in the TTS fallback chain
        # (app/tts_providers/chain.py FallbackTTS); label provider =
        # piper|mms|xtts|azure|spitch.
        self.tts_provider_failures = Counter(
            "tts_provider_failures_total",
            "TTS provider failures triggering chain fallback.",
            ("provider",),
        )
        self.tool_calls = Counter(
            "voice_tool_calls_total",
            "Tool calls executed by the voice tool layer.",
            ("tool", "result"),  # result=ok|error|timeout
        )
        self.active_sessions = Gauge(
            "voice_active_sessions", "Currently active voice/chat sessions."
        )
        # SPEC-W11 Part C: sessions where the live-turn emergency detector
        # (app/emergency.py) latched — one increment per emergency session.
        self.emergency_sessions = Counter(
            "voice_emergency_sessions_total",
            "Voice sessions flagged as emergencies (warm-handoff triggered).",
            (),
        )

    def render(self) -> str:
        lines: list[str] = []
        for metric in (
            self.stt_latency,
            self.llm_latency,
            self.llm_tokens,
            self.tts_latency,
            self.tts_provider_failures,
            self.tool_calls,
            self.active_sessions,
            self.emergency_sessions,
        ):
            lines.extend(metric.render())
        return "\n".join(lines) + "\n"


# Process-wide default registry. Call sites record directly into it; tests
# swap it via `set_registry`.
_registry = Registry()


def get_registry() -> Registry:
    return _registry


def set_registry(registry: Registry) -> None:
    global _registry
    _registry = registry


def reset_registry() -> Registry:
    """Fresh registry (test isolation); returns it."""
    registry = Registry()
    set_registry(registry)
    return registry


def render() -> str:
    return _registry.render()


def tts_provider_failure(provider: str) -> None:
    """SPEC-W10: one failed hop in the TTS provider fallback chain."""
    _registry.tts_provider_failures.inc(provider=provider)


def emergency_session() -> None:
    """SPEC-W11 Part C: one voice session flagged as an emergency."""
    _registry.emergency_sessions.inc()


# ---------------------------------------------------------------------------
# Per-session call-quality accumulator (CRM gap fix): while the registry
# above aggregates process-wide for Prometheus, a SessionMetrics instance
# tracks the signals of ONE conversation so they can be attached to the
# SessionEnded CloudEvent (consumed by crm-sync-service to write a Twenty
# call-summary note).
#
# The "active session" is a contextvar: the LiveKit worker activates one
# SessionMetrics per job (one room = one session per job process), and the
# recording call sites below no-op when no session is active (control-plane
# scrape paths, unit tests, sessions without instrumentation).
# ---------------------------------------------------------------------------
class SessionMetrics:
    """Thread-safe accumulator for one conversation's quality signals.

    Records STT/TTS call counts, per-call LLM latencies, tool-call counts,
    turn count and whether the LLM fallback chain was used. `quality_payload`
    renders the `quality` object for the SessionEnded event, or None when
    nothing was recorded (guard: no data -> no quality key on the event).
    """

    def __init__(self, conversation_id: str, *, clock=time.time) -> None:
        self.conversation_id = conversation_id
        self._clock = clock
        self.started_at = clock()
        self.turn_count = 0
        self.stt_calls = 0
        self.tts_calls = 0
        self.llm_fallback_used = False
        # SPEC-W11 Part C: latched when the live-turn emergency detector
        # flagged this session (app/emergency.py); flows into the
        # SessionEnded quality payload as an additive `emergency` bool key.
        self.emergency = False
        self._llm_latencies_ms: list[float] = []
        self._tool_calls: dict[str, int] = {}
        self._lock = threading.Lock()

    # ------------------------------------------------------------- recording
    def record_turn(self) -> None:
        with self._lock:
            self.turn_count += 1

    def record_stt(self) -> None:
        with self._lock:
            self.stt_calls += 1

    def record_tts(self) -> None:
        with self._lock:
            self.tts_calls += 1

    def record_tool_call(self, name: str) -> None:
        with self._lock:
            self._tool_calls[name] = self._tool_calls.get(name, 0) + 1

    def record_llm_latency(self, seconds: float) -> None:
        with self._lock:
            self._llm_latencies_ms.append(seconds * 1000.0)

    def record_llm_fallback(self) -> None:
        with self._lock:
            self.llm_fallback_used = True

    def record_emergency(self) -> None:
        with self._lock:
            self.emergency = True

    # -------------------------------------------------------------- snapshot
    def tool_calls(self) -> dict[str, int]:
        with self._lock:
            return dict(self._tool_calls)

    def has_data(self) -> bool:
        with self._lock:
            return bool(
                self.turn_count
                or self.stt_calls
                or self.tts_calls
                or self._llm_latencies_ms
                or self._tool_calls
                or self.emergency
            )

    def quality_payload(
        self,
        *,
        escalated: bool = False,
        confirmed_phone: str | None = None,
    ) -> dict[str, Any] | None:
        """SessionEnded `quality` object, or None when nothing was recorded.

        Latency fields are null (not 0) when the session made no LLM calls
        through the instrumented path (e.g. the LiveKit worker's plugin LLM
        node — only the fallback chain and the chat tool loop time calls).
        """
        with self._lock:
            latencies = list(self._llm_latencies_ms)
            tools = dict(self._tool_calls)
            empty = not (
                self.turn_count
                or self.stt_calls
                or self.tts_calls
                or latencies
                or tools
                or self.emergency
            )
            duration_s = round(self._clock() - self.started_at, 1)
            turn_count = self.turn_count
            stt_calls = self.stt_calls
            tts_calls = self.tts_calls
            fallback = self.llm_fallback_used
            emergency = self.emergency
        if empty:
            return None
        return {
            "duration_s": duration_s,
            "turn_count": turn_count,
            "tool_calls": tools,
            "avg_llm_latency_ms": (
                round(sum(latencies) / len(latencies)) if latencies else None
            ),
            "max_llm_latency_ms": round(max(latencies)) if latencies else None,
            "stt_calls": stt_calls,
            "tts_calls": tts_calls,
            "llm_fallback_used": fallback,
            "escalated": escalated,
            "confirmed_phone": confirmed_phone,
            # SPEC-W11 Part C (additive key; downstream consumers tolerate
            # unknown keys): True when the emergency lane fired this session.
            "emergency": emergency,
        }


_active_session: contextvars.ContextVar[SessionMetrics | None] = contextvars.ContextVar(
    "voice_session_metrics", default=None
)


def activate_session(session: SessionMetrics) -> SessionMetrics:
    """Make `session` the active per-session accumulator for this context.

    Returns the session so callers can keep a direct reference. The binding
    lives for the rest of the context (a LiveKit job runs exactly one
    session), so no explicit deactivate is needed; tests can rebind freely.
    """
    _active_session.set(session)
    return session


def get_active_session() -> SessionMetrics | None:
    return _active_session.get()


# Recording helpers used at the same call sites as the global registry. All
# are no-ops when no session is active in this context.
def session_turn() -> None:
    if (s := get_active_session()) is not None:
        s.record_turn()


def session_stt() -> None:
    if (s := get_active_session()) is not None:
        s.record_stt()


def session_tts() -> None:
    if (s := get_active_session()) is not None:
        s.record_tts()


def session_tool_call(name: str) -> None:
    if (s := get_active_session()) is not None:
        s.record_tool_call(name)


def session_llm_latency(seconds: float) -> None:
    if (s := get_active_session()) is not None:
        s.record_llm_latency(seconds)


def session_llm_fallback() -> None:
    if (s := get_active_session()) is not None:
        s.record_llm_fallback()


def session_emergency() -> None:
    """SPEC-W11 Part C: latch the emergency flag on the active session."""
    if (s := get_active_session()) is not None:
        s.record_emergency()
