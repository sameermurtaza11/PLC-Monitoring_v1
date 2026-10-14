package alarms

import (
	"context"
	"testing"
	"time"

	"PLC_Monitoring/internal/store"
)

type viewRepo struct {
	defs []store.AlarmDef
	occs []store.Occurrence
	err  error
}

func (r *viewRepo) ListAlarmDefs(context.Context) ([]store.AlarmDef, error) { return r.defs, r.err }
func (r *viewRepo) ActiveOccurrences(context.Context) ([]store.Occurrence, error) {
	return r.occs, r.err
}

func TestViewOrdersWhatNeedsTheOperatorFirst(t *testing.T) {
	d := func(id int64, p string) store.AlarmDef { return store.AlarmDef{ID: id, Priority: p} }
	r := &viewRepo{
		defs: []store.AlarmDef{d(1, store.PriorityMinor), d(2, store.PriorityCritical), d(3, store.PriorityMajor), d(4, store.PriorityCritical)},
		occs: []store.Occurrence{
			{ID: 10, AlarmID: 4, State: store.StateShlvd, ActivatedAt: at(0)},
			{ID: 11, AlarmID: 2, State: store.StateAcked, ActivatedAt: at(1)},
			{ID: 12, AlarmID: 1, State: store.StateUnack, ActivatedAt: at(2)},
			{ID: 13, AlarmID: 3, State: store.StateUnack, ActivatedAt: at(3)},
			{ID: 14, AlarmID: 3, State: store.StateRTNUN, ActivatedAt: at(4)},
			{ID: 15, AlarmID: 2, State: store.StateUnack, ActivatedAt: at(5)},
			{ID: 16, AlarmID: 2, State: store.StateUnack, ActivatedAt: at(6)},
		},
	}
	v := NewView(r, 0)
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, e := range v.Snapshot() {
		got = append(got, e.Occ.ID)
	}
	want := []int64{16, 15, 13, 12, 14, 11, 10} // unack (critical newest first, major, minor), returned, acked, shelved
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if c := v.Counts(); c.Unack != 4 || c.Returned != 1 || c.Acked != 1 || c.Shelved != 1 {
		t.Errorf("counts = %+v", c)
	}
}

func TestViewVersionChangesOnlyWhenTheListChanges(t *testing.T) {
	r := &viewRepo{defs: []store.AlarmDef{{ID: 1, Priority: store.PriorityMajor}},
		occs: []store.Occurrence{{ID: 1, AlarmID: 1, State: store.StateUnack, ActivatedAt: at(0)}}}
	v := NewView(r, 0)
	v.Refresh(context.Background())
	v1 := v.Version()
	v.Refresh(context.Background())
	if v.Version() != v1 {
		t.Error("version changed without a change")
	}
	r.occs[0].State = store.StateAcked
	r.occs[0].AckedAt = ptr(at(5))
	v.Refresh(context.Background())
	if v.Version() == v1 {
		t.Error("version did not change after an acknowledgement")
	}
	if e, ok := v.Get(1); !ok || e.Occ.State != store.StateAcked {
		t.Errorf("Get = %+v %v", e, ok)
	}
	if _, ok := v.Get(99); ok {
		t.Error("Get found an occurrence that does not exist")
	}
}

func TestViewKeepsTheLastListWhenTheDatabaseFails(t *testing.T) {
	r := &viewRepo{defs: []store.AlarmDef{{ID: 1}}, occs: []store.Occurrence{{ID: 1, AlarmID: 1, State: store.StateUnack}}}
	v := NewView(r, 0)
	v.Refresh(context.Background())
	r.err = context.DeadlineExceeded
	if err := v.Refresh(context.Background()); err == nil {
		t.Fatal("want the error")
	}
	if len(v.Snapshot()) != 1 {
		t.Error("the list was emptied by a database failure")
	}
}

func ptr(t time.Time) *time.Time { return &t }
