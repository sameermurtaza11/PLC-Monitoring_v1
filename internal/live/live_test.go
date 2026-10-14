package live

import (
	"context"
	"errors"
	"testing"
	"time"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/store"
)

func pv(id int, plc string, addr int) store.PV {
	return store.PV{ID: id, Name: "PV", RegisterAddress: addr, Min: 0, Max: 100}
}

func good(id int, plc string, addr int, v float64, ts time.Time) bus.Sample {
	return bus.Sample{PLCName: plc, ScanIntervalMS: 1000, PV: pv(id, plc, addr), Quality: store.QualityGood, Raw: 7, Value: v, TS: ts}
}

func rowsOf(rs ...store.LatestRow) Source {
	return func(context.Context) ([]store.LatestRow, error) { return rs, nil }
}

func TestSampleUpdatesTheValueWithoutTheDatabase(t *testing.T) {
	m := New(func(context.Context) ([]store.LatestRow, error) { t.Fatal("database was read"); return nil, nil }, nil, 10)
	now := time.Now()
	v0 := m.Version()
	m.Apply(good(1, "A", 30001, 42.5, now))
	r, ok := m.Get(1)
	if !ok || r.Value == nil || *r.Value != 42.5 || *r.Quality != store.QualityGood || r.LastGoodTS == nil {
		t.Fatalf("row = %+v", r)
	}
	if m.Version() == v0 {
		t.Error("version did not change")
	}
}

func TestFailedReadKeepsTheLastValueAndShowsTheQuality(t *testing.T) {
	m := New(rowsOf(), nil, 10)
	t0 := time.Now()
	m.Apply(good(1, "A", 30001, 42.5, t0))
	f := good(1, "A", 30001, 0, t0.Add(time.Second))
	f.Quality, f.Error = store.QualityCommFail, "PLC not reachable"
	m.Apply(f)
	r, _ := m.Get(1)
	if *r.Quality != store.QualityCommFail || r.Error != "PLC not reachable" || *r.Value != 42.5 || !r.LastGoodTS.Equal(t0) {
		t.Fatalf("row = %+v value=%v", r, *r.Value)
	}
	m.Apply(good(1, "A", 30001, 43, t0.Add(2*time.Second)))
	if r, _ = m.Get(1); r.Error != "" || *r.Quality != store.QualityGood {
		t.Errorf("recovery not shown: %+v", r)
	}
}

func TestOlderSampleIsIgnoredAndCounted(t *testing.T) {
	m := New(rowsOf(), nil, 10)
	t0 := time.Now()
	m.Apply(good(1, "A", 30001, 2, t0))
	m.Apply(good(1, "A", 30001, 1, t0.Add(-time.Second)))
	if r, _ := m.Get(1); *r.Value != 2 {
		t.Errorf("value = %v, want 2", *r.Value)
	}
	if st := m.Stats(); st.Ignored != 1 || st.Received != 2 {
		t.Errorf("stats = %+v", st)
	}
}

func TestSnapshotIsOrderedByPLCAndRegister(t *testing.T) {
	m := New(rowsOf(), nil, 10)
	now := time.Now()
	m.Apply(good(3, "B", 30001, 1, now))
	m.Apply(good(2, "A", 30005, 1, now))
	m.Apply(good(1, "A", 30001, 1, now))
	var ids []int
	for _, r := range m.Snapshot() {
		ids = append(ids, r.ID)
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Errorf("order = %v, want [1 2 3]", ids)
	}
}

func TestRefreshLoadsInitialValuesKeepsNewerLiveOnesAndDropsRemovedPVs(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	v := 10.0
	q := store.QualityGood
	dbRow := func(id int, ts time.Time) store.LatestRow {
		return store.LatestRow{PV: pv(id, "A", 30000+id), PLCName: "A", ScanIntervalMS: 1000, Value: &v, Quality: &q, TS: &ts}
	}
	rows := []store.LatestRow{dbRow(1, old), dbRow(2, old)}
	m := New(func(context.Context) ([]store.LatestRow, error) { return rows, nil }, nil, 10)

	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, ok := m.Get(1); !ok || *r.Value != 10 {
		t.Fatalf("initial value missing: %+v", r)
	}
	m.Apply(good(1, "A", 30001, 99, time.Now())) // newer than the stored value
	m.Apply(good(5, "A", 30005, 5, time.Now()))  // will be removed from the configuration

	rows = []store.LatestRow{dbRow(1, old), dbRow(2, old)}
	rows[0].Name = "renamed"
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Get(1)
	if *r.Value != 99 || r.Name != "renamed" {
		t.Errorf("want live value 99 with new configuration, got %v %q", *r.Value, r.Name)
	}
	if _, ok := m.Get(5); ok {
		t.Error("PV removed from the configuration is still shown")
	}
}

func TestRefreshFailureKeepsWhatIsShown(t *testing.T) {
	fail := false
	m := New(func(context.Context) ([]store.LatestRow, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return nil, nil
	}, nil, 10)
	m.Apply(good(1, "A", 30001, 1, time.Now()))
	fail = true
	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("want the error")
	}
	if _, ok := m.Get(1); !ok {
		t.Error("value lost when the database was unavailable")
	}
}

func TestRunConsumesTheBusAndStopsWhenItIsClosed(t *testing.T) {
	b := bus.New(func(string, ...any) {})
	m := New(rowsOf(), b.Subscribe("live", 100, bus.DropOldest), 10)
	done := make(chan struct{})
	go func() { m.Run(); close(done) }()
	for i := 1; i <= 50; i++ {
		b.Publish(good(1, "A", 30001, float64(i), time.Now().Add(time.Duration(i)*time.Millisecond)))
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the bus was closed")
	}
	if r, _ := m.Get(1); *r.Value != 50 {
		t.Errorf("last value = %v, want 50", *r.Value)
	}
}

func TestClientLimitRefusesAndCounts(t *testing.T) {
	m := New(rowsOf(), nil, 2)
	if !m.Attach() || !m.Attach() {
		t.Fatal("first two clients must be accepted")
	}
	if m.Attach() {
		t.Fatal("third client must be refused")
	}
	if st := m.Stats(); st.Clients != 2 || st.Rejected != 1 {
		t.Errorf("stats = %+v", st)
	}
	m.Detach()
	if !m.Attach() {
		t.Error("a freed slot must be usable again")
	}
}
