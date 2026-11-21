"""Voice registry for the LongCat-TTS sidecar (SPEC-W102 C1).

Persists enrolled voices to {VOICES_DIR}/voices.json and reference samples
to {VOICES_DIR}/samples/{voice_id}.wav. The registry is loaded once at
startup and kept in memory; every mutation is written back atomically
(tmp file + os.replace) so a crash mid-write cannot corrupt the catalog.

Consent is FIRST-CLASS here (unlike the xtts sidecar, where consent lives
only in the admin UI): every voice record carries
{voice_id, name, languages, source, license, consent_basis, enrolled_at}
and enrollment is FAIL-CLOSED — a missing/blank ``consent_basis`` is
rejected before any bytes are written (see app/main.py; docs/VOICES.md).
"""

from __future__ import annotations

import json
import os
import threading
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, List, Optional

# LongCat-AudioDiT (Meituan, MIT — code AND weights) is benchmarked on
# Chinese + English (Seed-bench ZH/EN); honest language coverage is exactly
# those two. Nigerian languages (pcm/yo/ha/ig) are NOT covered by the base
# model — do NOT advertise it as a Nigerian-voice path (docs/voices-sidecars.md).
LONGCAT_LANGUAGES = ("en", "zh")

MIN_SAMPLE_BYTES = 1000  # anything smaller cannot be a usable wav sample
MAX_SAMPLE_BYTES = 50 * 1024 * 1024  # 50 MB decoded ceiling


class RegistryError(RuntimeError):
    pass


class VoiceNotFound(RegistryError):
    pass


class VoiceRegistry:
    def __init__(self, voices_dir: str | os.PathLike[str]):
        self.dir = Path(voices_dir)
        self.samples_dir = self.dir / "samples"
        self.samples_dir.mkdir(parents=True, exist_ok=True)
        self._path = self.dir / "voices.json"
        self._lock = threading.Lock()
        self._voices: Dict[str, dict] = {}
        if self._path.exists():
            try:
                raw = json.loads(self._path.read_text(encoding="utf-8"))
            except (json.JSONDecodeError, OSError) as exc:
                raise RegistryError(f"corrupt voice registry at {self._path}: {exc}") from exc
            for entry in raw.get("voices", []):
                self._voices[entry["voice_id"]] = entry

    # -- reads ---------------------------------------------------------------
    def list(self) -> List[dict]:
        with self._lock:
            return [dict(v) for v in self._voices.values()]

    def get(self, voice_id: str) -> Optional[dict]:
        with self._lock:
            entry = self._voices.get(voice_id)
            return dict(entry) if entry else None

    def sample_path(self, voice_id: str) -> Path:
        return self.samples_dir / f"{voice_id}.wav"

    # -- mutations -------------------------------------------------------------
    def add(
        self,
        name: str,
        sample_bytes: bytes,
        *,
        consent_basis: str,
        source: str,
        license: str = "",
        languages: Optional[List[str]] = None,
        prompt_text: str = "",
    ) -> dict:
        """Enroll a voice. ``consent_basis`` MUST be a non-empty documented
        consent reference — the HTTP layer enforces this fail-closed; the
        registry asserts it again so no caller can bypass the gate."""
        consent_basis = (consent_basis or "").strip()
        if not consent_basis:
            raise RegistryError("consent_basis is required for voice enrollment")
        # prompt_text: transcript of the reference sample — LongCat zero-shot
        # cloning concatenates prompt_text + gen_text, so cloning is only as
        # good as this transcript (empty = real path falls back to plain TTS).
        voice_id = uuid.uuid4().hex
        entry = {
            "voice_id": voice_id,
            "name": name,
            "languages": list(languages) if languages else list(LONGCAT_LANGUAGES),
            "source": source,
            "license": license,
            "consent_basis": consent_basis,
            "prompt_text": (prompt_text or "").strip(),
            "enrolled_at": datetime.now(timezone.utc).isoformat(),
        }
        sample_file = self.sample_path(voice_id)
        with self._lock:
            sample_file.write_bytes(sample_bytes)
            self._voices[voice_id] = entry
            try:
                self._persist_locked()
            except BaseException:
                # Roll back in-memory state and the sample file.
                self._voices.pop(voice_id, None)
                sample_file.unlink(missing_ok=True)
                raise
        return dict(entry)

    def delete(self, voice_id: str) -> dict:
        with self._lock:
            entry = self._voices.pop(voice_id, None)
            if entry is None:
                raise VoiceNotFound(voice_id)
            self.sample_path(voice_id).unlink(missing_ok=True)
            self._persist_locked()
        return entry

    def _persist_locked(self) -> None:
        payload = json.dumps(
            {"voices": list(self._voices.values())}, indent=2, sort_keys=True
        )
        tmp = self._path.with_suffix(".json.tmp")
        tmp.write_text(payload, encoding="utf-8")
        os.replace(tmp, self._path)
