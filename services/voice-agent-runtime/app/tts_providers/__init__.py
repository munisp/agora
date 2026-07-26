"""TTS provider abstraction + fallback chain (SPEC-W10 Part A)."""

from .azure import AZURE_VOICES, AzureTTS
from .base import TTSProvider, Voice, split_voice_spec
from .chain import FallbackTTS, PiperProvider, build_fallback_tts, build_provider
from .mms import MmsTTS
from .spitch import SpitchTTS
from .xtts import XttsTTS

__all__ = [
    "AZURE_VOICES",
    "AzureTTS",
    "FallbackTTS",
    "MmsTTS",
    "PiperProvider",
    "SpitchTTS",
    "TTSProvider",
    "Voice",
    "XttsTTS",
    "build_fallback_tts",
    "build_provider",
    "split_voice_spec",
]
