"""clamd answers z-commands NUL-terminated: a clean verdict must still read as clean."""
from app.av import Clean, Infected, _parse_reply


def test_nul_terminated_replies():
    assert isinstance(_parse_reply(b"stream: OK\x00"), Clean)
    found = _parse_reply(b"stream: Eicar-Signature FOUND\x00")
    assert isinstance(found, Infected) and found.signature == "Eicar-Signature"
