package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/store"
)

// fakeWriter stands in for PostgreSQL.
type fakeWriter struct {
	mu       sync.Mutex
	calls    int
	history  []store.Sample
	latest   map[int]store.Sample
	failures map[int]store.Failure
	delay    time.Duration
	failNext int // fail this many calls with a transient error
	permNext int // fail this many calls with a permanent error
	times    []time.Time
}

func newWriter() *fakeWriter {
	return &fakeWriter{latest: map[int]store.Sample{}, failures: map[int]store.Failure{}}
}

var errTransient = errors.New("connection refused")
var errPermanent = errors.New("violates foreign key")

func (f *fakeWriter) WriteScan(_ context.Context, latest, history []store.Sample, failures []store.Failure) error {
	f.mu.Lock()
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.times = append(f.times, time.Now())
	if f.failNext > 0 {
		f.failNext--
		return errTransient
	}
	if f.permNext > 0 {
		f.permNext--
		return errPermanent
	}
	f.history = append(f.history, history...)
	for _, s := range latest {
		f.latest[s.PVID] = s
	}
	for _, x := range failures {
		f.failures[x.PVID] = x
		_ = x
	}
	return nil
}

func (f *fakeWriter) snapshot() (hist []store.Sample, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Sample(nil), f.history...), f.calls
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

// rig wires a bus, a storage module and a fake database.
type rig struct {
	b   *bus.Bus
	m   *Module
	w   *fakeWriter
	log *logSink
	end chan struct{}
}

func newRig(t *testing.T, subCap int, policy bus.Policy, opt Options) *rig {
	t.Helper()
	r := &rig{w: newWriter(), log: &logSink{}, end: make(chan struct{})}
	r.b = bus.New(r.log.logf)
	sub := r.b.Subscribe("storage", subCap, policy)
	if opt.FlushEvery == 0 {
		opt.FlushEvery = 5 * time.Millisecond
	}
	if opt.RetryMin == 0 {
		opt.RetryMin = 10 * time.Millisecond
	}
	if opt.RetryMax == 0 {
		opt.RetryMax = 40 * time.Millisecond
	}
	if opt.ShutdownGrace == 0 {
		opt.ShutdownGrace = 300 * time.Millisecond
	}
	opt.Logf = r.log.logf
	r.m = New(r.w, sub, opt)
	go func() { r.m.Run(); close(r.end) }()
	return r
}

// stop closes the bus and waits for the module to drain and finish.
func (r *rig) stop(t *testing.T) {
	t.Helper()
	r.b.Close()
	select {
	case <-r.end:
	case <-time.After(5 * time.Second):
		t.Fatal("the storage module did not finish after the bus was closed")
	}
}

func analog(pv int, deadband float64) store.PV {
	return store.PV{ID: pv, RegisterAddress: 30001 + pv, Name: fmt.Sprintf("PV%d", pv), Deadband: deadband}
}

func digital(pv int) store.PV { return store.PV{ID: pv, RegisterAddress: 10001 + pv} }

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func good(pv store.PV, v float64, sec float64) bus.Sample {
	return bus.Sample{PV: pv, Quality: store.QualityGood, Raw: uint16(v), Value: v, TS: t0.Add(time.Duration(sec * float64(time.Second)))}
}

func bad(pv store.PV, sec float64) bus.Sample {
	return bus.Sample{PV: pv, Quality: store.QualityCommFail, Error: "PLC not reachable", TS: t0.Add(time.Duration(sec * float64(time.Second)))}
}

// waitFor polls until the condition holds (or fails the test after 10 s).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the module to process the samples")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestGoodSamplesAreWrittenToHistoryAndLatest(t *testing.T) {
	r := newRig(t, 100, bus.DropOldest, Options{})
	pv := analog(1, 0)
	for i := 0; i < 10; i++ {
		r.b.Publish(good(pv, float64(i), float64(i)))
	}
	r.stop(t)

	hist, _ := r.w.snapshot()
	if len(hist) != 10 {
		t.Fatalf("history rows = %d, want 10 (deadband 0 stores every scan)", len(hist))
	}
	for i, h := range hist {
		if h.PVID != 1 || h.Value != float64(i) || !h.TS.Equal(t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("row %d = %+v: value/timestamp changed on the way", i, h)
		}
	}
	if l := r.w.latest[1]; l.Value != 9 {
		t.Errorf("pv_latest got %+v, want the newest sample (9)", l)
	}
}

func TestHistoryRulesAreThoseOfTheOriginalWorker(t *testing.T) {
	r := newRig(t, 1000, bus.DropOldest, Options{})
	dig, dead := digital(1), analog(2, 2)

	// digital: stored on change, plus a heartbeat after 60 s without change
	for i, v := range []float64{0, 0, 0, 1, 1, 0} {
		r.b.Publish(good(dig, v, float64(i)))
	}
	r.b.Publish(good(dig, 0, 70)) // unchanged for 64 s -> heartbeat row
	// analog with deadband 2: only moves >= 2 from the last stored value (+ heartbeat)
	for i, v := range []float64{10, 10.5, 11, 12.1, 12.2, 9.9, 9.8} {
		r.b.Publish(good(dead, v, float64(i)))
	}
	r.stop(t)

	hist, _ := r.w.snapshot()
	var d, a []float64
	for _, h := range hist {
		if h.PVID == 1 {
			d = append(d, h.Value)
		} else {
			a = append(a, h.Value)
		}
	}
	if fmt.Sprint(d) != "[0 1 0 0]" { // first sample, 0->1, 1->0, heartbeat
		t.Errorf("digital history = %v, want [0 1 0 0]", d)
	}
	if fmt.Sprint(a) != "[10 12.1 9.9]" { // first, +2.1, -2.2
		t.Errorf("analog history with deadband = %v, want [10 12.1 9.9]", a)
	}
}

func TestFailuresAndRecoveryEndInTheRightState(t *testing.T) {
	r := newRig(t, 100, bus.DropOldest, Options{FlushEvery: time.Hour}) // nothing flushes until stop
	pv := analog(1, 0)
	r.b.Publish(good(pv, 5, 0))
	r.b.Publish(bad(pv, 1)) // PLC fails after a good value
	r.b.Publish(good(analog(2, 0), 7, 1))
	r.b.Publish(bad(analog(2, 0), 2))
	r.b.Publish(good(analog(2, 0), 8, 3)) // PV 2 recovers within the same batch
	r.stop(t)

	if f, ok := r.w.failures[1]; !ok || f.Quality != store.QualityCommFail {
		t.Errorf("PV 1 should end as failed, failures = %+v", r.w.failures)
	}
	if l := r.w.latest[1]; l.Value != 5 {
		t.Errorf("the last good value of PV 1 must still be written: %+v", l)
	}
	if _, ok := r.w.failures[2]; ok {
		t.Error("PV 2 recovered: an older failure must not overwrite its newer good value")
	}
	if l := r.w.latest[2]; l.Value != 8 {
		t.Errorf("PV 2 latest = %+v, want 8", l)
	}
	hist, _ := r.w.snapshot()
	for _, h := range hist {
		if h.PVID == 1 && h.Value != 5 {
			t.Error("a failed read must never become a history value")
		}
	}
}

func TestASlowDatabaseDoesNotBlockPublishing(t *testing.T) {
	r := newRig(t, 100_000, bus.DropOldest, Options{FlushEvery: 20 * time.Millisecond})
	r.w.delay = 400 * time.Millisecond // every database call takes 0.4 s
	pv := analog(1, 0)

	start := time.Now()
	for i := 0; i < 5000; i++ {
		r.b.Publish(good(pv, float64(i), float64(i)))
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("publishing 5000 samples took %v while the database was slow: the scan loop would have stalled", took)
	}
	r.stop(t)
	if hist, _ := r.w.snapshot(); len(hist) != 5000 {
		t.Errorf("history rows = %d, want all 5000 once the slow database caught up", len(hist))
	}
	if d := r.m.Stats().Dropped; d != 0 {
		t.Errorf("dropped = %d, want 0", d)
	}
}

func TestTransientDatabaseFailureLosesNothingAndKeepsOrder(t *testing.T) {
	r := newRig(t, 10_000, bus.DropOldest, Options{})
	r.w.failNext = 6
	pv := analog(1, 0)
	for i := 0; i < 300; i++ {
		r.b.Publish(good(pv, float64(i), float64(i)))
		if i%50 == 0 {
			time.Sleep(15 * time.Millisecond) // spread over several flushes
		}
	}
	r.stop(t)

	hist, _ := r.w.snapshot()
	if len(hist) != 300 {
		t.Fatalf("history rows = %d, want 300", len(hist))
	}
	for i, h := range hist {
		if h.Value != float64(i) {
			t.Fatalf("row %d has value %v: order or content changed by the retries", i, h.Value)
		}
	}
	st := r.m.Stats()
	if st.WriteErrors < 6 || !st.DBUp || st.HistoryRows != 300 {
		t.Errorf("stats = %+v", st)
	}
	if !r.log.has("database write failed") || !r.log.has("database recovered") {
		t.Errorf("the failure and the recovery must be logged: %v", r.log.lines)
	}
}

func TestRetriesBackOffInsteadOfHammeringTheDatabase(t *testing.T) {
	r := newRig(t, 1000, bus.DropOldest, Options{FlushEvery: 2 * time.Millisecond, RetryMin: 50 * time.Millisecond, RetryMax: 50 * time.Millisecond})
	r.w.failNext = 1000
	r.b.Publish(good(analog(1, 0), 1, 0))
	time.Sleep(520 * time.Millisecond)
	_, calls := r.w.snapshot()
	r.w.mu.Lock()
	r.w.failNext = 0
	r.w.mu.Unlock()
	r.stop(t)
	if calls > 14 { // 520 ms / 50 ms back-off is about 10 attempts, not 260
		t.Errorf("%d database calls in half a second: the retry back-off is not applied", calls)
	}
	if calls < 3 {
		t.Errorf("only %d attempts: it must keep retrying", calls)
	}
}

func TestAPermanentErrorDropsTheBatchAndMovesOn(t *testing.T) {
	old := IsPermanent
	IsPermanent = func(err error) bool { return errors.Is(err, errPermanent) }
	defer func() { IsPermanent = old }()
	r := newRig(t, 1000, bus.DropOldest, Options{FlushEvery: 50 * time.Millisecond})
	r.w.permNext = 1
	pv := analog(1, 0)
	for i := 0; i < 5; i++ {
		r.b.Publish(good(pv, float64(i), float64(i)))
	}
	time.Sleep(120 * time.Millisecond) // first batch is rejected
	for i := 5; i < 8; i++ {
		r.b.Publish(good(pv, float64(i), float64(i)))
	}
	r.stop(t)

	st := r.m.Stats()
	hist, _ := r.w.snapshot()
	if st.Rejected == 0 || len(hist) == 0 {
		t.Fatalf("stats %+v history %d: a rejected batch must be counted and the later samples still written", st, len(hist))
	}
	if uint64(len(hist))+st.Rejected != 8 {
		t.Errorf("written %d + rejected %d != 8: every sample must be accounted for", len(hist), st.Rejected)
	}
	if !r.log.has("rejected") {
		t.Error("the rejection must be logged")
	}
}

func TestFullBufferDropsTheOldestAndSaysSo(t *testing.T) {
	r := newRig(t, 100_000, bus.DropOldest, Options{MaxPending: 200, ShutdownGrace: 100 * time.Millisecond})
	r.w.failNext = 1_000_000 // database down for the whole test
	pv := analog(1, 0)
	for i := 0; i < 1000; i++ {
		r.b.Publish(good(pv, float64(i), float64(i)))
	}
	waitFor(t, func() bool { return r.m.Stats().Received >= 1000 }) // the consumer has taken all of them
	st := r.m.Stats()
	if st.Pending > 200 {
		t.Errorf("pending = %d, the buffer must stay bounded at 200", st.Pending)
	}
	if st.Dropped < 800 {
		t.Errorf("dropped = %d, want at least 800 of 1000 counted", st.Dropped)
	}
	if !r.log.has("buffer full") {
		t.Error("an overflow must be logged as an error, never silent")
	}
	// the database comes back: the NEWEST samples are the ones that were kept
	r.w.mu.Lock()
	r.w.failNext = 0
	r.w.mu.Unlock()
	r.stop(t)
	hist, _ := r.w.snapshot()
	if len(hist) == 0 || hist[len(hist)-1].Value != 999 {
		t.Fatalf("the newest sample must survive the overflow, got %d rows", len(hist))
	}
	if uint64(len(hist))+r.m.Stats().Dropped != 1000 {
		t.Errorf("written %d + dropped %d != 1000: every sample must be accounted for", len(hist), r.m.Stats().Dropped)
	}
}

func TestShutdownWritesTheQueuedSamples(t *testing.T) {
	r := newRig(t, 10_000, bus.DropOldest, Options{FlushEvery: time.Hour})
	pv := analog(1, 0)
	for i := 0; i < 500; i++ {
		r.b.Publish(good(pv, float64(i), float64(i)))
	}
	r.stop(t) // nothing was flushed yet: the drain at shutdown must write everything
	if hist, _ := r.w.snapshot(); len(hist) != 500 {
		t.Errorf("history rows = %d, want 500", len(hist))
	}
}

func TestShutdownWithDeadDatabaseReportsExactlyWhatIsLost(t *testing.T) {
	r := newRig(t, 10_000, bus.DropOldest, Options{FlushEvery: time.Hour, ShutdownGrace: 150 * time.Millisecond})
	r.w.failNext = 1_000_000
	for i := 0; i < 40; i++ {
		r.b.Publish(good(analog(1, 0), float64(i), float64(i)))
	}
	r.stop(t)
	if !r.log.has("40 history samples that could not be written") {
		t.Errorf("the loss at shutdown must be reported with its size: %v", r.log.lines)
	}
	if r.m.Stats().Dropped != 40 {
		t.Errorf("dropped = %d, want 40", r.m.Stats().Dropped)
	}
}

func TestSamplesLostInTheBusAreDetectedFromSequenceGaps(t *testing.T) {
	r := newRig(t, 5, bus.DropOldest, Options{FlushEvery: time.Hour})
	r.w.delay = 0
	// a consumer that is not running yet: the queue overflows, as it would if storage stalled
	for i := 0; i < 20; i++ {
		r.b.Publish(good(analog(1, 0), float64(i), float64(i)))
	}
	time.Sleep(50 * time.Millisecond)
	r.b.Publish(good(analog(1, 0), 99, 99)) // storage sees a jump in Seq here (or earlier)
	r.stop(t)
	hist, _ := r.w.snapshot()
	st := r.m.Stats()
	if uint64(len(hist))+st.SeqGaps < 21 {
		t.Errorf("stored %d + detected-missing %d < 21 published: a sample vanished without being reported", len(hist), st.SeqGaps)
	}
}
