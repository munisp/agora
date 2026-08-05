"""Graph access layer (SPEC-W30 §4 WS-B).

``GraphClient`` is the seam between detectors/writers and the graph store:

* ``RedisGraphClient`` — production: talks to FalkorDB over the Redis
  protocol via the ``redis`` package (``GRAPH.QUERY`` / ``GRAPH.RO_QUERY``),
  with a small decoder for the standard (non-compact) result format. The
  ``redis`` package is imported lazily so tests/dev need no live server.
* Tests inject a fake ``GraphClient`` (see ``tests/fakes.py``); the pytest
  suite never needs a live FalkorDB or Kafka.

Tenant isolation (SPEC-W30 §5 gate 1 / SPEC-W28 §6 gate 1): every query this
service emits binds ``$tenant_id``; ``GraphClient.query`` callers pass it in
``params`` and ``fraud_engine.detectors.base.assert_tenant_bound`` refuses to
emit any statement that does not.
"""

from __future__ import annotations

import threading
from typing import Any, Protocol


class GraphError(Exception):
    """Graph store failure (mapped to 502 by the API layer)."""


class GraphClient(Protocol):
    """Minimal async-free client surface (FalkorDB calls are sync + fast)."""

    def query(self, cypher: str, params: dict[str, Any]) -> list[dict[str, Any]]:
        """Run a read (or write) statement, returning rows as dicts.

        Write statements return an empty list on success.
        """
        ...

    def ping(self) -> bool: ...

    def close(self) -> None: ...


# ---------------------------------------------------------------------------
# FalkorDB over the Redis protocol (production)
# ---------------------------------------------------------------------------
# FalkorDB/RedisGraph standard (non-compact) value type tags.
_VALUE_TYPES = {
    0: "unknown",
    1: "null",
    2: "string",
    3: "integer",
    4: "boolean",
    5: "double",
    6: "array",
    7: "edge",
    8: "node",
    9: "path",
    10: "map",
    11: "point",
}


def _decode_scalar(value: Any) -> Any:
    """Decode a typed scalar [type_tag, raw] from the standard result format."""
    if not isinstance(value, (list, tuple)) or len(value) != 2 or not isinstance(value[0], int):
        return value  # already a plain redis reply (e.g. older servers)
    tag, raw = value
    kind = _VALUE_TYPES.get(tag, "unknown")
    if kind == "null":
        return None
    if kind in {"string", "integer", "double"}:
        return raw
    if kind == "boolean":
        return raw in ("true", True, 1)
    if kind == "array":
        return [_decode_scalar(v) for v in raw]
    if kind == "map":
        # flat [k1, v1, k2, v2, ...] with typed values
        out: dict[str, Any] = {}
        for i in range(0, len(raw), 2):
            out[raw[i]] = _decode_scalar(raw[i + 1])
        return out
    if kind == "node":
        return _decode_node(raw)
    if kind == "edge":
        return _decode_edge(raw)
    return raw


def _decode_props(raw_props: Any) -> dict[str, Any]:
    props: dict[str, Any] = {}
    for entry in raw_props or []:
        # [key, type_tag, raw_value]
        key, tag, raw = entry
        props[key] = _decode_scalar([tag, raw])
    return props


def _decode_node(raw: Any) -> dict[str, Any]:
    node_id, labels, props = raw
    return {"id": node_id, "labels": list(labels), "properties": _decode_props(props)}


def _decode_edge(raw: Any) -> dict[str, Any]:
    edge_id, rel_type, src, dst, props = raw
    return {
        "id": edge_id,
        "type": rel_type,
        "src": src,
        "dst": dst,
        "properties": _decode_props(props),
    }


def _substitute_params(cypher: str, params: dict[str, Any]) -> str:
    """Inline $params as Cypher literals.

    The Redis-protocol ``GRAPH.QUERY`` command takes a single statement
    string; parameters are inlined with proper escaping (Cypher's
    ``CYPHER key=value`` preamble is deprecated in FalkorDB). Values are
    quoted defensively — never interpolated from untrusted request input
    without going through this escaper.
    """

    def lit(v: Any) -> str:
        if v is None:
            return "null"
        if isinstance(v, bool):
            return "true" if v else "false"
        if isinstance(v, (int, float)):
            return repr(v)
        if isinstance(v, (list, tuple)):
            return "[" + ", ".join(lit(x) for x in v) + "]"
        s = str(v).replace("\\", "\\\\").replace("'", "\\'")
        return f"'{s}'"

    out = cypher
    # longest keys first so $tenant_id never clobbers $tenant_id2-style names
    for key in sorted(params, key=len, reverse=True):
        out = out.replace(f"${key}", lit(params[key]))
    return out


class RedisGraphClient:
    """FalkorDB client over the Redis protocol (``redis`` package)."""

    def __init__(
        self,
        host: str,
        port: int,
        graph_name: str,
        username: str = "",
        password: str = "",
    ) -> None:
        try:
            import redis  # lazy: driver not needed in tests
        except ImportError as exc:  # pragma: no cover
            raise GraphError("redis package not installed") from exc
        kwargs: dict[str, Any] = {
            "host": host,
            "port": port,
            "decode_responses": True,
            "socket_timeout": 10,
        }
        if username:
            kwargs["username"] = username
        if password:
            kwargs["password"] = password
        self._redis = redis.Redis(**kwargs)
        self.graph_name = graph_name
        self._lock = threading.Lock()  # redis-py is thread-safe; belt+braces

    def _execute(self, command: str, cypher: str, params: dict[str, Any]) -> Any:
        statement = _substitute_params(cypher, params)
        with self._lock:
            return self._redis.execute_command(command, self.graph_name, statement)

    def query(self, cypher: str, params: dict[str, Any]) -> list[dict[str, Any]]:
        try:
            raw = self._execute("GRAPH.QUERY", cypher, params)
        except Exception as exc:  # noqa: BLE001 — driver/redis outage
            raise GraphError(f"falkordb query failed: {type(exc).__name__}: {exc}") from exc
        if not raw:
            return []
        header, result_set = raw[0], (raw[1] if len(raw) > 1 else [])
        if not header:
            return []  # write-only statement: [ [], [], stats ]
        columns = [col[1] if isinstance(col, (list, tuple)) else col for col in header]
        rows: list[dict[str, Any]] = []
        for record in result_set or []:
            rows.append({col: _decode_scalar(val) for col, val in zip(columns, record)})
        return rows

    def ping(self) -> bool:
        try:
            self._execute("GRAPH.RO_QUERY", "RETURN 1", {})
            return True
        except Exception:  # noqa: BLE001
            return False

    def close(self) -> None:
        try:
            self._redis.close()
        except Exception:  # noqa: BLE001
            pass


def client_from_settings(settings: Any) -> GraphClient:
    """Production factory (tests inject fakes instead)."""
    return RedisGraphClient(
        host=settings.falkordb_host,
        port=settings.falkordb_port,
        graph_name=settings.falkordb_graph,
        username=settings.falkordb_username,
        password=settings.falkordb_password,
    )
