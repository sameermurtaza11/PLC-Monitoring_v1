-- =====================================================================
-- TEST DATA — alarm configuration for every PV of the demo plant.
--
-- NOT a schema migration and NOT a formal rationalization: these are sample
-- alarms (limits, priorities, deadbands, delays chosen inside each PV's
-- engineering range) so the Alarm
-- Management System can be exercised against live data. Limits follow common
-- practice (ISA-18.2: alarms must be actionable and infrequent): HI / HIHI at about
-- 80 / 90 % of the engineering range, LO / LOLO at about 20 / 10 %, deadband about 2 %
-- of the range, on/off delays of 10 s to ride through noise (2 s on the protective
-- HIHI / LOLO), 60-120 s on status alarms of equipment that cycles. They are stored as
-- APPROVED, approver "test-data seed", so the alarm engine evaluates them;
-- replace them with rationalized alarms before real use.
--
-- Only touches alarms created by migration 005 or by this script, so alarms
-- entered by users are never changed. Replaced migrated drafts are kept in
-- alarm_audit_log (before_data). Safe to run more than once.
-- Needs 004 and 005.
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f scripts/seed_test_alarms.sql
-- =====================================================================
BEGIN;

CREATE TEMP TABLE seed (
    plc_name text, pv_name text, alarm_type text, setpoint double precision, trigger_state smallint,
    deadband double precision, on_delay_s int, off_delay_s int, priority text, alarm_class text,
    message text, operator_action text, consequences text, probable_cause text, max_response_time_s int
) ON COMMIT DROP;

INSERT INTO seed VALUES
    ('PLC-1', 'PT101_PRESSURE', 'HIHI', 90, NULL, 2, 2, 5, 'Critical', 'EQUIPMENT_PROTECTION', 'PT101_PRESSURE very high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment damage or an automatic shutdown is likely.', 'Process upset, blocked line or instrument fault.', 30),
    ('PLC-1', 'PT101_PRESSURE', 'HI', 80, NULL, 2, 10, 10, 'Major', 'PROCESS', 'PT101_PRESSURE high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-1', 'PT101_PRESSURE', 'LO', 20, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'PT101_PRESSURE low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-1', 'PT101_PRESSURE', 'LOLO', 10, NULL, 2, 2, 5, 'Major', 'PROCESS', 'PT101_PRESSURE very low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of the process, dry running or damage to equipment.', 'Supply loss, leak, closed valve or instrument fault.', 60),
    ('PLC-1', 'TT101', 'HI', 75, NULL, 2, 10, 10, 'Major', 'PROCESS', 'TT101 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-1', 'TT101', 'LO', 10, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'TT101 low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-1', 'TT101_TEMP', 'HIHI', 95, NULL, 2, 2, 5, 'Critical', 'EQUIPMENT_PROTECTION', 'TT101_TEMP very high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment damage or an automatic shutdown is likely.', 'Process upset, blocked line or instrument fault.', 30),
    ('PLC-1', 'TT101_TEMP', 'HI', 85, NULL, 2, 10, 10, 'Major', 'PROCESS', 'TT101_TEMP high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-1', 'TT101_TEMP', 'LO', 20, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'TT101_TEMP low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-1', 'FT101_FLOW', 'HI', 90, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'FT101_FLOW high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 300),
    ('PLC-1', 'FT101_FLOW', 'LO', 25, NULL, 2, 10, 10, 'Major', 'PROCESS', 'FT101_FLOW low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 60),
    ('PLC-1', 'PT102_PRESSURE', 'HI', 12, NULL, 0.3, 10, 10, 'Major', 'PROCESS', 'PT102_PRESSURE high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-1', 'PT102_PRESSURE', 'LO', 2, NULL, 0.3, 10, 10, 'Minor', 'PROCESS', 'PT102_PRESSURE low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-1', 'LT101', 'HI', 85, NULL, 2, 10, 10, 'Major', 'PROCESS', 'LT101 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-1', 'LT101', 'LO', 15, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'LT101 low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-1', 'XS101', 'DISCRETE', NULL, 0, 0, 60, 5, 'Major', 'PROCESS', 'XS101 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 60),
    ('PLC-1', 'ZS101', 'DISCRETE', NULL, 0, 0, 120, 5, 'Minor', 'PROCESS', 'ZS101 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 300),
    ('PLC-1', 'XA101', 'DISCRETE', NULL, 1, 0, 0, 0, 'Critical', 'EQUIPMENT_PROTECTION', 'XA101 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 30),
    ('PLC-2', 'PT201', 'HIHI', 9, NULL, 0.2, 2, 5, 'Critical', 'EQUIPMENT_PROTECTION', 'PT201 very high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment damage or an automatic shutdown is likely.', 'Process upset, blocked line or instrument fault.', 30),
    ('PLC-2', 'PT201', 'HI', 8, NULL, 0.2, 10, 10, 'Major', 'PROCESS', 'PT201 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-2', 'PT201', 'LO', 2, NULL, 0.2, 10, 10, 'Minor', 'PROCESS', 'PT201 low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-2', 'PT201', 'LOLO', 1, NULL, 0.2, 2, 5, 'Major', 'PROCESS', 'PT201 very low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of the process, dry running or damage to equipment.', 'Supply loss, leak, closed valve or instrument fault.', 60),
    ('PLC-2', 'FT201', 'HI', 320, NULL, 8, 10, 10, 'Minor', 'PROCESS', 'FT201 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 300),
    ('PLC-2', 'FT201', 'LO', 80, NULL, 8, 10, 10, 'Minor', 'PROCESS', 'FT201 low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-2', 'IT201', 'HIHI', 115, NULL, 2, 2, 5, 'Critical', 'EQUIPMENT_PROTECTION', 'IT201 very high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment damage or an automatic shutdown is likely.', 'Process upset, blocked line or instrument fault.', 30),
    ('PLC-2', 'IT201', 'HI', 105, NULL, 2, 10, 10, 'Major', 'EQUIPMENT_PROTECTION', 'IT201 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-2', 'XS201', 'DISCRETE', NULL, 0, 0, 60, 5, 'Major', 'PROCESS', 'XS201 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 60),
    ('PLC-2', 'XS202', 'DISCRETE', NULL, 0, 0, 60, 5, 'Minor', 'PROCESS', 'XS202 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 300),
    ('PLC-2', 'XA201', 'DISCRETE', NULL, 1, 0, 0, 0, 'Critical', 'EQUIPMENT_PROTECTION', 'XA201 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 30),
    ('PLC-3', 'LT301', 'HI', 85, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'LT301 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 300),
    ('PLC-3', 'LT301', 'LO', 15, NULL, 2, 10, 10, 'Major', 'PROCESS', 'LT301 low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 60),
    ('PLC-3', 'LT301', 'LOLO', 8, NULL, 2, 2, 5, 'Critical', 'EQUIPMENT_PROTECTION', 'LT301 very low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of the process, dry running or damage to equipment.', 'Supply loss, leak, closed valve or instrument fault.', 30),
    ('PLC-3', 'LT302', 'HI', 85, NULL, 2, 10, 10, 'Major', 'PROCESS', 'LT302 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-3', 'LT302', 'LO', 15, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'LT302 low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-3', 'TT301', 'HIHI', 70, NULL, 2, 2, 5, 'Critical', 'EQUIPMENT_PROTECTION', 'TT301 very high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment damage or an automatic shutdown is likely.', 'Process upset, blocked line or instrument fault.', 30),
    ('PLC-3', 'TT301', 'HI', 55, NULL, 2, 10, 10, 'Major', 'PROCESS', 'TT301 high', 'Check the process and reduce the load; verify the reading on the local instrument.', 'Equipment protection may trip, or the product may be off specification.', 'Process upset, blocked line or instrument fault.', 60),
    ('PLC-3', 'TT301', 'LO', -10, NULL, 2, 10, 10, 'Minor', 'PROCESS', 'TT301 low', 'Check supply and the process; verify the reading on the local instrument.', 'Loss of supply or flow; dry running or an off-specification product.', 'Supply loss, leak, closed valve or instrument fault.', 300),
    ('PLC-3', 'LSH301', 'DISCRETE', NULL, 1, 0, 30, 5, 'Major', 'PROCESS', 'LSH301 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 60),
    ('PLC-3', 'ZS301', 'DISCRETE', NULL, 0, 0, 120, 5, 'Minor', 'PROCESS', 'ZS301 alarm state', 'Check the equipment and its local status; restore normal operation or call maintenance.', 'The process or equipment may stop or be damaged.', 'Equipment fault, trip or manual intervention.', 300);

-- every seeded PV must exist (a typo must not silently skip an alarm)
DO $$
DECLARE missing text;
BEGIN
    SELECT string_agg(s.plc_name || '/' || s.pv_name, ', ') INTO missing
    FROM seed s LEFT JOIN plc_metadata p ON p.plc_name = s.plc_name
                LEFT JOIN pv_metadata v ON v.plc_id = p.plc_id AND v.pv_name = s.pv_name
    WHERE v.pv_id IS NULL;
    IF missing IS NOT NULL THEN RAISE EXCEPTION 'unknown PV(s) in the seed: %', missing; END IF;
END $$;

-- keep what is about to be replaced (migrated drafts, earlier test values) in the audit trail
INSERT INTO alarm_audit_log (user_name, action, alarm_id, outcome, reason, before_data)
SELECT 'test-data seed', 'ALARM_UPDATE', m.alarm_id, 'success',
       'previous configuration replaced by the test configuration (original values in before_data)', to_jsonb(m)
FROM alarm_metadata m
JOIN pv_metadata v ON v.pv_id = m.pv_id
JOIN plc_metadata p ON p.plc_id = v.plc_id
JOIN seed s ON s.plc_name = p.plc_name AND s.pv_name = v.pv_name AND s.alarm_type = m.alarm_type
WHERE m.created_by_name = 'migration 005'
   OR (m.created_by_name = 'test-data seed'
       AND (m.setpoint IS DISTINCT FROM s.setpoint OR m.trigger_state IS DISTINCT FROM s.trigger_state
            OR m.deadband <> s.deadband OR m.on_delay_s <> s.on_delay_s OR m.off_delay_s <> s.off_delay_s
            OR m.priority <> s.priority OR m.alarm_class <> s.alarm_class));

INSERT INTO alarm_metadata
        (pv_id, alarm_type, setpoint, trigger_state, setpoint_unit, deadband, on_delay_s, off_delay_s,
         priority, alarm_class, alarm_message, operator_action, consequences_of_inaction, probable_cause,
         max_response_time_s, rationale, rationalization_status, approved_by_name, approved_at, is_enabled,
         created_by_name, updated_by_name)
    SELECT v.pv_id, s.alarm_type, s.setpoint, s.trigger_state, v.unit, s.deadband, s.on_delay_s, s.off_delay_s,
           s.priority, s.alarm_class, s.message, s.operator_action, s.consequences, s.probable_cause,
           s.max_response_time_s,
           'TEST DATA: limits at about 80/90 % (HI/HIHI) and 20/10 % (LO/LOLO) of the range, deadband about 2 %, delays to ride through noise; not a formal rationalization',
           'APPROVED', 'test-data seed', now(), TRUE, 'test-data seed', 'test-data seed'
    FROM seed s
    JOIN plc_metadata p ON p.plc_name = s.plc_name
    JOIN pv_metadata v ON v.plc_id = p.plc_id AND v.pv_name = s.pv_name
    ON CONFLICT (pv_id, alarm_type) DO UPDATE SET
        setpoint = EXCLUDED.setpoint, trigger_state = EXCLUDED.trigger_state, setpoint_unit = EXCLUDED.setpoint_unit,
        deadband = EXCLUDED.deadband, on_delay_s = EXCLUDED.on_delay_s, off_delay_s = EXCLUDED.off_delay_s,
        priority = EXCLUDED.priority, alarm_class = EXCLUDED.alarm_class, alarm_message = EXCLUDED.alarm_message,
        operator_action = EXCLUDED.operator_action, consequences_of_inaction = EXCLUDED.consequences_of_inaction,
        probable_cause = EXCLUDED.probable_cause, max_response_time_s = EXCLUDED.max_response_time_s,
        rationale = EXCLUDED.rationale, rationalization_status = 'APPROVED',
        approved_by = NULL, approved_by_name = 'test-data seed', approved_at = now(), is_enabled = TRUE,
        updated_at = now(), updated_by_name = 'test-data seed'
    WHERE alarm_metadata.created_by_name IN ('migration 005', 'test-data seed');

-- one audit line per alarm written by this run (now() is the transaction time)
INSERT INTO alarm_audit_log (user_name, action, alarm_id, outcome, reason, after_data)
SELECT 'test-data seed', 'ALARM_APPROVE', m.alarm_id, 'success',
       'test configuration seeded (scripts/seed_test_alarms.sql), not a formal rationalization', to_jsonb(m)
FROM alarm_metadata m
WHERE m.approved_by_name = 'test-data seed' AND m.approved_at = now();

COMMIT;
