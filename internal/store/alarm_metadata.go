package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Alarm types, priorities, classes and rationalization states.
const (
	AlarmHI       = "HI"
	AlarmHIHI     = "HIHI"
	AlarmLO       = "LO"
	AlarmLOLO     = "LOLO"
	AlarmDiscrete = "DISCRETE"

	PriorityMinor    = "Minor"
	PriorityMajor    = "Major"
	PriorityCritical = "Critical"

	RationDraft    = "DRAFT"
	RationApproved = "APPROVED"
)

var (
	AlarmTypes      = []string{AlarmHIHI, AlarmHI, AlarmLO, AlarmLOLO, AlarmDiscrete}
	AlarmPriorities = []string{PriorityMinor, PriorityMajor, PriorityCritical}
	AlarmClasses    = []string{"PROCESS", "EQUIPMENT_PROTECTION", "ENVIRONMENTAL", "SAFETY_CRITICAL"}
)

var (
	ErrAlarmExists     = errors.New("an alarm of this type already exists for this PV")
	ErrAlarmNotFound   = errors.New("alarm not found")
	ErrNotRationalized = errors.New("rationalization is incomplete")
)

// Actor is the logged-in user performing an action. It always comes from the
// server-side session, never from the request.
type Actor struct {
	ID       int64
	Username string
	IP       string
}

// AlarmDef is one alarm definition together with the PV it belongs to.
type AlarmDef struct {
	ID int64

	PVID       int
	PLCName    string
	PVName     string
	PVUnit     string
	PVMin      float64
	PVMax      float64
	Digital    bool
	State0Text string
	State1Text string
	PVArchived bool // the PV was deleted; its alarm records are kept for the history

	AlarmType    string
	Setpoint     *float64
	TriggerState *int
	SetpointUnit string
	Deadband     float64
	OnDelayS     int
	OffDelayS    int

	Priority               string
	AlarmClass             string
	Message                string
	OperatorAction         string
	ConsequencesOfInaction string
	ProbableCause          string
	MaxResponseTimeS       *int
	Rationale              string
	RationalizationStatus  string
	ApprovedByName         string
	ApprovedAt             *time.Time

	Enabled bool

	CreatedAt     time.Time
	CreatedByName string
	UpdatedAt     time.Time
	UpdatedByName string
}

// AlarmInput is what a user may edit. The PV and the alarm type are fixed
// when the alarm is created.
type AlarmInput struct {
	PVID      int    // new alarm only
	AlarmType string // new alarm only

	Setpoint     *float64
	TriggerState *int
	// Unit is the unit the user was shown next to the setpoint. It is not
	// stored: the unit saved with the alarm is always the PV's own, and a
	// different Unit here is rejected.
	Unit      string
	Deadband  float64
	OnDelayS  int
	OffDelayS int

	Priority               string
	AlarmClass             string
	Message                string
	OperatorAction         string
	ConsequencesOfInaction string
	ProbableCause          string
	MaxResponseTimeS       *int
	Rationale              string
	Enabled                bool
}

// Issues lists why a saved definition cannot be used as it stands (the PV's
// unit or range changed after the alarm was approved). The alarm engine does
// not evaluate an alarm that has issues.
func (a AlarmDef) Issues() []string {
	var out []string
	if a.SetpointUnit != a.PVUnit {
		out = append(out, fmt.Sprintf("unit changed: alarm was set in %q, the PV is now %q", a.SetpointUnit, a.PVUnit))
	}
	if !a.Digital && a.Setpoint != nil && (*a.Setpoint < a.PVMin || *a.Setpoint > a.PVMax) {
		out = append(out, fmt.Sprintf("setpoint %g is outside the PV range %g … %g", *a.Setpoint, a.PVMin, a.PVMax))
	}
	if a.Digital != (a.AlarmType == AlarmDiscrete) {
		out = append(out, "alarm type does not fit the PV type (digital / analog)")
	}
	return out
}

// Evaluable: approved, enabled and without issues — the alarms the engine uses.
func (a AlarmDef) Evaluable() bool {
	return a.Enabled && a.RationalizationStatus == RationApproved && len(a.Issues()) == 0
}

// Trigger is the condition as an operator reads it, e.g. "≥ 80 PSI".
func (a AlarmDef) Trigger() string {
	switch {
	case a.AlarmType == AlarmDiscrete && a.TriggerState != nil:
		if *a.TriggerState == 1 {
			return "when " + a.State1Text
		}
		return "when " + a.State0Text
	case a.Setpoint == nil:
		return "—"
	case a.AlarmType == AlarmHI || a.AlarmType == AlarmHIHI:
		return fmt.Sprintf("≥ %g %s", *a.Setpoint, a.SetpointUnit)
	default:
		return fmt.Sprintf("≤ %g %s", *a.Setpoint, a.SetpointUnit)
	}
}

// ReapprovalNeeded: changing what an approved alarm DOES (limit, delays,
// deadband, priority, class, the operator's instructions or the rationale)
// puts it back to DRAFT until an administrator approves it again.
// Enabling/disabling and wording-only fields do not.
func ReapprovalNeeded(old AlarmDef, in AlarmInput) bool {
	return !floatPtrEq(old.Setpoint, in.Setpoint) || !intPtrEq(old.TriggerState, in.TriggerState) ||
		old.Deadband != in.Deadband || old.OnDelayS != in.OnDelayS || old.OffDelayS != in.OffDelayS ||
		old.Priority != in.Priority || old.AlarmClass != in.AlarmClass ||
		old.OperatorAction != in.OperatorAction || old.ConsequencesOfInaction != in.ConsequencesOfInaction ||
		old.Rationale != in.Rationale
}

// unchanged reports whether saving `in` would change nothing about the alarm.
func unchanged(old AlarmDef, in AlarmInput, unit string) bool {
	return !ReapprovalNeeded(old, in) && old.SetpointUnit == unit &&
		old.Message == in.Message && old.ProbableCause == in.ProbableCause &&
		intPtrEq(old.MaxResponseTimeS, in.MaxResponseTimeS) && old.Enabled == in.Enabled
}

func floatPtrEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func intPtrEq(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// RationalizationGaps lists what an alarm still lacks before it can be approved.
func RationalizationGaps(a AlarmDef) []string {
	var gaps []string
	if strings.TrimSpace(a.Rationale) == "" {
		gaps = append(gaps, "rationale (why this priority and class)")
	}
	if strings.TrimSpace(a.OperatorAction) == "" {
		gaps = append(gaps, "operator action")
	}
	if strings.TrimSpace(a.ConsequencesOfInaction) == "" {
		gaps = append(gaps, "consequences of inaction")
	}
	return gaps
}

const alarmDefSelect = `
	SELECT m.alarm_id, m.pv_id, plc.plc_name, pv.pv_name, pv.unit, pv.pv_min, pv.pv_max,
	       pv.register_address BETWEEN 10001 AND 19999, pv.state0_text, pv.state1_text,
	       m.alarm_type, m.setpoint, m.trigger_state, m.setpoint_unit, m.deadband, m.on_delay_s, m.off_delay_s,
	       m.priority, m.alarm_class, m.alarm_message, m.operator_action, m.consequences_of_inaction,
	       m.probable_cause, m.max_response_time_s, m.rationale, m.rationalization_status,
	       m.approved_by_name, m.approved_at, m.is_enabled,
	       m.created_at, m.created_by_name, m.updated_at, m.updated_by_name, pv.deleted_at IS NOT NULL
	FROM alarm_metadata m
	JOIN pv_metadata pv   ON pv.pv_id = m.pv_id
	JOIN plc_metadata plc ON plc.plc_id = pv.plc_id`

func scanAlarmDef(r pgx.Row, a *AlarmDef) error {
	return r.Scan(&a.ID, &a.PVID, &a.PLCName, &a.PVName, &a.PVUnit, &a.PVMin, &a.PVMax,
		&a.Digital, &a.State0Text, &a.State1Text,
		&a.AlarmType, &a.Setpoint, &a.TriggerState, &a.SetpointUnit, &a.Deadband, &a.OnDelayS, &a.OffDelayS,
		&a.Priority, &a.AlarmClass, &a.Message, &a.OperatorAction, &a.ConsequencesOfInaction,
		&a.ProbableCause, &a.MaxResponseTimeS, &a.Rationale, &a.RationalizationStatus,
		&a.ApprovedByName, &a.ApprovedAt, &a.Enabled,
		&a.CreatedAt, &a.CreatedByName, &a.UpdatedAt, &a.UpdatedByName, &a.PVArchived)
}

// ListAlarmDefs returns every alarm definition.
func (s *Store) ListAlarmDefs(ctx context.Context) ([]AlarmDef, error) {
	rows, err := s.pool.Query(ctx, alarmDefSelect+`
		ORDER BY plc.plc_name, pv.register_address,
		         array_position(ARRAY['HIHI','HI','LO','LOLO','DISCRETE'], m.alarm_type)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmDef, error) {
		var a AlarmDef
		return a, scanAlarmDef(r, &a)
	})
}

// GetAlarmDef returns one definition. ErrAlarmNotFound if it does not exist.
func (s *Store) GetAlarmDef(ctx context.Context, id int64) (AlarmDef, error) {
	var a AlarmDef
	err := scanAlarmDef(s.pool.QueryRow(ctx, alarmDefSelect+` WHERE m.alarm_id = $1`, id), &a)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrAlarmNotFound
	}
	return a, err
}

// SiblingSetpoints returns the limits of the other analog alarms of a PV, by
// type, to check HIHI > HI > LO > LOLO.
func (s *Store) SiblingSetpoints(ctx context.Context, pvID int, exceptAlarmID int64) (map[string]float64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT alarm_type, setpoint FROM alarm_metadata
		WHERE pv_id = $1 AND alarm_id <> $2 AND setpoint IS NOT NULL`, pvID, exceptAlarmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var t string
		var sp float64
		if err := rows.Scan(&t, &sp); err != nil {
			return nil, err
		}
		out[t] = sp
	}
	return out, rows.Err()
}

// PVForAlarm returns the PV an alarm would be attached to (range, unit, type).
func (s *Store) PVForAlarm(ctx context.Context, pvID int) (AlarmPV, error) {
	var p AlarmPV
	err := s.pool.QueryRow(ctx, `
		SELECT pv.pv_id, plc.plc_name, pv.pv_name, pv.unit, pv.pv_min, pv.pv_max,
		       pv.register_address BETWEEN 10001 AND 19999
		FROM pv_metadata pv JOIN plc_metadata plc ON plc.plc_id = pv.plc_id
		WHERE pv.pv_id = $1 AND pv.deleted_at IS NULL`, pvID).
		Scan(&p.ID, &p.PLCName, &p.Name, &p.Unit, &p.Min, &p.Max, &p.Digital)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, pgx.ErrNoRows
	}
	return p, err
}

// AlarmPV is one PV an alarm can be attached to.
type AlarmPV struct {
	ID      int
	PLCName string
	Name    string
	Unit    string
	Min     float64
	Max     float64
	Digital bool
}

// ListAlarmPVs returns the PVs for the "new alarm" form.
func (s *Store) ListAlarmPVs(ctx context.Context) ([]AlarmPV, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pv.pv_id, plc.plc_name, pv.pv_name, pv.unit, pv.pv_min, pv.pv_max,
		       pv.register_address BETWEEN 10001 AND 19999
		FROM pv_metadata pv JOIN plc_metadata plc ON plc.plc_id = pv.plc_id
		WHERE pv.deleted_at IS NULL
		ORDER BY plc.plc_name, pv.register_address`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmPV, error) {
		var p AlarmPV
		return p, r.Scan(&p.ID, &p.PLCName, &p.Name, &p.Unit, &p.Min, &p.Max, &p.Digital)
	})
}

// SaveAlarmDef creates (id == 0) or updates an alarm definition and writes
// the change to alarm_audit_log in the same transaction. `unit` is the PV's
// unit, stored with the setpoint. It reports whether an approved alarm was put
// back to DRAFT by this change.
func (s *Store) SaveAlarmDef(ctx context.Context, id int64, in AlarmInput, unit, changeReason string, actor Actor) (savedID int64, resetToDraft bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var e error
		savedID, _, resetToDraft, e = saveAlarmDefTx(ctx, tx, id, in, unit, changeReason, actor)
		return e
	})
	return savedID, resetToDraft, asConstraintError(err)
}

// What saveAlarmDefTx did.
const (
	alarmCreated   = "created"
	alarmUpdated   = "updated"
	alarmUnchanged = "unchanged"
)

// saveAlarmDefTx creates (id == 0) or updates a definition inside the caller's
// transaction, together with its audit line.
func saveAlarmDefTx(ctx context.Context, tx pgx.Tx, id int64, in AlarmInput, unit, changeReason string, actor Actor) (savedID int64, outcome string, resetToDraft bool, err error) {
	if id == 0 {
		if e := tx.QueryRow(ctx, `
			INSERT INTO alarm_metadata
				(pv_id, alarm_type, setpoint, trigger_state, setpoint_unit, deadband, on_delay_s, off_delay_s,
				 priority, alarm_class, alarm_message, operator_action, consequences_of_inaction, probable_cause,
				 max_response_time_s, rationale, is_enabled, created_by, created_by_name, updated_by, updated_by_name)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$18,$19)
			RETURNING alarm_id`,
			in.PVID, in.AlarmType, in.Setpoint, in.TriggerState, unit, in.Deadband, in.OnDelayS, in.OffDelayS,
			in.Priority, in.AlarmClass, in.Message, in.OperatorAction, in.ConsequencesOfInaction, in.ProbableCause,
			in.MaxResponseTimeS, in.Rationale, in.Enabled, nullableID(actor.ID), actor.Username).Scan(&savedID); e != nil {
			if isConstraintName(e, "alarm_unique_pv_type") {
				return 0, "", false, ErrAlarmExists
			}
			return 0, "", false, e
		}
		after, _ := alarmJSON(ctx, tx, savedID)
		return savedID, alarmCreated, false, insertAlarmAudit(ctx, tx, AlarmAudit{Actor: actor, Action: "ALARM_CREATE", AlarmID: savedID,
			Outcome: "success", Reason: changeReason, After: after})
	}

	var old AlarmDef
	if e := scanAlarmDef(tx.QueryRow(ctx, alarmDefSelect+` WHERE m.alarm_id = $1 FOR UPDATE OF m`, id), &old); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return 0, "", false, ErrAlarmNotFound
		}
		return 0, "", false, e
	}
	if unchanged(old, in, unit) {
		return id, alarmUnchanged, false, nil // nothing to store, nothing to audit; updated_at stays truthful
	}
	before, _ := alarmJSON(ctx, tx, id)
	resetToDraft = old.RationalizationStatus == RationApproved && ReapprovalNeeded(old, in)

	if _, e := tx.Exec(ctx, `
		UPDATE alarm_metadata SET
			setpoint = $2, trigger_state = $3, setpoint_unit = $4, deadband = $5, on_delay_s = $6, off_delay_s = $7,
			priority = $8, alarm_class = $9, alarm_message = $10, operator_action = $11,
			consequences_of_inaction = $12, probable_cause = $13, max_response_time_s = $14, rationale = $15,
			is_enabled = $16, updated_at = now(), updated_by = $17, updated_by_name = $18,
			rationalization_status = CASE WHEN $19 THEN 'DRAFT' ELSE rationalization_status END,
			approved_by = CASE WHEN $19 THEN NULL ELSE approved_by END,
			approved_by_name = CASE WHEN $19 THEN '' ELSE approved_by_name END,
			approved_at = CASE WHEN $19 THEN NULL ELSE approved_at END
		WHERE alarm_id = $1`,
		id, in.Setpoint, in.TriggerState, unit, in.Deadband, in.OnDelayS, in.OffDelayS,
		in.Priority, in.AlarmClass, in.Message, in.OperatorAction,
		in.ConsequencesOfInaction, in.ProbableCause, in.MaxResponseTimeS, in.Rationale,
		in.Enabled, nullableID(actor.ID), actor.Username, resetToDraft); e != nil {
		return 0, "", false, e
	}
	after, _ := alarmJSON(ctx, tx, id)
	if string(before) == string(after) {
		return id, alarmUnchanged, resetToDraft, nil // nothing changed: nothing to audit
	}
	reason := changeReason
	if resetToDraft {
		reason = strings.TrimSpace(reason + " [approval withdrawn: the rationalized values changed]")
	}
	return id, alarmUpdated, resetToDraft, insertAlarmAudit(ctx, tx, AlarmAudit{Actor: actor, Action: "ALARM_UPDATE", AlarmID: id,
		Outcome: "success", Reason: reason, Before: before, After: after})
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ApproveAlarmDef marks the rationalization as approved by `actor`.
// ErrNotRationalized (with the gaps in the message) if it is incomplete.
func (s *Store) ApproveAlarmDef(ctx context.Context, id int64, actor Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return approveAlarmDefTx(ctx, tx, id, actor) })
}

func approveAlarmDefTx(ctx context.Context, tx pgx.Tx, id int64, actor Actor) error {
	var a AlarmDef
	if e := scanAlarmDef(tx.QueryRow(ctx, alarmDefSelect+` WHERE m.alarm_id = $1 FOR UPDATE OF m`, id), &a); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrAlarmNotFound
		}
		return e
	}
	if gaps := RationalizationGaps(a); len(gaps) > 0 {
		return fmt.Errorf("%w: missing %s", ErrNotRationalized, strings.Join(gaps, ", "))
	}
	if a.RationalizationStatus == RationApproved {
		return nil
	}
	before, _ := alarmJSON(ctx, tx, id)
	if _, e := tx.Exec(ctx, `
		UPDATE alarm_metadata SET rationalization_status = 'APPROVED', approved_by = $2, approved_by_name = $3,
			approved_at = now(), updated_at = now(), updated_by = $2, updated_by_name = $3
		WHERE alarm_id = $1`, id, actor.ID, actor.Username); e != nil {
		return e
	}
	after, _ := alarmJSON(ctx, tx, id)
	return insertAlarmAudit(ctx, tx, AlarmAudit{Actor: actor, Action: "ALARM_APPROVE", AlarmID: id,
		Outcome: "success", Before: before, After: after})
}

func alarmJSON(ctx context.Context, tx pgx.Tx, id int64) ([]byte, error) {
	var b []byte
	err := tx.QueryRow(ctx, `SELECT to_jsonb(m) FROM alarm_metadata m WHERE alarm_id = $1`, id).Scan(&b)
	return b, err
}

// AlarmAudit is one line of alarm_audit_log.
type AlarmAudit struct {
	Actor   Actor
	Action  string
	AlarmID int64  // 0 = none
	Outcome string // success | denied | rejected
	Reason  string
	Before  []byte
	After   []byte
}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertAlarmAudit(ctx context.Context, db execer, e AlarmAudit) error {
	var uid *int64
	if e.Actor.ID != 0 {
		uid = &e.Actor.ID
	}
	var aid *int64
	if e.AlarmID != 0 {
		aid = &e.AlarmID
	}
	_, err := db.Exec(ctx, `
		INSERT INTO alarm_audit_log (user_id, user_name, action, alarm_id, outcome, reason, before_data, after_data, client_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uid, e.Actor.Username, e.Action, aid, e.Outcome, e.Reason, nilBytes(e.Before), nilBytes(e.After), e.Actor.IP)
	return err
}

func nilBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// RecordAlarmAudit writes one audit line outside any other transaction (for
// refused actions).
func (s *Store) RecordAlarmAudit(ctx context.Context, e AlarmAudit) error {
	return insertAlarmAudit(ctx, s.pool, e)
}

func isConstraintName(err error, name string) bool {
	var ce *ConstraintError
	if errors.As(asConstraintError(err), &ce) {
		return ce.Constraint == name
	}
	return false
}
