-- =====================================================================
-- PLC Monitoring — migration 004: basic alarm management (Version 01)
--
-- ADDS tables only. plc_metadata / pv_metadata / pv_latest / pv_history and
-- users / sessions are not changed. Safe to run more than once.
-- Needs 003 (users).
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/004_alarms.sql
--
-- Design (ISA-18.2 basic alarms guide, three tables):
--   alarm_metadata  one row per configured alarm definition
--   active_alarms   one row per OUTSTANDING alarm occurrence
--   alarm_history   one row per alarm event / state transition (append-only)
-- plus
--   alarm_audit_log append-only audit trail of configuration changes and of
--                   refused actions (kept separate from the history)
--
-- States held in active_alarms: UNACK, ACKED, RTNUN, SHLVD.
-- "Normal" (NORM) is not a stored state: a completed occurrence is removed
-- from active_alarms and its whole timeline stays in alarm_history, where
-- the closing event has new_state = 'NORM'.
--
-- The rules below are enforced BY THE DATABASE, so no generic UPDATE, form or
-- script can move an alarm into a state the model does not allow, or change a
-- state without writing the matching history row in the same transaction.
-- =====================================================================

BEGIN;

DO $$
BEGIN
    IF to_regclass('public.users') IS NULL OR to_regclass('public.pv_metadata') IS NULL THEN
        RAISE EXCEPTION 'Needs the tables pv_metadata (migration 001) and users (migration 003). Apply those first.';
    END IF;
END $$;

-- ---------------------------------------------------------------------
-- 1. alarm_metadata — the approved definition of each alarm
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_metadata (
    alarm_id           BIGSERIAL PRIMARY KEY,
    pv_id              INTEGER      NOT NULL REFERENCES pv_metadata(pv_id) ON DELETE RESTRICT,

    -- the abnormal condition (priority is a SEPARATE attribute below)
    alarm_type         VARCHAR(10)  NOT NULL
                       CONSTRAINT alarm_type_check CHECK (alarm_type IN ('HI', 'HIHI', 'LO', 'LOLO', 'DISCRETE')),
    setpoint           DOUBLE PRECISION NULL,          -- analog types
    trigger_state      SMALLINT     NULL                -- DISCRETE: the abnormal state (0 or 1)
                       CONSTRAINT alarm_trigger_state_check CHECK (trigger_state IN (0, 1)),
    setpoint_unit      TEXT         NOT NULL DEFAULT '', -- must equal the PV's unit when saved
    deadband           DOUBLE PRECISION NOT NULL DEFAULT 0 CONSTRAINT alarm_deadband_check CHECK (deadband >= 0),
    on_delay_s         INTEGER      NOT NULL DEFAULT 0 CONSTRAINT alarm_on_delay_check  CHECK (on_delay_s  BETWEEN 0 AND 3600),
    off_delay_s        INTEGER      NOT NULL DEFAULT 0 CONSTRAINT alarm_off_delay_check CHECK (off_delay_s BETWEEN 0 AND 3600),

    -- rationalization
    priority           VARCHAR(10)  NOT NULL
                       CONSTRAINT alarm_priority_check CHECK (priority IN ('Minor', 'Major', 'Critical')),
    alarm_class        VARCHAR(30)  NOT NULL
                       CONSTRAINT alarm_class_check CHECK (alarm_class IN ('PROCESS', 'EQUIPMENT_PROTECTION', 'ENVIRONMENTAL', 'SAFETY_CRITICAL')),
    alarm_message      TEXT         NOT NULL CONSTRAINT alarm_message_check CHECK (btrim(alarm_message) <> ''),
    operator_action    TEXT         NOT NULL DEFAULT '',
    consequences_of_inaction TEXT   NOT NULL DEFAULT '',
    probable_cause     TEXT         NOT NULL DEFAULT '',
    max_response_time_s INTEGER     NULL CONSTRAINT alarm_response_time_check CHECK (max_response_time_s > 0),  -- seconds; NOT the on-delay
    rationale          TEXT         NOT NULL DEFAULT '',   -- why this priority and class (the justification)
    rationalization_status VARCHAR(10) NOT NULL DEFAULT 'DRAFT'
                       CONSTRAINT alarm_rationalization_check CHECK (rationalization_status IN ('DRAFT', 'APPROVED')),
    approved_by        BIGINT       NULL REFERENCES users(id) ON DELETE SET NULL,
    approved_by_name   TEXT         NOT NULL DEFAULT '',
    approved_at        TIMESTAMPTZ  NULL,

    is_enabled         BOOLEAN      NOT NULL DEFAULT TRUE,

    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_by         BIGINT       NULL REFERENCES users(id) ON DELETE SET NULL,
    created_by_name    TEXT         NOT NULL DEFAULT '',
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by         BIGINT       NULL REFERENCES users(id) ON DELETE SET NULL,
    updated_by_name    TEXT         NOT NULL DEFAULT '',

    -- one alarm per PV and type: "one row per configured alarm condition"
    CONSTRAINT alarm_unique_pv_type UNIQUE (pv_id, alarm_type),
    -- an analog alarm has a setpoint, a discrete alarm an abnormal state, never both
    CONSTRAINT alarm_trigger_exclusive CHECK ((setpoint IS NULL) <> (trigger_state IS NULL)),
    CONSTRAINT alarm_trigger_matches_type CHECK (
        (alarm_type = 'DISCRETE' AND trigger_state IS NOT NULL) OR
        (alarm_type <> 'DISCRETE' AND setpoint IS NOT NULL)),
    CONSTRAINT alarm_approval_complete CHECK (
        rationalization_status <> 'APPROVED' OR (approved_at IS NOT NULL AND btrim(rationale) <> ''))
);

-- ---------------------------------------------------------------------
-- Shared guard for the append-only tables
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION alarm_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (% is not allowed)', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------
-- 2. active_alarms — one row per outstanding occurrence
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS active_alarms (
    occurrence_id      BIGSERIAL PRIMARY KEY,
    alarm_id           BIGINT      NOT NULL REFERENCES alarm_metadata(alarm_id) ON DELETE RESTRICT,
    alarm_state        VARCHAR(5)  NOT NULL
                       CONSTRAINT active_alarm_state_check CHECK (alarm_state IN ('UNACK', 'ACKED', 'RTNUN', 'SHLVD')),
    pv_value           DOUBLE PRECISION NULL,            -- value at activation
    activated_at       TIMESTAMPTZ NOT NULL,
    acked_at           TIMESTAMPTZ NULL,
    acked_by           BIGINT      NULL REFERENCES users(id) ON DELETE SET NULL,
    acked_by_name      TEXT        NOT NULL DEFAULT '',
    returned_to_normal_at TIMESTAMPTZ NULL,              -- not overwritten by the acknowledgement
    shelved_at         TIMESTAMPTZ NULL,
    shelved_by         BIGINT      NULL REFERENCES users(id) ON DELETE SET NULL,
    shelved_by_name    TEXT        NOT NULL DEFAULT '',
    shelved_expiry_time TIMESTAMPTZ NULL,
    shelve_reason      TEXT        NOT NULL DEFAULT '',
    -- does the underlying process condition remain abnormal? (kept true/false
    -- even while the alarm is shelved: shelving never hides the real condition)
    process_condition_active BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- the columns must tell the same story as the state
    CONSTRAINT active_alarm_fields_match_state CHECK (
        (alarm_state = 'UNACK' AND process_condition_active AND acked_at IS NULL AND shelved_at IS NULL)
     OR (alarm_state = 'ACKED' AND process_condition_active AND acked_at IS NOT NULL AND shelved_at IS NULL)
     OR (alarm_state = 'RTNUN' AND NOT process_condition_active AND returned_to_normal_at IS NOT NULL
                               AND acked_at IS NULL AND shelved_at IS NULL)
     OR (alarm_state = 'SHLVD' AND shelved_at IS NOT NULL AND shelved_expiry_time IS NOT NULL
                               AND shelved_expiry_time > shelved_at AND btrim(shelve_reason) <> '')
    )
);

CREATE INDEX IF NOT EXISTS active_alarms_alarm_idx ON active_alarms (alarm_id);
CREATE INDEX IF NOT EXISTS active_alarms_shelf_idx ON active_alarms (shelved_expiry_time) WHERE alarm_state = 'SHLVD';

-- At most ONE occurrence per alarm whose process condition is abnormal.
-- (An older occurrence that already returned to normal but still waits for
-- its acknowledgement may exist next to a new activation.) This is what stops
-- repeated scans from creating duplicate activations.
CREATE UNIQUE INDEX IF NOT EXISTS active_alarms_one_abnormal_idx
    ON active_alarms (alarm_id) WHERE process_condition_active;

-- ---------------------------------------------------------------------
-- 3. alarm_history — one row per event or state transition (append-only)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_history (
    event_id           BIGSERIAL PRIMARY KEY,
    occurrence_id      BIGINT      NOT NULL,   -- no foreign key: the occurrence row is removed when it completes
    alarm_id           BIGINT      NOT NULL REFERENCES alarm_metadata(alarm_id) ON DELETE RESTRICT,
    event_type         VARCHAR(20) NOT NULL,
    previous_state     VARCHAR(5)  NULL,       -- NULL: no previous state (activation)
    new_state          VARCHAR(5)  NULL,       -- NULL: informational event, no state change
    pv_value           DOUBLE PRECISION NULL,
    event_timestamp    TIMESTAMPTZ NOT NULL,   -- when it happened (process time for automatic events)
    recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id            BIGINT      NULL,       -- no foreign key: the record outlives the account
    user_name          TEXT        NOT NULL DEFAULT '',   -- '' for automatic events
    details            TEXT        NOT NULL DEFAULT '',
    reason             TEXT        NOT NULL DEFAULT '',
    -- the definition values that applied when the event happened, so a later
    -- configuration change cannot change the meaning of an old event
    priority           VARCHAR(10) NULL,
    setpoint           DOUBLE PRECISION NULL,
    txid               BIGINT      NOT NULL DEFAULT txid_current(),  -- ties the event to its transaction

    CONSTRAINT alarm_history_states_check CHECK (
        (previous_state IS NULL OR previous_state IN ('UNACK', 'ACKED', 'RTNUN', 'SHLVD'))
        AND (new_state IS NULL OR new_state IN ('UNACK', 'ACKED', 'RTNUN', 'SHLVD', 'NORM'))),

    -- the only (event, from, to) combinations that exist; '-' = none.
    -- The last two rows are informational events while an alarm is shelved.
    CONSTRAINT alarm_history_transition_check CHECK (
        (event_type, COALESCE(previous_state, '-'), COALESCE(new_state, '-')) IN (
            ('ACTIVATED',          '-',     'UNACK'),
            ('ACKNOWLEDGED',       'UNACK', 'ACKED'),
            ('ACKNOWLEDGED',       'RTNUN', 'NORM'),
            ('RETURNED_TO_NORMAL', 'UNACK', 'RTNUN'),
            ('RETURNED_TO_NORMAL', 'ACKED', 'NORM'),
            ('SHELVED',            'UNACK', 'SHLVD'),
            ('SHELVED',            'ACKED', 'SHLVD'),
            ('SHELVED',            'RTNUN', 'SHLVD'),
            ('UNSHELVED',          'SHLVD', 'UNACK'),
            ('UNSHELVED',          'SHLVD', 'RTNUN'),
            ('ACTIVATED',          'SHLVD', '-'),
            ('RETURNED_TO_NORMAL', 'SHLVD', '-')))
);

CREATE INDEX IF NOT EXISTS alarm_history_occurrence_idx ON alarm_history (occurrence_id, event_id);
CREATE INDEX IF NOT EXISTS alarm_history_alarm_idx      ON alarm_history (alarm_id, event_timestamp DESC);
CREATE INDEX IF NOT EXISTS alarm_history_time_idx       ON alarm_history (event_timestamp DESC);

DROP TRIGGER IF EXISTS alarm_history_no_change   ON alarm_history;
DROP TRIGGER IF EXISTS alarm_history_no_truncate ON alarm_history;
CREATE TRIGGER alarm_history_no_change   BEFORE UPDATE OR DELETE ON alarm_history
    FOR EACH ROW EXECUTE FUNCTION alarm_append_only();
CREATE TRIGGER alarm_history_no_truncate BEFORE TRUNCATE ON alarm_history
    FOR EACH STATEMENT EXECUTE FUNCTION alarm_append_only();

-- ---------------------------------------------------------------------
-- State-machine guards on active_alarms
-- ---------------------------------------------------------------------

-- A new occurrence always starts as UNACK.
CREATE OR REPLACE FUNCTION active_alarms_before_insert() RETURNS trigger AS $$
BEGIN
    IF NEW.alarm_state <> 'UNACK' THEN
        RAISE EXCEPTION 'a new alarm occurrence must start as UNACK, not %', NEW.alarm_state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Only the transitions of the model are possible, and the identity of an
-- occurrence cannot be rewritten.
CREATE OR REPLACE FUNCTION active_alarms_before_update() RETURNS trigger AS $$
BEGIN
    IF NEW.occurrence_id <> OLD.occurrence_id OR NEW.alarm_id <> OLD.alarm_id
       OR NEW.activated_at <> OLD.activated_at THEN
        RAISE EXCEPTION 'the identity of an alarm occurrence cannot be changed';
    END IF;
    IF NEW.alarm_state <> OLD.alarm_state THEN
        IF (OLD.alarm_state, NEW.alarm_state) NOT IN (
               ('UNACK', 'ACKED'), ('UNACK', 'RTNUN'),
               ('UNACK', 'SHLVD'), ('ACKED', 'SHLVD'), ('RTNUN', 'SHLVD'),
               ('SHLVD', 'UNACK'), ('SHLVD', 'RTNUN')) THEN
            RAISE EXCEPTION 'alarm state change % -> % is not allowed', OLD.alarm_state, NEW.alarm_state;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- An occurrence completes (-> NORM) only from ACKED (process returned) or
-- RTNUN (acknowledged).
CREATE OR REPLACE FUNCTION active_alarms_before_delete() RETURNS trigger AS $$
BEGIN
    IF OLD.alarm_state NOT IN ('ACKED', 'RTNUN') THEN
        RAISE EXCEPTION 'an alarm in state % cannot be removed (it cannot go to NORM from there)', OLD.alarm_state;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

-- Every state change must come with its history row IN THE SAME TRANSACTION.
-- Checked when the transaction commits (deferred), so the order of the two
-- statements does not matter, but a missing history row aborts everything.
CREATE OR REPLACE FUNCTION active_alarms_require_history() RETURNS trigger AS $$
DECLARE
    occ  BIGINT;
    prev TEXT;
    nxt  TEXT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        occ := NEW.occurrence_id; prev := NULL;           nxt := NEW.alarm_state;
    ELSIF TG_OP = 'UPDATE' THEN
        IF NEW.alarm_state = OLD.alarm_state THEN
            RETURN NULL;                                   -- no state change, nothing to record here
        END IF;
        occ := NEW.occurrence_id; prev := OLD.alarm_state; nxt := NEW.alarm_state;
    ELSE
        occ := OLD.occurrence_id; prev := OLD.alarm_state; nxt := 'NORM';
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM alarm_history h
        WHERE h.occurrence_id = occ
          AND h.txid = txid_current()
          AND h.new_state = nxt
          AND h.previous_state IS NOT DISTINCT FROM prev) THEN
        RAISE EXCEPTION 'alarm occurrence %: state change % -> % has no alarm_history row in this transaction',
            occ, COALESCE(prev, '(new)'), nxt;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS active_alarms_t_insert  ON active_alarms;
DROP TRIGGER IF EXISTS active_alarms_t_update  ON active_alarms;
DROP TRIGGER IF EXISTS active_alarms_t_delete  ON active_alarms;
DROP TRIGGER IF EXISTS active_alarms_t_history ON active_alarms;
CREATE TRIGGER active_alarms_t_insert BEFORE INSERT ON active_alarms
    FOR EACH ROW EXECUTE FUNCTION active_alarms_before_insert();
CREATE TRIGGER active_alarms_t_update BEFORE UPDATE ON active_alarms
    FOR EACH ROW EXECUTE FUNCTION active_alarms_before_update();
CREATE TRIGGER active_alarms_t_delete BEFORE DELETE ON active_alarms
    FOR EACH ROW EXECUTE FUNCTION active_alarms_before_delete();
CREATE CONSTRAINT TRIGGER active_alarms_t_history AFTER INSERT OR UPDATE OR DELETE ON active_alarms
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION active_alarms_require_history();

-- ---------------------------------------------------------------------
-- 4. alarm_audit_log — configuration changes and refused actions
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_audit_log (
    audit_id    BIGSERIAL PRIMARY KEY,
    ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id     BIGINT      NULL,                      -- no foreign key: the record outlives the account
    user_name   TEXT        NOT NULL DEFAULT '',
    action      VARCHAR(30) NOT NULL,                  -- ALARM_CREATE, ALARM_UPDATE, ALARM_APPROVE, ACTION_DENIED ...
    alarm_id    BIGINT      NULL,
    outcome     VARCHAR(10) NOT NULL CHECK (outcome IN ('success', 'denied', 'rejected')),
    reason      TEXT        NOT NULL DEFAULT '',
    before_data JSONB       NULL,
    after_data  JSONB       NULL,
    client_ip   TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alarm_audit_ts_idx    ON alarm_audit_log (ts DESC);
CREATE INDEX IF NOT EXISTS alarm_audit_alarm_idx ON alarm_audit_log (alarm_id, ts DESC);

DROP TRIGGER IF EXISTS alarm_audit_no_change   ON alarm_audit_log;
DROP TRIGGER IF EXISTS alarm_audit_no_truncate ON alarm_audit_log;
CREATE TRIGGER alarm_audit_no_change   BEFORE UPDATE OR DELETE ON alarm_audit_log
    FOR EACH ROW EXECUTE FUNCTION alarm_append_only();
CREATE TRIGGER alarm_audit_no_truncate BEFORE TRUNCATE ON alarm_audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION alarm_append_only();

COMMIT;
