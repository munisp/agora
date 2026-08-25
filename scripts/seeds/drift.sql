-- drift.sql — Platform seed drift gate (SPEC-W17 §8.7 #5, Agent D).
--
-- Compares information_schema + rowcount/hash aggregates of the cac.* seed
-- tables against the expectations recorded in cac.seed_run_log (written by
-- _lib.log_seed_run after every loader) and against the fixed, scale-
-- independent cardinalities of the reference data.
--
-- CONTRACT: returns 0 ROWS ON SUCCESS. Any returned row is a drift incident:
--   check_name               meaning
--   missing_table            contract-D table absent from schema cac
--   missing_seed_run_log_row table seeded but never logged (or log lost)
--   rowcount_mismatch        seed_run_log.rowcount != live COUNT(*)
--   cardinality_drift        cardinality expectation violated
--                            (fixed: lgas=774, channels=32,
--                             channel_unit_costs=768, fx_series>=365;
--                             scaled: wards=int(8812*s), agents=int(5000*s),
--                             customers=int(200000*s), s=seed_scale)
--   empty_table              seeded table has 0 rows
--   non_synthetic_rows       is_synthetic=false rows present (§8.8 breach —
--                            seed tables must be 100% synthetic)
--   fx_series_gap            daily FX series not contiguous
--
-- Column contract relied upon (docs/data-seeding.md §Schema contract):
--   every cac.* data table: id text PK (deterministic id), is_synthetic bool;
--   cac.fx_series.series_date date (daily series key);
--   cac.seed_run_log(table_name text PK, rowcount bigint, seeded_at timestamptz)
-- Scaled tables (agents, customers) have scale-dependent cardinality, so they
-- are checked for log-consistency/non-zero only, not absolute counts.
--
-- Precondition: sql/postgres/ddl applied (bootstrap.sh step 1). A missing
-- schema/table fails this script loudly (psql error = gate failure), which is
-- the desired behaviour for a gate.
--
-- Usage: psql "$DATABASE_URL" -At -f scripts/seeds/drift.sql   # 0 rows = OK
--        make seed-drift
--
-- Scale awareness: cardinality expectations for the scaled tables
-- (wards, agents, customers) follow _lib.scaled() = int(N * SEED_SCALE).
-- Pass the scale the run used:  psql -v seed_scale=0.05 -f drift.sql
-- (make seed-drift forwards SEED_SCALE; default 1.0).

\if :{?seed_scale}
\else
  \set seed_scale 1.0
\endif

WITH tables(table_name) AS (
    VALUES ('lgas'), ('wards'), ('channels'), ('channel_unit_costs'),
           ('agents'), ('customers'), ('fx_series'), ('seed_run_log')
),
counts AS (  -- live rowcount + id-hash aggregate + synthetic compliance per data table
    SELECT 'lgas' AS table_name, count(*) AS n,
           md5(coalesce(string_agg(id, ',' ORDER BY id), '')) AS id_hash,
           count(*) FILTER (WHERE NOT is_synthetic) AS non_synth
    FROM cac.lgas
    UNION ALL
    SELECT 'wards', count(*), md5(coalesce(string_agg(id, ',' ORDER BY id), '')),
           count(*) FILTER (WHERE NOT is_synthetic) FROM cac.wards
    UNION ALL
    SELECT 'channels', count(*), md5(coalesce(string_agg(id, ',' ORDER BY id), '')),
           count(*) FILTER (WHERE NOT is_synthetic) FROM cac.channels
    UNION ALL
    SELECT 'channel_unit_costs', count(*), md5(coalesce(string_agg(id, ',' ORDER BY id), '')),
           count(*) FILTER (WHERE NOT is_synthetic) FROM cac.channel_unit_costs
    UNION ALL
    SELECT 'agents', count(*), md5(coalesce(string_agg(id, ',' ORDER BY id), '')),
           count(*) FILTER (WHERE NOT is_synthetic) FROM cac.agents
    UNION ALL
    SELECT 'customers', count(*), md5(coalesce(string_agg(id, ',' ORDER BY id), '')),
           count(*) FILTER (WHERE NOT is_synthetic) FROM cac.customers
    UNION ALL
    SELECT 'fx_series', count(*), md5(coalesce(string_agg(id, ',' ORDER BY id), '')),
           count(*) FILTER (WHERE NOT is_synthetic) FROM cac.fx_series
),
fx_contiguity AS (
    SELECT count(DISTINCT series_date) AS days,
           (max(series_date) - min(series_date) + 1) AS span_days
    FROM cac.fx_series
),
drift AS (
    -- 1. contract-D tables missing from schema cac
    SELECT 'missing_table'::text AS check_name, t.table_name,
           'present'::text AS expected, 'absent'::text AS actual,
           'apply sql/postgres/ddl (bootstrap.sh step 1)'::text AS detail
    FROM tables t
    WHERE NOT EXISTS (SELECT 1 FROM information_schema.tables
                      WHERE table_schema = 'cac' AND table_name = t.table_name)
    UNION ALL
    -- 2. data table never logged in cac.seed_run_log
    SELECT 'missing_seed_run_log_row', t.table_name, 'log row', 'none',
           'table exists but _lib.log_seed_run never recorded a run'
    FROM tables t
    WHERE t.table_name <> 'seed_run_log'
      -- W43 (G-05): seed scripts log schema-qualified names ('cac.wards');
      -- accept both spellings or every run reads as a missing log row.
      AND NOT EXISTS (SELECT 1 FROM cac.seed_run_log l
                      WHERE l.table_name IN (t.table_name, 'cac.' || t.table_name))
    UNION ALL
    -- 3. logged rowcount != live rowcount (reseed partial failure / manual edits)
    SELECT 'rowcount_mismatch', c.table_name, l.rowcount::text, c.n::text,
           'id_hash=' || c.id_hash || '; last seeded_at=' || l.seeded_at::text
    FROM counts c
    JOIN cac.seed_run_log l ON l.table_name IN (c.table_name, 'cac.' || c.table_name)
    WHERE l.rowcount <> c.n
    UNION ALL
    -- 4. cardinality expectations: fixed for reference data, int(N*scale)
    --    for scaled tables (wards/agents/customers — matches _lib.scaled())
    -- W43 (G-05): e.expected is integer; the UNION's expected column is
    -- text ('present'/'>=365'/...) — cast or the whole gate errors out.
    SELECT 'cardinality_drift', c.table_name, e.expected::text, c.n::text,
           'id_hash=' || c.id_hash
    FROM counts c
    -- W43 (G-05): floor() — ::int on float8 ROUNDS in Postgres while
    -- _lib.scaled() TRUNCATES (int(N*scale)); the gate must match the seeder.
    JOIN (VALUES ('lgas', 774), ('channels', 32), ('channel_unit_costs', 768),
                 ('wards', floor(8812 * :seed_scale::float)::int),
                 ('agents', floor(5000 * :seed_scale::float)::int),
                 ('customers', floor(200000 * :seed_scale::float)::int)) AS e(table_name, expected)
      ON e.table_name = c.table_name
    WHERE c.n <> e.expected
    UNION ALL
    SELECT 'cardinality_drift', c.table_name, '>=365', c.n::text,
           'FX series needs >=365 contiguous daily points (§8.7)'
    FROM counts c WHERE c.table_name = 'fx_series' AND c.n < 365
    UNION ALL
    -- 5. empty seeded table
    SELECT 'empty_table', c.table_name, '>0', '0', 'seed loader produced no rows'
    FROM counts c WHERE c.n = 0
    UNION ALL
    -- 6. synthetic purity (§8.8: no real-person rows may enter seed tables)
    SELECT 'non_synthetic_rows', c.table_name, '0', c.non_synth::text,
           'rows with is_synthetic=false in seed table — compliance incident'
    FROM counts c WHERE c.non_synth > 0
    UNION ALL
    -- 7. FX daily contiguity
    SELECT 'fx_series_gap', 'fx_series', 'contiguous daily',
           f.days::text || ' distinct days in ' || f.span_days::text || '-day span',
           'reseed seed_fx.py; series must be gap-free (§8.7)'
    FROM fx_contiguity f WHERE f.days <> f.span_days
)
SELECT check_name, table_name, expected, actual, detail
FROM drift
ORDER BY check_name, table_name;
