"""2026-11-20: the edge (demo) provider's voices must survive Voice.as_dict (GET /voice/voices); a list for labels
made the catalog drop them."""
import asyncio

from app.tts_providers.edge import EdgeTTS


def test_edge_voices_serialise():
    tts = EdgeTTS.__new__(EdgeTTS)
    voices = [v.as_dict() for v in asyncio.run(tts.list_voices())]
    assert {v["id"] for v in voices} == {"en-NG-AbeoNeural", "en-NG-EzinneNeural"}
    assert all("edge" in v["labels"] for v in voices)
