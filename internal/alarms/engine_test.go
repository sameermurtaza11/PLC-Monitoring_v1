package alarms

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/store"
)

// ---------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------

type engineLock struct{ held bool }

func (l *engineLock) Held(context.Context) bool { return l.held }
func (l *engineLock) Release()                  {}

type call struct {
	kind  string // ACTIVATE | RETURN
	alarm int64
	occ   int64
	value float64
	ts    time.Time
}

type engineRepo struct {
	mu       sync.Mutex
	defs     []store.AlarmDef
	occs     []store.Occurrence
	calls    []call
	failNext int  // fail this many write calls
	lockFree bool // TryLock succeeds
	lock     *engineLock
	nextOcc  int64
	expired  int
	ended    []string // "alarmID:reason" of TakeOutOfService calls
}

func newEngineRepo(defs ...store.AlarmDef) *engineRepo {
	return &engineRepo{defs: defs, lockFree: true, lock: &engineLock{held: true}, nextOcc: 100}
}

func (r *engineRepo) ListAlarmDefs(context.Context) ([]store.AlarmDef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.AlarmDef(nil), r.defs...), nil
}
func (r *engineRepo) ActiveOccurrences(context.Context) ([]store.Occurrence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.Occurrence(nil), r.occs...), nil
}
func (r *engineRepo) ActivateAlarm(_ context.Context, a store.AlarmDef, v float64, ts time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext > 0 {
		r.failNext--
		return 0, errors.New("db down")
	}
	r.nextOcc++
	r.calls = append(r.calls, call{"ACTIVATE", a.ID, r.nextOcc, v, ts})
	r.occs = append(r.occs, store.Occurrence{ID: r.nextOcc, AlarmID: a.ID, State: store.StateUnack, ProcessConditionActive: true})
	return r.nextOcc, nil
}
func (r *engineRepo) ReturnToNormal(_ context.Context, occ int64, v float64, ts time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext > 0 {
		r.failNext--
		return errors.New("db down")
	}
	r.calls = append(r.calls, call{"RETURN", 0, occ, v, ts})
	for i := range r.occs {
		if r.occs[i].ID == occ {
			r.occs = append(r.occs[:i], r.occs[i+1:]...)
			break
		}
	}
	return nil
}
func (r *engineRepo) ExpireShelves(context.Context, time.Time) (int, error) { return r.expired, nil }
func (r *engineRepo) TakeOutOfService(_ context.Context, alarmID int64, reason string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, fmt.Sprintf("%d:%s", alarmID, reason))
	return 1, nil
}
func (r *engineRepo) TryLock(context.Context) (Lock, error) {
	if !r.lockFree {
		return nil, nil
	}
	return r.lock, nil
}
func (r *engineRepo) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		out = append(out, c.kind)
	}
	return out
}

func f(v float64) *float64 { return &v }

func hi(id int64, sp, db float64, on, off int) store.AlarmDef {
	return store.AlarmDef{ID: id, PVID: 1, PLCName: "PLC-1", PVName: "Pressure", PVUnit: "PSI", PVMin: 0, PVMax: 200,
		AlarmType: store.AlarmHI, Setpoint: f(sp), SetpointUnit: "PSI", Deadband: db, OnDelayS: on, OffDelayS: off,
		Priority: store.PriorityMajor, Message: "High pressure", Enabled: true, RationalizationStatus: store.RationApproved}
}

func lo(id int64, sp, db float64) store.AlarmDef {
	d := hi(id, sp, db, 0, 0)
	d.AlarmType = store.AlarmLO
	return d
}

func discrete(id int64, state int) store.AlarmDef {
	return store.AlarmDef{ID: id, PVID: 1, PLCName: "PLC-1", PVName: "Pump fault", Digital: true,
		AlarmType: store.AlarmDiscrete, TriggerState: i(state), Priority: store.PriorityCritical, Message: "Pump fault",
		Enabled: true, RationalizationStatus: store.RationApproved}
}

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func smp(v float64, sec int) bus.Sample {
	return bus.Sample{PLCName: "PLC-1", PV: store.PV{ID: 1}, Quality: store.QualityGood, Value: v, TS: at(sec)}
}

func bad(sec int, q string) bus.Sample {
	return bus.Sample{PLCName: "PLC-1", PV: store.PV{ID: 1}, Quality: q, Error: "x", TS: at(sec)}
}

// started returns a module whose engine lock is held and definitions loaded.
func started(t *testing.T, r *engineRepo) *Module {
	t.Helper()
	m := New(r, nil, Options{Logf: func(string, ...any) {}, DBRetry: time.Millisecond})
	m.ensureLock()
	if !m.Stats().Running {
		t.Fatal("engine did not start")
	}
	return m
}

// ---------------------------------------------------------------------
// the rules
// ---------------------------------------------------------------------

func TestAnalogLimitsAndDeadband(t *testing.T) {
	h := hi(1, 80, 5, 0, 0)
	l := lo(2, 20, 5)
	for _, c := range []struct {
		d    store.AlarmDef
		cur  bool
		v    float64
		want bool
	}{
		{h, false, 79.9, false}, {h, false, 80, true}, {h, false, 95, true},
		{h, true, 79, true}, {h, true, 75, true}, {h, true, 74.9, false}, // clears only below 80 − 5
		{l, false, 20.1, false}, {l, false, 20, true}, {l, false, 5, true},
		{l, true, 24, true}, {l, true, 25, true}, {l, true, 25.1, false}, // clears only above 20 + 5
		{discrete(3, 1), false, 1, true}, {discrete(3, 1), false, 0, false}, {discrete(3, 0), false, 0, true},
	} {
		if got := nextCond(c.d, c.cur, c.v); got != c.want {
			t.Errorf("%s sp=%v cur=%v v=%v: got %v, want %v", c.d.AlarmType, c.d.Setpoint, c.cur, c.v, got, c.want)
		}
	}
}

func TestActivationWithoutDelayIsImmediateAndOnlyOnce(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 0, 0))
	m := started(t, r)
	m.handle(smp(70, 0))
	m.handle(smp(85, 1))
	m.handle(smp(86, 2)) // still abnormal: no duplicate
	m.handle(smp(90, 3))
	if got := r.kinds(); len(got) != 1 || got[0] != "ACTIVATE" {
		t.Fatalf("calls = %v, want one ACTIVATE", got)
	}
	if c := r.calls[0]; !c.ts.Equal(at(1)) || c.value != 85 {
		t.Errorf("activation = %+v, want the first abnormal sample (t=1, value 85)", c)
	}
}

func TestOnDelayMustHoldWithoutInterruption(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 5, 0))
	m := started(t, r)
	m.handle(smp(85, 0))
	m.handle(smp(85, 4))
	if len(r.calls) != 0 {
		t.Fatal("activated before the 5 s on-delay")
	}
	m.handle(smp(70, 4)) // back to normal: timer restarts
	m.handle(smp(85, 5))
	m.handle(smp(85, 9))
	if len(r.calls) != 0 {
		t.Fatal("activated although the condition was interrupted")
	}
	m.handle(smp(85, 10))
	if got := r.kinds(); len(got) != 1 {
		t.Fatalf("calls = %v, want ACTIVATE after 5 s of continuous abnormal", got)
	}
	if !r.calls[0].ts.Equal(at(10)) {
		t.Errorf("activated_at = %v, want when the delay was satisfied (t=10)", r.calls[0].ts)
	}
}

func TestOffDelayAndDeadbandBeforeReturnToNormal(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 5, 0, 3))
	m := started(t, r)
	m.handle(smp(85, 0)) // activate
	m.handle(smp(77, 1)) // inside deadband: still abnormal
	m.handle(smp(74, 2)) // below 75: normal, off-delay starts
	m.handle(smp(74, 4))
	if got := r.kinds(); len(got) != 1 {
		t.Fatalf("calls = %v, want only the activation before the 3 s off-delay", got)
	}
	m.handle(smp(78, 4)) // bounces into the deadband band: cond still false (needs >= 80 to trip again)
	m.handle(smp(74, 5))
	m.handle(smp(74, 8))
	if got := r.kinds(); len(got) != 2 || got[1] != "RETURN" {
		t.Fatalf("calls = %v, want ACTIVATE then RETURN", got)
	}
}

func TestBadQualityNeitherActivatesNorClears(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 3, 3))
	m := started(t, r)
	m.handle(smp(85, 0))
	m.handle(bad(2, store.QualityCommFail)) // pending on-delay cancelled
	m.handle(smp(85, 3))
	m.handle(smp(85, 5))
	if len(r.calls) != 0 {
		t.Fatalf("activated across a communication failure: %v", r.kinds())
	}
	m.handle(smp(85, 6)) // 3 s continuous since t=3 → hmm t=6
	if got := r.kinds(); len(got) != 1 {
		t.Fatalf("calls = %v, want ACTIVATE after 3 s of good data", got)
	}

	m.handle(smp(10, 10)) // normal, off-delay starts
	m.handle(bad(11, store.QualityCommFail))
	m.handle(smp(10, 12))
	m.handle(smp(10, 14))
	if got := r.kinds(); len(got) != 1 {
		t.Fatalf("alarm cleared on unconfirmed data: %v", got)
	}
	m.handle(smp(10, 15))
	if got := r.kinds(); len(got) != 2 || got[1] != "RETURN" {
		t.Fatalf("calls = %v, want RETURN after 3 s of good normal data", got)
	}
	if m.Stats().Unknown != 2 {
		t.Errorf("unknown readings = %d, want 2", m.Stats().Unknown)
	}
}

func TestDatabaseFailureDelaysButNeverLosesAnActivation(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 0, 0))
	r.failNext = 2
	m := started(t, r)
	m.handle(smp(85, 0)) // fails
	time.Sleep(3 * time.Millisecond)
	m.handle(smp(85, 1)) // fails
	time.Sleep(3 * time.Millisecond)
	m.handle(smp(85, 2)) // works
	if got := r.kinds(); len(got) != 1 || got[0] != "ACTIVATE" {
		t.Fatalf("calls = %v, want the activation after the database recovered", got)
	}
	if !r.calls[0].ts.Equal(at(0)) {
		t.Errorf("activated_at = %v, want the time the alarm first became due (t=0)", r.calls[0].ts)
	}
	if st := m.Stats(); st.DBErrors != 2 || st.Activations != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRestartKeepsAnActiveAlarmAndDoesNotDuplicateIt(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 5, 0, 0))
	r.occs = []store.Occurrence{{ID: 7, AlarmID: 1, State: store.StateUnack, ProcessConditionActive: true}}
	m := started(t, r)
	m.handle(smp(90, 0))
	m.handle(smp(78, 1)) // inside the deadband: the alarm is still abnormal
	if len(r.calls) != 0 {
		t.Fatalf("calls = %v, want none: the alarm is already active", r.kinds())
	}
	m.handle(smp(70, 2))
	if got := r.kinds(); len(got) != 1 || got[0] != "RETURN" || r.calls[0].occ != 7 {
		t.Fatalf("calls = %+v, want RETURN of occurrence 7", r.calls)
	}
}

func TestOnlyApprovedEnabledConsistentAlarmsAreEvaluated(t *testing.T) {
	draft := hi(1, 80, 0, 0, 0)
	draft.RationalizationStatus = store.RationDraft
	off := hi(2, 80, 0, 0, 0)
	off.Enabled = false
	unit := hi(3, 80, 0, 0, 0)
	unit.SetpointUnit = "bar"
	outside := hi(4, 500, 0, 0, 0)
	r := newEngineRepo(draft, off, unit, outside)
	m := started(t, r)
	m.handle(smp(900, 0))
	if len(r.calls) != 0 || m.Stats().Alarms != 0 {
		t.Fatalf("evaluated alarms that are not approved/enabled/consistent: calls=%v alarms=%d", r.kinds(), m.Stats().Alarms)
	}
}

func TestSeveralAlarmsOnOnePVWorkIndependently(t *testing.T) {
	hh := hi(2, 90, 0, 0, 0)
	hh.AlarmType = store.AlarmHIHI
	r := newEngineRepo(hi(1, 80, 0, 0, 0), hh)
	m := started(t, r)
	m.handle(smp(85, 0))
	m.handle(smp(95, 1))
	if got := r.kinds(); len(got) != 2 || r.calls[0].alarm != 1 || r.calls[1].alarm != 2 {
		t.Fatalf("calls = %+v, want HI then HIHI", r.calls)
	}
}

func TestDiscreteAlarm(t *testing.T) {
	r := newEngineRepo(discrete(1, 1))
	m := started(t, r)
	m.handle(smp(0, 0))
	m.handle(smp(1, 1))
	m.handle(smp(0, 2))
	if got := r.kinds(); len(got) != 2 || got[0] != "ACTIVATE" || got[1] != "RETURN" {
		t.Fatalf("calls = %v", got)
	}
}

func TestAnotherInstanceOwningTheEngineMeansNoEvaluation(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 0, 0))
	r.lockFree = false
	m := New(r, nil, Options{Logf: func(string, ...any) {}})
	m.ensureLock()
	m.handle(smp(95, 0))
	if len(r.calls) != 0 || m.Stats().Skipped != 1 || m.Stats().Running {
		t.Fatalf("calls=%v stats=%+v", r.kinds(), m.Stats())
	}
	r.lockFree = true // the other instance went away
	m.ensureLock()
	m.handle(smp(95, 1))
	if got := r.kinds(); len(got) != 1 {
		t.Fatalf("calls = %v, want the engine to take over", got)
	}
}

func TestLostLockPausesEvaluation(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 0, 0))
	m := started(t, r)
	r.lock.held = false
	m.checkLock()
	m.handle(smp(95, 0))
	if len(r.calls) != 0 || m.Stats().Running {
		t.Fatalf("evaluated without the lock: %v %+v", r.kinds(), m.Stats())
	}
}

func TestConfigChangeIsPickedUpOnReloadAndKeepsRunningTimers(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 5, 0))
	m := started(t, r)
	m.handle(smp(85, 0))
	r.mu.Lock()
	r.defs[0].Message = "renamed"
	r.mu.Unlock()
	m.refresh() // timers must survive a reload
	m.handle(smp(85, 5))
	if got := r.kinds(); len(got) != 1 {
		t.Fatalf("calls = %v: the on-delay timer was lost by the reload", got)
	}
	r.mu.Lock()
	r.defs[0].Enabled = false
	r.mu.Unlock()
	m.refresh()
	if m.Stats().Alarms != 0 {
		t.Error("a disabled alarm is still evaluated")
	}
}

func TestRunConsumesTheBusAndStopsWhenItIsClosed(t *testing.T) {
	r := newEngineRepo(hi(1, 80, 0, 0, 0))
	b := bus.New(func(string, ...any) {})
	m := New(r, b.Subscribe("alarms", 100, bus.DropOldest), Options{Logf: func(string, ...any) {}})
	done := make(chan struct{})
	go func() { m.Run(); close(done) }()
	for k := 0; k < 20; k++ {
		s := smp(95, k)
		b.Publish(s)
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the bus was closed")
	}
	if got := r.kinds(); len(got) != 1 {
		t.Fatalf("calls = %v, want one ACTIVATE", got)
	}
}

// An alarm that stops being evaluated (disabled, draft again, its PV deleted)
// while it still has an abnormal occurrence must not leave that occurrence
// active forever: the engine ends it.
func TestOccurrencesOfAnAlarmThatLeftServiceAreEnded(t *testing.T) {
	disabled := hi(1, 80, 0, 0, 0)
	disabled.Enabled = false
	draft := hi(2, 80, 0, 0, 0)
	draft.RationalizationStatus = store.RationDraft
	archived := hi(3, 80, 0, 0, 0)
	archived.PVArchived, archived.Enabled = true, false
	quiet := hi(4, 80, 0, 0, 0)
	quiet.Enabled = false // out of service but nothing abnormal: nothing to end
	inService := hi(5, 80, 0, 0, 0)

	r := newEngineRepo(disabled, draft, archived, quiet, inService)
	r.occs = []store.Occurrence{
		{ID: 1, AlarmID: 1, State: store.StateUnack, ProcessConditionActive: true},
		{ID: 2, AlarmID: 2, State: store.StateAcked, ProcessConditionActive: true},
		{ID: 3, AlarmID: 3, State: store.StateUnack, ProcessConditionActive: true},
		{ID: 4, AlarmID: 4, State: store.StateRTNUN, ProcessConditionActive: false},
		{ID: 5, AlarmID: 5, State: store.StateUnack, ProcessConditionActive: true},
	}
	var changes int
	m := New(r, nil, Options{Logf: func(string, ...any) {}, OnChange: func() { changes++ }})
	m.ensureLock()
	want := []string{"1:the alarm was disabled", "2:its approval was withdrawn", "3:its PV was deleted"}
	if len(r.ended) != len(want) {
		t.Fatalf("ended = %v, want %v", r.ended, want)
	}
	for i := range want {
		if r.ended[i] != want[i] {
			t.Errorf("ended[%d] = %q, want %q", i, r.ended[i], want[i])
		}
	}
	if m.Stats().OutOfService != 3 || changes != 3 {
		t.Errorf("stats %+v, list changes %d", m.Stats(), changes)
	}
}
