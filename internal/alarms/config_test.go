package alarms

import (
	"context"
	"errors"
	"strings"
	"testing"

	"PLC_Monitoring/internal/store"
)

func f64(v float64) *float64 { return &v }
func i(v int) *int           { return &v }

// 0…10 bar analog PV and a digital PV
var (
	analog  = store.AlarmPV{ID: 1, PLCName: "PLC-1", Name: "PT101", Unit: "bar", Min: 0, Max: 10}
	digital = store.AlarmPV{ID: 2, PLCName: "PLC-1", Name: "XS101", Digital: true}
)

func valid(typ string) store.AlarmInput {
	in := store.AlarmInput{
		AlarmType: typ, Setpoint: f64(8), Deadband: 0.2, Priority: "Major", AlarmClass: "PROCESS",
		Message: "PT101 high pressure", Enabled: true,
	}
	if typ == store.AlarmDiscrete {
		in.Setpoint, in.Deadband, in.TriggerState = nil, 0, i(1)
	}
	return in
}

func TestValidAlarmsAreAccepted(t *testing.T) {
	if f := Validate(valid("HI"), "HI", analog, nil); len(f) != 0 {
		t.Errorf("analog HI: %v", f)
	}
	if f := Validate(valid("DISCRETE"), "DISCRETE", digital, nil); len(f) != 0 {
		t.Errorf("discrete: %v", f)
	}
}

func TestPriorityIsIndependentOfTheAlarmType(t *testing.T) {
	// HIHI does not imply Critical, and HI may be Critical: all combinations are valid
	for _, typ := range []string{"HI", "HIHI", "LO", "LOLO"} {
		for _, prio := range store.AlarmPriorities {
			in := valid(typ)
			in.Priority = prio
			if typ == "HIHI" {
				in.Setpoint = f64(9)
			}
			if typ == "LO" || typ == "LOLO" {
				in.Setpoint, in.Deadband = f64(2), 0
				if typ == "LOLO" {
					in.Setpoint = f64(1)
				}
			}
			if f := Validate(in, typ, analog, nil); len(f) != 0 {
				t.Errorf("%s / %s: %v", typ, prio, f)
			}
		}
	}
	in := valid("HI")
	in.Priority = "High" // the old LOW/MEDIUM/HIGH labels are not the agreed ones
	if f := Validate(in, "HI", analog, nil); f["priority"] == "" {
		t.Error("only Minor, Major and Critical are valid priorities")
	}
}

func TestTypeMustFitThePVType(t *testing.T) {
	if f := Validate(valid("HI"), "HI", digital, nil); f["alarm_type"] == "" {
		t.Error("HI on a digital input must be refused")
	}
	if f := Validate(valid("DISCRETE"), "DISCRETE", analog, nil); f["alarm_type"] == "" {
		t.Error("DISCRETE on an analog PV must be refused")
	}
	if f := Validate(valid("HI"), "HIGH", analog, nil); f["alarm_type"] == "" {
		t.Error("an unknown type must be refused")
	}
}

func TestSetpointMustLieInsideThePVRange(t *testing.T) {
	in := valid("HI")
	in.Setpoint = f64(11)
	if f := Validate(in, "HI", analog, nil); f["setpoint"] == "" {
		t.Error("a setpoint above the PV range can never trigger and must be refused")
	}
	in.Setpoint = f64(-1)
	if f := Validate(in, "HI", analog, nil); f["setpoint"] == "" {
		t.Error("a setpoint below the PV range must be refused")
	}
	in.Setpoint = nil
	if f := Validate(in, "HI", analog, nil); f["setpoint"] == "" {
		t.Error("an analog alarm needs a setpoint")
	}
}

func TestRangeMessageIsNotHiddenByTheOrderingMessage(t *testing.T) {
	lo := valid("LO")
	lo.Setpoint, lo.Deadband = f64(500), 0 // far outside the range, and above the HI at 8
	f := Validate(lo, "LO", analog, map[string]float64{"HI": 8})
	if !strings.Contains(f["setpoint"], "Outside the PV range") {
		t.Errorf("the range problem is the more basic one and must be the message shown: %q", f["setpoint"])
	}
}

func TestUnitMustMatchThePV(t *testing.T) {
	in := valid("HI")
	in.Unit = "psi" // PV is in bar
	if f := Validate(in, "HI", analog, nil); f["setpoint_unit"] == "" {
		t.Error("a setpoint in a different unit must be refused")
	}
	in.Unit = "bar"
	if f := Validate(in, "HI", analog, nil); len(f) != 0 {
		t.Errorf("matching unit: %v", f)
	}
}

func TestLimitOrdering(t *testing.T) {
	in := valid("HIHI")
	in.Setpoint = f64(7) // below the existing HI at 8
	if f := Validate(in, "HIHI", analog, map[string]float64{"HI": 8}); f["setpoint"] == "" {
		t.Error("HIHI below HI must be refused")
	}
	in.Setpoint = f64(9)
	if f := Validate(in, "HIHI", analog, map[string]float64{"HI": 8}); len(f) != 0 {
		t.Errorf("HIHI above HI: %v", f)
	}
	lo := valid("LO")
	lo.Setpoint, lo.Deadband = f64(8), 0
	if f := Validate(lo, "LO", analog, map[string]float64{"HI": 8}); f["setpoint"] == "" {
		t.Error("LO equal to HI must be refused")
	}
}

func TestDeadbandCannotReachTheOppositeLimit(t *testing.T) {
	in := valid("HI")
	in.Deadband = 6 // HI at 8 would only clear at 2, at/below the LO limit (3)
	if f := Validate(in, "HI", analog, map[string]float64{"LO": 3}); f["deadband"] == "" {
		t.Error("a deadband reaching the LO limit must be refused")
	}
	in.Deadband = 0.2
	if f := Validate(in, "HI", analog, map[string]float64{"LO": 3}); len(f) != 0 {
		t.Errorf("small deadband: %v", f)
	}
	in.Deadband = -1
	if f := Validate(in, "HI", analog, nil); f["deadband"] == "" {
		t.Error("a negative deadband must be refused")
	}
	in.Deadband = 10
	if f := Validate(in, "HI", analog, nil); f["deadband"] == "" {
		t.Error("a deadband as wide as the PV range must be refused")
	}
}

func TestDelaysTextsAndRequiredFields(t *testing.T) {
	in := valid("HI")
	in.OnDelayS, in.OffDelayS = -1, 4000
	f := Validate(in, "HI", analog, nil)
	if f["on_delay_s"] == "" || f["off_delay_s"] == "" {
		t.Errorf("delays outside 0…3600 s must be refused: %v", f)
	}

	in = valid("HI")
	in.Message, in.Priority, in.AlarmClass = " ", "", "WHATEVER"
	f = Validate(in, "HI", analog, nil)
	for _, k := range []string{"alarm_message", "priority", "alarm_class"} {
		if f[k] == "" {
			t.Errorf("%s should be reported", k)
		}
	}
	in = valid("HI")
	in.Message = strings.Repeat("x", 300)
	zero := 0
	in.MaxResponseTimeS = &zero
	f = Validate(in, "HI", analog, nil)
	if f["alarm_message"] == "" || f["max_response_time_s"] == "" {
		t.Errorf("over-long message and a zero response time must be refused: %v", f)
	}
}

func TestDiscreteRules(t *testing.T) {
	in := valid("DISCRETE")
	in.TriggerState = nil
	if f := Validate(in, "DISCRETE", digital, nil); f["trigger_state"] == "" {
		t.Error("a DISCRETE alarm needs its abnormal state")
	}
	in = valid("DISCRETE")
	in.Setpoint = f64(1)
	if f := Validate(in, "DISCRETE", digital, nil); f["setpoint"] == "" {
		t.Error("a DISCRETE alarm has no analog setpoint")
	}
	in = valid("DISCRETE")
	in.Deadband = 1
	if f := Validate(in, "DISCRETE", digital, nil); f["deadband"] == "" {
		t.Error("a DISCRETE alarm has no deadband (use the delays)")
	}
}

// --- service ---------------------------------------------------------------

type fakeRepo struct {
	def      store.AlarmDef
	saved    []store.AlarmInput
	savedID  int64
	unit     string
	approved int
	reset    bool
}

func (f *fakeRepo) PVForAlarm(_ context.Context, id int) (store.AlarmPV, error) {
	if id == digital.ID {
		return digital, nil
	}
	return analog, nil
}
func (f *fakeRepo) SiblingSetpoints(context.Context, int, int64) (map[string]float64, error) {
	return nil, nil
}
func (f *fakeRepo) GetAlarmDef(_ context.Context, id int64) (store.AlarmDef, error) {
	if f.def.ID != id {
		return store.AlarmDef{}, store.ErrAlarmNotFound
	}
	return f.def, nil
}
func (f *fakeRepo) SaveAlarmDef(_ context.Context, id int64, in store.AlarmInput, unit, _ string, _ store.Actor) (int64, bool, error) {
	f.saved = append(f.saved, in)
	f.unit = unit
	return 7, f.reset, nil
}
func (f *fakeRepo) ApproveAlarmDef(context.Context, int64, store.Actor) error {
	f.approved++
	return nil
}

func TestSaveStoresTheUnitOfThePVAndNormalisesText(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewConfigService(repo)
	in := valid("HI")
	in.PVID, in.Message = 1, "  PT101 high  "
	if _, _, err := svc.Save(context.Background(), store.Actor{ID: 1, Username: "sup"}, 0, in, " initial "); err != nil {
		t.Fatal(err)
	}
	if repo.unit != "bar" || repo.saved[0].Message != "PT101 high" {
		t.Errorf("unit=%q input=%+v", repo.unit, repo.saved[0])
	}
}

func TestSaveOfAnExistingAlarmUsesItsStoredPVAndTypeNotTheRequests(t *testing.T) {
	repo := &fakeRepo{def: store.AlarmDef{ID: 5, PVID: 1, AlarmType: "HI"}}
	svc := NewConfigService(repo)
	in := valid("HI")
	in.PVID, in.AlarmType = 999, "LOLO" // a client trying to move the alarm to another PV / type
	if _, _, err := svc.Save(context.Background(), store.Actor{}, 5, in, ""); err != nil {
		t.Fatal(err)
	}
	if got := repo.saved[0]; got.PVID != 1 || got.AlarmType != "HI" {
		t.Errorf("PV/type must come from the stored alarm: %+v", got)
	}
	if _, _, err := svc.Save(context.Background(), store.Actor{}, 6, in, ""); !errors.Is(err, store.ErrAlarmNotFound) {
		t.Errorf("unknown alarm: %v, want ErrAlarmNotFound", err)
	}
}

func TestInvalidInputIsNotStored(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewConfigService(repo)
	in := valid("HI")
	in.PVID, in.Setpoint = 1, f64(99)
	_, _, err := svc.Save(context.Background(), store.Actor{}, 0, in, "")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["setpoint"] == "" {
		t.Fatalf("got %v, want a setpoint validation error", err)
	}
	if len(repo.saved) != 0 {
		t.Error("invalid input must not reach the database")
	}
}

// --- change management (pure rule in the store package) --------------------

func TestWhichChangesWithdrawTheApproval(t *testing.T) {
	base := store.AlarmDef{Setpoint: f64(8), Deadband: 0.2, OnDelayS: 5, OffDelayS: 5, Priority: "Major",
		AlarmClass: "PROCESS", OperatorAction: "reduce load", ConsequencesOfInaction: "trip", Rationale: "why"}
	same := store.AlarmInput{Setpoint: f64(8), Deadband: 0.2, OnDelayS: 5, OffDelayS: 5, Priority: "Major",
		AlarmClass: "PROCESS", OperatorAction: "reduce load", ConsequencesOfInaction: "trip", Rationale: "why", Enabled: true}
	if store.ReapprovalNeeded(base, same) {
		t.Fatal("an identical save must not withdraw the approval")
	}
	cases := map[string]func(in *store.AlarmInput){
		"setpoint":    func(in *store.AlarmInput) { in.Setpoint = f64(9) },
		"deadband":    func(in *store.AlarmInput) { in.Deadband = 0.3 },
		"on-delay":    func(in *store.AlarmInput) { in.OnDelayS = 6 },
		"off-delay":   func(in *store.AlarmInput) { in.OffDelayS = 6 },
		"priority":    func(in *store.AlarmInput) { in.Priority = "Critical" },
		"class":       func(in *store.AlarmInput) { in.AlarmClass = "SAFETY_CRITICAL" },
		"action":      func(in *store.AlarmInput) { in.OperatorAction = "other" },
		"consequence": func(in *store.AlarmInput) { in.ConsequencesOfInaction = "other" },
		"rationale":   func(in *store.AlarmInput) { in.Rationale = "other" },
	}
	for name, change := range cases {
		in := same
		change(&in)
		if !store.ReapprovalNeeded(base, in) {
			t.Errorf("changing the %s must withdraw the approval", name)
		}
	}
	// enabling/disabling and wording-only changes keep the approval
	in := same
	in.Enabled = false
	in.Message = "new wording"
	in.ProbableCause = "new cause"
	if store.ReapprovalNeeded(base, in) {
		t.Error("enable/disable and wording changes must not withdraw the approval")
	}
}

func TestRationalizationGapsAndEvaluability(t *testing.T) {
	a := store.AlarmDef{}
	if gaps := store.RationalizationGaps(a); len(gaps) != 3 {
		t.Errorf("gaps = %v, want rationale, action and consequences", gaps)
	}
	a = store.AlarmDef{Rationale: "r", OperatorAction: "a", ConsequencesOfInaction: "c"}
	if gaps := store.RationalizationGaps(a); len(gaps) != 0 {
		t.Errorf("complete rationalization has gaps: %v", gaps)
	}

	ok := store.AlarmDef{AlarmType: "HI", Setpoint: f64(8), SetpointUnit: "bar", PVUnit: "bar", PVMin: 0, PVMax: 10,
		Enabled: true, RationalizationStatus: "APPROVED"}
	if !ok.Evaluable() {
		t.Errorf("approved, enabled, consistent alarm should be evaluated: %v", ok.Issues())
	}
	draft := ok
	draft.RationalizationStatus = "DRAFT"
	disabled := ok
	disabled.Enabled = false
	unitChanged := ok
	unitChanged.PVUnit = "psi"
	rangeChanged := ok
	rangeChanged.PVMax = 5 // the PV range was reduced below the setpoint
	for name, a := range map[string]store.AlarmDef{"draft": draft, "disabled": disabled, "unit changed": unitChanged, "range changed": rangeChanged} {
		if a.Evaluable() {
			t.Errorf("%s: must not be evaluated", name)
		}
	}
}
