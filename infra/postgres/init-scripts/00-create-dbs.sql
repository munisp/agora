-- 00-create-dbs.sql — one database per service (SPEC §7) plus infra DBs.
-- Runs once on first container start (postgres docker-entrypoint-initdb.d).
-- NOTE: subsequent scripts use psql meta-commands (\c) and must run under
-- docker-entrypoint-initdb.d's `*.sql` psql path — do not pipe them to plain psql.

CREATE DATABASE identity;
CREATE DATABASE booking;
CREATE DATABASE conversation;
CREATE DATABASE knowledge;
CREATE DATABASE analytics_meta;
CREATE DATABASE temporal;
CREATE DATABASE keycloak;
CREATE DATABASE permify;
CREATE DATABASE iceberg;
-- SPEC-CRM §A/§B: Twenty CRM database + crm-sync-service sync_map database.
CREATE DATABASE twenty;
CREATE DATABASE crm_sync;
-- Wave 5 #10: notification-worker webhook platform (webhook_subscriptions /
-- webhook_deliveries, bootstrapped idempotently by the service itself).
CREATE DATABASE notifications;
-- Wave 7 (SPEC-W7 Part B): billing-engine metering/invoicing database. Schema
-- (usage_records, rate_cards, invoices, processed_events) is applied
-- idempotently by the service itself from services/billing-engine/migrations.
CREATE DATABASE billing;
-- Wave 12 (SPEC-W12): kyc-service (:7013) verification-audit database. Schema
-- (kyc_audit with FORCE ROW LEVEL SECURITY) is applied idempotently by the
-- service itself from services/kyc-service/internal/store.
CREATE DATABASE kyc;
