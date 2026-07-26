"""Voice registry for the XTTS sidecar.

Persists enrolled voices to {VOICES_DIR}/voices.json and reference samples to
{VOICES_DIR}/samples/{voice_id}.wav. The registry is loaded once at startup
and kept in memory; every mutation is written back atomically (tmp file +
os.replace) so a crash mid-write cannot corrupt the catalog.
"""

from __future__ import annotations

import json
import os
import threading
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, List, Optional

# XTTS-v2 supported languages (Coqui docs). Kept as the single source of
# truth for request validation.
XTTS_LANGUAGES = (
    "en", "es", "fr", "de", "it", "pt", "pl", "tr", "ru", "nl",
    "cs", "ar", "zh-cn", "ja", "ko", "hu", "hi",
)

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
                self._voices[entry["id"]] = entry

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
    def add(self, name: str, sample_bytes: bytes) -> dict:
        voice_id = uuid.uuid4().hex
        entry = {
            "id": voice_id,
            "name": name,
            "languages": list(XTTS_LANGUAGES),
            "gender": "cloned",
            "labels": ["xtts-v2", "voice-clone", "custom"],
            "created_at": datetime.now(timezone.utc).isoformat(),
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
