-- 04-knowledge-schema.sql — knowledge DB schema (SPEC §7) with tenant RLS.
\c knowledge

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Knowledge base source documents (RAG grounding for the voice agent, SPEC §10).
CREATE TABLE documents (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    source_url TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_documents_tenant ON documents (tenant_id, created_at DESC);

-- Chunked content; embeddings live in OpenSearch `kb-chunks` (384-dim).
CREATE TABLE chunks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id UUID NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    content     TEXT NOT NULL,
    UNIQUE (document_id, seq)
);

-- ---------------- Row Level Security (SPEC §7) ----------------
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON documents
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- chunks carries no tenant_id per SPEC §7; isolate through the parent row.
ALTER TABLE chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chunks
    USING (EXISTS (
        SELECT 1 FROM documents d
        WHERE d.id = chunks.document_id
          AND d.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ));
