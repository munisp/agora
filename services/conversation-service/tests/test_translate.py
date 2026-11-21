from app.intel import parse_translation


def test_english_lines_are_marked_not_translated():
    assert parse_translation("ENGLISH", "Good afternoon") == ""
    assert parse_translation("english.", "Hello") == ""
    assert parse_translation("Good afternoon", "Good afternoon") == ""  # an echo is not a translation
    assert parse_translation("", "x") is None  # no reply: not stored, retried later


def test_translation_is_kept_clean():
    assert parse_translation('"Good evening, I want to open a pension account."', "Ẹ kú ìrọ̀lẹ́ …") == \
        "Good evening, I want to open a pension account."


def test_recording_notices_are_translated_exactly():
    import asyncio
    import json

    import httpx

    from app.intel import translate_to_english

    seen = []

    def handler(request):
        seen.append(json.loads(request.content)["messages"][1]["content"])
        return httpx.Response(200, json={"choices": [{"message": {"content": "Welcome to Leadway Pensure PFA!"}}]})

    class Cfg:
        translate_llm_base_url = "http://llm"
        translate_llm_model = "m"
        translate_llm_timeout_s = 5

    async def run(text):
        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            return await translate_to_english(text, Cfg(), client=client)

    assert asyncio.run(run("A lè ṣe ìgbàsílẹ̀ ìpè yìí. Ẹ kú àbọ̀ sí Leadway Pensure PFA!")) == \
        "This call may be recorded. Welcome to Leadway Pensure PFA!"
    assert seen == ["Ẹ kú àbọ̀ sí Leadway Pensure PFA!"]  # the model never sees the notice
    assert asyncio.run(run("Ana iya nadar wannan kiran.")) == "This call may be recorded."
    assert len(seen) == 1
