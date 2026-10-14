package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Database-level rules of migration 004 (the alarm tables). The state model
// is enforced by PostgreSQL itself, so these tests talk to it with raw SQL,
// the way a careless script or a generic UPDATE would.
//
// Opt-in, scratch/test databases only (see openTestStore). The alarm history
// is append-only and cannot be cleaned, so run it on a FRESH scratch database.

// try runs statements in one transaction and returns the first error, from a
// statement or from COMMIT (the history rule is checked at commit).
func try(ctx context.Context, s *Store, stmts ...string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, st); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
	}
	return tx.Commit(ctx)
}

func mustFail(t *testing.T, ctx context.Context, s *Store, wantInMessage, name string, stmts ...string) {
	t.Helper()
	err := try(ctx, s, stmts...)
	if err == nil {
		t.Errorf("%s: the database accepted it, but it must be refused", name)
		return
	}
	if wantInMessage != "" && !strings.Contains(err.Error(), wantInMessage) {
		t.Errorf("%s: refused for another reason than expected\n  want text %q\n  got       %v", name, wantInMessage, err)
	}
}

func mustWork(t *testing.T, ctx context.Context, s *Store, name string, stmts ...string) {
	t.Helper()
	if err := try(ctx, s, stmts...); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func hist(occ int64, alarm int64, event, prev, next string) string {
	p, n := "NULL", "NULL"
	if prev != "" {
		p = "'" + prev + "'"
	}
	if next != "" {
		n = "'" + next + "'"
	}
	return fmt.Sprintf(`INSERT INTO alarm_history (occurrence_id, alarm_id, event_type, previous_state, new_state, event_timestamp, pv_value)
		VALUES (%d, %d, '%s', %s, %s, now(), 1)`, occ, alarm, event, p, n)
}

func TestIntegrationAlarmSchema(t *testing.T) {
	s, ctx := openTestStore(t)

	var existing int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_metadata`).Scan(&existing); err != nil {
		t.Fatalf("alarm tables missing? apply migrations/004_alarms.sql: %v", err)
	}
	if existing != 0 {
		t.Fatal("this test needs a FRESH scratch database (alarm_history cannot be cleaned): recreate it")
	}
	var pvs []int
	rows, err := s.pool.Query(ctx, `SELECT pv_id FROM pv_metadata ORDER BY pv_id LIMIT 3`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int
		rows.Scan(&id)
		pvs = append(pvs, id)
	}
	rows.Close()
	if len(pvs) < 3 {
		t.Fatal("need at least 3 PVs in the test database")
	}

	insertAlarm := func(pv int, typ, extra string) (int64, error) {
		var id int64
		sql := fmt.Sprintf(`INSERT INTO alarm_metadata (pv_id, alarm_type, setpoint, priority, alarm_class, alarm_message %s)
			VALUES (%d, '%s', 10, 'Major', 'PROCESS', 'test alarm' %s) RETURNING alarm_id`, extraCols(extra), pv, typ, extraVals(extra))
		err := s.pool.QueryRow(ctx, sql).Scan(&id)
		return id, err
	}

	t.Run("alarm definitions are validated by the database", func(t *testing.T) {
		a, err := insertAlarm(pvs[0], "HI", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = a
		mustFail(t, ctx, s, "alarm_unique_pv_type", "second HI on the same PV",
			fmt.Sprintf(`INSERT INTO alarm_metadata (pv_id, alarm_type, setpoint, priority, alarm_class, alarm_message) VALUES (%d,'HI',5,'Major','PROCESS','dup')`, pvs[0]))
		bad := map[string]string{
			"alarm_priority_check":        `priority = 'High'`,
			"alarm_class_check":           `alarm_class = 'WHATEVER'`,
			"alarm_deadband_check":        `deadband = -1`,
			"alarm_on_delay_check":        `on_delay_s = 4000`,
			"alarm_off_delay_check":       `off_delay_s = -5`,
			"alarm_response_time_check":   `max_response_time_s = 0`,
			"alarm_message_check":         `alarm_message = '  '`,
			"alarm_rationalization_check": `rationalization_status = 'MAYBE'`,
			"alarm_approval_complete":     `rationalization_status = 'APPROVED'`, // approved without approver time / rationale
			"alarm_trigger_exclusive":     `trigger_state = 1`,                   // analog alarm with BOTH a setpoint and a state
			"alarm_type_check":            `alarm_type = 'HIGHEST'`,
		}
		for constraint, set := range bad {
			mustFail(t, ctx, s, constraint, "update "+set,
				fmt.Sprintf(`UPDATE alarm_metadata SET %s WHERE alarm_id = %d`, set, a))
		}
		mustFail(t, ctx, s, "alarm_trigger_matches_type", "DISCRETE alarm without an abnormal state",
			fmt.Sprintf(`INSERT INTO alarm_metadata (pv_id, alarm_type, setpoint, priority, alarm_class, alarm_message) VALUES (%d,'DISCRETE',1,'Major','PROCESS','x')`, pvs[1]))
		// priority and type are independent: HIHI does not imply Critical
		if _, err := insertAlarm(pvs[1], "HIHI", ""); err != nil {
			t.Errorf("HIHI with priority Major must be possible: %v", err)
		}
	})

	var alarmID int64
	if err := s.pool.QueryRow(ctx, `SELECT alarm_id FROM alarm_metadata WHERE alarm_type = 'HI' LIMIT 1`).Scan(&alarmID); err != nil {
		t.Fatal(err)
	}
	var occ int64
	next := func() int64 { // reserve an occurrence id the way the application will
		var id int64
		if err := s.pool.QueryRow(ctx, `SELECT nextval('active_alarms_occurrence_id_seq')`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	activate := func(id int64) string {
		return fmt.Sprintf(`INSERT INTO active_alarms (occurrence_id, alarm_id, alarm_state, pv_value, activated_at)
			VALUES (%d, %d, 'UNACK', 11, now())`, id, alarmID)
	}

	t.Run("every state change needs its history row in the same transaction", func(t *testing.T) {
		occ = next()
		mustFail(t, ctx, s, "has no alarm_history row", "activation without history", activate(occ))
		mustFail(t, ctx, s, "must start as UNACK", "an occurrence that starts as ACKED",
			fmt.Sprintf(`INSERT INTO active_alarms (occurrence_id, alarm_id, alarm_state, activated_at, acked_at) VALUES (%d,%d,'ACKED',now(),now())`, next(), alarmID))
		mustWork(t, ctx, s, "activation with history", activate(occ), hist(occ, alarmID, "ACTIVATED", "", "UNACK"))

		mustFail(t, ctx, s, "has no alarm_history row", "UNACK -> ACKED without history",
			fmt.Sprintf(`UPDATE active_alarms SET alarm_state='ACKED', acked_at=now() WHERE occurrence_id=%d`, occ))
		mustFail(t, ctx, s, "has no alarm_history row", "UNACK -> ACKED with a history row of another transition",
			fmt.Sprintf(`UPDATE active_alarms SET alarm_state='ACKED', acked_at=now() WHERE occurrence_id=%d`, occ),
			hist(occ, alarmID, "RETURNED_TO_NORMAL", "UNACK", "RTNUN"))
		var state string
		s.pool.QueryRow(ctx, `SELECT alarm_state FROM active_alarms WHERE occurrence_id=$1`, occ).Scan(&state)
		if state != "UNACK" {
			t.Fatalf("a refused change must leave the state alone, got %s", state)
		}
	})

	t.Run("only the transitions of the model are possible", func(t *testing.T) {
		set := func(sql string) string { return fmt.Sprintf(sql, occ) }
		mustFail(t, ctx, s, "", "history row for an event that does not exist in the model",
			hist(occ, alarmID, "ACKNOWLEDGED", "ACKED", "UNACK"))
		mustFail(t, ctx, s, "alarm_history_transition_check", "ACKNOWLEDGED UNACK -> RTNUN",
			hist(occ, alarmID, "ACKNOWLEDGED", "UNACK", "RTNUN"))
		mustFail(t, ctx, s, "cannot be removed", "UNACK straight to NORM (delete)",
			set(`DELETE FROM active_alarms WHERE occurrence_id=%d`))
		mustFail(t, ctx, s, "cannot be changed", "rewriting the alarm of an occurrence",
			set(`UPDATE active_alarms SET alarm_id = alarm_id + 1000 WHERE occurrence_id=%d`))
		mustFail(t, ctx, s, "active_alarm_fields_match_state", "ACKED without an acknowledgement time",
			set(`UPDATE active_alarms SET alarm_state='ACKED' WHERE occurrence_id=%d`),
			hist(occ, alarmID, "ACKNOWLEDGED", "UNACK", "ACKED"))

		// the normal path: UNACK -> ACKED
		mustWork(t, ctx, s, "acknowledge", set(`UPDATE active_alarms SET alarm_state='ACKED', acked_at=now(), acked_by_name='t' WHERE occurrence_id=%d`),
			hist(occ, alarmID, "ACKNOWLEDGED", "UNACK", "ACKED"))
		mustFail(t, ctx, s, "is not allowed", "ACKED -> UNACK by a plain UPDATE",
			set(`UPDATE active_alarms SET alarm_state='UNACK', acked_at=NULL WHERE occurrence_id=%d`),
			hist(occ, alarmID, "UNSHELVED", "SHLVD", "UNACK"))
		mustFail(t, ctx, s, "is not allowed", "ACKED -> RTNUN",
			set(`UPDATE active_alarms SET alarm_state='RTNUN', process_condition_active=false, returned_to_normal_at=now(), acked_at=NULL WHERE occurrence_id=%d`),
			hist(occ, alarmID, "RETURNED_TO_NORMAL", "UNACK", "RTNUN"))

		// ACKED -> NORM when the process returns to normal
		mustWork(t, ctx, s, "ACKED -> NORM", set(`DELETE FROM active_alarms WHERE occurrence_id=%d`),
			hist(occ, alarmID, "RETURNED_TO_NORMAL", "ACKED", "NORM"))
		var n int
		s.pool.QueryRow(ctx, `SELECT count(*) FROM active_alarms WHERE alarm_id=$1`, alarmID).Scan(&n)
		if n != 0 {
			t.Fatalf("a completed occurrence must leave active_alarms, %d rows left", n)
		}
		var events int
		s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_history WHERE occurrence_id=$1`, occ).Scan(&events)
		if events != 3 {
			t.Errorf("the timeline of the occurrence has %d events, want 3 (ACTIVATED, ACKNOWLEDGED, RETURNED_TO_NORMAL)", events)
		}
	})

	t.Run("UNACK -> RTNUN -> NORM on acknowledgement, and a re-activation is a NEW occurrence", func(t *testing.T) {
		o1 := next()
		mustWork(t, ctx, s, "activate", activate(o1), hist(o1, alarmID, "ACTIVATED", "", "UNACK"))
		mustWork(t, ctx, s, "UNACK -> RTNUN", fmt.Sprintf(`UPDATE active_alarms SET alarm_state='RTNUN', process_condition_active=false, returned_to_normal_at=now() WHERE occurrence_id=%d`, o1),
			hist(o1, alarmID, "RETURNED_TO_NORMAL", "UNACK", "RTNUN"))

		// the alarm activates again while the first occurrence still waits for its acknowledgement
		o2 := next()
		mustWork(t, ctx, s, "second activation next to the RTNUN one", activate(o2), hist(o2, alarmID, "ACTIVATED", "", "UNACK"))
		o3 := next()
		mustFail(t, ctx, s, "active_alarms_one_abnormal_idx", "a duplicate activation while one is already abnormal",
			activate(o3), hist(o3, alarmID, "ACTIVATED", "", "UNACK"))

		mustWork(t, ctx, s, "RTNUN -> NORM on acknowledgement", fmt.Sprintf(`DELETE FROM active_alarms WHERE occurrence_id=%d`, o1),
			hist(o1, alarmID, "ACKNOWLEDGED", "RTNUN", "NORM"))
		mustFail(t, ctx, s, "", "the second occurrence has its own timeline; o1's history cannot close it",
			fmt.Sprintf(`DELETE FROM active_alarms WHERE occurrence_id=%d`, o2), hist(o1, alarmID, "ACKNOWLEDGED", "RTNUN", "NORM"))
		// close o2 to leave a clean slate
		mustWork(t, ctx, s, "o2 ack", fmt.Sprintf(`UPDATE active_alarms SET alarm_state='ACKED', acked_at=now() WHERE occurrence_id=%d`, o2), hist(o2, alarmID, "ACKNOWLEDGED", "UNACK", "ACKED"))
		mustWork(t, ctx, s, "o2 clear", fmt.Sprintf(`DELETE FROM active_alarms WHERE occurrence_id=%d`, o2), hist(o2, alarmID, "RETURNED_TO_NORMAL", "ACKED", "NORM"))
	})

	t.Run("shelving needs a reason and an expiry; the process condition stays recorded", func(t *testing.T) {
		o := next()
		mustWork(t, ctx, s, "activate", activate(o), hist(o, alarmID, "ACTIVATED", "", "UNACK"))
		shelve := func(extra string) string {
			return fmt.Sprintf(`UPDATE active_alarms SET alarm_state='SHLVD', shelved_at=now() %s WHERE occurrence_id=%d`, extra, o)
		}
		h := hist(o, alarmID, "SHELVED", "UNACK", "SHLVD")
		mustFail(t, ctx, s, "active_alarm_fields_match_state", "shelving without a reason", shelve(`, shelved_expiry_time = now() + interval '10 minutes'`), h)
		mustFail(t, ctx, s, "active_alarm_fields_match_state", "shelving without an expiry", shelve(`, shelve_reason = 'maintenance'`), h)
		mustFail(t, ctx, s, "active_alarm_fields_match_state", "an expiry in the past", shelve(`, shelve_reason='x', shelved_expiry_time = now() - interval '1 minute'`), h)
		mustWork(t, ctx, s, "shelve", shelve(`, shelved_by_name='sup', shelve_reason='planned maintenance', shelved_expiry_time = now() + interval '10 minutes'`), h)

		var active bool
		s.pool.QueryRow(ctx, `SELECT process_condition_active FROM active_alarms WHERE occurrence_id=$1`, o).Scan(&active)
		if !active {
			t.Error("shelving must not erase the underlying process condition")
		}
		// condition clears while shelved: informational history, the alarm stays SHLVD
		mustWork(t, ctx, s, "condition clears while shelved",
			fmt.Sprintf(`UPDATE active_alarms SET process_condition_active=false, returned_to_normal_at=now() WHERE occurrence_id=%d`, o),
			hist(o, alarmID, "RETURNED_TO_NORMAL", "SHLVD", ""))
		mustFail(t, ctx, s, "", "completing a shelved alarm directly", fmt.Sprintf(`DELETE FROM active_alarms WHERE occurrence_id=%d`, o))
		// unshelve: the process is normal -> RTNUN
		mustWork(t, ctx, s, "unshelve", fmt.Sprintf(`UPDATE active_alarms SET alarm_state='RTNUN', shelved_at=NULL, shelved_expiry_time=NULL, shelved_by=NULL, shelved_by_name='', shelve_reason='' WHERE occurrence_id=%d`, o),
			hist(o, alarmID, "UNSHELVED", "SHLVD", "RTNUN"))
		mustWork(t, ctx, s, "ack the RTNUN", fmt.Sprintf(`DELETE FROM active_alarms WHERE occurrence_id=%d`, o), hist(o, alarmID, "ACKNOWLEDGED", "RTNUN", "NORM"))
		var shelvedEvents int
		s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_history WHERE occurrence_id=$1 AND event_type IN ('SHELVED','UNSHELVED')`, o).Scan(&shelvedEvents)
		if shelvedEvents != 2 {
			t.Errorf("shelving history events = %d, want 2", shelvedEvents)
		}
	})

	t.Run("history and audit log are append-only", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE alarm_history SET details = 'edited'`,
			`DELETE FROM alarm_history`,
			`TRUNCATE alarm_history`,
		} {
			mustFail(t, ctx, s, "append-only", stmt, stmt)
		}
		mustWork(t, ctx, s, "audit insert", `INSERT INTO alarm_audit_log (action, outcome) VALUES ('ALARM_UPDATE', 'success')`)
		for _, stmt := range []string{`UPDATE alarm_audit_log SET reason = 'x'`, `DELETE FROM alarm_audit_log`, `TRUNCATE alarm_audit_log`} {
			mustFail(t, ctx, s, "append-only", stmt, stmt)
		}
	})

	_ = pgx.ErrNoRows
}

func extraCols(extra string) string {
	if extra == "" {
		return ""
	}
	return ", " + strings.Split(extra, "=")[0]
}

func extraVals(extra string) string {
	if extra == "" {
		return ""
	}
	return ", " + strings.SplitN(extra, "=", 2)[1]
}
