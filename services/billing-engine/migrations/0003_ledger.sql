-- SIM-029: durable Postgres-backed billing ledger (the default
-- BILLING_LEDGER_IMPL=postgres implementation).
--
-- Mirrors the in-memory SimLedgerClient semantics: accounts keyed by name
-- (SPEC-W7 codes 200 AR-control / 201 revenue / 202 payments-clearing),
-- transfers idempotent by id (deterministic uuid v5 derived by callers).
--
-- RLS note: unlike the tenant tables in 0002_rls.sql these are
-- platform-internal accounting tables (the clearing account
-- `platform:billing:clearing` has no tenant), so they carry NO row-level
-- policies and must only be reachable by the billing service roles. Access
-- from tenants goes through the service's own code paths.

CREATE TABLE IF NOT EXISTS ledger_accounts (
    name           TEXT PRIMARY KEY,
    -- deterministic 128-bit id (uuid v5 of name), rendered as 32 hex chars
    id             TEXT        NOT NULL,
    ledger         INT         NOT NULL,
    code           INT         NOT NULL,
    debits_posted  BIGINT      NOT NULL DEFAULT 0 CHECK (debits_posted >= 0),
    credits_posted BIGINT      NOT NULL DEFAULT 0 CHECK (credits_posted >= 0)
);

CREATE TABLE IF NOT EXISTS ledger_transfers (
    id             UUID        PRIMARY KEY,
    debit_account  TEXT        NOT NULL REFERENCES ledger_accounts (name),
    credit_account TEXT        NOT NULL REFERENCES ledger_accounts (name),
    -- minor units (cents); always positive
    amount         BIGINT      NOT NULL CHECK (amount > 0),
    ledger         INT         NOT NULL,
    code           INT         NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ledger_transfers_debit_idx
    ON ledger_transfers (debit_account);
CREATE INDEX IF NOT EXISTS ledger_transfers_credit_idx
    ON ledger_transfers (credit_account);
