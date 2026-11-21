"""Corpus registry for the voice-corpus service (SPEC-W103 C1).

Persists the commissioned native-speaker corpus catalog to
{CORPUS_DIR}/registry.json and audio to
{CORPUS_DIR}/audio/{speaker_id}/{utterance_id}.wav. The registry is loaded
once at startup and kept in memory; every mutation is written back
atomically (tmp file + os.replace) so a crash mid-write cannot corrupt the
catalog.

Consent is FIRST-CLASS (mirrors the cosyvoice/longcat sidecars): every
speaker record carries {speaker_id, pseudonym, languages, consent_basis,
consent_reference, source, created_at} and enrollment is FAIL-CLOSED — a
missing/blank ``consent_basis`` is rejected before any bytes are written
(the pydantic 422 gate in app/main.py is the primary gate; add_speaker
re-asserts here, defense in depth).

NDPA posture: speakers are identified by PSEUDONYM ONLY (data minimization
— never legal names); ``consent_reference`` points at the signed consent
artefact (e.g. a document id in the DMS); consent revocation is
DELETE /v1/speakers/{id}, which removes the speaker's audio files and every
record — real deletion, not a tombstone.
"""

from __future__ import annotations

import json
import os
import shutil
import threading
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, List, Optional


class RegistryError(RuntimeError):
    pass


class SpeakerNotFound(RegistryError):
    pass


class UtteranceNotFound(RegistryError):
    pass


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


class CorpusRegistry:
    def __init__(self, corpus_dir: str | os.PathLike[str]):
        self.dir = Path(corpus_dir)
        self.audio_dir = self.dir / "audio"
        self.audio_dir.mkdir(parents=True, exist_ok=True)
        self._path = self.dir / "registry.json"
        self._lock = threading.Lock()
        self._speakers: Dict[str, dict] = {}
        self._utterances: Dict[str, dict] = {}
        if self._path.exists():
            try:
                raw = json.loads(self._path.read_text(encoding="utf-8"))
            except (json.JSONDecodeError, OSError) as exc:
                raise RegistryError(
                    f"corrupt corpus registry at {self._path}: {exc}"
                ) from exc
            for entry in raw.get("speakers", []):
                self._speakers[entry["speaker_id"]] = entry
            for entry in raw.get("utterances", []):
                self._utterances[entry["utterance_id"]] = entry

    # -- persistence -----------------------------------------------------------
    def _persist_locked(self) -> None:
        tmp = self._path.with_suffix(".json.tmp")
        payload = {
            "speakers": list(self._speakers.values()),
            "utterances": list(self._utterances.values()),
        }
        tmp.write_text(json.dumps(payload, indent=1), encoding="utf-8")
        os.replace(tmp, self._path)

    # -- speakers ----------------------------------------------------------------
    def list_speakers(self) -> List[dict]:
        with self._lock:
            return [dict(s) for s in self._speakers.values()]

    def get_speaker(self, speaker_id: str) -> Optional[dict]:
        with self._lock:
            entry = self._speakers.get(speaker_id)
            return dict(entry) if entry else None

    def add_speaker(
        self,
        pseudonym: str,
        languages: List[str],
        *,
        consent_basis: str,
        consent_reference: str = "",
        source: str,
    ) -> dict:
        # FAIL-CLOSED re-assert (defense in depth behind the pydantic gate):
        # no documented consent basis, no record — nothing is written.
        consent_basis = (consent_basis or "").strip()
        if not consent_basis:
            raise RegistryError("consent_basis must be a non-empty consent reference")
        pseudonym = (pseudonym or "").strip()
        if not pseudonym:
            raise RegistryError("pseudonym is required (NDPA: never legal names)")
        with self._lock:
            speaker_id = f"spk-{uuid.uuid4().hex[:12]}"
            record = {
                "speaker_id": speaker_id,
                "pseudonym": pseudonym,
                "languages": list(languages),
                "consent_basis": consent_basis,
                "consent_reference": (consent_reference or "").strip(),
                "source": (source or "").strip(),
                "created_at": _now(),
            }
            self._speakers[speaker_id] = record
            self._persist_locked()
            return dict(record)

    def delete_speaker(self, speaker_id: str) -> dict:
        """Consent revocation = real deletion: records AND audio bytes."""
        with self._lock:
            if speaker_id not in self._speakers:
                raise SpeakerNotFound(speaker_id)
            utts = [
                u for u in self._utterances.values()
                if u["speaker_id"] == speaker_id
            ]
            deleted_bytes = 0
            speaker_dir = self.audio_dir / speaker_id
            if speaker_dir.is_dir():
                for f in speaker_dir.rglob("*"):
                    if f.is_file():
                        deleted_bytes += f.stat().st_size
                shutil.rmtree(speaker_dir)
            for u in utts:
                del self._utterances[u["utterance_id"]]
            del self._speakers[speaker_id]
            self._persist_locked()
            return {
                "deleted_speaker": speaker_id,
                "deleted_utterances": len(utts),
                "deleted_bytes": deleted_bytes,
            }

    # -- utterances ---------------------------------------------------------------
    def list_utterances(
        self, *, speaker_id: str = "", language: str = ""
    ) -> List[dict]:
        with self._lock:
            out = []
            for u in self._utterances.values():
                if speaker_id and u["speaker_id"] != speaker_id:
                    continue
                if language and u["language"] != language:
                    continue
                out.append(dict(u))
            return out

    def get_utterance(self, utterance_id: str) -> Optional[dict]:
        with self._lock:
            entry = self._utterances.get(utterance_id)
            return dict(entry) if entry else None

    def utterance_path(self, speaker_id: str, utterance_id: str) -> Path:
        return self.audio_dir / speaker_id / f"{utterance_id}.wav"

    def add_utterance(
        self,
        speaker_id: str,
        language: str,
        transcript: str,
        audio_bytes: bytes,
        *,
        has_tone_marks: bool,
        duration_s: float,
        sample_rate: int,
        channels: int,
    ) -> dict:
        with self._lock:
            if speaker_id not in self._speakers:
                raise SpeakerNotFound(speaker_id)
            utterance_id = f"utt-{uuid.uuid4().hex[:12]}"
            path = self.utterance_path(speaker_id, utterance_id)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(audio_bytes)
            record = {
                "utterance_id": utterance_id,
                "speaker_id": speaker_id,
                "language": language,
                "transcript": transcript,
                "has_tone_marks": has_tone_marks,
                "duration_s": round(duration_s, 3),
                "sample_rate": sample_rate,
                "channels": channels,
                "audio_relpath": str(path.relative_to(self.dir)),
                "created_at": _now(),
            }
            self._utterances[utterance_id] = record
            self._persist_locked()
            return dict(record)

    def delete_utterance(self, utterance_id: str) -> dict:
        with self._lock:
            record = self._utterances.get(utterance_id)
            if record is None:
                raise UtteranceNotFound(utterance_id)
            path = self.dir / record["audio_relpath"]
            if path.is_file():
                path.unlink()
            del self._utterances[utterance_id]
            self._persist_locked()
            return {"deleted": utterance_id}
