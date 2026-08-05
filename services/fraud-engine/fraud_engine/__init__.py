"""fraud-engine — graph-based fraud detection over the W28 tenant knowledge graph.

SPEC-W30 §4 WS-B. Detection != punishment: detectors create Alert nodes and
(for F1/F2/F3-high only) set Person.quarantine=true. Humans adjudicate via
the graph-service alerts router. No auto-erasure anywhere.
"""

__version__ = "0.1.0"
