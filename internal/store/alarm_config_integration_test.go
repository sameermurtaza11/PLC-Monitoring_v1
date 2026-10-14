package store

import (
	"errors"
	"strings"
	"testing"
)

// Alarm definitions, their rationalization approval and the audit trail, on a
// REAL database (opt-in, scratch/test databases only — see openTestStore).
// Needs a fresh scratch database: the audit log and history cannot be cleaned.
func TestIntegrationAlarmConfiguration(t *testing.T) {
	s, ctx := openTestStore(t)

	var existing int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_metadata`).Scan(&existing); err != nil {
		t.Fatalf("apply migrations/004_alarms.sql first: %v", err)
	}
	if existing != 0 {
		t.Fatal("needs a FRESH scratch database")
	}

	supID, err := s.CreateUser(ctx, "sup", "sup-pass-123", RoleSupervisor)
	if err != nil {
		t.Fatal(err)
	}
	admID, _ := s.CreateUser(ctx, "adm", "adm-pass-123", RoleAdmin)
	sup := Actor{ID: supID, Username: "sup", IP: "10.0.0.1"}
	adm := Actor{ID: admID, Username: "adm", IP: "10.0.0.2"}

	// an analog PV, with its real range and unit
	var pvID int
	var unit string
	var pvMin, pvMax float64
	if err := s.pool.QueryRow(ctx, `SELECT pv_id, unit, pv_min, pv_max FROM pv_metadata WHERE register_address >= 30001 ORDER BY pv_id LIMIT 1`).Scan(&pvID, &unit, &pvMin, &pvMax); err != nil {
		t.Fatal(err)
	}
	sp := pvMin + (pvMax-pvMin)*0.8
	in := AlarmInput{PVID: pvID, AlarmType: AlarmHI, Setpoint: &sp, Deadband: 1, OnDelayS: 5, OffDelayS: 5,
		Priority: PriorityMajor, AlarmClass: "PROCESS", Message: "high", Enabled: true}

	var id int64
	t.Run("a new alarm starts as an unapproved DRAFT and is audited", func(t *testing.T) {
		id, _, err = s.SaveAlarmDef(ctx, 0, in, unit, "first version", sup)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := s.GetAlarmDef(ctx, id)
		if d.RationalizationStatus != RationDraft || d.Evaluable() {
			t.Errorf("a new alarm must be DRAFT and not evaluated: %+v", d)
		}
		if d.CreatedByName != "sup" || d.SetpointUnit != unit {
			t.Errorf("who/unit not recorded: %+v", d)
		}
		var action, who, outcome string
		var after []byte
		if err := s.pool.QueryRow(ctx, `SELECT action, user_name, outcome, after_data FROM alarm_audit_log WHERE alarm_id=$1 ORDER BY audit_id DESC LIMIT 1`, id).
			Scan(&action, &who, &outcome, &after); err != nil {
			t.Fatal(err)
		}
		if action != "ALARM_CREATE" || who != "sup" || outcome != "success" || !strings.Contains(string(after), `"priority": "Major"`) {
			t.Errorf("audit: %s %s %s %s", action, who, outcome, after)
		}
	})

	t.Run("a second alarm of the same type on the PV is refused", func(t *testing.T) {
		if _, _, err := s.SaveAlarmDef(ctx, 0, in, unit, "", sup); !errors.Is(err, ErrAlarmExists) {
			t.Fatalf("got %v, want ErrAlarmExists", err)
		}
	})

	t.Run("approval needs a complete rationalization", func(t *testing.T) {
		err := s.ApproveAlarmDef(ctx, id, adm)
		if !errors.Is(err, ErrNotRationalized) || !strings.Contains(err.Error(), "rationale") {
			t.Fatalf("got %v, want ErrNotRationalized naming what is missing", err)
		}
		in.OperatorAction, in.ConsequencesOfInaction, in.Rationale = "reduce load", "relief valve lifts", "Major: needs action within minutes"
		if _, _, err := s.SaveAlarmDef(ctx, id, in, unit, "rationalized", sup); err != nil {
			t.Fatal(err)
		}
		if err := s.ApproveAlarmDef(ctx, id, adm); err != nil {
			t.Fatal(err)
		}
		d, _ := s.GetAlarmDef(ctx, id)
		if d.RationalizationStatus != RationApproved || d.ApprovedByName != "adm" || d.ApprovedAt == nil || !d.Evaluable() {
			t.Fatalf("after approval: %+v", d)
		}
	})

	auditCount := func() int {
		var n int
		s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_audit_log WHERE alarm_id=$1`, id).Scan(&n)
		return n
	}

	t.Run("enabling, disabling and wording changes keep the approval but are audited", func(t *testing.T) {
		before := auditCount()
		in.Enabled, in.Message = false, "high (reworded)"
		_, reset, err := s.SaveAlarmDef(ctx, id, in, unit, "taken out of service for maintenance", sup)
		if err != nil || reset {
			t.Fatalf("err=%v reset=%v", err, reset)
		}
		d, _ := s.GetAlarmDef(ctx, id)
		if d.RationalizationStatus != RationApproved || d.Enabled || d.Evaluable() {
			t.Errorf("approved but disabled: %+v", d)
		}
		if auditCount() != before+1 {
			t.Error("the change must be audited")
		}
		in.Enabled = true
		s.SaveAlarmDef(ctx, id, in, unit, "back in service", sup)
	})

	t.Run("an identical save writes nothing", func(t *testing.T) {
		before := auditCount()
		if _, _, err := s.SaveAlarmDef(ctx, id, in, unit, "", sup); err != nil {
			t.Fatal(err)
		}
		if auditCount() != before {
			t.Error("an unchanged save must not create an audit line")
		}
	})

	t.Run("changing the setpoint withdraws the approval; the audit has the old and the new value", func(t *testing.T) {
		newSP := sp - 1
		in.Setpoint = &newSP
		_, reset, err := s.SaveAlarmDef(ctx, id, in, unit, "limit lowered after review", sup)
		if err != nil || !reset {
			t.Fatalf("err=%v reset=%v, want the approval withdrawn", err, reset)
		}
		d, _ := s.GetAlarmDef(ctx, id)
		if d.RationalizationStatus != RationDraft || d.ApprovedAt != nil || d.ApprovedByName != "" || d.Evaluable() {
			t.Errorf("must be DRAFT again, approval cleared: %+v", d)
		}
		var before, after []byte
		var reason string
		s.pool.QueryRow(ctx, `SELECT before_data, after_data, reason FROM alarm_audit_log WHERE alarm_id=$1 AND action='ALARM_UPDATE' ORDER BY audit_id DESC LIMIT 1`, id).Scan(&before, &after, &reason)
		if !strings.Contains(string(before), `"rationalization_status": "APPROVED"`) || !strings.Contains(string(after), `"rationalization_status": "DRAFT"`) {
			t.Errorf("before/after do not show the withdrawal:\n%s\n%s", before, after)
		}
		if !strings.Contains(reason, "limit lowered") || !strings.Contains(reason, "approval withdrawn") {
			t.Errorf("reason = %q", reason)
		}
	})

	t.Run("a PV unit or range change after approval makes the alarm unusable", func(t *testing.T) {
		if err := s.ApproveAlarmDef(ctx, id, adm); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE pv_metadata SET unit = 'changed-unit' WHERE pv_id = $1`, pvID); err != nil {
			t.Fatal(err)
		}
		d, _ := s.GetAlarmDef(ctx, id)
		if len(d.Issues()) == 0 || d.Evaluable() {
			t.Errorf("a changed PV unit must be flagged and stop evaluation: %+v", d.Issues())
		}
		s.pool.Exec(ctx, `UPDATE pv_metadata SET unit = $2 WHERE pv_id = $1`, pvID, unit)
	})

	t.Run("unknown alarms and refused actions", func(t *testing.T) {
		if err := s.ApproveAlarmDef(ctx, 999999, adm); !errors.Is(err, ErrAlarmNotFound) {
			t.Errorf("approve unknown: %v", err)
		}
		if _, _, err := s.SaveAlarmDef(ctx, 999999, in, unit, "", sup); !errors.Is(err, ErrAlarmNotFound) {
			t.Errorf("update unknown: %v", err)
		}
		if err := s.RecordAlarmAudit(ctx, AlarmAudit{Actor: sup, Action: "ACTION_DENIED", AlarmID: id, Outcome: "denied", Reason: "test"}); err != nil {
			t.Errorf("record a refused action: %v", err)
		}
	})
}
