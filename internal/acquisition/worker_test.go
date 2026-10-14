package acquisition

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goburrow/modbus"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/plc"
	"PLC_Monitoring/internal/storage"
	"PLC_Monitoring/internal/store"
)

// fakePLC is a reader with scripted behaviour.
type fakePLC struct {
	mu         sync.Mutex
	connected  bool
	connectErr error
	data       map[uint16]uint16 // register words by offset
	bits       map[uint16]uint16 // discrete input bits (0/1) by offset
	readErr    func(b plc.Block) error
	delay      time.Duration // per ReadBlock
	closed     int
	connects   int
}

func (f *fakePLC) Connect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connected = true
	return nil
}
func (f *fakePLC) Connected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connected }
func (f *fakePLC) Close()          { f.mu.Lock(); f.connected = false; f.closed++; f.mu.Unlock() }
func (f *fakePLC) ReadBlock(b plc.Block) ([]uint16, error) {
	f.mu.Lock()
	delay, rerr := f.delay, f.readErr
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if rerr != nil {
		if err := rerr(b); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint16, b.Count)
	for i := range out {
		if b.Kind == plc.DiscreteInput {
			out[i] = f.bits[b.Start+uint16(i)]
		} else {
			out[i] = f.data[b.Start+uint16(i)]
		}
	}
	return out, nil
}

func plcCfg(name string, id int, interval int, pvs ...store.PV) store.PLC {
	return store.PLC{ID: id, Name: name, IP: "127.0.0.1", Port: 502, UnitID: 1, ScanIntervalMS: interval, TimeoutMS: 100, PVs: pvs}
}

func apv(id, reg int) store.PV {
	return store.PV{ID: id, PLCID: 1, RegisterAddress: reg, Name: "PV", Min: 0, Max: 100}
}

func drain(c <-chan bus.Sample, n int, within time.Duration) []bus.Sample {
	var out []bus.Sample
	deadline := time.After(within)
	for len(out) < n {
		select {
		case s := <-c:
			out = append(out, s)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestScanPublishesScaledSamplesWithTimestampAndMetadata(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	sub := b.Subscribe("test", 100, bus.DropOldest)
	f := &fakePLC{data: map[uint16]uint16{0: 32768, 1: 65535}, bits: map[uint16]uint16{0: 1}}
	dig := store.PV{ID: 7, RegisterAddress: 10001, Name: "XS101", State0Text: "OFF", State1Text: "ON"}
	w := newWorkerWith(plcCfg("PLC-1", 1, 1000, apv(1, 30001), apv(2, 30002), dig), b, f)

	before := time.Now()
	w.scan(context.Background())
	after := time.Now()

	got := drain(sub.C(), 3, time.Second)
	if len(got) != 3 {
		t.Fatalf("%d samples, want 3 (one per PV)", len(got))
	}
	byPV := map[int]bus.Sample{}
	for _, s := range got {
		byPV[s.PV.ID] = s
		if s.PLCName != "PLC-1" || s.PLCID != 1 || s.ScanIntervalMS != 1000 {
			t.Errorf("PLC metadata missing: %+v", s)
		}
		if s.TS.Before(before.UTC().Add(-time.Second)) || s.TS.After(after.UTC().Add(time.Second)) {
			t.Errorf("acquisition timestamp %v is not from this scan", s.TS)
		}
		if !s.Good() || s.Error != "" {
			t.Errorf("quality %q error %q", s.Quality, s.Error)
		}
	}
	if want := Scale(32768, 0, 100); byPV[1].Value != want || byPV[1].Raw != 32768 {
		t.Errorf("analog sample = %+v, want raw 32768 scaled to %v", byPV[1], want)
	}
	if byPV[2].Value != Scale(65535, 0, 100) {
		t.Errorf("second PV value = %v", byPV[2].Value)
	}
	if byPV[7].Value != 1 || byPV[7].Raw != 1 || byPV[7].PV.Name != "XS101" || byPV[7].PV.State1Text != "ON" {
		t.Errorf("digital sample = %+v: value must stay 0/1 (no scaling) and carry the PV metadata", byPV[7])
	}
}

func TestAnUnreachablePLCGivesCommFailForEveryPV(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	sub := b.Subscribe("test", 100, bus.DropOldest)
	f := &fakePLC{connectErr: errors.New("connection refused")}
	w := newWorkerWith(plcCfg("PLC-1", 1, 1000, apv(1, 30001), apv(2, 30002)), b, f)
	w.scan(context.Background())

	got := drain(sub.C(), 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("%d samples, want 2", len(got))
	}
	for _, s := range got {
		if s.Quality != store.QualityCommFail || s.Error == "" || s.Good() {
			t.Errorf("sample = %+v: a PLC that cannot be reached must be COMM_FAIL, never a value", s)
		}
	}
}

func TestANetworkErrorMidScanFailsTheRestAndReconnects(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	sub := b.Subscribe("test", 100, bus.DropOldest)
	f := &fakePLC{data: map[uint16]uint16{}}
	f.readErr = func(plc.Block) error { return errors.New("i/o timeout") }           // not a Modbus exception
	w := newWorkerWith(plcCfg("PLC-1", 1, 1000, apv(1, 30001), apv(2, 30100)), b, f) // two separate blocks
	w.scan(context.Background())

	got := drain(sub.C(), 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("%d samples, want 2 (both PVs failed)", len(got))
	}
	for _, s := range got {
		if s.Quality != store.QualityCommFail {
			t.Errorf("quality %q, want COMM_FAIL", s.Quality)
		}
	}
	if f.closed != 1 || f.Connected() {
		t.Error("after a network error the socket must be closed so the next scan reconnects")
	}
}

func TestAModbusExceptionOnlyFailsItsOwnBlock(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	sub := b.Subscribe("test", 100, bus.DropOldest)
	f := &fakePLC{data: map[uint16]uint16{0: 1000}}
	f.readErr = func(blk plc.Block) error {
		if blk.Start >= 50 { // the second block answers with an "illegal address" exception
			return &modbus.ModbusError{FunctionCode: 4, ExceptionCode: 2}
		}
		return nil
	}
	w := newWorkerWith(plcCfg("PLC-1", 1, 1000, apv(1, 30001), apv(2, 30100)), b, f)
	w.scan(context.Background())

	got := drain(sub.C(), 2, time.Second)
	q := map[int]string{}
	for _, s := range got {
		q[s.PV.ID] = s.Quality
	}
	if q[1] != store.QualityGood || q[2] != store.QualityReadFail {
		t.Errorf("qualities = %v, want PV 1 GOOD and PV 2 READ_FAIL", q)
	}
	if f.closed != 0 {
		t.Error("a Modbus exception must keep the connection")
	}
}

func TestOnePLCFailureDoesNotStopAlarmOrStorageOfTheOthers(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	sub := b.Subscribe("test", 10_000, bus.DropOldest)
	good := &fakePLC{data: map[uint16]uint16{0: 20000}}
	dead := &fakePLC{connectErr: errors.New("connection refused")}
	wa := newWorkerWith(plcCfg("PLC-A", 1, 10, apv(1, 30001)), b, good)
	wb := newWorkerWith(plcCfg("PLC-B", 2, 10, apv(2, 30001)), b, dead)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, w := range []*worker{wa, wb} {
		wg.Add(1)
		go func(w *worker) { defer wg.Done(); w.run(ctx) }(w)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	wg.Wait()
	b.Close()

	var goodA, failB int
	for s := range sub.C() {
		switch {
		case s.PLCName == "PLC-A" && s.Good():
			goodA++
		case s.PLCName == "PLC-B" && s.Quality == store.QualityCommFail:
			failB++
		}
	}
	if goodA < 10 {
		t.Errorf("PLC-A delivered only %d good samples in 300 ms: it was held up by PLC-B being down", goodA)
	}
	if failB < 10 {
		t.Errorf("PLC-B reported only %d COMM_FAIL samples; a dead PLC must keep reporting its failure", failB)
	}
}

// slowDB is a database that takes a long time for every write.
type slowDB struct {
	mu    sync.Mutex
	rows  int
	delay time.Duration
}

func (d *slowDB) WriteScan(_ context.Context, _, history []store.Sample, _ []store.Failure) error {
	time.Sleep(d.delay)
	d.mu.Lock()
	d.rows += len(history)
	d.mu.Unlock()
	return nil
}

func TestAcquisitionKeepsItsRhythmWhileTheDatabaseIsSlowAndConsumersAreStuck(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	b.Subscribe("alarm-stuck", 1, bus.DropOldest) // a consumer that never reads
	b.Subscribe("live-stuck", 1, bus.DropOldest)
	db := &slowDB{delay: 600 * time.Millisecond} // every database call takes 0.6 s
	st := storage.New(db, b.Subscribe("storage", 10_000, bus.DropOldest), storage.Options{
		FlushEvery: 20 * time.Millisecond, Logf: func(string, ...any) {}})
	storageDone := make(chan struct{})
	go func() { st.Run(); close(storageDone) }()

	f := &fakePLC{data: map[uint16]uint16{0: 1234}}
	w := newWorkerWith(plcCfg("PLC-1", 1, 10, apv(1, 30001), apv(2, 30002)), b, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	time.Sleep(1 * time.Second)
	cancel()
	<-done

	stats := w.Stats()
	// Scans every 10 ms for 1 s: about 100. If the loop had waited for the 0.6 s
	// database it would have managed a handful.
	if stats.Scans < 60 {
		t.Fatalf("only %d scans in 1 s (expected about 100): the scan loop was held up by a slow consumer", stats.Scans)
	}
	// A 10 ms rhythm can be disturbed by the test machine itself (other test
	// packages run in parallel), so a few overruns are tolerated; a scan loop held
	// up by the 0.6 s database would be caught by the scan count above.
	if stats.Overruns > 3 {
		t.Errorf("%d scan overruns: scans repeatedly took longer than the interval", stats.Overruns)
	}
	if stats.Published != stats.Scans*2 {
		t.Errorf("published %d samples for %d scans of 2 PVs", stats.Published, stats.Scans)
	}

	// Afterwards the slow database catches up: nothing was lost on the way to storage.
	b.Close()
	select {
	case <-storageDone:
	case <-time.After(30 * time.Second):
		t.Fatal("storage did not finish")
	}
	db.mu.Lock()
	rows := db.rows
	db.mu.Unlock()
	if uint64(rows) != stats.Published {
		t.Errorf("database got %d rows, acquisition published %d: samples were lost between them", rows, stats.Published)
	}
	for _, s := range b.Stats() {
		if s.Name == "storage" && s.Dropped != 0 {
			t.Errorf("storage queue dropped %d samples", s.Dropped)
		}
		if s.Name == "alarm-stuck" && s.Dropped == 0 {
			t.Error("the stuck consumers must have been dropped from (and counted), not waited for")
		}
	}
}

func TestASlowScanIsCountedAndReported(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	b.Subscribe("x", 1000, bus.DropOldest)
	f := &fakePLC{data: map[uint16]uint16{}, delay: 120 * time.Millisecond} // a slow PLC
	w := newWorkerWith(plcCfg("PLC-1", 1, 40, apv(1, 30001)), b, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	s := w.Stats()
	if s.Overruns == 0 || s.MissedTicks == 0 {
		t.Errorf("stats = %+v: scans of 120 ms on a 40 ms interval must be counted as overruns and missed ticks", s)
	}
	if s.LastScan < 100*time.Millisecond {
		t.Errorf("last scan duration = %v, want about 120 ms", s.LastScan)
	}
}
