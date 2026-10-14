package alarms

import (
	"strings"

	"PLC_Monitoring/internal/store"
)

// AnalogTypes are the alarm types of an analog PV, highest limit first.
var AnalogTypes = []string{store.AlarmHIHI, store.AlarmHI, store.AlarmLO, store.AlarmLOLO}

// TypesFor returns the alarm types a PV can have: HIHI/HI/LO/LOLO for an analog
// PV, DISCRETE for a digital one.
func TypesFor(digital bool) []string {
	if digital {
		return []string{store.AlarmDiscrete}
	}
	return AnalogTypes
}

// ValidatePVAlarms checks the alarms submitted with a PV (the unified PV form)
// against the PV as it will be saved: its range and unit, the limits ordered
// HIHI > HI > LO > LOLO across the submitted alarms, deadbands, delays and the
// abnormal state of a digital alarm. Only alarms that are switched on are
// checked. The result maps "TYPE.field" to a message (field names as in
// Validate); it is empty when everything is valid.
//
// When approve is set (an administrator approving in the same save) the
// rationalization of each alarm must be complete as well.
func ValidatePVAlarms(pv store.AlarmPV, specs []store.AlarmSpec, approve bool) map[string]string {
	out := map[string]string{}
	siblings := func(except string) map[string]float64 {
		m := map[string]float64{}
		for _, sp := range specs {
			if sp.Enabled && sp.Type != except && sp.Input.Setpoint != nil {
				m[sp.Type] = *sp.Input.Setpoint
			}
		}
		return m
	}
	for _, sp := range specs {
		if !sp.Enabled {
			continue
		}
		in := sp.Input
		in.Unit = pv.Unit
		for field, msg := range Validate(in, sp.Type, pv, siblings(sp.Type)) {
			out[sp.Type+"."+field] = msg
		}
		if approve {
			a := store.AlarmDef{Rationale: in.Rationale, OperatorAction: in.OperatorAction, ConsequencesOfInaction: in.ConsequencesOfInaction}
			if gaps := store.RationalizationGaps(a); len(gaps) > 0 {
				out[sp.Type+".rationale"] = "To approve, fill in: " + strings.Join(gaps, ", ")
			}
		}
	}
	return out
}
