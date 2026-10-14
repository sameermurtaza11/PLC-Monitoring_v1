package alarms

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/store"
)

// EngineRepo is what the engine needs from the database (store.Store implements it).
type EngineRepo interface {
	ListAlarmDefs(ctx context.Context) ([]store.AlarmDef, error)
	ActiveOccurrences(ctx context.Context) ([]store.Occurrence, error)
	ActivateAlarm(ctx context.Context, a store.AlarmDef, value float64, ts time.Time) (int64, error)
	ReturnToNormal(ctx context.Context, occID int64, value float64, ts time.Time) error
	ExpireShelves(ctx context.Context, now time.Time) (int, error)
	// TryLock takes the single-engine lock; (nil, nil) if another process holds it.
	TryLock(ctx context.Context) (Lock, error)
}

// Lock is the engine lock (store.EngineLock).
type Lock interface {
	Held(ctx context.Context) bool
	Release()
}

// FromStore adapts the database layer to EngineRepo.
func FromStore(s *store.Store) EngineRepo { return storeRepo{s} }

type storeRepo struct{ *store.Store }

func (r storeRepo) TryLock(ctx context.Context) (Lock, error) {
	l, err := r.Store.TryEngineLock(ctx)
	if l == nil || err != nil {
		return nil, err
	}
	return l, nil
}

// ---------------------------------------------------------------------
// Evaluation of one alarm (pure, no database)
// ---------------------------------------------------------------------

type action int

const (
	none action = iota
	activate
	clear
)

// alarmState is what the engine remembers about one alarm definition.
type alarmState struct {
	def   store.AlarmDef
	cond  bool      // the process condition is currently abnormal (with deadband)
	since time.Time // when cond last changed (start of the on/off delay)
	occ   int64     // abnormal occurrence of this alarm in the database; 0 = none
	due   time.Time // when the pending activation/return first became due (kept across database retries)
}

// nextCond applies the trip limit and the deadband. Once abnormal, a high alarm
// stays abnormal until the value is below setpoint − deadband (low alarm: above
// setpoint + deadband), so a value hovering at the limit does not chatter.
func nextCond(d store.AlarmDef, cur bool, v float64) bool {
	switch d.AlarmType {
	case store.AlarmHI, store.AlarmHIHI:
		if cur {
			return v >= *d.Setpoint-d.Deadband
		}
		return v >= *d.Setpoint
	case store.AlarmLO, store.AlarmLOLO:
		if cur {
			return v <= *d.Setpoint+d.Deadband
		}
		return v <= *d.Setpoint
	default: // DISCRETE
		return int(v+0.5) == *d.TriggerState
	}
}

// observe takes one good reading and says what is due. The on-delay and
// off-delay are measured on the samples' own timestamps. Nothing here changes
// `occ`: the caller does that after the database accepted the change, so a
// failed write simply makes the same action due again with the next sample.
func (a *alarmState) observe(v float64, ts time.Time) (action, time.Time) {
	if c := nextCond(a.def, a.cond, v); c != a.cond {
		a.cond, a.since, a.due = c, ts, time.Time{}
	}
	var act action
	switch {
	case a.cond && a.occ == 0 && ts.Sub(a.since) >= time.Duration(a.def.OnDelayS)*time.Second:
		act = activate
	case !a.cond && a.occ != 0 && ts.Sub(a.since) >= time.Duration(a.def.OffDelayS)*time.Second:
		act = clear
	default:
		return none, time.Time{}
	}
	if a.due.IsZero() {
		a.due = ts
	}
	return act, a.due
}

// unknown handles a reading that cannot be trusted (communication or read
// failure). It counts neither as abnormal nor as normal: a pending on-delay is
// cancelled (the condition was not confirmed) and a pending off-delay is
// cancelled (the return to normal was not confirmed), so nothing is activated
// or cleared on unknown data. An alarm that is already active stays active.
func (a *alarmState) unknown() {
	want := a.occ != 0
	if a.cond != want {
		a.cond, a.since, a.due = want, time.Time{}, time.Time{}
	}
}

// ---------------------------------------------------------------------
// Module
// ---------------------------------------------------------------------

// Options tune the module; zero values mean the defaults.
type Options struct {
	DefRefresh time.Duration // reload alarm definitions and occurrences (default 10 s)
	ShelfCheck time.Duration // look for ended shelves (default 1 s)
	LockRetry  time.Duration // retry the engine lock (default 5 s)
	DBRetry    time.Duration // pause database writes after a failure (default 2 s)
	DBTimeout  time.Duration // per database call (default 5 s)
	Logf       func(format string, args ...any)
	OnChange   func() // called after the engine changed the alarm list (activation, return to normal, shelf ended)
}

func (o *Options) fill() {
	if o.DefRefresh == 0 {
		o.DefRefresh = 10 * time.Second
	}
	if o.ShelfCheck == 0 {
		o.ShelfCheck = time.Second
	}
	if o.LockRetry == 0 {
		o.LockRetry = 5 * time.Second
	}
	if o.DBRetry == 0 {
		o.DBRetry = 2 * time.Second
	}
	if o.DBTimeout == 0 {
		o.DBTimeout = 5 * time.Second
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.OnChange == nil {
		o.OnChange = func() {}
	}
}

// Stats describe the engine; every refusal and failure is counted.
type Stats struct {
	Running      bool   // holds the engine lock and evaluates
	Alarms       int    // evaluable alarm definitions
	Samples      uint64 // samples taken from the bus
	Evaluated    uint64 // samples checked against at least one alarm
	Unknown      uint64 // untrusted readings (no alarm decision taken)
	Skipped      uint64 // samples ignored because another instance owns the engine
	Activations  uint64
	Returns      uint64
	ShelvesEnded uint64
	DBErrors     uint64
}

// Module is the alarm engine: a consumer of the sample bus. It decides when
// alarms activate and return to normal and records that in the database; it
// never touches acquisition.
type Module struct {
	repo EngineRepo
	sub  *bus.Subscription
	opt  Options

	byAlarm map[int64]*alarmState
	byPV    map[int][]*alarmState

	lock        Lock
	dbPausedTil time.Time
	lastErrLog  time.Time
	reload      chan struct{}

	running                                      atomic.Bool
	nAlarms                                      atomic.Int64
	samples, evaluated, unknown, skipped         atomic.Uint64
	activations, returns, shelvesEnded, dbErrors atomic.Uint64
}

func New(repo EngineRepo, sub *bus.Subscription, opt Options) *Module {
	opt.fill()
	return &Module{repo: repo, sub: sub, opt: opt, byAlarm: map[int64]*alarmState{}, byPV: map[int][]*alarmState{},
		reload: make(chan struct{}, 1)}
}

// Reload asks for the definitions to be read again soon (after a config save).
func (m *Module) Reload() {
	select {
	case m.reload <- struct{}{}:
	default:
	}
}

func (m *Module) Stats() Stats {
	return Stats{Running: m.running.Load(), Alarms: int(m.nAlarms.Load()), Samples: m.samples.Load(),
		Evaluated: m.evaluated.Load(), Unknown: m.unknown.Load(), Skipped: m.skipped.Load(),
		Activations: m.activations.Load(), Returns: m.returns.Load(), ShelvesEnded: m.shelvesEnded.Load(),
		DBErrors: m.dbErrors.Load()}
}

// Run consumes the bus until it is closed and drained.
func (m *Module) Run() {
	defer func() {
		if m.lock != nil {
			m.lock.Release()
		}
		m.running.Store(false)
	}()
	defRefresh := time.NewTicker(m.opt.DefRefresh)
	shelf := time.NewTicker(m.opt.ShelfCheck)
	lockRetry := time.NewTicker(m.opt.LockRetry)
	defer defRefresh.Stop()
	defer shelf.Stop()
	defer lockRetry.Stop()

	m.ensureLock()
	for {
		select {
		case s, ok := <-m.sub.C():
			if !ok {
				return
			}
			m.handle(s)
		case <-defRefresh.C:
			m.refresh()
		case <-m.reload:
			m.refresh()
		case <-shelf.C:
			m.checkLock()
			m.expireShelves()
		case <-lockRetry.C:
			m.ensureLock()
		}
	}
}

func (m *Module) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), m.opt.DBTimeout)
}

// logError reports a database failure (counted always, logged at most every 10 s).
func (m *Module) logError(what string, err error) {
	m.dbErrors.Add(1)
	m.dbPausedTil = time.Now().Add(m.opt.DBRetry)
	if time.Since(m.lastErrLog) >= 10*time.Second {
		m.lastErrLog = time.Now()
		m.opt.Logf("alarms: ERROR %s: %v (alarm changes are retried, %d database errors so far)", what, err, m.dbErrors.Load())
	}
}

// ensureLock makes sure this process owns the engine; on gaining it, the
// definitions and outstanding occurrences are loaded.
func (m *Module) ensureLock() {
	if m.lock != nil {
		return
	}
	ctx, cancel := m.ctx()
	defer cancel()
	l, err := m.repo.TryLock(ctx)
	if err != nil {
		m.logError("alarm engine lock", err)
		return
	}
	if l == nil {
		return // another instance runs the engine
	}
	m.lock = l
	m.running.Store(true)
	m.opt.Logf("alarms: engine started")
	m.refresh()
}

// checkLock gives the engine up if its database connection is gone (PostgreSQL
// then released the lock, so another instance may have taken over).
func (m *Module) checkLock() {
	if m.lock == nil {
		return
	}
	ctx, cancel := m.ctx()
	defer cancel()
	if !m.lock.Held(ctx) {
		m.opt.Logf("alarms: engine lock lost, evaluation paused until it is regained")
		m.lock.Release()
		m.lock = nil
		m.running.Store(false)
	}
}

// refresh reloads the alarm definitions and the outstanding occurrences. State
// of alarms that stay evaluable (condition, delay timers) is kept.
func (m *Module) refresh() {
	if m.lock == nil {
		return
	}
	ctx, cancel := m.ctx()
	defer cancel()
	defs, err := m.repo.ListAlarmDefs(ctx)
	if err != nil {
		m.logError("loading alarm definitions", err)
		return // keep evaluating with what is loaded
	}
	occs, err := m.repo.ActiveOccurrences(ctx)
	if err != nil {
		m.logError("loading outstanding alarms", err)
		return
	}
	abnormal := map[int64]int64{} // alarm → occurrence whose process condition is abnormal
	for _, o := range occs {
		if o.ProcessConditionActive {
			abnormal[o.AlarmID] = o.ID
		}
	}

	byAlarm := map[int64]*alarmState{}
	byPV := map[int][]*alarmState{}
	for _, d := range defs {
		if !d.Evaluable() {
			continue // draft, disabled or inconsistent: not evaluated
		}
		st := m.byAlarm[d.ID]
		if st == nil {
			st = &alarmState{}
		}
		st.def = d
		st.occ = abnormal[d.ID]
		if st.occ != 0 && !st.cond && st.since.IsZero() {
			st.cond = true // restart with an active alarm: deadband applies to its return
		}
		byAlarm[d.ID] = st
		byPV[d.PVID] = append(byPV[d.PVID], st)
	}
	m.byAlarm, m.byPV = byAlarm, byPV
	m.nAlarms.Store(int64(len(byAlarm)))
}

func (m *Module) handle(s bus.Sample) {
	m.samples.Add(1)
	if m.lock == nil {
		m.skipped.Add(1)
		return
	}
	states := m.byPV[s.PV.ID]
	if len(states) == 0 {
		return
	}
	m.evaluated.Add(1)
	for _, st := range states {
		if !s.Good() {
			m.unknown.Add(1)
			st.unknown()
			continue
		}
		act, due := st.observe(s.Value, s.TS)
		if act == none || time.Now().Before(m.dbPausedTil) {
			continue // nothing due, or the database failed a moment ago: stays due, retried with the next sample
		}
		ctx, cancel := m.ctx()
		switch act {
		case activate:
			id, err := m.repo.ActivateAlarm(ctx, st.def, s.Value, due)
			if err != nil {
				m.logError("activating alarm "+st.def.Message, err)
			} else {
				st.occ, st.due = id, time.Time{}
				m.activations.Add(1)
				m.opt.OnChange()
				m.opt.Logf("alarms: ACTIVATED %s %s %s (value %g)", st.def.PLCName, st.def.PVName, st.def.AlarmType, s.Value)
			}
		case clear:
			err := m.repo.ReturnToNormal(ctx, st.occ, s.Value, due)
			if err != nil && !errors.Is(err, store.ErrOccurrenceGone) {
				m.logError("returning alarm to normal", err)
			} else {
				st.occ, st.due = 0, time.Time{}
				m.returns.Add(1)
				m.opt.OnChange()
				m.opt.Logf("alarms: RETURNED TO NORMAL %s %s %s (value %g)", st.def.PLCName, st.def.PVName, st.def.AlarmType, s.Value)
			}
		}
		cancel()
	}
}

func (m *Module) expireShelves() {
	if m.lock == nil || time.Now().Before(m.dbPausedTil) {
		return
	}
	ctx, cancel := m.ctx()
	defer cancel()
	n, err := m.repo.ExpireShelves(ctx, time.Now())
	if err != nil {
		m.logError("ending shelves", err)
		return
	}
	if n > 0 {
		m.shelvesEnded.Add(uint64(n))
		m.opt.OnChange()
		m.opt.Logf("alarms: %d shelved alarm(s) returned to the alarm list (shelf time ended)", n)
	}
}
