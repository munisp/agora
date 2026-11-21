"""2026-11-21: FalkorDB's standard reply stringifies array values; as_list turns them back into lists."""
from fraud_engine.graph import as_list


def test_as_list_variants():
    assert as_list(None) == []
    assert as_list(["a", "b"]) == ["a", "b"]
    assert as_list("[f9151875-4257, d2e6e3fb-4f11, a256d23b-742f]") == ["f9151875-4257", "d2e6e3fb-4f11", "a256d23b-742f"]
    assert as_list("[0.1, 0.25, -1]") == [0.1, 0.25, -1]
    assert as_list("[]") == []
