-- RS-001: durable billing event outbox. Rows are written IN THE SAME
-- TRANSACTION as the state change they describe (e.g. the invoice paid
-- commit), so a Kafka/topic failure can never silently lose an event: the
-- relay (src/outbox.rs) republishes with bounded backoff until the broker
-- accepts the record.
--
-- Provisioning note: Kafka auto-create is OFF, so the target topic
-- (`BILLING_EVENTS_TOPIC`, default `opendesk.billing.events`) must exist.

CREATE TABLE IF NOT EXISTS billing_outbox (
    id              UUID        PRIMARY KEY,
    topic           TEXT        NOT NULL,
    -- Kafka record key (tenant id)
    event_key       TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT
);

CREATE INDEX IF NOT EXISTS billing_outbox_pending_idx
    ON billing_outbox (next_attempt_at)
    WHERE published_at IS NULL;
