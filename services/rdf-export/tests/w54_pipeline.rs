//! SPEC-W54 Scope A offline pipeline tests (NO network): golden NDJSON
//! fixture (test-only, hand-written — every ontology class + predicate plus
//! unknown-label/predicate rows and decoy PII fields) -> mapping ->
//! Turtle + JSON-LD -> oxirs round-trip parse.
//!
//! The crate is binary-only; modules are compiled into this test crate via
//! `#[path]` (geo-service tests/w52_http.rs idiom).

// The pipeline tests exercise the pure mapping/serialize seam and the
// NDJSON splitter; the networked GraphClient is covered in w54_http.rs.
#[allow(dead_code)]
#[path = "../src/client.rs"]
mod client;
#[path = "../src/mapping.rs"]
mod mapping;
#[path = "../src/serialize.rs"]
mod serialize;

use client::{EdgeRow, NodeRow};
use mapping::RdfMapper;

const TENANT: &str = "11111111-2222-4333-8444-555555555555";
const NODES_NDJSON: &str = include_str!("fixtures/graph_nodes.ndjson");
const EDGES_NDJSON: &str = include_str!("fixtures/graph_edges.ndjson");

// Golden expectations (derived by hand from the fixture; see README tests
// section). 12 node rows (11 known labels + 1 unknown "Widget"), 14 edge
// rows (12 known predicates + unknown MESSAGED + unknown CLICKED).
const EXPECT_NODES: u64 = 12;
const EXPECT_EDGES: u64 = 14;
const EXPECT_SKIPPED: u64 = 3;
// 53 node quads (rdf:type + datatype-mapped properties) + 12 edge triples
// + 1 stub rdf:type for the edge-only endpoint case-0000 + 3 snapshot
// metadata quads = 69.
const EXPECT_QUADS: u64 = 69;

// Decoy plaintext PII baked into the fixture (the real export contract
// never carries these fields; serde drops them and the mapper never reads
// them). The grep test proves they cannot surface in RDF output.
const PII_MARKERS: [&str; 6] = [
    "Adaeze Testperson",
    "+2348012345678",
    "UNKNOWN-LABEL-NAME-MARKER",
    "+2348099999999",
    "widget-0001",
    "smoke-signal",
];

fn mapped_fixture() -> RdfMapper {
    let mut m = RdfMapper::new(TENANT);
    for row in parse_lines::<NodeRow>(NODES_NDJSON) {
        m.add_node(&row);
    }
    for row in parse_lines::<EdgeRow>(EDGES_NDJSON) {
        m.add_edge(&row);
    }
    m.add_snapshot_metadata("2026-01-15");
    m
}

/// NDJSON line iteration THROUGH the production splitter (blank lines and a
/// missing trailing newline included) — the same code path client.rs drives
/// off the wire.
fn parse_lines<T: serde::de::DeserializeOwned>(ndjson: &str) -> Vec<T> {
    let mut splitter = client::LineSplitter::new();
    let mut out = Vec::new();
    // Feed in awkward 7-byte chunks to exercise chunk-boundary handling.
    for chunk in ndjson.as_bytes().chunks(7) {
        for line in splitter.push(chunk) {
            out.push(serde_json::from_slice::<T>(&line).expect("fixture line parses"));
        }
    }
    if let Some(line) = splitter.finish() {
        out.push(serde_json::from_slice::<T>(&line).expect("fixture tail parses"));
    }
    out
}

#[test]
fn golden_fixture_counts_and_skip_reporting() {
    let m = mapped_fixture();
    assert_eq!(m.nodes, EXPECT_NODES, "node rows consumed");
    assert_eq!(m.edges, EXPECT_EDGES, "edge rows consumed");
    assert_eq!(
        m.skipped_unknown, EXPECT_SKIPPED,
        "unknown label/predicate rows skipped+counted"
    );
    assert_eq!(m.quads.len() as u64, EXPECT_QUADS, "total quads");

    // Every quad lives in EXACTLY the tenant named graph (never a shared
    // default graph mixing tenants).
    let want_graph = mapping::named_graph_iri(TENANT);
    for q in &m.quads {
        match q.graph_name() {
            oxirs_core::model::GraphName::NamedNode(n) => {
                assert_eq!(n.as_str(), want_graph)
            }
            other => panic!("quad in non-named graph: {other}"),
        }
    }
}

#[test]
fn unknown_labels_and_predicates_are_skipped_not_guessed() {
    let mut m = RdfMapper::new(TENANT);
    let unknown_node: NodeRow = serde_json::json!({
        "snapshot_date":"2026-01-15","tenant_id":TENANT,"label":"Gadget","node_id":"g-1"
    })
    .to_string()
    .pipe(|s| serde_json::from_str(&s).unwrap());
    m.add_node(&unknown_node);
    let unknown_edge: EdgeRow = serde_json::from_str(
        &serde_json::json!({
            "snapshot_date":"2026-01-15","tenant_id":TENANT,"edge_type":"TELEPORTED",
            "src_label":"Person","src_id":"a","dst_label":"Person","dst_id":"b"
        })
        .to_string(),
    )
    .unwrap();
    m.add_edge(&unknown_edge);
    // Known predicate but unknown endpoint label: also skipped.
    let bad_endpoint: EdgeRow = serde_json::from_str(
        &serde_json::json!({
            "snapshot_date":"2026-01-15","tenant_id":TENANT,"edge_type":"BOOKED",
            "src_label":"Person","src_id":"a","dst_label":"Gadget","dst_id":"g-1"
        })
        .to_string(),
    )
    .unwrap();
    m.add_edge(&bad_endpoint);
    assert_eq!(m.skipped_unknown, 3);
    assert!(m.quads.is_empty(), "skipped rows emit zero triples");
}

// Tiny helper to keep the json!().to_string() pipe readable.
trait Pipe: Sized {
    fn pipe<R>(self, f: impl FnOnce(Self) -> R) -> R {
        f(self)
    }
}
impl<T> Pipe for T {}

#[test]
fn turtle_roundtrip_via_oxirs() {
    let m = mapped_fixture();
    let docs = serialize::render(&m.quads).expect("render");
    let parsed = oxirs_ttl::turtle::TurtleParser::new()
        .parse_document(&docs.turtle)
        .expect("generated Turtle must re-parse with oxirs");
    assert_eq!(
        parsed.len() as u64,
        EXPECT_QUADS,
        "Turtle round-trip triple count"
    );

    // Spot triples: rdf:type Person and a datatype-mapped compliance flag.
    let p1 = mapping::node_iri(
        TENANT,
        "Person",
        "3a7bd3e2f1c94a58b06d2e9f47c1a5d8b903e6f42a71c08d5e6b49a07f3c2d1a",
    );
    let rendered: Vec<String> = parsed.iter().map(|t| t.to_string()).collect();
    assert!(
        rendered
            .iter()
            .any(|t| t.contains(&p1) && t.contains("Person")),
        "rdf:type Person spot triple present: {rendered:?}"
    );
    assert!(
        docs.turtle.contains("\"true\"^^xsd:boolean"),
        "consent flag typed xsd:boolean: {}",
        docs.turtle
    );
    assert!(
        docs.turtle.contains("\"15000\"^^xsd:integer"),
        "ltv_cents typed xsd:integer"
    );
}

#[test]
fn jsonld_roundtrip_named_graph_separation() {
    let m = mapped_fixture();
    let docs = serialize::render(&m.quads).expect("render");
    let value: serde_json::Value = serde_json::from_str(&docs.jsonld).expect("valid JSON");
    assert!(value.get("@context").is_some(), "inline @context present");
    let quads = oxirs_ttl::jsonld::JsonLdProcessor::to_rdf(&value)
        .expect("generated JSON-LD must convert back to RDF with oxirs");
    assert_eq!(
        quads.len() as u64,
        EXPECT_QUADS,
        "JSON-LD round-trip quad count"
    );
    let want_graph = mapping::named_graph_iri(TENANT);
    for q in &quads {
        let g = q
            .graph
            .as_ref()
            .expect("every quad carries the tenant named graph");
        assert_eq!(
            g.to_nquads_string()
                .trim_start_matches('<')
                .trim_end_matches('>'),
            want_graph,
            "named-graph separation: no default-graph leakage"
        );
    }
}

#[test]
fn zero_plaintext_pii_in_both_outputs() {
    let m = mapped_fixture();
    let docs = serialize::render(&m.quads).expect("render");
    for marker in PII_MARKERS {
        assert!(!docs.turtle.contains(marker), "Turtle leaks {marker:?}");
        assert!(!docs.jsonld.contains(marker), "JSON-LD leaks {marker:?}");
    }
}

#[test]
fn ndjson_robustness_blank_lines_crlf_missing_final_newline() {
    // Blank lines, CRLF, and NO trailing newline on the last record.
    let raw = "{\"snapshot_date\":\"2026-01-15\",\"tenant_id\":\"t\",\"label\":\"Tenant\",\"node_id\":\"t\"}\r\n\n   \n{\"snapshot_date\":\"2026-01-15\",\"tenant_id\":\"t\",\"label\":\"Tenant\",\"node_id\":\"t\"}";
    let rows: Vec<NodeRow> = parse_lines(raw);
    assert_eq!(rows.len(), 2, "blank lines skipped, unterminated tail kept");
    assert!(rows.iter().all(|r| r.label == "Tenant"));
}

#[test]
fn ontology_mapping_coherence() {
    // Every class/predicate declared in ontology/agora.ttl is handled by
    // mapping.rs and vice versa (V-INT-54 coherence gate).
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../../ontology/agora.ttl");
    let ttl = std::fs::read_to_string(path).expect("ontology/agora.ttl");
    let triples = oxirs_ttl::turtle::TurtleParser::new()
        .parse_document(&ttl)
        .expect("ontology parses as Turtle");
    let rendered: String = triples.iter().map(|t| t.to_string()).collect();

    for (_, class) in mapping::CLASS_TABLE {
        let iri = mapping::class_iri(class);
        assert!(
            rendered.contains(&format!("owl#Class")) && rendered.contains(&iri),
            "ontology declares agora:{class} as owl:Class"
        );
    }
    for (_, pred) in mapping::PREDICATE_TABLE {
        let iri = mapping::predicate_iri(pred);
        assert!(
            rendered.contains("owl#ObjectProperty") && rendered.contains(&iri),
            "ontology declares agora:{pred} as owl:ObjectProperty"
        );
    }
    // Reverse direction: no owl:Class / owl:ObjectProperty in the ontology
    // that mapping.rs does not handle, except the three documented
    // metadata-exception classes — asserted by NAME via exact set-equality
    // below (stronger than the old count check):
    //   * agora:GraphSnapshot — named-graph metadata node (snapshot IRI,
    //     tenant, snapshot_date quads), never a property-graph label;
    //   * agora:Conversation / agora:LedgerBalance — GDPR export-bundle
    //     terms (W54-C, ontology v1.1.0): JSON-LD bundle vocabulary only,
    //     never FalkorDB labels, so mapping.rs intentionally ignores them.
    let prop_decls = count_decls(&triples, "owl#ObjectProperty");
    assert_eq!(
        prop_decls,
        mapping::PREDICATE_TABLE.len(),
        "predicates == mapping table"
    );
    // Local names of every owl:Class declaration absent from the mapping
    // table must be EXACTLY the three metadata exceptions (set-equality by
    // name — a new unmapped class fails loudly, and so does removing one).
    let mapped: std::collections::BTreeSet<&str> = mapping::CLASS_TABLE
        .iter()
        .map(|(_, class)| *class)
        .collect();
    let unmapped: std::collections::BTreeSet<String> = triples
        .iter()
        .filter(|t| {
            t.predicate().to_string().contains("rdf-syntax-ns#type")
                && t.object().to_string().contains("owl#Class")
        })
        .filter_map(|t| {
            t.subject()
                .to_string()
                .trim_start_matches('<')
                .trim_end_matches('>')
                .strip_prefix(mapping::AGORA_NS)
                .map(str::to_owned)
        })
        .filter(|local| !mapped.contains(local.as_str()))
        .collect();
    let metadata_exceptions: std::collections::BTreeSet<String> =
        ["GraphSnapshot", "Conversation", "LedgerBalance"]
            .iter()
            .map(|s| s.to_string())
            .collect();
    assert_eq!(
        unmapped, metadata_exceptions,
        "non-mapping classes are exactly the three metadata exceptions \
         (GraphSnapshot, Conversation, LedgerBalance)"
    );
}

fn count_decls(triples: &[oxirs_core::model::Triple], type_local: &str) -> usize {
    triples
        .iter()
        .filter(|t| {
            t.predicate().to_string().contains("rdf-syntax-ns#type")
                && t.object().to_string().contains(type_local)
        })
        .count()
}
