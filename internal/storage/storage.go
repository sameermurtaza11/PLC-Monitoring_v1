// Package storage persists acquired samples in PostgreSQL, independently of the
// acquisition loop.
//
// It consumes samples from the bus and writes them in batches with the
// existing store.WriteScan (the same SQL as before: pv_latest, pv_history and
// failure rows in one transaction). The PLC scan loop never waits for it.
//
// Failure behaviour:
//   - Database unavailable: history samples are kept in memory (up to
//     MaxPending) and written, in order, as soon as it is back; retries back
//     off between RetryMin and RetryMax; every failure and the recovery are logged.
//   - Memory buffer full: the OLDEST history samples are dropped, counted and
//     logged as an error. Nothing grows without limit and nothing is dropped silently.
//   - A write the database rejects for good (integrity violation): the batch is
//     dropped, counted and logged, so it cannot block everything behind it.
//   - Samples missing before they reach this module (bus overflow) are detected
//     from gaps in the sample sequence number and reported.
//
// This is not a guarantee of zero data loss: samples held only in memory are
// lost if the process dies, and a database outage longer than the buffer can
// hold loses the oldest samples (reported, never silent).
package storage

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/store"
)

// Writer is what the module needs from the database layer (*store.Store).
type Writer interface {
	WriteScan(ctx context.Context, latest, history []store.Sample, failures []store.Failure) error
}

// IsPermanent reports whether an error will fail again on retry. It is a
// variable so tests can substitute it.
var IsPermanent = store.IsPermanent

// historyHeartbeat: with a deadband, still store at least one history row per
// PV this often (unchanged from the original acquisition worker).
const historyHeartbeat = 60 * time.Second

// Options configure the module. Zero values get defaults.
type Options struct {
	FlushEvery    time.Duration // how often a batch is written (default 250 ms)
	MaxBatch      int           // history samples per database call (default 2000)
	MaxPending    int           // history samples kept in memory while the DB is unavailable (default 500 000, about 20 MB)
	WriteTimeout  time.Duration // limit of one database call (default 5 s)
	RetryMin      time.Duration // first wait after a failed write (default 500 ms)
	RetryMax      time.Duration // longest wait between retries (default 10 s)
	ShutdownGrace time.Duration // how long to keep trying to write at shutdown (default 10 s)
	Logf          func(format string, args ...any)
	Now           func() time.Time
}

// Stats is a snapshot of the module.
type Stats struct {
	Received    uint64 // samples taken from the bus
	SeqGaps     uint64 // samples that never reached this module (bus overflow), from Seq gaps
	HistoryRows uint64 // history samples successfully written
	Dropped     uint64 // history samples dropped because the memory buffer was full
	Rejected    uint64 // history samples dropped because the database rejected them
	Writes      uint64 // successful database calls
	WriteErrors uint64 // failed database calls
	Pending     int    // history samples waiting to be written
	DBUp        bool
	LastError   string
	LastSuccess time.Time
}

type lastStored struct {
	value float64
	ts    time.Time
}

// Module is the storage consumer. Create it with New and call Run.
type Module struct {
	w   Writer
	sub *bus.Subscription
	opt Options

	// owned by the Run goroutine
	pending    []store.Sample
	latest     map[int]store.Sample
	failures   map[int]store.Failure
	lastStored map[int]lastStored
	lastSeq    uint64
	nextTry    time.Time
	backoff    time.Duration
	downSince  time.Time
	lastErrLog time.Time
	lastOvfLog time.Time

	mu    sync.Mutex
	stats Stats
}

func New(w Writer, sub *bus.Subscription, opt Options) *Module {
	if opt.FlushEvery <= 0 {
		opt.FlushEvery = 250 * time.Millisecond
	}
	if opt.MaxBatch <= 0 {
		opt.MaxBatch = 2000
	}
	if opt.MaxPending <= 0 {
		opt.MaxPending = 500_000
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = 5 * time.Second
	}
	if opt.RetryMin <= 0 {
		opt.RetryMin = 500 * time.Millisecond
	}
	if opt.RetryMax <= 0 {
		opt.RetryMax = 10 * time.Second
	}
	if opt.ShutdownGrace <= 0 {
		opt.ShutdownGrace = 10 * time.Second
	}
	if opt.Logf == nil {
		opt.Logf = log.Printf
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Module{
		w: w, sub: sub, opt: opt,
		latest: map[int]store.Sample{}, failures: map[int]store.Failure{}, lastStored: map[int]lastStored{},
		backoff: opt.RetryMin,
		stats:   Stats{DBUp: true},
	}
}

func (m *Module) logf(format string, args ...any) { m.opt.Logf("storage: "+format, args...) }

// Stats returns a snapshot; safe to call from any goroutine.
func (m *Module) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

func (m *Module) update(f func(s *Stats)) {
	m.mu.Lock()
	f(&m.stats)
	m.stats.Pending = len(m.pending)
	m.mu.Unlock()
}

// Run consumes samples until the bus is closed, then writes what is left and
// returns. It must run in its own goroutine.
func (m *Module) Run() {
	ticker := time.NewTicker(m.opt.FlushEvery)
	defer ticker.Stop()
	for {
		select {
		case smp, ok := <-m.sub.C():
			if !ok {
				m.shutdown()
				return
			}
			m.accept(smp)
			if len(m.pending) >= m.opt.MaxBatch {
				m.flush(false)
			}
		case <-ticker.C:
			m.flush(false)
		}
	}
}

// accept turns one sample into what the database layer stores.
func (m *Module) accept(smp bus.Sample) {
	if smp.Seq > m.lastSeq+1 { // consumers subscribe before the first Publish, so the first Seq must be 1
		gap := smp.Seq - m.lastSeq - 1
		m.update(func(s *Stats) { s.SeqGaps += gap })
		m.logf("ERROR %d sample(s) never reached storage (bus queue overflow before this module)", gap)
	}
	if smp.Seq > m.lastSeq {
		m.lastSeq = smp.Seq
	}
	m.update(func(s *Stats) { s.Received++ })

	if !smp.Good() {
		// The newest failure per PV is enough: pv_latest only shows the current quality.
		m.failures[smp.PV.ID] = store.Failure{PVID: smp.PV.ID, TS: smp.TS, Quality: smp.Quality, Error: smp.Error}
		return
	}
	rec := store.Sample{PVID: smp.PV.ID, TS: smp.TS, Raw: smp.Raw, Value: smp.Value}
	m.latest[smp.PV.ID] = rec
	delete(m.failures, smp.PV.ID) // a newer good value supersedes an older failure
	if m.keepHistory(smp.PV, rec) {
		m.pending = append(m.pending, rec)
		m.limitPending()
	}
}

// keepHistory decides whether a sample goes into pv_history. Unchanged logic
// of the original acquisition worker:
//   - Digital input: only when the state changes (+ heartbeat). Nothing is
//     lost: every transition is stored with the timestamp of the scan that saw it.
//   - Analog, deadband = 0: every scan.
//   - Analog, deadband > 0: only changes >= deadband (+ heartbeat).
//
// Heartbeat = one row per minute even without change, so a flat line is
// visibly "still alive" in the trend.
func (m *Module) keepHistory(pv store.PV, s store.Sample) bool {
	last, ok := m.lastStored[pv.ID]
	if pv.IsDigital() && ok && s.Value == last.value && s.TS.Sub(last.ts) < historyHeartbeat {
		return false
	}
	if !pv.IsDigital() && pv.Deadband > 0 && ok &&
		math.Abs(s.Value-last.value) < pv.Deadband &&
		s.TS.Sub(last.ts) < historyHeartbeat {
		return false
	}
	m.lastStored[pv.ID] = lastStored{s.Value, s.TS}
	return true
}

// limitPending keeps the memory buffer bounded. When it is full the oldest
// samples go (in one step, 10 % extra, so this is not done per sample) and the
// loss is counted and logged.
func (m *Module) limitPending() {
	if len(m.pending) <= m.opt.MaxPending {
		return
	}
	drop := len(m.pending) - m.opt.MaxPending + m.opt.MaxPending/10
	if drop > len(m.pending) {
		drop = len(m.pending)
	}
	m.pending = m.pending[drop:]
	m.update(func(s *Stats) { s.Dropped += uint64(drop) })
	now := m.opt.Now()
	if m.lastOvfLog.IsZero() || now.Sub(m.lastOvfLog) >= 5*time.Second {
		m.lastOvfLog = now
		m.logf("ERROR buffer full (%d samples): dropped the %d oldest history samples; the database has been unavailable since %s",
			m.opt.MaxPending, drop, m.downSinceText())
	}
}

func (m *Module) downSinceText() string {
	if m.downSince.IsZero() {
		return "(it is reachable; it cannot keep up)"
	}
	return m.downSince.Format(time.RFC3339)
}

// flush writes one batch. Unless force is set it does nothing while a retry
// back-off is running.
func (m *Module) flush(force bool) {
	if len(m.pending) == 0 && len(m.latest) == 0 && len(m.failures) == 0 {
		return
	}
	now := m.opt.Now()
	if !force && now.Before(m.nextTry) {
		return
	}

	n := len(m.pending)
	if n > m.opt.MaxBatch {
		n = m.opt.MaxBatch
	}
	history := m.pending[:n]
	latest := make([]store.Sample, 0, len(m.latest))
	for _, s := range m.latest {
		latest = append(latest, s)
	}
	failures := make([]store.Failure, 0, len(m.failures))
	for _, f := range m.failures {
		failures = append(failures, f)
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.opt.WriteTimeout)
	err := m.w.WriteScan(ctx, latest, history, failures)
	cancel()

	switch {
	case err == nil:
		recovered := !m.downSince.IsZero()
		wasPending := len(m.pending)
		m.pending = m.pending[n:]
		m.latest = map[int]store.Sample{}
		m.failures = map[int]store.Failure{}
		m.backoff, m.nextTry = m.opt.RetryMin, time.Time{}
		m.update(func(s *Stats) {
			s.Writes++
			s.HistoryRows += uint64(n)
			s.DBUp, s.LastError, s.LastSuccess = true, "", now
		})
		if recovered {
			m.logf("database recovered after %s: writing the %d buffered history samples", now.Sub(m.downSince).Round(time.Second), wasPending)
			m.downSince = time.Time{}
		}

	case IsPermanent(err):
		// Retrying would fail forever and block everything behind it.
		m.logf("ERROR database rejected %d history samples, dropped: %v", n, err)
		m.pending = m.pending[n:]
		m.latest = map[int]store.Sample{}
		m.failures = map[int]store.Failure{}
		m.update(func(s *Stats) { s.Rejected += uint64(n); s.WriteErrors++; s.LastError = err.Error() })

	default:
		if m.downSince.IsZero() {
			m.downSince = now
		}
		m.nextTry = now.Add(m.backoff)
		m.backoff = min(m.backoff*2, m.opt.RetryMax)
		m.update(func(s *Stats) { s.WriteErrors++; s.DBUp = false; s.LastError = err.Error() })
		if m.lastErrLog.IsZero() || now.Sub(m.lastErrLog) >= 30*time.Second {
			m.lastErrLog = now
			m.logf("ERROR database write failed, %d history samples buffered, retrying: %v", len(m.pending), err)
		}
	}
}

// shutdown writes what is left. It keeps trying for ShutdownGrace and then
// says exactly how many samples could not be saved.
func (m *Module) shutdown() {
	deadline := m.opt.Now().Add(m.opt.ShutdownGrace)
	for len(m.pending) > 0 || len(m.latest) > 0 || len(m.failures) > 0 {
		m.flush(true)
		if len(m.pending) == 0 && len(m.latest) == 0 && len(m.failures) == 0 {
			break
		}
		if !m.opt.Now().Before(deadline) {
			lost := len(m.pending)
			m.update(func(s *Stats) { s.Dropped += uint64(lost) })
			m.logf("ERROR shutting down with %d history samples that could not be written (database unavailable): they are lost", lost)
			m.pending = nil
			return
		}
		time.Sleep(min(500*time.Millisecond, m.opt.RetryMin))
	}
	m.logf("stopped: %s", m.summary())
}

func (m *Module) summary() string {
	s := m.Stats()
	return fmt.Sprintf("%d samples received, %d history rows written, %d dropped, %d rejected, %d lost before storage",
		s.Received, s.HistoryRows, s.Dropped, s.Rejected, s.SeqGaps)
}
