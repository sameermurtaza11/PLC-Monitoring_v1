// Package alarms is the basic alarm management of Version 01: alarm
// definitions and rationalization (this file), and the alarm engine that turns
// PV values into outstanding alarms. It has no HTTP code; the web layer calls it.
package alarms

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/store"
)

// ValidationError carries one message per form field.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for f, m := range e.Fields {
		parts = append(parts, f+": "+m)
	}
	return "invalid alarm definition: " + strings.Join(parts, "; ")
}

const (
	maxMessage = 200
	maxText    = 1000
	maxDelayS  = 3600
)

// analogOrder lists the analog types from the highest limit to the lowest.
var analogOrder = []string{store.AlarmHIHI, store.AlarmHI, store.AlarmLO, store.AlarmLOLO}

func isHigh(t string) bool { return t == store.AlarmHI || t == store.AlarmHIHI }

// Validate checks an alarm definition against the PV it belongs to and the
// other alarms of that PV. It returns one message per field, empty if valid.
// alarmType is the type of an existing alarm, or the chosen one for a new alarm.
func Validate(in store.AlarmInput, alarmType string, pv store.AlarmPV, siblings map[string]float64) map[string]string {
	f := map[string]string{}

	switch alarmType {
	case store.AlarmHI, store.AlarmHIHI, store.AlarmLO, store.AlarmLOLO:
		if pv.Digital {
			f["alarm_type"] = "a digital input can only have a DISCRETE alarm"
		}
	case store.AlarmDiscrete:
		if !pv.Digital {
			f["alarm_type"] = "DISCRETE alarms are for digital inputs only"
		}
	default:
		f["alarm_type"] = "choose HIHI, HI, LO, LOLO or DISCRETE"
	}

	if strings.TrimSpace(in.Message) == "" {
		f["alarm_message"] = "Required"
	} else if len(in.Message) > maxMessage {
		f["alarm_message"] = fmt.Sprintf("At most %d characters", maxMessage)
	}
	for name, v := range map[string]string{
		"operator_action": in.OperatorAction, "consequences_of_inaction": in.ConsequencesOfInaction,
		"probable_cause": in.ProbableCause, "rationale": in.Rationale,
	} {
		if len(v) > maxText {
			f[name] = fmt.Sprintf("At most %d characters", maxText)
		}
	}

	if !contains(store.AlarmPriorities, in.Priority) {
		f["priority"] = "Choose Minor, Major or Critical"
	}
	if !contains(store.AlarmClasses, in.AlarmClass) {
		f["alarm_class"] = "Choose an alarm class"
	}
	if in.MaxResponseTimeS != nil && *in.MaxResponseTimeS <= 0 {
		f["max_response_time_s"] = "Must be greater than 0 seconds (or leave empty)"
	}
	if in.OnDelayS < 0 || in.OnDelayS > maxDelayS {
		f["on_delay_s"] = fmt.Sprintf("Between 0 and %d seconds", maxDelayS)
	}
	if in.OffDelayS < 0 || in.OffDelayS > maxDelayS {
		f["off_delay_s"] = fmt.Sprintf("Between 0 and %d seconds", maxDelayS)
	}
	if math.IsNaN(in.Deadband) || math.IsInf(in.Deadband, 0) || in.Deadband < 0 {
		f["deadband"] = "Must be 0 or more"
	}
	// The setpoint is always stored in the PV's own unit; a different unit is a mistake.
	if in.Unit != "" && in.Unit != pv.Unit {
		f["setpoint_unit"] = fmt.Sprintf("The PV is measured in %q, not %q", pv.Unit, in.Unit)
	}

	if alarmType == store.AlarmDiscrete {
		if in.TriggerState == nil || (*in.TriggerState != 0 && *in.TriggerState != 1) {
			f["trigger_state"] = "Choose the abnormal state"
		}
		if in.Setpoint != nil {
			f["setpoint"] = "Not used for a DISCRETE alarm"
		}
		if in.Deadband != 0 {
			f["deadband"] = "Not used for a DISCRETE alarm (use the delays)"
		}
		return f
	}

	if in.TriggerState != nil {
		f["trigger_state"] = "Only for DISCRETE alarms"
	}
	sp := in.Setpoint
	if sp == nil || math.IsNaN(*sp) || math.IsInf(*sp, 0) {
		f["setpoint"] = "Required"
		return f
	}
	if *sp < pv.Min || *sp > pv.Max {
		f["setpoint"] = fmt.Sprintf("Outside the PV range %g … %g %s — it could never trigger", pv.Min, pv.Max, pv.Unit)
	}
	if in.Deadband >= pv.Max-pv.Min {
		f["deadband"] = "Must be smaller than the PV range"
	}

	// HIHI > HI > LO > LOLO, together with the other alarms of this PV
	all := map[string]float64{}
	for k, v := range siblings {
		all[k] = v
	}
	all[alarmType] = *sp
	for i, hi := range analogOrder {
		hv, ok := all[hi]
		if !ok {
			continue
		}
		for _, lo := range analogOrder[i+1:] {
			if lv, ok := all[lo]; ok && hv <= lv && (hi == alarmType || lo == alarmType) && f["setpoint"] == "" {
				f["setpoint"] = fmt.Sprintf("%s (%g) must be above %s (%g)", hi, hv, lo, lv)
			}
		}
	}

	// A deadband that reaches the opposite side would only let the alarm
	// clear after the PV passed the other limit, hiding that condition.
	if f["setpoint"] == "" && f["deadband"] == "" {
		for _, other := range analogOrder {
			ov, ok := siblings[other]
			if !ok || isHigh(other) == isHigh(alarmType) {
				continue
			}
			if isHigh(alarmType) && *sp-in.Deadband <= ov {
				f["deadband"] = fmt.Sprintf("The alarm would only clear at %g, at or below the %s limit (%g)", *sp-in.Deadband, other, ov)
			}
			if !isHigh(alarmType) && *sp+in.Deadband >= ov {
				f["deadband"] = fmt.Sprintf("The alarm would only clear at %g, at or above the %s limit (%g)", *sp+in.Deadband, other, ov)
			}
		}
	}
	return f
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Repo is the part of the store the configuration service needs.
type Repo interface {
	PVForAlarm(ctx context.Context, pvID int) (store.AlarmPV, error)
	SiblingSetpoints(ctx context.Context, pvID int, exceptAlarmID int64) (map[string]float64, error)
	GetAlarmDef(ctx context.Context, id int64) (store.AlarmDef, error)
	SaveAlarmDef(ctx context.Context, id int64, in store.AlarmInput, unit, changeReason string, actor store.Actor) (int64, bool, error)
	ApproveAlarmDef(ctx context.Context, id int64, actor store.Actor) error
}

// ConfigService validates and stores alarm definitions. Who may call it is
// decided by the access rules (auth.Rules) before the request gets here.
type ConfigService struct{ repo Repo }

func NewConfigService(repo Repo) *ConfigService { return &ConfigService{repo: repo} }

// Save creates (id == 0) or updates an alarm. For an existing alarm the PV and
// the type come from the stored definition, never from the request.
// resetToDraft reports that an approved alarm lost its approval by this change.
func (s *ConfigService) Save(ctx context.Context, actor store.Actor, id int64, in store.AlarmInput, changeReason string) (savedID int64, resetToDraft bool, err error) {
	pvID, alarmType := in.PVID, strings.ToUpper(strings.TrimSpace(in.AlarmType))
	if id != 0 {
		def, e := s.repo.GetAlarmDef(ctx, id)
		if e != nil {
			return 0, false, e
		}
		pvID, alarmType = def.PVID, def.AlarmType
	}
	in.PVID, in.AlarmType = pvID, alarmType
	in = normalize(in)

	pv, e := s.repo.PVForAlarm(ctx, pvID)
	if errors.Is(e, pgx.ErrNoRows) {
		return 0, false, &ValidationError{Fields: map[string]string{"pv_id": "Choose an existing PV"}}
	}
	if e != nil {
		return 0, false, e
	}
	siblings, e := s.repo.SiblingSetpoints(ctx, pvID, id)
	if e != nil {
		return 0, false, fmt.Errorf("load the other alarms of the PV: %w", e)
	}
	if fields := Validate(in, alarmType, pv, siblings); len(fields) > 0 {
		return 0, false, &ValidationError{Fields: fields}
	}
	return s.repo.SaveAlarmDef(ctx, id, in, pv.Unit, strings.TrimSpace(changeReason), actor)
}

// Approve records the approval of the rationalization.
func (s *ConfigService) Approve(ctx context.Context, actor store.Actor, id int64) error {
	return s.repo.ApproveAlarmDef(ctx, id, actor)
}

func normalize(in store.AlarmInput) store.AlarmInput {
	in.Message = strings.TrimSpace(in.Message)
	in.OperatorAction = strings.TrimSpace(in.OperatorAction)
	in.ConsequencesOfInaction = strings.TrimSpace(in.ConsequencesOfInaction)
	in.ProbableCause = strings.TrimSpace(in.ProbableCause)
	in.Rationale = strings.TrimSpace(in.Rationale)
	in.Unit = strings.TrimSpace(in.Unit)
	return in
}
