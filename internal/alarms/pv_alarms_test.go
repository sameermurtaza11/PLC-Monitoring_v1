package alarms

import (
	"strings"
	"testing"

	"PLC_Monitoring/internal/store"
)

func analogPV() store.AlarmPV {
	return store.AlarmPV{ID: 1, Name: "PT101", Unit: "PSI", Min: 0, Max: 100}
}

func spec(typ string, sp float64, db float64) store.AlarmSpec {
	s := sp
	return store.AlarmSpec{Type: typ, Enabled: true, Input: store.AlarmInput{AlarmType: typ, Setpoint: &s, Deadband: db,
		OnDelayS: 10, OffDelayS: 10, Priority: "Major", AlarmClass: "PROCESS", Message: typ + " message"}}
}

func discreteSpec(normalTrigger int) store.AlarmSpec {
	t := normalTrigger
	return store.AlarmSpec{Type: store.AlarmDiscrete, Enabled: true, Input: store.AlarmInput{AlarmType: store.AlarmDiscrete,
		TriggerState: &t, Priority: "Critical", AlarmClass: "EQUIPMENT_PROTECTION", Message: "Pump stopped"}}
}

func TestFourAnalogAlarmsOrderedAndInRangeAreAccepted(t *testing.T) {
	specs := []store.AlarmSpec{spec("HIHI", 90, 2), spec("HI", 80, 2), spec("LO", 20, 2), spec("LOLO", 10, 2)}
	if got := ValidatePVAlarms(analogPV(), specs, false); len(got) != 0 {
		t.Errorf("valid alarms refused: %v", got)
	}
}

func TestAnalogLimitsMustBeOrderedInsideTheRangeWithSensibleDeadbandsAndDelays(t *testing.T) {
	bad := func(name string, specs []store.AlarmSpec, wantKey string) {
		t.Helper()
		got := ValidatePVAlarms(analogPV(), specs, false)
		if _, ok := got[wantKey]; !ok {
			t.Errorf("%s: no error on %s (got %v)", name, wantKey, got)
		}
	}
	bad("HI at or below LO", []store.AlarmSpec{spec("HI", 20, 1), spec("LO", 30, 1)}, "HI.setpoint")
	bad("HIHI below HI", []store.AlarmSpec{spec("HIHI", 70, 1), spec("HI", 80, 1)}, "HIHI.setpoint")
	bad("LOLO above LO", []store.AlarmSpec{spec("LO", 20, 1), spec("LOLO", 25, 1)}, "LOLO.setpoint")
	bad("setpoint above the PV maximum", []store.AlarmSpec{spec("HI", 150, 1)}, "HI.setpoint")
	bad("setpoint below the PV minimum", []store.AlarmSpec{spec("LO", -5, 1)}, "LO.setpoint")
	bad("negative deadband", []store.AlarmSpec{spec("HI", 80, -1)}, "HI.deadband")
	bad("deadband as large as the range", []store.AlarmSpec{spec("HI", 80, 100)}, "HI.deadband")
	bad("deadband that crosses the opposite limit", []store.AlarmSpec{spec("HI", 60, 45), spec("LO", 40, 1)}, "HI.deadband")

	s := spec("HI", 80, 1)
	s.Input.OnDelayS = 4000
	bad("on-delay too long", []store.AlarmSpec{s}, "HI.on_delay_s")
	s = spec("HI", 80, 1)
	s.Input.OffDelayS = -1
	bad("negative off-delay", []store.AlarmSpec{s}, "HI.off_delay_s")
	s = spec("HI", 80, 1)
	s.Input.Priority = "Urgent"
	bad("unknown priority", []store.AlarmSpec{s}, "HI.priority")
	s = spec("HI", 80, 1)
	s.Input.Message = "  "
	bad("empty message", []store.AlarmSpec{s}, "HI.alarm_message")
	s = spec("HI", 80, 1)
	s.Input.Setpoint = nil
	bad("missing setpoint", []store.AlarmSpec{s}, "HI.setpoint")
}

func TestSwitchedOffAlarmsAreNotValidatedAndDoNotConstrainTheOthers(t *testing.T) {
	off := spec("LO", 95, 1) // would break the order if it counted
	off.Enabled = false
	got := ValidatePVAlarms(analogPV(), []store.AlarmSpec{spec("HI", 80, 1), off}, false)
	if len(got) != 0 {
		t.Errorf("a switched-off alarm was validated: %v", got)
	}
}

func TestAnAnalogPVCannotGetADiscreteAlarmAndTheOtherWayRound(t *testing.T) {
	if got := ValidatePVAlarms(analogPV(), []store.AlarmSpec{discreteSpec(1)}, false); got["DISCRETE.alarm_type"] == "" {
		t.Errorf("discrete alarm on an analog PV accepted: %v", got)
	}
	di := store.AlarmPV{ID: 2, Name: "XS101", Digital: true}
	if got := ValidatePVAlarms(di, []store.AlarmSpec{spec("HI", 0.5, 0)}, false); got["HI.alarm_type"] == "" {
		t.Errorf("analog alarm on a digital PV accepted: %v", got)
	}
}

func TestDiscreteAlarmNeedsAnAbnormalStateAndNoAnalogFields(t *testing.T) {
	di := store.AlarmPV{ID: 2, Name: "XS101", Digital: true}
	if got := ValidatePVAlarms(di, []store.AlarmSpec{discreteSpec(0)}, false); len(got) != 0 {
		t.Errorf("valid discrete alarm refused: %v", got)
	}
	noState := discreteSpec(0)
	noState.Input.TriggerState = nil
	if got := ValidatePVAlarms(di, []store.AlarmSpec{noState}, false); got["DISCRETE.trigger_state"] == "" {
		t.Errorf("missing abnormal state accepted: %v", got)
	}
	withDeadband := discreteSpec(1)
	withDeadband.Input.Deadband = 2
	if got := ValidatePVAlarms(di, []store.AlarmSpec{withDeadband}, false); got["DISCRETE.deadband"] == "" {
		t.Errorf("a deadband on a discrete alarm accepted: %v", got)
	}
}

func TestApprovalNeedsACompleteRationalization(t *testing.T) {
	s := spec("HI", 80, 2)
	got := ValidatePVAlarms(analogPV(), []store.AlarmSpec{s}, true)
	if msg := got["HI.rationale"]; !strings.Contains(msg, "operator action") || !strings.Contains(msg, "consequences") {
		t.Errorf("approval without rationalization: %v", got)
	}
	s.Input.OperatorAction, s.Input.ConsequencesOfInaction, s.Input.Rationale = "reduce load", "relief valve lifts", "needs action"
	if got := ValidatePVAlarms(analogPV(), []store.AlarmSpec{s}, true); len(got) != 0 {
		t.Errorf("complete rationalization refused: %v", got)
	}
}

func TestTypesForThePVKind(t *testing.T) {
	if got := TypesFor(true); len(got) != 1 || got[0] != store.AlarmDiscrete {
		t.Errorf("digital types = %v", got)
	}
	got := TypesFor(false)
	if len(got) != 4 || got[0] != store.AlarmHIHI || got[3] != store.AlarmLOLO {
		t.Errorf("analog types = %v", got)
	}
}
