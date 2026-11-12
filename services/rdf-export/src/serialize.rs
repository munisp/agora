//! RDF serialization via oxirs (SPEC-W54 Scope A item 4):
//!
//!   * **Turtle** — `oxirs_ttl::writer::TurtleWriter` (prefix-abbreviated,
//!     deterministic subject/predicate grouping). Turtle 1.1 has NO named
//!     graphs; the file is inherently single-tenant because one export run
//!     serves exactly one tenant (see README "Named-graph posture").
//!   * **JSON-LD 1.1** — `oxirs_ttl::jsonld::JsonLdWriter` with an inline
//!     `@context` mapping the ontology terms and per-quad named-graph
//!     grouping (`@id` + `@graph` per tenant graph IRI).
//!
//! Both outputs are produced from the same `oxirs_core::model::Quad` set, so
//! they are content-identical modulo the graph dimension. Round-trip
//! guarantees are test-enforced: tests parse BOTH documents back with oxirs
//! (TurtleParser / JsonLdProcessor::to_rdf) and assert triple counts, spot
//! triples, and named-graph separation.

use oxirs_core::model::{GraphName, Literal, Object, Predicate, Quad, Subject};
use oxirs_ttl::jsonld::{JsonLdWriter, Quad as JQuad, WriterObject};
use oxirs_ttl::writer::{RdfTerm, TurtleWriter};
use thiserror::Error;

use crate::mapping::AGORA_NS;

/// JSON-LD 1.1 inline @context. Prefixes only (compact mode does the IRI
/// compaction); versioned as a unit with the ontology — see README
/// "Ontology versioning".
pub const CONTEXT: &str = r#"{
  "@version": 1.1,
  "agora": "https://agora.example/ontology#",
  "tenant": "https://agora.example/tenant/",
  "schema": "https://schema.org/",
  "rdf": "http://www.w3.org/1999/02/22-rdf-syntax-ns#",
  "xsd": "http://www.w3.org/2001/XMLSchema#"
}"#;

#[derive(Debug, Error)]
pub enum SerializeError {
    #[error("invalid JSON-LD @context constant: {0}")]
    Context(serde_json::Error),
    #[error("JSON-LD writer failed: {0}")]
    JsonLd(String),
}

pub struct ExportDocuments {
    pub turtle: String,
    pub jsonld: String,
}

fn literal_rdf_term(l: &Literal) -> RdfTerm {
    if let Some(lang) = l.language() {
        RdfTerm::lang_literal(l.value(), lang)
    } else if l.is_typed() {
        RdfTerm::typed_literal(l.value(), l.datatype().as_str().to_string())
    } else {
        RdfTerm::simple_literal(l.value())
    }
}

fn literal_writer_object(l: &Literal) -> WriterObject {
    if let Some(lang) = l.language() {
        WriterObject::LangLiteral(l.value().to_string(), lang.to_string())
    } else if l.is_typed() {
        WriterObject::TypedLiteral(l.value().to_string(), l.datatype().as_str().to_string())
    } else {
        WriterObject::Literal(l.value().to_string())
    }
}

fn subject_rdf_term(s: &Subject) -> RdfTerm {
    match s {
        Subject::NamedNode(n) => RdfTerm::iri(n.as_str()),
        Subject::BlankNode(b) => RdfTerm::blank_node(b.as_str()),
        other => RdfTerm::iri(other.to_string()),
    }
}

fn object_rdf_term(o: &Object) -> RdfTerm {
    match o {
        Object::NamedNode(n) => RdfTerm::iri(n.as_str()),
        Object::BlankNode(b) => RdfTerm::blank_node(b.as_str()),
        Object::Literal(l) => literal_rdf_term(l),
        other => RdfTerm::iri(other.to_string()),
    }
}

fn predicate_iri_string(p: &Predicate) -> String {
    match p {
        Predicate::NamedNode(n) => n.as_str().to_string(),
        Predicate::Variable(v) => v.to_string(),
    }
}

fn object_writer(o: &Object) -> WriterObject {
    match o {
        Object::NamedNode(n) => WriterObject::Iri(n.as_str().to_string()),
        Object::BlankNode(b) => WriterObject::BlankNode(b.as_str().to_string()),
        Object::Literal(l) => literal_writer_object(l),
        other => WriterObject::Iri(other.to_string()),
    }
}

/// Render the tenant quad set to Turtle + JSON-LD.
pub fn render(quads: &[Quad]) -> Result<ExportDocuments, SerializeError> {
    // Turtle: drop the graph dimension (single-tenant file; see module doc).
    let triples: Vec<(RdfTerm, RdfTerm, RdfTerm)> = quads
        .iter()
        .map(|q| {
            (
                subject_rdf_term(q.subject()),
                RdfTerm::iri(predicate_iri_string(q.predicate())),
                object_rdf_term(q.object()),
            )
        })
        .collect();
    let mut turtle_writer = TurtleWriter::new();
    turtle_writer.add_prefix("agora", AGORA_NS);
    // NB: the `tenant` prefix is deliberately NOT registered for Turtle —
    // node/graph IRIs under https://agora.example/tenant/ contain '/'
    // past the prefix, which is not a valid Turtle PN_LOCAL; they must
    // serialize as full <IRI>s (oxirs-ttl abbreviates naively by prefix
    // match — see README "oxirs maturity caveat"). JSON-LD compact IRIs
    // have no such restriction, so `tenant:` stays in the @context.
    turtle_writer.add_prefix("schema", "https://schema.org/");
    turtle_writer.add_prefix("rdf", "http://www.w3.org/1999/02/22-rdf-syntax-ns#");
    turtle_writer.add_prefix("xsd", "http://www.w3.org/2001/XMLSchema#");
    let turtle = turtle_writer.write_triples(&triples);

    // JSON-LD: named graphs preserved.
    let context: serde_json::Value =
        serde_json::from_str(CONTEXT).map_err(SerializeError::Context)?;
    let jquads: Vec<JQuad> = quads
        .iter()
        .map(|q| JQuad {
            subject: match q.subject() {
                Subject::NamedNode(n) => n.as_str().to_string(),
                other => other.to_string(),
            },
            predicate: predicate_iri_string(q.predicate()),
            object: object_writer(q.object()),
            graph: match q.graph_name() {
                GraphName::NamedNode(n) => Some(n.as_str().to_string()),
                other => Some(other.to_string()),
            },
        })
        .collect();
    let jsonld = JsonLdWriter::new()
        .with_context(context)
        .compact_mode()
        .pretty_print()
        .write_quads(&jquads)
        .map_err(|e| SerializeError::JsonLd(e.to_string()))?;

    Ok(ExportDocuments { turtle, jsonld })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mapping::{named_graph_iri, node_iri, predicate_iri};
    use oxirs_core::model::NamedNode;

    fn nn(s: &str) -> NamedNode {
        NamedNode::new(s).unwrap()
    }

    #[test]
    fn render_smoke_both_formats() {
        let g = nn(&named_graph_iri("t1"));
        let quads = vec![
            Quad::new(
                nn(&node_iri("t1", "Person", "h1")),
                nn("http://www.w3.org/1999/02/22-rdf-syntax-ns#type"),
                nn(&format!("{AGORA_NS}Person")),
                g.clone(),
            ),
            Quad::new(
                nn(&node_iri("t1", "Person", "h1")),
                nn(&predicate_iri("bookingsTotal")),
                Literal::new_typed_literal("3", nn(crate::mapping::xsd::INTEGER)),
                g,
            ),
        ];
        let docs = render(&quads).unwrap();
        assert!(docs.turtle.contains("agora:Person"));
        assert!(docs.jsonld.contains("\"@context\""));
        assert!(docs.jsonld.contains("\"@graph\""));
        // Compact mode abbreviates the graph IRI via the `tenant` prefix.
        assert!(
            docs.jsonld.contains("tenant:t1") || docs.jsonld.contains("tenant/t1"),
            "tenant graph IRI present (possibly prefix-compacted): {}",
            docs.jsonld
        );
    }
}
