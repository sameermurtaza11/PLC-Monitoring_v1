package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Alarm states held in active_alarms. NORM is not stored: a completed
// occurrence is removed and only its history remains.
const (
	StateUnack = "UNACK"
	StateAcked = "ACKED"
	StateRTNUN = "RTNUN"
	StateShlvd = "SHLVD"
	StateNorm  = "NORM"
)

var (
	ErrOccurrenceGone  = errors.New("the alarm occurrence no longer exists")
	ErrWrongAlarmState = errors.New("the alarm is not in a state that allows this action")
)

// Occurrence is one row of active_alarms: an outstanding alarm.
type Occurrence struct {
	ID                     int64
	AlarmID                int64
	State                  string
	PVValue                *float64
	ActivatedAt            time.Time
	AckedAt                *time.Time
	AckedByName            string
	ReturnedToNormal       *time.Time
	ShelvedAt              *time.Time
	ShelvedByName          string
	ShelvedExpiry          *time.Time
	ShelveReason           string
	ProcessConditionActive bool
}

const occurrenceSelect = `
	SELECT occurrence_id, alarm_id, alarm_state, pv_value, activated_at, acked_at, acked_by_name,
	       returned_to_normal_at, shelved_at, shelved_by_name, shelved_expiry_time, shelve_reason,
	       process_condition_active
	FROM active_alarms`

func scanOccurrence(r pgx.Row, o *Occurrence) error {
	return r.Scan(&o.ID, &o.AlarmID, &o.State, &o.PVValue, &o.ActivatedAt, &o.AckedAt, &o.AckedByName,
		&o.ReturnedToNormal, &o.ShelvedAt, &o.ShelvedByName, &o.ShelvedExpiry, &o.ShelveReason,
		&o.ProcessConditionActive)
}

// ActiveOccurrences returns every outstanding alarm.
func (s *Store) ActiveOccurrences(ctx context.Context) ([]Occurrence, error) {
	rows, err := s.pool.Query(ctx, occurrenceSelect+` ORDER BY activated_at, occurrence_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Occurrence, error) {
		var o Occurrence
		return o, scanOccurrence(r, &o)
	})
}

// historyRow is one alarm_history insert.
type historyRow struct {
	occ              int64
	alarm            AlarmDef
	event            string
	prev, next       *string
	value            *float64
	ts               time.Time
	userID           *int64
	userName, reason string
	details          string
}

func str(s string) *string { return &s }

func insertHistory(ctx context.Context, tx pgx.Tx, h historyRow) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO alarm_history (occurrence_id, alarm_id, event_type, previous_state, new_state, pv_value,
		                           event_timestamp, user_id, user_name, details, reason, priority, setpoint)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		h.occ, h.alarm.ID, h.event, h.prev, h.next, h.value, h.ts, h.userID, h.userName, h.details, h.reason,
		h.alarm.Priority, h.alarm.Setpoint)
	return err
}

func actorID(a Actor) *int64 {
	if a.ID == 0 {
		return nil
	}
	return &a.ID
}

// lockOccurrence reads an occurrence and its alarm definition, locking the row.
func lockOccurrence(ctx context.Context, tx pgx.Tx, id int64) (Occurrence, AlarmDef, error) {
	var o Occurrence
	err := scanOccurrence(tx.QueryRow(ctx, occurrenceSelect+` WHERE occurrence_id = $1 FOR UPDATE`, id), &o)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, AlarmDef{}, ErrOccurrenceGone
	}
	if err != nil {
		return o, AlarmDef{}, err
	}
	var a AlarmDef
	if err := scanAlarmDef(tx.QueryRow(ctx, alarmDefSelect+` WHERE m.alarm_id = $1`, o.AlarmID), &a); err != nil {
		return o, a, err
	}
	return o, a, nil
}

// ActivateAlarm records that the alarm's condition has held for its on-delay.
// A new UNACK occurrence is created, or — if an older occurrence of the same
// alarm is shelved and had returned to normal — that shelved occurrence becomes
// abnormal again (it stays shelved). The occurrence and its history row are
// written in one transaction. Returns the occurrence id.
func (s *Store) ActivateAlarm(ctx context.Context, a AlarmDef, value float64, ts time.Time) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var shelved int64
		err := tx.QueryRow(ctx, `
			SELECT occurrence_id FROM active_alarms
			WHERE alarm_id = $1 AND alarm_state = 'SHLVD' AND NOT process_condition_active
			FOR UPDATE`, a.ID).Scan(&shelved)
		switch {
		case err == nil:
			if _, err := tx.Exec(ctx, `
				UPDATE active_alarms SET process_condition_active = TRUE, returned_to_normal_at = NULL, updated_at = now()
				WHERE occurrence_id = $1`, shelved); err != nil {
				return err
			}
			id = shelved
			return insertHistory(ctx, tx, historyRow{occ: id, alarm: a, event: "ACTIVATED", prev: str(StateShlvd),
				value: &value, ts: ts, details: "condition became abnormal again while shelved"})
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO active_alarms (alarm_id, alarm_state, pv_value, activated_at)
			VALUES ($1, 'UNACK', $2, $3) RETURNING occurrence_id`, a.ID, value, ts).Scan(&id); err != nil {
			return err
		}
		return insertHistory(ctx, tx, historyRow{occ: id, alarm: a, event: "ACTIVATED", next: str(StateUnack),
			value: &value, ts: ts, details: fmt.Sprintf("value %g, %s", value, a.Trigger())})
	})
	return id, asConstraintError(err)
}

// ReturnToNormal records that the process condition stayed normal for the
// off-delay. UNACK → RTNUN; ACKED → NORM (the occurrence completes); SHLVD stays
// shelved with the condition marked normal.
func (s *Store) ReturnToNormal(ctx context.Context, occID int64, value float64, ts time.Time) error {
	return asConstraintError(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return returnToNormalTx(ctx, tx, occID, &value, ts, fmt.Sprintf("value %g", value), Actor{})
	}))
}

// returnToNormalTx is the transition behind ReturnToNormal. `by` is empty for
// the automatic return and names the user when a configuration change ends the
// condition (alarm taken out of service).
func returnToNormalTx(ctx context.Context, tx pgx.Tx, occID int64, value *float64, ts time.Time, details string, by Actor) error {
	o, a, err := lockOccurrence(ctx, tx, occID)
	if err != nil {
		return err
	}
	h := historyRow{occ: occID, alarm: a, event: "RETURNED_TO_NORMAL", prev: &o.State, value: value, ts: ts,
		details: details, userID: actorID(by), userName: by.Username}
	switch o.State {
	case StateUnack:
		h.next = str(StateRTNUN)
		if err := insertHistory(ctx, tx, h); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE active_alarms SET alarm_state = 'RTNUN', process_condition_active = FALSE,
				returned_to_normal_at = $2, updated_at = now() WHERE occurrence_id = $1`, occID, ts)
		return err
	case StateAcked:
		h.next = str(StateNorm)
		if err := insertHistory(ctx, tx, h); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM active_alarms WHERE occurrence_id = $1`, occID)
		return err
	case StateShlvd:
		if !o.ProcessConditionActive {
			return nil
		}
		if err := insertHistory(ctx, tx, h); err != nil { // new_state NULL: informational
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE active_alarms SET process_condition_active = FALSE, returned_to_normal_at = $2, updated_at = now()
			WHERE occurrence_id = $1`, occID, ts)
		return err
	}
	return nil // RTNUN: already returned
}

// takeOutOfServiceTx ends every abnormal occurrence of an alarm that is no
// longer in service (disabled, its PV removed or its approval withdrawn): the
// condition is recorded as returned to normal, with the reason in the history.
// Unacknowledged occurrences stay on the list as RTNUN until someone
// acknowledges them, so nothing disappears unseen. Returns how many were ended.
func takeOutOfServiceTx(ctx context.Context, tx pgx.Tx, alarmID int64, reason string, by Actor) (int, error) {
	rows, err := tx.Query(ctx, `SELECT occurrence_id FROM active_alarms WHERE alarm_id = $1 AND process_condition_active FOR UPDATE`, alarmID)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := returnToNormalTx(ctx, tx, id, nil, time.Now(), "alarm taken out of service: "+reason, by); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// TakeOutOfService is takeOutOfServiceTx in its own transaction; the alarm
// engine calls it for an alarm that stopped being evaluable while it had an
// abnormal occurrence.
func (s *Store) TakeOutOfService(ctx context.Context, alarmID int64, reason string) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var e error
		n, e = takeOutOfServiceTx(ctx, tx, alarmID, reason, Actor{})
		return e
	})
	return n, asConstraintError(err)
}

// Acknowledge: UNACK → ACKED, RTNUN → NORM. Any role may do it.
// valueNow is the process value at the time of the action (nil if unknown):
// it is what the history records, not the value at activation.
func (s *Store) Acknowledge(ctx context.Context, occID int64, actor Actor, valueNow *float64) error {
	return asConstraintError(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		o, a, err := lockOccurrence(ctx, tx, occID)
		if err != nil {
			return err
		}
		now := time.Now()
		h := historyRow{occ: occID, alarm: a, event: "ACKNOWLEDGED", prev: &o.State, ts: now,
			userID: actorID(actor), userName: actor.Username}
		switch o.State {
		case StateUnack:
			h.next, h.value = str(StateAcked), valueNow
			if err := insertHistory(ctx, tx, h); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				UPDATE active_alarms SET alarm_state = 'ACKED', acked_at = $2, acked_by = $3, acked_by_name = $4, updated_at = now()
				WHERE occurrence_id = $1`, occID, now, actorID(actor), actor.Username)
			return err
		case StateRTNUN:
			h.next, h.value = str(StateNorm), valueNow
			if err := insertHistory(ctx, tx, h); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM active_alarms WHERE occurrence_id = $1`, occID)
			return err
		}
		return fmt.Errorf("%w: %s", ErrWrongAlarmState, o.State)
	}))
}

// Shelve suppresses an existing occurrence until `expiry`. The caller (the web
// layer) enforces who may shelve and the maximum duration.
func (s *Store) Shelve(ctx context.Context, occID int64, expiry time.Time, reason string, actor Actor, valueNow *float64) error {
	return asConstraintError(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		o, a, err := lockOccurrence(ctx, tx, occID)
		if err != nil {
			return err
		}
		if o.State == StateShlvd {
			return fmt.Errorf("%w: already shelved", ErrWrongAlarmState)
		}
		now := time.Now()
		if err := insertHistory(ctx, tx, historyRow{occ: occID, alarm: a, event: "SHELVED", prev: &o.State, next: str(StateShlvd),
			value: valueNow, ts: now, userID: actorID(actor), userName: actor.Username, reason: reason,
			details: "until " + expiry.Local().Format("2006-01-02 15:04:05")}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE active_alarms SET alarm_state = 'SHLVD', shelved_at = $2, shelved_by = $3, shelved_by_name = $4,
				shelved_expiry_time = $5, shelve_reason = $6, updated_at = now()
			WHERE occurrence_id = $1`, occID, now, actorID(actor), actor.Username, expiry, reason)
		return err
	}))
}

// Unshelve ends a shelf (by a user, or automatically when actor.ID == 0 and
// actor.Username == ""). The alarm becomes UNACK if the condition is still
// abnormal, else RTNUN — it must be acknowledged again either way.
func (s *Store) Unshelve(ctx context.Context, occID int64, actor Actor, valueNow *float64) error {
	return asConstraintError(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return unshelve(ctx, tx, occID, actor, "", valueNow)
	}))
}

func unshelve(ctx context.Context, tx pgx.Tx, occID int64, actor Actor, reason string, valueNow *float64) error {
	o, a, err := lockOccurrence(ctx, tx, occID)
	if err != nil {
		return err
	}
	if o.State != StateShlvd {
		return fmt.Errorf("%w: %s", ErrWrongAlarmState, o.State)
	}
	next := StateRTNUN
	if o.ProcessConditionActive {
		next = StateUnack
	}
	if err := insertHistory(ctx, tx, historyRow{occ: occID, alarm: a, event: "UNSHELVED", prev: str(StateShlvd), next: &next,
		value: valueNow, ts: time.Now(), userID: actorID(actor), userName: actor.Username, reason: reason}); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE active_alarms SET alarm_state = $2, acked_at = NULL, acked_by = NULL, acked_by_name = '',
			shelved_at = NULL, shelved_by = NULL, shelved_by_name = '', shelved_expiry_time = NULL, shelve_reason = '',
			updated_at = now()
		WHERE occurrence_id = $1`, occID, next)
	return err
}

// ExpireShelves unshelves every occurrence whose shelf time is over and returns
// how many it handled.
func (s *Store) ExpireShelves(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT occurrence_id FROM active_alarms WHERE alarm_state = 'SHLVD' AND shelved_expiry_time <= $1`, now)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			return unshelve(ctx, tx, id, Actor{}, "shelf time ended", nil)
		})
		if err != nil && !errors.Is(err, ErrOccurrenceGone) && !errors.Is(err, ErrWrongAlarmState) {
			return n, asConstraintError(err)
		}
		n++
	}
	return n, nil
}

// EngineLock keeps a session-level advisory lock so that only one process
// runs the alarm engine against a database.
type EngineLock struct {
	conn *pgxpool.Conn
}

const engineLockKey = 7_100_001 // "alarm engine"

// TryEngineLock returns nil (and no error) if another process holds the lock.
func (s *Store) TryEngineLock(ctx context.Context) (*EngineLock, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, engineLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, err
	}
	if !ok {
		conn.Release()
		return nil, nil
	}
	return &EngineLock{conn: conn}, nil
}

// Held checks that the lock's connection is still alive (a lost connection
// means PostgreSQL released the lock).
func (l *EngineLock) Held(ctx context.Context) bool {
	var one int
	return l.conn.QueryRow(ctx, `SELECT 1`).Scan(&one) == nil
}

// Release drops the lock and returns the connection to the pool.
func (l *EngineLock) Release() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, engineLockKey); err != nil {
		l.conn.Conn().Close(ctx) // cannot unlock: drop the connection so the lock goes with it
	}
	l.conn.Release()
}
