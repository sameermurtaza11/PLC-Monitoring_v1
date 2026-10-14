package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The Configuration page edits a PV and its alarms in one form. The data stays
// in two tables (pv_metadata, alarm_metadata); these functions write both in
// ONE transaction, so a failure in either part leaves nothing half saved.

// AlarmSpec is one alarm type as the form submitted it.
type AlarmSpec struct {
	Type    string
	Enabled bool
	Input   AlarmInput // PVID, AlarmType and Unit are filled in by SavePVWithAlarms
}

// AlarmChange says what happened to one alarm type.
type AlarmChange struct {
	ID           int64
	Type         string
	Action       string // created | updated | enabled | disabled | unchanged
	ResetToDraft bool   // an approved alarm whose rationalized values changed
	Ended        int    // abnormal occurrences ended because the alarm left service
	Approved     bool   // approved in the same save (administrators)
}

// PVSaveResult is the outcome of SavePVWithAlarms.
type PVSaveResult struct {
	PVID    int
	Changes []AlarmChange
}

// ListAlarmDefsOfPV returns every alarm definition of one PV (enabled or not).
func (s *Store) ListAlarmDefsOfPV(ctx context.Context, pvID int) ([]AlarmDef, error) {
	rows, err := s.pool.Query(ctx, alarmDefSelect+` WHERE m.pv_id = $1 ORDER BY m.alarm_id`, pvID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmDef, error) {
		var a AlarmDef
		return a, scanAlarmDef(r, &a)
	})
}

// alarmFits: an analog PV has HIHI/HI/LO/LOLO alarms, a digital one a DISCRETE alarm.
func alarmFits(alarmType string, digital bool) bool { return (alarmType == AlarmDiscrete) == digital }

// SavePVWithAlarms saves the PV and its alarms together.
//
//   - alarmsOn false: every alarm of the PV is taken out of service (kept, disabled).
//   - alarmsOn true: each submitted spec that is enabled is created or updated
//     (never duplicated: one definition per PV and type); a disabled spec, or an
//     existing alarm that is not submitted or no longer fits the PV type, is
//     disabled. Disabled definitions are kept with their history.
//   - An alarm that leaves service has its abnormal occurrences ended (recorded
//     as returned to normal, with the reason); they stay on the alarm list until
//     acknowledged.
//   - A change to rationalized values withdraws an approval (as in Alarm
//     Configuration); enabling or disabling does not. With approve (the caller
//     checks the user is an administrator) the alarms that are enabled and not
//     approved are approved in the same transaction; an incomplete
//     rationalization makes the whole save fail.
func (s *Store) SavePVWithAlarms(ctx context.Context, r PVRecord, alarmsOn bool, specs []AlarmSpec, approve bool, changeReason string, actor Actor) (PVSaveResult, error) {
	var res PVSaveResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		pvID, err := savePVTx(ctx, tx, r)
		if err != nil {
			return err
		}
		res = PVSaveResult{PVID: pvID}

		rows, err := tx.Query(ctx, alarmDefSelect+` WHERE m.pv_id = $1 ORDER BY m.alarm_id FOR UPDATE OF m`, pvID)
		if err != nil {
			return err
		}
		existing, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmDef, error) {
			var a AlarmDef
			return a, scanAlarmDef(r, &a)
		})
		if err != nil {
			return err
		}
		have := map[string]bool{}
		bySpec := map[string]AlarmSpec{}
		for _, sp := range specs {
			bySpec[sp.Type] = sp
		}

		for _, d := range existing {
			have[d.AlarmType] = true
			sp, submitted := bySpec[d.AlarmType]
			if alarmsOn && submitted && sp.Enabled && alarmFits(d.AlarmType, r.IsDigital()) {
				in := sp.Input
				in.PVID, in.AlarmType, in.Unit, in.Enabled = pvID, d.AlarmType, r.Unit, true
				alarmID, outcome, reset, err := saveAlarmDefTx(ctx, tx, d.ID, in, r.Unit, changeReason, actor)
				if err != nil {
					return err
				}
				action := outcome
				if !d.Enabled && outcome != alarmCreated {
					action = "enabled"
				}
				res.Changes = append(res.Changes, AlarmChange{ID: alarmID, Type: d.AlarmType, Action: action, ResetToDraft: reset})
				continue
			}
			if !d.Enabled {
				continue // already out of service
			}
			why := "alarms switched off for this PV"
			switch {
			case alarmsOn && !alarmFits(d.AlarmType, r.IsDigital()):
				why = "the PV is now " + map[bool]string{true: "digital", false: "analog"}[r.IsDigital()]
			case alarmsOn:
				why = "this alarm was switched off"
			}
			ended, err := disableAlarmTx(ctx, tx, d, why, changeReason, actor)
			if err != nil {
				return err
			}
			res.Changes = append(res.Changes, AlarmChange{ID: d.ID, Type: d.AlarmType, Action: "disabled", Ended: ended})
		}

		if alarmsOn {
			for _, sp := range specs {
				if !sp.Enabled || have[sp.Type] || !alarmFits(sp.Type, r.IsDigital()) {
					continue
				}
				in := sp.Input
				in.PVID, in.AlarmType, in.Unit, in.Enabled = pvID, sp.Type, r.Unit, true
				alarmID, _, _, err := saveAlarmDefTx(ctx, tx, 0, in, r.Unit, changeReason, actor)
				if err != nil {
					return err
				}
				res.Changes = append(res.Changes, AlarmChange{ID: alarmID, Type: sp.Type, Action: alarmCreated})
			}
		}
		if approve {
			for i, c := range res.Changes {
				if c.Action == "disabled" || c.ID == 0 {
					continue
				}
				var status string
				if err := tx.QueryRow(ctx, `SELECT rationalization_status FROM alarm_metadata WHERE alarm_id = $1`, c.ID).Scan(&status); err != nil {
					return err
				}
				if status == RationApproved {
					continue
				}
				if err := approveAlarmDefTx(ctx, tx, c.ID, actor); err != nil {
					return fmt.Errorf("%s: %w", c.Type, err)
				}
				res.Changes[i].Approved = true
			}
		}
		return nil
	})
	return res, asConstraintError(err)
}

// disableAlarmTx takes an alarm out of service: it is switched off (the
// definition and its history stay) and its abnormal occurrences are ended.
func disableAlarmTx(ctx context.Context, tx pgx.Tx, d AlarmDef, why, changeReason string, actor Actor) (int, error) {
	before, _ := alarmJSON(ctx, tx, d.ID)
	if _, err := tx.Exec(ctx, `
		UPDATE alarm_metadata SET is_enabled = FALSE, updated_at = now(), updated_by = $2, updated_by_name = $3
		WHERE alarm_id = $1`, d.ID, nullableID(actor.ID), actor.Username); err != nil {
		return 0, err
	}
	after, _ := alarmJSON(ctx, tx, d.ID)
	reason := strings.TrimSpace("taken out of service: " + why + ". " + changeReason)
	if err := insertAlarmAudit(ctx, tx, AlarmAudit{Actor: actor, Action: "ALARM_UPDATE", AlarmID: d.ID,
		Outcome: "success", Reason: reason, Before: before, After: after}); err != nil {
		return 0, err
	}
	return takeOutOfServiceTx(ctx, tx, d.ID, why, actor)
}

// ---------------------------------------------------------------------
// Deleting a PV
// ---------------------------------------------------------------------

// PVDeletionImpact is what deleting a PV would touch, shown before the user confirms.
type PVDeletionImpact struct {
	PVID         int
	PVName       string
	PLCName      string
	AlarmDefs    int // alarm configurations of the PV
	EnabledAlarm int // of which in service
	Outstanding  int // outstanding alarm occurrences (not yet acknowledged / completed)
	AlarmEvents  int // alarm history events (kept)
	ValueHistory int // stored value history rows
}

// RetainsRecords says whether the PV cannot be erased completely: it has alarm
// history, outstanding alarms or (unless the user deletes it too) value history.
func (i PVDeletionImpact) RetainsRecords(deleteValueHistory bool) bool {
	return i.AlarmEvents > 0 || i.Outstanding > 0 || (i.ValueHistory > 0 && !deleteValueHistory)
}

func (s *Store) PVDeletionImpact(ctx context.Context, pvID int) (PVDeletionImpact, error) {
	i := PVDeletionImpact{PVID: pvID}
	err := s.pool.QueryRow(ctx, `
		SELECT pv.pv_name, p.plc_name,
		       (SELECT count(*) FROM alarm_metadata m WHERE m.pv_id = pv.pv_id),
		       (SELECT count(*) FROM alarm_metadata m WHERE m.pv_id = pv.pv_id AND m.is_enabled),
		       (SELECT count(*) FROM active_alarms a JOIN alarm_metadata m ON m.alarm_id = a.alarm_id WHERE m.pv_id = pv.pv_id),
		       (SELECT count(*) FROM alarm_history h JOIN alarm_metadata m ON m.alarm_id = h.alarm_id WHERE m.pv_id = pv.pv_id),
		       (SELECT count(*) FROM pv_history x WHERE x.pv_id = pv.pv_id)
		FROM pv_metadata pv JOIN plc_metadata p ON p.plc_id = pv.plc_id
		WHERE pv.pv_id = $1 AND pv.deleted_at IS NULL`, pvID).
		Scan(&i.PVName, &i.PLCName, &i.AlarmDefs, &i.EnabledAlarm, &i.Outstanding, &i.AlarmEvents, &i.ValueHistory)
	return i, err
}

// PVDeleteResult reports what a deletion did.
type PVDeleteResult struct {
	PVName              string
	Erased              bool // the PV and its alarm configurations were removed completely
	Archived            bool // records had to be kept: the PV was archived instead
	AlarmDefs           int
	OccurrencesEnded    int
	AlarmEvents         int // alarm history events kept
	Outstanding         int // outstanding occurrences kept until acknowledged
	ValueHistoryDeleted int
	ValueHistoryKept    int
}

// DeletePVWithAlarms deletes a PV together with its alarm configurations, in one
// transaction. Records that must be kept are never erased:
//
//   - abnormal occurrences are ended first (recorded as returned to normal);
//   - alarm history, audit records, outstanding occurrences and (unless
//     deleteValueHistory) the value history are kept: then the PV is ARCHIVED —
//     switched off, hidden from the configuration, its alarms disabled — instead
//     of erased, so every reference stays valid;
//   - only a PV without any of these is removed completely.
//
// The audit log records what was done and keeps the removed definitions.
func (s *Store) DeletePVWithAlarms(ctx context.Context, pvID int, deleteValueHistory bool, actor Actor) (PVDeleteResult, error) {
	var res PVDeleteResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var pvJSON []byte
		err := tx.QueryRow(ctx, `
			SELECT pv.pv_name, to_jsonb(pv) FROM pv_metadata pv
			WHERE pv.pv_id = $1 AND pv.deleted_at IS NULL FOR UPDATE`, pvID).Scan(&res.PVName, &pvJSON)
		if err != nil {
			return err // pgx.ErrNoRows: not found (or already deleted)
		}
		rows, err := tx.Query(ctx, alarmDefSelect+` WHERE m.pv_id = $1 ORDER BY m.alarm_id FOR UPDATE OF m`, pvID)
		if err != nil {
			return err
		}
		defs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmDef, error) {
			var a AlarmDef
			return a, scanAlarmDef(r, &a)
		})
		if err != nil {
			return err
		}
		res.AlarmDefs = len(defs)
		for _, d := range defs {
			if d.Enabled {
				n, err := disableAlarmTx(ctx, tx, d, "the PV "+res.PVName+" was deleted", "", actor)
				if err != nil {
					return err
				}
				res.OccurrencesEnded += n
			} else if n, err := takeOutOfServiceTx(ctx, tx, d.ID, "the PV "+res.PVName+" was deleted", actor); err != nil {
				return err
			} else {
				res.OccurrencesEnded += n
			}
		}

		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM alarm_history h JOIN alarm_metadata m ON m.alarm_id = h.alarm_id WHERE m.pv_id = $1),
			       (SELECT count(*) FROM active_alarms a JOIN alarm_metadata m ON m.alarm_id = a.alarm_id WHERE m.pv_id = $1)`, pvID).
			Scan(&res.AlarmEvents, &res.Outstanding); err != nil {
			return err
		}
		if deleteValueHistory {
			tag, err := tx.Exec(ctx, `DELETE FROM pv_history WHERE pv_id = $1`, pvID)
			if err != nil {
				return err
			}
			res.ValueHistoryDeleted = int(tag.RowsAffected())
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pv_history WHERE pv_id = $1`, pvID).Scan(&res.ValueHistoryKept); err != nil {
			return err
		}

		res.Erased = res.AlarmEvents == 0 && res.Outstanding == 0 && res.ValueHistoryKept == 0
		res.Archived = !res.Erased
		if res.Erased {
			for _, d := range defs {
				before, _ := alarmJSON(ctx, tx, d.ID)
				if err := insertAlarmAudit(ctx, tx, AlarmAudit{Actor: actor, Action: "ALARM_DELETE", AlarmID: d.ID, Outcome: "success",
					Reason: "alarm configuration deleted with the PV " + res.PVName, Before: before}); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM alarm_metadata WHERE pv_id = $1`, pvID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM pv_metadata WHERE pv_id = $1`, pvID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `DELETE FROM pv_latest WHERE pv_id = $1`, pvID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE pv_metadata SET enabled = FALSE, deleted_at = now(), updated_at = now() WHERE pv_id = $1`, pvID); err != nil {
				return err
			}
		}
		what := "erased completely"
		if res.Archived {
			what = fmt.Sprintf("archived (kept: %d alarm history events, %d outstanding alarms, %d value history rows)",
				res.AlarmEvents, res.Outstanding, res.ValueHistoryKept)
		}
		return insertAlarmAudit(ctx, tx, AlarmAudit{Actor: actor, Action: "PV_DELETE", Outcome: "success",
			Reason: fmt.Sprintf("PV %s deleted with %d alarm configuration(s): %s", res.PVName, res.AlarmDefs, what), Before: pvJSON})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}
	return res, asConstraintError(err)
}
