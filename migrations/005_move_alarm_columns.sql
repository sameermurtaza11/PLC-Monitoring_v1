-- =====================================================================
-- PLC Monitoring — migration 005: alarm settings leave pv_metadata
--
-- pv_metadata describes a PV (where it is read, its scaling, texts). Alarm
-- limits and priorities belong to the alarm tables (migration 004). This
-- migration:
--
--   1. PRESERVES the old per-PV alarm settings as alarm definitions in
--      alarm_metadata, as unapproved DRAFTS (they are not evaluated until an
--      administrator rationalizes and approves them):
--          alarm_high  -> HI alarm       alarm_low  -> LO alarm
--          alarm_state -> DISCRETE alarm (abnormal state 0 or 1)
--          priority    -> Minor / Major / Critical  (1-2 Critical, 3 Major, 4-5 Minor)
--   2. DROPS the columns priority, alarm_low, alarm_high, alarm_state and
--      their check constraints.
--
-- KEPT on purpose: pv_metadata.deadband. It is the HISTORY deadband (the
-- storage module writes an analog value to pv_history only when it moved by
-- at least this much), not an alarm deadband.
--
-- Safe to run more than once (does nothing when the columns are already gone).
-- Needs 004. Take a backup first (pg_dump) — dropping columns cannot be undone.
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/005_move_alarm_columns.sql
-- =====================================================================

BEGIN;

DO $$
BEGIN
    IF to_regclass('public.alarm_metadata') IS NULL THEN
        RAISE EXCEPTION 'Needs migration 004 (alarm tables). Apply it first.';
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'public' AND table_name = 'pv_metadata' AND column_name = 'alarm_high') THEN

        -- analog high / low limits
        INSERT INTO alarm_metadata
            (pv_id, alarm_type, setpoint, trigger_state, setpoint_unit, deadband, on_delay_s, off_delay_s,
             priority, alarm_class, alarm_message, rationale, rationalization_status, is_enabled,
             created_by_name, updated_by_name)
        SELECT pv.pv_id, t.alarm_type,
               CASE t.alarm_type WHEN 'HI' THEN pv.alarm_high ELSE pv.alarm_low END,
               NULL, pv.unit, 0, 0, 0,
               CASE WHEN pv.priority <= 2 THEN 'Critical' WHEN pv.priority = 3 THEN 'Major' ELSE 'Minor' END,
               'PROCESS',
               pv.pv_name || CASE t.alarm_type WHEN 'HI' THEN ' high' ELSE ' low' END,
               'Migrated from the former pv_metadata alarm limit; needs rationalization and approval',
               'DRAFT', TRUE, 'migration 005', 'migration 005'
        FROM pv_metadata pv
        JOIN (VALUES ('HI'), ('LO')) AS t(alarm_type)
          ON (t.alarm_type = 'HI' AND pv.alarm_high IS NOT NULL) OR (t.alarm_type = 'LO' AND pv.alarm_low IS NOT NULL)
        WHERE pv.register_address NOT BETWEEN 10001 AND 19999
        ON CONFLICT (pv_id, alarm_type) DO NOTHING;

        -- digital alarm state
        INSERT INTO alarm_metadata
            (pv_id, alarm_type, setpoint, trigger_state, setpoint_unit, deadband, on_delay_s, off_delay_s,
             priority, alarm_class, alarm_message, rationale, rationalization_status, is_enabled,
             created_by_name, updated_by_name)
        SELECT pv.pv_id, 'DISCRETE', NULL, pv.alarm_state, '', 0, 0, 0,
               CASE WHEN pv.priority <= 2 THEN 'Critical' WHEN pv.priority = 3 THEN 'Major' ELSE 'Minor' END,
               'PROCESS',
               pv.pv_name || ' ' || CASE pv.alarm_state WHEN 1 THEN pv.state1_text ELSE pv.state0_text END,
               'Migrated from the former pv_metadata alarm state; needs rationalization and approval',
               'DRAFT', TRUE, 'migration 005', 'migration 005'
        FROM pv_metadata pv
        WHERE pv.alarm_state IS NOT NULL AND pv.register_address BETWEEN 10001 AND 19999
        ON CONFLICT (pv_id, alarm_type) DO NOTHING;

        RAISE NOTICE 'alarm settings of pv_metadata copied to alarm_metadata (drafts)';
    END IF;
END $$;

ALTER TABLE pv_metadata DROP CONSTRAINT IF EXISTS pv_alarm_state_valid;
ALTER TABLE pv_metadata DROP CONSTRAINT IF EXISTS pv_metadata_priority_check;
ALTER TABLE pv_metadata DROP COLUMN IF EXISTS priority;
ALTER TABLE pv_metadata DROP COLUMN IF EXISTS alarm_low;
ALTER TABLE pv_metadata DROP COLUMN IF EXISTS alarm_high;
ALTER TABLE pv_metadata DROP COLUMN IF EXISTS alarm_state;

COMMIT;
