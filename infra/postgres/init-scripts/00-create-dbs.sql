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
-- W53 (SPEC-W53 Scope A): ocr-service (:7020) document-metadata database.
-- Schema (ocr_documents with FORCE ROW LEVEL SECURITY) is applied by
-- 31-ocr-schema.sql and, idempotently, at boot by the service itself
-- (services/ocr-service/app/store.py ensure* idiom; roles/grants in
-- 05-app-roles.sql).
CREATE DATABASE ocr;
-- W47 (SPEC-W47 / A-DB DB-01 CRIT): payments-service database. Previously the
-- tenant-scoped money tables (payout_beneficiaries, deposit_provenance,
-- payout_attempts, rail_attempts) lived in the default superuser database
-- over a superuser DSN with NO RLS. Schema (CREATE TABLE IF NOT EXISTS) +
-- RLS migration are applied idempotently by the service at boot
-- (services/payments-service/migrations/0001_rls.sql); roles/grants are in
-- 05-app-roles.sql. The service connects as app_payments_login /
-- app_payments_internal_login (see docker-compose.yml payments block).
CREATE DATABASE payments;
