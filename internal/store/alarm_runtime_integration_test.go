package store

import (
	"errors"
	"testing"
	"time"
)

// The alarm life cycle (activation, acknowledgement, return to normal,
// shelving) on a REAL database. Opt-in (PLC_TEST_DATABASE_URL), scratch/test
// databases only, and a FRESH one: alarm_history cannot be cleaned. Do not run
// it in the same database as TestIntegrationAlarmSchema / ...Configuration.
func TestIntegrationAlarmLifecycle(t *testing.T) {
	s, ctx := openTestStore(t)

	var existing int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_metadata`).Scan(&existing); err != nil {
		t.Fatalf("apply migrations/004_alarms.sql first: %v", err)
	}
	if existing != 0 {
		t.Fatal("needs a FRESH scratch database")
	}
	opID, _ := s.CreateUser(ctx, "op", "op-pass-1234", RoleOperator)
	supID, _ := s.CreateUser(ctx, "sup", "sup-pass-123", RoleSupervisor)
	admID, _ := s.CreateUser(ctx, "adm", "adm-pass-123", RoleAdmin)
	op := Actor{ID: opID, Username: "op"}
	sup := Actor{ID: supID, Username: "sup"}
	adm := Actor{ID: admID, Username: "adm"}

	var pvID int
	var unit string
	var pvMin, pvMax float64
	if err := s.pool.QueryRow(ctx, `SELECT pv_id, unit, pv_min, pv_max FROM pv_metadata WHERE register_address >= 30001 ORDER BY pv_id LIMIT 1`).Scan(&pvID, &unit, &pvMin, &pvMax); err != nil {
		t.Fatal(err)
	}
	sp := pvMin + (pvMax-pvMin)*0.8
	in := AlarmInput{PVID: pvID, AlarmType: AlarmHI, Setpoint: &sp, Priority: PriorityMajor, AlarmClass: "PROCESS",
		Message: "high", OperatorAction: "reduce load", ConsequencesOfInaction: "relief valve lifts",
		Rationale: "Major: needs action within minutes", Enabled: true}
	id, _, err := s.SaveAlarmDef(ctx, 0, in, unit, "test", sup)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveAlarmDef(ctx, id, adm); err != nil {
		t.Fatal(err)
	}
	def, _ := s.GetAlarmDef(ctx, id)
	if !def.Evaluable() {
		t.Fatalf("definition not evaluable: %+v %v", def, def.Issues())
	}

	state := func(occ int64) string {
		var st string
		if err := s.pool.QueryRow(ctx, `SELECT alarm_state FROM active_alarms WHERE occurrence_id=$1`, occ).Scan(&st); err != nil {
			return "(gone)"
		}
		return st
	}
	events := func(occ int64) string {
		rows, err := s.pool.Query(ctx, `SELECT event_type||':'||COALESCE(previous_state,'-')||'>'||COALESCE(new_state,'-') FROM alarm_history WHERE occurrence_id=$1 ORDER BY event_id`, occ)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := ""
		for rows.Next() {
			var e string
			rows.Scan(&e)
			out += e + " "
		}
		return out
	}
	hi := sp + 5
	now := func() time.Time { return time.Now() }

	t.Run("activate, acknowledge, return to normal completes the occurrence", func(t *testing.T) {
		occ, err := s.ActivateAlarm(ctx, def, hi, now())
		if err != nil {
			t.Fatal(err)
		}
		if state(occ) != StateUnack {
			t.Fatalf("state = %s", state(occ))
		}
		if _, err := s.ActivateAlarm(ctx, def, hi, now()); err == nil {
			t.Error("a second activation of an abnormal alarm must be refused (no duplicates)")
		}
		ackValue := sp + 3
		if err := s.Acknowledge(ctx, occ, op, &ackValue); err != nil {
			t.Fatal(err)
		}
		var recorded *float64
		s.pool.QueryRow(ctx, `SELECT pv_value FROM alarm_history WHERE occurrence_id=$1 AND event_type='ACKNOWLEDGED'`, occ).Scan(&recorded)
		if recorded == nil || *recorded != ackValue {
			t.Errorf("history value of the acknowledgement = %v, want the value at that moment (%v), not the activation value", recorded, ackValue)
		}
		if state(occ) != StateAcked {
			t.Fatalf("state = %s", state(occ))
		}
		if err := s.ReturnToNormal(ctx, occ, sp-5, now()); err != nil {
			t.Fatal(err)
		}
		if state(occ) != "(gone)" {
			t.Fatalf("state = %s, want the completed occurrence removed", state(occ))
		}
		if got := events(occ); got != "ACTIVATED:->UNACK ACKNOWLEDGED:UNACK>ACKED RETURNED_TO_NORMAL:ACKED>NORM " {
			t.Errorf("history = %q", got)
		}
	})

	t.Run("return to normal before acknowledgement waits for it (RTNUN), then NORM; a new activation is a new occurrence", func(t *testing.T) {
		occ, _ := s.ActivateAlarm(ctx, def, hi, now())
		if err := s.ReturnToNormal(ctx, occ, sp-5, now()); err != nil {
			t.Fatal(err)
		}
		if state(occ) != StateRTNUN {
			t.Fatalf("state = %s", state(occ))
		}
		occ2, err := s.ActivateAlarm(ctx, def, hi, now())
		if err != nil || occ2 == occ {
			t.Fatalf("re-activation while the older one waits for acknowledgement: id %d err %v, want a new occurrence", occ2, err)
		}
		if err := s.Acknowledge(ctx, occ, op, nil); err != nil { // RTNUN → NORM
			t.Fatal(err)
		}
		if state(occ) != "(gone)" || state(occ2) != StateUnack {
			t.Fatalf("states = %s / %s", state(occ), state(occ2))
		}
		if got := events(occ); got != "ACTIVATED:->UNACK RETURNED_TO_NORMAL:UNACK>RTNUN ACKNOWLEDGED:RTNUN>NORM " {
			t.Errorf("history = %q", got)
		}
		s.Acknowledge(ctx, occ2, op, nil)
		s.ReturnToNormal(ctx, occ2, sp-5, now())
	})

	t.Run("shelving keeps tracking the process condition", func(t *testing.T) {
		occ, _ := s.ActivateAlarm(ctx, def, hi, now())
		if err := s.Shelve(ctx, occ, now().Add(time.Hour), "", sup, nil); err == nil {
			t.Error("shelving without a reason must be refused")
		}
		if err := s.Shelve(ctx, occ, now().Add(time.Hour), "instrument being repaired", sup, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Shelve(ctx, occ, now().Add(time.Hour), "again", sup, nil); !errors.Is(err, ErrWrongAlarmState) {
			t.Errorf("shelving twice: %v", err)
		}
		if err := s.Acknowledge(ctx, occ, op, nil); !errors.Is(err, ErrWrongAlarmState) {
			t.Errorf("acknowledging a shelved alarm: %v", err)
		}
		if err := s.ReturnToNormal(ctx, occ, sp-5, now()); err != nil {
			t.Fatal(err)
		}
		if state(occ) != StateShlvd {
			t.Fatalf("state = %s, want it to stay shelved while the condition is normal", state(occ))
		}
		again, err := s.ActivateAlarm(ctx, def, hi, now()) // abnormal again while shelved
		if err != nil || again != occ {
			t.Fatalf("got occurrence %d err %v, want the shelved occurrence %d", again, err, occ)
		}
		if err := s.Unshelve(ctx, occ, sup, nil); err != nil {
			t.Fatal(err)
		}
		if state(occ) != StateUnack {
			t.Fatalf("after unshelving an abnormal alarm: %s, want UNACK", state(occ))
		}
		if got := events(occ); got != "ACTIVATED:->UNACK SHELVED:UNACK>SHLVD RETURNED_TO_NORMAL:SHLVD>- ACTIVATED:SHLVD>- UNSHELVED:SHLVD>UNACK " {
			t.Errorf("history = %q", got)
		}
		s.Acknowledge(ctx, occ, op, nil)
		s.ReturnToNormal(ctx, occ, sp-5, now())
	})

	t.Run("a shelf that ends returns the alarm to the list: UNACK if still abnormal, RTNUN if not", func(t *testing.T) {
		a, _ := s.ActivateAlarm(ctx, def, hi, now())
		s.Acknowledge(ctx, a, op, nil)
		if err := s.Shelve(ctx, a, now().Add(300*time.Millisecond), "short shelf", sup, nil); err != nil {
			t.Fatal(err)
		}
		if n, _ := s.ExpireShelves(ctx, now()); n != 0 {
			t.Fatalf("expired %d too early", n)
		}
		time.Sleep(400 * time.Millisecond)
		n, err := s.ExpireShelves(ctx, now())
		if err != nil || n != 1 {
			t.Fatalf("expired %d err %v, want 1", n, err)
		}
		if state(a) != StateUnack { // it had been acknowledged before, but must be acknowledged again
			t.Fatalf("state = %s, want UNACK", state(a))
		}

		s.ReturnToNormal(ctx, a, sp-5, now()) // UNACK → RTNUN
		s.Shelve(ctx, a, now().Add(300*time.Millisecond), "again", sup, nil)
		time.Sleep(400 * time.Millisecond)
		if n, _ := s.ExpireShelves(ctx, now()); n != 1 || state(a) != StateRTNUN {
			t.Fatalf("expired %d state %s, want RTNUN", n, state(a))
		}
		s.Acknowledge(ctx, a, op, nil)
	})

	t.Run("acknowledging something that does not exist or is already acknowledged is refused", func(t *testing.T) {
		if err := s.Acknowledge(ctx, 999999, op, nil); !errors.Is(err, ErrOccurrenceGone) {
			t.Errorf("got %v", err)
		}
		a, _ := s.ActivateAlarm(ctx, def, hi, now())
		s.Acknowledge(ctx, a, op, nil)
		if err := s.Acknowledge(ctx, a, op, nil); !errors.Is(err, ErrWrongAlarmState) {
			t.Errorf("second acknowledgement: %v", err)
		}
		s.ReturnToNormal(ctx, a, sp-5, now())
	})

	t.Run("only one process can run the engine", func(t *testing.T) {
		l1, err := s.TryEngineLock(ctx)
		if err != nil || l1 == nil {
			t.Fatalf("first lock: %v %v", l1, err)
		}
		if !l1.Held(ctx) {
			t.Error("lock reported lost")
		}
		l2, err := s.TryEngineLock(ctx)
		if err != nil || l2 != nil {
			t.Fatalf("second lock: %v %v, want none while the first is held", l2, err)
		}
		l1.Release()
		l3, err := s.TryEngineLock(ctx)
		if err != nil || l3 == nil {
			t.Fatalf("lock after release: %v %v", l3, err)
		}
		l3.Release()
	})

	t.Run("events and metadata of one PV", func(t *testing.T) {
		evs, total, err := s.AlarmEvents(ctx, HistoryFilter{PVID: pvID, Limit: 5})
		if err != nil || total == 0 || len(evs) == 0 || len(evs) > 5 {
			t.Fatalf("events of the PV: %d of %d, err %v", len(evs), total, err)
		}
		for _, e := range evs {
			if e.PVName == "" || e.AlarmID != id {
				t.Errorf("event of another PV: %+v", e)
			}
		}
		if _, other, _ := s.AlarmEvents(ctx, HistoryFilter{PVID: pvID + 100000, Limit: 5}); other != 0 {
			t.Errorf("a PV without alarms has %d events", other)
		}
		info, err := s.GetPVInfo(ctx, pvID)
		if err != nil || info.PVID != pvID || info.PLCName == "" || info.PLCAddress == "" || info.CreatedAt.IsZero() {
			t.Errorf("PV info = %+v, err %v", info, err)
		}
		if _, err := s.GetPVInfo(ctx, pvID+100000); err == nil {
			t.Error("a missing PV must be an error")
		}
	})

	t.Run("the event log can be read page by page", func(t *testing.T) {
		all, total, err := s.AlarmEvents(ctx, HistoryFilter{Limit: 1000})
		if err != nil || total != len(all) || total < 6 {
			t.Fatalf("all events: %d of %d, %v", len(all), total, err)
		}
		var paged []int64
		for off := 0; off < total; off += 4 {
			part, n, err := s.AlarmEvents(ctx, HistoryFilter{Limit: 4, Offset: off})
			if err != nil || n != total {
				t.Fatalf("page at %d: %v, total %d", off, err, n)
			}
			for _, e := range part {
				paged = append(paged, e.ID)
			}
		}
		if len(paged) != len(all) {
			t.Fatalf("pages hold %d events, want %d", len(paged), len(all))
		}
		for i := range all {
			if paged[i] != all[i].ID {
				t.Fatalf("event %d differs between the full list and the pages", i)
			}
		}
		if past, n, _ := s.AlarmEvents(ctx, HistoryFilter{Limit: 4, Offset: total + 50}); len(past) != 0 || n != total {
			t.Errorf("past the end: %d rows, total %d", len(past), n)
		}
	})

	var open int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM active_alarms`).Scan(&open)
	if open != 0 {
		t.Errorf("%d occurrences left outstanding", open)
	}
}
