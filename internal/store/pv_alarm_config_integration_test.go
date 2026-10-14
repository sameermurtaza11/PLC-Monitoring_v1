package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A PV and its alarms are saved and deleted together (one transaction). Opt-in
// (PLC_TEST_DATABASE_URL), scratch/test databases only, a FRESH one (the alarm
// history cannot be cleaned): migrations 001-006 and the demo PLCs.
func TestIntegrationPVWithAlarms(t *testing.T) {
	s, ctx := openTestStore(t)

	var plcID int
	if err := s.pool.QueryRow(ctx, `SELECT plc_id FROM plc_metadata ORDER BY plc_id LIMIT 1`).Scan(&plcID); err != nil {
		t.Fatalf("need at least one PLC: %v", err)
	}
	var existing int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_metadata`).Scan(&existing); err != nil || existing != 0 {
		t.Fatalf("needs a FRESH scratch database with migrations 001-006 (alarm_metadata rows: %d, err %v)", existing, err)
	}
	if _, err := s.pool.Exec(ctx, `SELECT deleted_at FROM pv_metadata LIMIT 0`); err != nil {
		t.Fatalf("apply migrations/006_pv_archive.sql: %v", err)
	}
	admID, _ := s.CreateUser(ctx, "adm", "adm-pass-123", RoleAdmin)
	supID, _ := s.CreateUser(ctx, "sup", "sup-pass-123", RoleSupervisor)
	adm := Actor{ID: admID, Username: "adm"}
	sup := Actor{ID: supID, Username: "sup"}

	analog := PVRecord{PV: PV{PLCID: plcID, RegisterAddress: 39001, Name: "TEST_PT", SystemName: "Test", Equipment: "E1", Description: "d",
		Unit: "bar", Min: 0, Max: 10, State0Text: "OFF", State1Text: "ON"}, Enabled: true}
	in := func(typ string, sp float64, db float64) AlarmSpec {
		v := sp
		return AlarmSpec{Type: typ, Enabled: true, Input: AlarmInput{AlarmType: typ, Setpoint: &v, Deadband: db, OnDelayS: 10, OffDelayS: 10,
			Priority: PriorityMajor, AlarmClass: "PROCESS", Message: "TEST_PT " + typ}}
	}
	four := func() []AlarmSpec {
		return []AlarmSpec{in(AlarmHIHI, 9, 0.2), in(AlarmHI, 8, 0.2), in(AlarmLO, 2, 0.2), in(AlarmLOLO, 1, 0.2)}
	}
	defsOf := func(pv int) map[string]AlarmDef {
		out := map[string]AlarmDef{}
		list, err := s.ListAlarmDefsOfPV(ctx, pv)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range list {
			out[d.AlarmType] = d
		}
		return out
	}
	count := func(q string, args ...any) int {
		var n int
		if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var pvID int

	t.Run("a new analog PV with four alarms is saved together", func(t *testing.T) {
		res, err := s.SavePVWithAlarms(ctx, analog, true, four(), false, "first version", sup)
		if err != nil {
			t.Fatal(err)
		}
		pvID = res.PVID
		defs := defsOf(pvID)
		if len(defs) != 4 {
			t.Fatalf("%d alarm definitions, want 4", len(defs))
		}
		for typ, d := range defs {
			if !d.Enabled || d.RationalizationStatus != RationDraft || d.PVID != pvID || d.SetpointUnit != "bar" {
				t.Errorf("%s: %+v", typ, d)
			}
			if d.CreatedByName != "sup" {
				t.Errorf("%s: created by %q", typ, d.CreatedByName)
			}
		}
		if n := count(`SELECT count(*) FROM alarm_audit_log WHERE action='ALARM_CREATE' AND user_name='sup'`); n != 4 {
			t.Errorf("%d creation audit lines, want 4", n)
		}
	})

	t.Run("editing updates the alarms in place: no duplicates, individual types switched off", func(t *testing.T) {
		edited := analog
		edited.ID = pvID
		edited.Description = "changed"
		specs := four()
		specs[1] = in(AlarmHI, 7.5, 0.2) // HI limit changed
		specs[3].Enabled = false         // LOLO switched off
		res, err := s.SavePVWithAlarms(ctx, edited, true, specs, false, "limits revised", sup)
		if err != nil {
			t.Fatal(err)
		}
		defs := defsOf(pvID)
		if len(defs) != 4 || count(`SELECT count(*) FROM alarm_metadata WHERE pv_id=$1`, pvID) != 4 {
			t.Fatalf("duplicates created: %d definitions", len(defs))
		}
		if *defs[AlarmHI].Setpoint != 7.5 || !defs[AlarmHI].Enabled {
			t.Errorf("HI not updated: %+v", defs[AlarmHI])
		}
		if defs[AlarmLOLO].Enabled {
			t.Error("LOLO was switched off but is still enabled")
		}
		if !defs[AlarmHIHI].Enabled || !defs[AlarmLO].Enabled {
			t.Error("the other alarms must stay in service")
		}
		got := map[string]string{}
		for _, c := range res.Changes {
			got[c.Type] = c.Action
		}
		if got[AlarmHI] != "updated" || got[AlarmLOLO] != "disabled" || got[AlarmHIHI] != "unchanged" || got[AlarmLO] != "unchanged" {
			t.Errorf("changes = %v", got)
		}
		var desc string
		s.pool.QueryRow(ctx, `SELECT description FROM pv_metadata WHERE pv_id=$1`, pvID).Scan(&desc)
		if desc != "changed" {
			t.Errorf("PV not updated: %q", desc)
		}
	})

	t.Run("an approved alarm whose limit changes goes back to draft; switching on/off keeps the approval", func(t *testing.T) {
		hi := defsOf(pvID)[AlarmHI]
		in := AlarmInput{PVID: pvID, AlarmType: AlarmHI, Setpoint: hi.Setpoint, Deadband: hi.Deadband, OnDelayS: 10, OffDelayS: 10,
			Priority: PriorityMajor, AlarmClass: "PROCESS", Message: hi.Message, OperatorAction: "reduce load",
			ConsequencesOfInaction: "valve lifts", Rationale: "needs action", Enabled: true}
		if _, _, err := s.SaveAlarmDef(ctx, hi.ID, in, "bar", "rationalized", sup); err != nil {
			t.Fatal(err)
		}
		if err := s.ApproveAlarmDef(ctx, hi.ID, adm); err != nil {
			t.Fatal(err)
		}
		edited := analog
		edited.ID = pvID
		specs := four()
		for i := range specs {
			specs[i].Input.OperatorAction, specs[i].Input.ConsequencesOfInaction, specs[i].Input.Rationale = "reduce load", "valve lifts", "needs action"
		}
		specs[1] = AlarmSpec{Type: AlarmHI, Enabled: true, Input: in}
		specs[1].Input.Setpoint = func() *float64 { v := 7.5; return &v }()
		specs[3].Enabled = false
		if _, err := s.SavePVWithAlarms(ctx, edited, true, specs, false, "", sup); err != nil {
			t.Fatal(err)
		}
		if d := defsOf(pvID)[AlarmHI]; d.RationalizationStatus != RationApproved {
			t.Fatalf("an unchanged approved alarm lost its approval: %s", d.RationalizationStatus)
		}
		// change the limit → approval withdrawn
		specs[1].Input.Setpoint = func() *float64 { v := 7.0; return &v }()
		res, err := s.SavePVWithAlarms(ctx, edited, true, specs, false, "", sup)
		if err != nil {
			t.Fatal(err)
		}
		if d := defsOf(pvID)[AlarmHI]; d.RationalizationStatus != RationDraft {
			t.Errorf("approval not withdrawn after a limit change: %s", d.RationalizationStatus)
		}
		for _, c := range res.Changes {
			if c.Type == AlarmHI && !c.ResetToDraft {
				t.Error("the result does not report the withdrawn approval")
			}
		}
	})

	t.Run("switching alarms off ends active occurrences, keeps definitions and history", func(t *testing.T) {
		d := defsOf(pvID)[AlarmHI]
		occ, err := s.ActivateAlarm(ctx, d, 9, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		before := count(`SELECT count(*) FROM alarm_history`)
		edited := analog
		edited.ID = pvID
		res, err := s.SavePVWithAlarms(ctx, edited, false, nil, false, "plant modification", sup)
		if err != nil {
			t.Fatal(err)
		}
		for typ, d := range defsOf(pvID) {
			if d.Enabled {
				t.Errorf("%s still in service", typ)
			}
		}
		if len(defsOf(pvID)) != 4 {
			t.Error("definitions were deleted")
		}
		var state string
		var normal bool
		if err := s.pool.QueryRow(ctx, `SELECT alarm_state, process_condition_active FROM active_alarms WHERE occurrence_id=$1`, occ).Scan(&state, &normal); err != nil {
			t.Fatal(err)
		}
		if state != StateRTNUN || normal {
			t.Errorf("occurrence = %s active=%v, want RTNUN and not active", state, normal)
		}
		var details, who string
		s.pool.QueryRow(ctx, `SELECT details, user_name FROM alarm_history WHERE occurrence_id=$1 ORDER BY event_id DESC LIMIT 1`, occ).Scan(&details, &who)
		if !strings.Contains(details, "taken out of service") || who != "sup" {
			t.Errorf("history: %q by %q", details, who)
		}
		if after := count(`SELECT count(*) FROM alarm_history`); after != before+1 {
			t.Errorf("history grew by %d, want 1", after-before)
		}
		ended := 0
		for _, c := range res.Changes {
			ended += c.Ended
		}
		if ended != 1 {
			t.Errorf("reported %d ended occurrences, want 1", ended)
		}
		// the audit trail says why
		if n := count(`SELECT count(*) FROM alarm_audit_log WHERE action='ALARM_UPDATE' AND reason LIKE 'taken out of service%'`); n < 3 {
			t.Errorf("%d out-of-service audit lines", n)
		}
		if err := s.Acknowledge(ctx, occ, Actor{ID: supID, Username: "sup"}, nil); err != nil { // the operator can still acknowledge it
			t.Errorf("cannot acknowledge the ended occurrence: %v", err)
		}
	})

	t.Run("switching them on again re-enables the same definitions", func(t *testing.T) {
		edited := analog
		edited.ID = pvID
		res, err := s.SavePVWithAlarms(ctx, edited, true, []AlarmSpec{in(AlarmHI, 8, 0.2), in(AlarmLO, 2, 0.2)}, false, "", sup)
		if err != nil {
			t.Fatal(err)
		}
		if n := count(`SELECT count(*) FROM alarm_metadata WHERE pv_id=$1`, pvID); n != 4 {
			t.Fatalf("%d definitions, want the same 4", n)
		}
		on := 0
		for typ, d := range defsOf(pvID) {
			if d.Enabled {
				on++
				if typ != AlarmHI && typ != AlarmLO {
					t.Errorf("%s was switched on", typ)
				}
			}
		}
		if on != 2 {
			t.Errorf("%d alarms in service, want 2", on)
		}
		for _, c := range res.Changes {
			if c.Action != "enabled" && c.Action != "updated" {
				t.Errorf("%s: %s", c.Type, c.Action)
			}
		}
	})

	t.Run("nothing is half saved when the alarm part fails", func(t *testing.T) {
		edited := analog
		edited.ID = pvID
		edited.Description = "must not be stored"
		broken := in(AlarmHI, 8, 0.2)
		broken.Input.Setpoint = nil // refused by the database (an analog alarm needs a limit)
		if _, err := s.SavePVWithAlarms(ctx, edited, true, []AlarmSpec{in(AlarmLO, 3, 0.2), broken}, false, "", sup); err == nil {
			t.Fatal("the invalid alarm was accepted")
		}
		var desc string
		s.pool.QueryRow(ctx, `SELECT description FROM pv_metadata WHERE pv_id=$1`, pvID).Scan(&desc)
		if desc == "must not be stored" {
			t.Error("the PV change survived although the alarm part failed")
		}
		if d := defsOf(pvID)[AlarmLO]; *d.Setpoint != 2 {
			t.Errorf("the LO change survived: %v", *d.Setpoint)
		}

		// a NEW PV: nothing may be created
		fresh := analog
		fresh.RegisterAddress, fresh.Name = 39002, "TEST_NEVER"
		if _, err := s.SavePVWithAlarms(ctx, fresh, true, []AlarmSpec{broken}, false, "", sup); err == nil {
			t.Fatal("the invalid alarm was accepted")
		}
		if n := count(`SELECT count(*) FROM pv_metadata WHERE pv_name='TEST_NEVER'`); n != 0 {
			t.Error("the PV was created although its alarm failed")
		}
	})

	t.Run("approval in the same save needs a complete rationalization and is all-or-nothing", func(t *testing.T) {
		pv := analog
		pv.RegisterAddress, pv.Name = 39003, "TEST_APPROVE"
		specs := []AlarmSpec{in(AlarmHI, 8, 0.2)}
		if _, err := s.SavePVWithAlarms(ctx, pv, true, specs, true, "", adm); !errors.Is(err, ErrNotRationalized) {
			t.Fatalf("approval without rationalization: %v", err)
		}
		if n := count(`SELECT count(*) FROM pv_metadata WHERE pv_name='TEST_APPROVE'`); n != 0 {
			t.Error("the PV was created although the approval failed")
		}
		specs[0].Input.OperatorAction, specs[0].Input.ConsequencesOfInaction, specs[0].Input.Rationale = "act", "bad", "why"
		res, err := s.SavePVWithAlarms(ctx, pv, true, specs, true, "", adm)
		if err != nil {
			t.Fatal(err)
		}
		d := defsOf(res.PVID)[AlarmHI]
		if d.RationalizationStatus != RationApproved || d.ApprovedByName != "adm" || !res.Changes[0].Approved {
			t.Errorf("not approved: %+v", d)
		}
		if _, err := s.DeletePVWithAlarms(ctx, res.PVID, false, adm); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a digital PV gets one discrete alarm; changing the PV type retires the other alarms", func(t *testing.T) {
		di := PVRecord{PV: PV{PLCID: plcID, RegisterAddress: 19001, Name: "TEST_DI", Unit: "", Min: 0, Max: 1, State0Text: "STOPPED", State1Text: "RUNNING"}, Enabled: true}
		one := 0
		spec := AlarmSpec{Type: AlarmDiscrete, Enabled: true, Input: AlarmInput{AlarmType: AlarmDiscrete, TriggerState: &one, OnDelayS: 5, OffDelayS: 2,
			Priority: PriorityMajor, AlarmClass: "PROCESS", Message: "TEST_DI stopped"}}
		res, err := s.SavePVWithAlarms(ctx, di, true, []AlarmSpec{spec}, false, "", sup)
		if err != nil {
			t.Fatal(err)
		}
		defs := defsOf(res.PVID)
		if len(defs) != 1 || defs[AlarmDiscrete].TriggerState == nil || *defs[AlarmDiscrete].TriggerState != 0 || defs[AlarmDiscrete].SetpointUnit != "" {
			t.Fatalf("discrete alarm = %+v", defs)
		}
		// the same PV becomes analog: the discrete alarm no longer fits and is retired
		asAnalog := di
		asAnalog.ID = res.PVID
		asAnalog.RegisterAddress, asAnalog.Unit, asAnalog.Min, asAnalog.Max = 39004, "°C", 0, 100
		if _, err := s.SavePVWithAlarms(ctx, asAnalog, true, []AlarmSpec{in(AlarmHI, 80, 2)}, false, "", sup); err != nil {
			t.Fatal(err)
		}
		defs = defsOf(res.PVID)
		if defs[AlarmDiscrete].Enabled || !defs[AlarmHI].Enabled || len(defs) != 2 {
			t.Errorf("after the type change: %+v", defs)
		}
		if _, err := s.DeletePVWithAlarms(ctx, res.PVID, false, sup); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a PV name or register cannot be used twice, but an archived PV frees them", func(t *testing.T) {
		dup := analog
		dup.RegisterAddress = 39005
		_, err := s.SavePVWithAlarms(ctx, dup, false, nil, false, "", sup) // same name TEST_PT
		var ce *ConstraintError
		if !errors.As(err, &ce) || ce.Constraint != "pv_unique_name" {
			t.Errorf("duplicate name: %v", err)
		}
	})

	t.Run("deleting a PV that nothing refers to erases it with its alarm configurations", func(t *testing.T) {
		pv := analog
		pv.RegisterAddress, pv.Name = 39010, "TEST_CLEAN"
		res, err := s.SavePVWithAlarms(ctx, pv, true, []AlarmSpec{in(AlarmHI, 8, 0.2), in(AlarmLO, 2, 0.2)}, false, "", sup)
		if err != nil {
			t.Fatal(err)
		}
		imp, err := s.PVDeletionImpact(ctx, res.PVID)
		if err != nil || imp.AlarmDefs != 2 || imp.AlarmEvents != 0 || imp.RetainsRecords(false) {
			t.Fatalf("impact = %+v, %v", imp, err)
		}
		var ids []int64
		for _, d := range defsOf(res.PVID) {
			ids = append(ids, d.ID)
		}
		del, err := s.DeletePVWithAlarms(ctx, res.PVID, false, adm)
		if err != nil {
			t.Fatal(err)
		}
		if !del.Erased || del.Archived || del.AlarmDefs != 2 {
			t.Errorf("result = %+v", del)
		}
		if count(`SELECT count(*) FROM pv_metadata WHERE pv_id=$1`, res.PVID) != 0 || count(`SELECT count(*) FROM alarm_metadata WHERE pv_id=$1`, res.PVID) != 0 {
			t.Error("rows remain after the complete erase")
		}
		// the audit trail keeps what was deleted
		if n := count(`SELECT count(*) FROM alarm_audit_log WHERE action='ALARM_DELETE' AND before_data IS NOT NULL AND alarm_id = ANY($1)`, ids); n != 2 {
			t.Errorf("%d ALARM_DELETE audit lines with the removed definition, want 2", n)
		}
		if n := count(`SELECT count(*) FROM alarm_audit_log WHERE action='PV_DELETE' AND reason LIKE '%TEST_CLEAN%'`); n != 1 {
			t.Errorf("%d PV_DELETE audit lines", n)
		}
		if _, err := s.DeletePVWithAlarms(ctx, res.PVID, false, adm); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("deleting twice: %v", err)
		}
	})

	t.Run("deleting a PV with alarm history archives it: history and audit stay, name and register are free again", func(t *testing.T) {
		histBefore := count(`SELECT count(*) FROM alarm_history`)
		auditBefore := count(`SELECT count(*) FROM alarm_audit_log`)
		imp, err := s.PVDeletionImpact(ctx, pvID)
		if err != nil || imp.AlarmEvents == 0 || imp.AlarmDefs != 4 || !imp.RetainsRecords(true) {
			t.Fatalf("impact = %+v, %v", imp, err)
		}
		// an alarm that is active at the moment of deletion
		hi := defsOf(pvID)[AlarmHI]
		occ, _ := s.ActivateAlarm(ctx, hi, 9, time.Now())
		del, err := s.DeletePVWithAlarms(ctx, pvID, true, adm)
		if err != nil {
			t.Fatal(err)
		}
		if del.Erased || !del.Archived || del.AlarmDefs != 4 || del.OccurrencesEnded != 1 || del.AlarmEvents == 0 {
			t.Errorf("result = %+v", del)
		}
		if got := count(`SELECT count(*) FROM alarm_history`); got < histBefore {
			t.Errorf("alarm history lost rows: %d → %d", histBefore, got)
		}
		if got := count(`SELECT count(*) FROM alarm_audit_log`); got <= auditBefore {
			t.Error("the deletion was not audited")
		}
		if n := count(`SELECT count(*) FROM alarm_metadata WHERE pv_id=$1 AND is_enabled`, pvID); n != 0 {
			t.Errorf("%d alarms of the archived PV are still in service", n)
		}
		var archived bool
		var enabled bool
		s.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL, enabled FROM pv_metadata WHERE pv_id=$1`, pvID).Scan(&archived, &enabled)
		if !archived || enabled {
			t.Errorf("archived=%v enabled=%v", archived, enabled)
		}
		var state string
		if err := s.pool.QueryRow(ctx, `SELECT alarm_state FROM active_alarms WHERE occurrence_id=$1`, occ).Scan(&state); err != nil || state != StateRTNUN {
			t.Errorf("the active occurrence = %q, %v (it waits for acknowledgement)", state, err)
		}
		// hidden from the configuration
		if _, err := s.GetPV(ctx, pvID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetPV of an archived PV: %v", err)
		}
		list, _ := s.ListPVs(ctx, 0)
		for _, p := range list {
			if p.ID == pvID {
				t.Error("the archived PV is listed")
			}
		}
		pickers, _ := s.ListAlarmPVs(ctx)
		for _, p := range pickers {
			if p.ID == pvID {
				t.Error("the archived PV can still be chosen for a new alarm")
			}
		}
		// still visible in the alarm views that need the records
		list2, _ := s.ListAlarmDefs(ctx)
		seen := false
		for _, d := range list2 {
			if d.PVID == pvID {
				seen = d.PVArchived
			}
		}
		if !seen {
			t.Error("the alarm definitions of an archived PV must stay readable (flagged archived) for the alarm list and history")
		}
		if evs, _, err := s.AlarmEvents(ctx, HistoryFilter{PVID: pvID, Limit: 50}); err != nil || len(evs) == 0 {
			t.Errorf("history of the archived PV: %d events, %v", len(evs), err)
		}
		// the same name and register can be used again
		again := analog
		if _, err := s.SavePVWithAlarms(ctx, again, true, four(), false, "", sup); err != nil {
			t.Fatalf("re-using the name and register of an archived PV: %v", err)
		}
		// the operator can still acknowledge the ended occurrence, which then completes
		if err := s.Acknowledge(ctx, occ, Actor{ID: supID, Username: "sup"}, nil); err != nil {
			t.Errorf("acknowledge: %v", err)
		}
	})

	t.Run("value history alone also archives, unless the user deletes it too", func(t *testing.T) {
		pv := analog
		pv.RegisterAddress, pv.Name = 39020, "TEST_VALUES"
		res, _ := s.SavePVWithAlarms(ctx, pv, false, nil, false, "", sup)
		s.pool.Exec(ctx, `INSERT INTO pv_history (pv_id, ts, raw_value, pv_value) VALUES ($1, now(), 1, 1.5)`, res.PVID)
		del, err := s.DeletePVWithAlarms(ctx, res.PVID, false, sup)
		if err != nil || !del.Archived || del.ValueHistoryKept != 1 {
			t.Fatalf("keeping value history: %+v, %v", del, err)
		}
		if count(`SELECT count(*) FROM pv_history WHERE pv_id=$1`, res.PVID) != 1 {
			t.Error("value history was deleted without being asked")
		}

		pv.RegisterAddress, pv.Name = 39021, "TEST_VALUES2"
		res, _ = s.SavePVWithAlarms(ctx, pv, false, nil, false, "", sup)
		s.pool.Exec(ctx, `INSERT INTO pv_history (pv_id, ts, raw_value, pv_value) VALUES ($1, now(), 1, 1.5)`, res.PVID)
		del, err = s.DeletePVWithAlarms(ctx, res.PVID, true, sup)
		if err != nil || !del.Erased || del.ValueHistoryDeleted != 1 {
			t.Fatalf("deleting value history too: %+v, %v", del, err)
		}
	})
}
