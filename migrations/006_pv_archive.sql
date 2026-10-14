-- =====================================================================
-- PLC Monitoring — migration 006: archive instead of erase (PV deletion)
--
-- Alarm history (alarm_history) is append-only and refers to alarm_metadata,
-- which refers to pv_metadata; value history (pv_history) refers to the PV as
-- well. A PV that has any of these cannot be erased without destroying records
-- that must be kept, so "Delete PV" archives it instead:
--
--   pv_metadata.deleted_at   set when a PV was deleted but its records are kept
--
-- An archived PV is not acquired (enabled = false), not listed in the
-- configuration, and its name and register address can be used again: the two
-- uniqueness rules of a PV therefore apply to PVs that are NOT archived.
-- A PV without any history is still erased completely.
--
-- Safe to run more than once. Needs 001-005.
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/006_pv_archive.sql
-- =====================================================================

BEGIN;

ALTER TABLE pv_metadata ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ NULL;

-- Same names as before, so error messages and code that look for them keep working.
ALTER TABLE pv_metadata DROP CONSTRAINT IF EXISTS pv_unique_name;
ALTER TABLE pv_metadata DROP CONSTRAINT IF EXISTS pv_unique_register;
CREATE UNIQUE INDEX IF NOT EXISTS pv_unique_name     ON pv_metadata (plc_id, pv_name)          WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS pv_unique_register ON pv_metadata (plc_id, register_address) WHERE deleted_at IS NULL;

COMMIT;
