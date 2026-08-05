"""Backward-compat shim (SPEC-W29 §3 WS-B): the segment compiler now lives in
``app.segment.compiler``. This module re-exports its public surface so older
imports (``from app.compiler import compile_segment_query``) keep working.
"""

from __future__ import annotations

from .segment.compiler import compile_segment_query

__all__ = ["compile_segment_query"]
