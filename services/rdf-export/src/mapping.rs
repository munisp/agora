//! Property-graph -> RDF mapping (SPEC-W54 Scope A item 3).
//!
//! The term set is EXACTLY the platform ontology `ontology/agora.ttl`
//! (coherence is test-enforced: every class/predicate below exists in the
//! ontology and vice versa — the ontology header cites the source of truth,
//! docs/graph.md §2 "Graph schema (v1)").
//!
//!   * node label  -> agora: class IRI (rdf:type)
//!   * edge type   -> agora: predicate IRI
//!   * tenant_id   -> per-tenant NAMED GRAPH `https://agora.example/tenant/{uuid}`
//!                    (never a shared default graph mixing tenants)
//!   * node id     -> subject/object IRI `…/tenant/{uuid}/node/{label}/{id}`
//!                    — ids arrive W28-HASHED at source (export.py w28_hash);
//!                    they pass through VERBATIM. We never re-hash and never
//!                    emit name literals (the export contract carries none).
//!   * properties  -> datatype-mapped literal triples (xsd:integer /
//!                    xsd:double / xsd:boolean / xsd:dateTime; empty skipped).
//!                    consent_marketing + quarantine are compliance-relevant
//!                    and are always carried when present.
//!
//! Unknown labels/predicates: skip + count + report (`skipped_unknown`) —
//! NEVER guess a mapping. Edge endpoint nodes are always rdf:type'd (a
//! Booking/Consent/Location/Tenant/Case appears in the node NDJSON only via
//! its edges — export.py's node contract emits just
//! Person/Contact/Offering/Campaign rows), so the RDF graph stays fully
//! typed without inventing properties for them.

use std::collections::HashSet;

use oxirs_core::model::{Literal, NamedNode, Quad};

use crate::client::{EdgeRow, NodeRow};

/// Ontology namespace (placeholder — see ontology/agora.ttl header).
pub const AGORA_NS: &str = "https://agora.example/ontology#";
/// Per-tenant named-graph base.
pub const TENANT_NS: &str = "https://agora.example/tenant/";
/// rdf:type
pub const RDF_TYPE: &str = "http://www.w3.org/1999/02/22-rdf-syntax-ns#type";

/// label -> class local name. Single source of truth: CLASS_TABLE below.
pub fn class_local_name(label: &str) -> Option<&'static str> {
    CLASS_TABLE
        .iter()
        .find(|(l, _)| *l == label)
        .map(|(_, c)| *c)
}

/// edge type -> predicate local name. Single source of truth:
/// PREDICATE_TABLE below.
pub fn predicate_local_name(edge_type: &str) -> Option<&'static str> {
    PREDICATE_TABLE
        .iter()
        .find(|(e, _)| *e == edge_type)
        .map(|(_, p)| *p)
}

/// All (label, class local name) pairs — the exact term set the ontology
/// must declare (coherence test walks this table against agora.ttl). The 9
/// canonical classes of docs/graph.md §2 (+W32 Case) plus Alert (SPEC-W30
/// fraud-engine, alerts.py — written today with a FLAGGED edge; see README
/// "not-yet-mapped edge types").
pub const CLASS_TABLE: [(&str, &str); 10] = [
    ("Person", "Person"),
    ("Contact", "Contact"),
    ("Consent", "Consent"),
    ("Booking", "Booking"),
    ("Case", "Case"),
    ("Location", "Location"),
    ("Tenant", "Tenant"),
    ("Offering", "Offering"),
    ("Campaign", "Campaign"),
    ("Alert", "Alert"),
];

/// All (edge type, predicate local name) pairs — coherence-test table. The
/// 12 edge types: 11 from graph-sync/internal/graph/queries.go
/// (HAS_CONTACT :123, CONSENTED :168, BOOKED :215, FOR :221, REFERRED :231,
/// CAPTURED_AT :140, PART_OF :128, MERGE_CANDIDATE :272, REPORTED :324,
/// AT :338, MERGED_INTO :388) plus ALERTED (SPEC-W54 enumeration for the
/// fraud-engine alert->person linkage).
pub const PREDICATE_TABLE: [(&str, &str); 12] = [
    ("HAS_CONTACT", "hasContact"),
    ("CONSENTED", "consented"),
    ("BOOKED", "booked"),
    ("FOR", "for"),
    ("REFERRED", "referred"),
    ("CAPTURED_AT", "capturedAt"),
    ("PART_OF", "partOf"),
    ("MERGE_CANDIDATE", "mergeCandidate"),
    ("REPORTED", "reported"),
    ("AT", "at"),
    ("MERGED_INTO", "mergedInto"),
    ("ALERTED", "alerted"),
];

pub fn class_iri(local: &str) -> String {
    format!("{AGORA_NS}{local}")
}

pub fn predicate_iri(local: &str) -> String {
    format!("{AGORA_NS}{local}")
}

/// Per-tenant named graph: `https://agora.example/tenant/{tenant_id}`.
pub fn named_graph_iri(tenant_id: &str) -> String {
    format!("{TENANT_NS}{tenant_id}")
}

/// Node IRI: `https://agora.example/tenant/{t}/node/{label-lower}/{id}`.
/// `id` is the export's identifier (W28 hash for Person — pass-through,
/// never re-hashed; lead/offering/booking/... ids are not PII per
/// export.py:80-87).
pub fn node_iri(tenant_id: &str, label: &str, id: &str) -> String {
    format!(
        "{TENANT_NS}{tenant_id}/node/{}/{}",
        label.to_ascii_lowercase(),
        id
    )
}

/// IRI-path-safe component check (SigV4-safe superset minus '/'): keeps
/// generated IRIs well-formed and S3 keys valid (storage.rs shares the
/// charset rule). Violating rows are skipped+counted, never "fixed".
fn iri_safe(s: &str) -> bool {
    !s.is_empty()
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '.' | '_' | '~'))
}

/// XSD datatype IRIs used by the datatype mapping.
pub mod xsd {
    pub const INTEGER: &str = "http://www.w3.org/2001/XMLSchema#integer";
    pub const DOUBLE: &str = "http://www.w3.org/2001/XMLSchema#double";
    pub const BOOLEAN: &str = "http://www.w3.org/2001/XMLSchema#boolean";
    pub const DATE: &str = "http://www.w3.org/2001/XMLSchema#date";
    pub const DATETIME: &str = "http://www.w3.org/2001/XMLSchema#dateTime";
}

fn nn(iri: String) -> NamedNode {
    NamedNode::new(iri).expect("IRIs built by rdf-export are well-formed by construction")
}

/// Accumulating mapper: rows in, quads (one tenant named graph) out.
pub struct RdfMapper {
    tenant_id: String,
    graph: NamedNode,
    pub quads: Vec<Quad>,
    /// Node rows consumed.
    pub nodes: u64,
    /// Edge rows consumed.
    pub edges: u64,
    /// Rows skipped because the label/predicate/id was unknown or unsafe.
    pub skipped_unknown: u64,
    typed_nodes: HashSet<String>,
}

impl RdfMapper {
    pub fn new(tenant_id: &str) -> Self {
        Self {
            tenant_id: tenant_id.to_string(),
            graph: nn(named_graph_iri(tenant_id)),
            quads: Vec::new(),
            nodes: 0,
            edges: 0,
            skipped_unknown: 0,
            typed_nodes: HashSet::new(),
        }
    }

    fn push(
        &mut self,
        subject: NamedNode,
        predicate: &str,
        object: impl Into<oxirs_core::model::Object>,
    ) {
        self.quads.push(Quad::new(
            subject,
            nn(predicate.to_string()),
            object,
            self.graph.clone(),
        ));
    }

    fn type_node(&mut self, label: &str, id: &str) -> Option<NamedNode> {
        let class = class_local_name(label)?;
        if !iri_safe(id) {
            return None;
        }
        let subject = nn(node_iri(&self.tenant_id, label, id));
        if self
            .typed_nodes
            .insert(node_iri(&self.tenant_id, label, id))
        {
            self.push(subject.clone(), RDF_TYPE, nn(class_iri(class)));
        }
        Some(subject)
    }

    fn literal(&mut self, subject: &NamedNode, pred_local: &str, value: String, datatype: &str) {
        self.push(
            subject.clone(),
            &predicate_iri(pred_local),
            Literal::new_typed_literal(value, nn(datatype.to_string())),
        );
    }

    /// Map one node row. Unknown label / unsafe id -> skip+count.
    pub fn add_node(&mut self, row: &NodeRow) {
        self.nodes += 1;
        let Some(subject) = self.type_node(&row.label, &row.node_id) else {
            self.skipped_unknown += 1;
            return;
        };
        // Datatype mapping: integers, doubles, booleans, ISO-8601 dates.
        if let Some(v) = row.in_degree {
            self.literal(&subject, "inDegree", v.to_string(), xsd::INTEGER);
        }
        if let Some(v) = row.out_degree {
            self.literal(&subject, "outDegree", v.to_string(), xsd::INTEGER);
        }
        if let Some(v) = row.consent_marketing {
            // Compliance-relevant consent flag — always carried when present.
            self.literal(&subject, "consentMarketing", v.to_string(), xsd::BOOLEAN);
        }
        if let Some(v) = row.quarantine {
            // Compliance-relevant quarantine flag — always carried.
            self.literal(&subject, "quarantine", v.to_string(), xsd::BOOLEAN);
        }
        if let Some(v) = row.bookings_total {
            self.literal(&subject, "bookingsTotal", v.to_string(), xsd::INTEGER);
        }
        if let Some(v) = row.ltv_cents {
            self.literal(&subject, "ltvCents", v.to_string(), xsd::INTEGER);
        }
        if let Some(v) = row.no_show_rate {
            self.literal(&subject, "noShowRate", fmt_double(v), xsd::DOUBLE);
        }
        if let Some(v) = row.propensity_show {
            self.literal(&subject, "propensityShow", fmt_double(v), xsd::DOUBLE);
        }
        if let Some(v) = row.propensity_convert {
            self.literal(&subject, "propensityConvert", fmt_double(v), xsd::DOUBLE);
        }
        if let Some(v) = row
            .channel_of_first_touch
            .as_deref()
            .filter(|s| !s.is_empty())
        {
            self.push(
                subject.clone(),
                &predicate_iri("channelOfFirstTouch"),
                Literal::new_simple_literal(v),
            );
        }
        if let Some(v) = row.last_active_at.as_deref().filter(|s| !s.is_empty()) {
            self.literal(&subject, "lastActiveAt", v.to_string(), xsd::DATETIME);
        }
    }

    /// Map one edge row. Unknown predicate OR unknown/unsafe endpoint label
    /// -> skip+count (never guess). Endpoints are rdf:type'd on first sight
    /// so nodes absent from the node NDJSON stream still get a class.
    pub fn add_edge(&mut self, row: &EdgeRow) {
        self.edges += 1;
        let Some(pred_local) = predicate_local_name(&row.edge_type) else {
            self.skipped_unknown += 1;
            return;
        };
        if class_local_name(&row.src_label).is_none()
            || class_local_name(&row.dst_label).is_none()
            || !iri_safe(&row.src_id)
            || !iri_safe(&row.dst_id)
        {
            self.skipped_unknown += 1;
            return;
        }
        let Some(src) = self.type_node(&row.src_label, &row.src_id) else {
            self.skipped_unknown += 1;
            return;
        };
        let Some(dst) = self.type_node(&row.dst_label, &row.dst_id) else {
            self.skipped_unknown += 1;
            return;
        };
        self.push(src, &predicate_iri(pred_local), dst);
        // Edge scalar properties (weight, edge_at) are NOT reified in v1 —
        // documented limitation (README "Mapping notes"); the platform edge
        // schema's compliance-relevant data lives on the nodes.
    }

    /// Snapshot metadata quads: the named graph describes itself
    /// (agora:GraphSnapshot + tenant id + snapshot partition date). Emitted
    /// INSIDE the tenant named graph — there is still no default graph.
    pub fn add_snapshot_metadata(&mut self, snapshot_date: &str) {
        let g = self.graph.clone();
        self.quads.push(Quad::new(
            g.clone(),
            nn(RDF_TYPE.to_string()),
            nn(class_iri("GraphSnapshot")),
            self.graph.clone(),
        ));
        self.push(
            g.clone(),
            &predicate_iri("tenantId"),
            Literal::new_simple_literal(self.tenant_id.clone()),
        );
        self.literal(&g, "snapshotDate", snapshot_date.to_string(), xsd::DATE);
    }
}

/// Shortest lossless-ish double lexical form (Rust Display is
/// shortest-round-trip; "2" for 2.0 is valid xsd:double lexical space).
fn fmt_double(v: f64) -> String {
    format!("{v}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tables_match_lookup_fns() {
        for (label, class) in CLASS_TABLE {
            assert_eq!(class_local_name(label), Some(class));
        }
        for (edge, pred) in PREDICATE_TABLE {
            assert_eq!(predicate_local_name(edge), Some(pred));
        }
        assert_eq!(class_local_name("Widget"), None);
        assert_eq!(predicate_local_name("MESSAGED"), None);
    }

    #[test]
    fn iri_and_graph_shapes() {
        assert_eq!(
            named_graph_iri("11111111-2222-3333-4444-555555555555"),
            "https://agora.example/tenant/11111111-2222-3333-4444-555555555555"
        );
        assert_eq!(
            node_iri("t", "Person", "abc123"),
            "https://agora.example/tenant/t/node/person/abc123"
        );
        assert!(iri_safe("a-b_c.d~e9"));
        assert!(!iri_safe("a/b"));
        assert!(!iri_safe(""));
        assert!(!iri_safe("a b"));
    }
}
