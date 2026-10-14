package alarms

import (
	"context"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"PLC_Monitoring/internal/store"
)

// ViewRepo is what the outstanding-alarm list reads (store.Store implements it).
type ViewRepo interface {
	ListAlarmDefs(ctx context.Context) ([]store.AlarmDef, error)
	ActiveOccurrences(ctx context.Context) ([]store.Occurrence, error)
}

// Entry is one outstanding alarm with the definition it belongs to.
type Entry struct {
	Occ store.Occurrence
	Def store.AlarmDef
}

// View keeps the list of outstanding alarms in memory for the alarm page. It
// is refreshed when something changes (the engine and the acknowledge/shelve
// handlers call Poke) and, as a safety net, every few seconds — so a change
// made by another process still shows up.
type View struct {
	repo    ViewRepo
	every   time.Duration
	poke    chan struct{}
	mu      sync.RWMutex
	entries []Entry
	version atomic.Uint64
	errLog  time.Time
}

func NewView(repo ViewRepo, safetyRefresh time.Duration) *View {
	if safetyRefresh <= 0 {
		safetyRefresh = 5 * time.Second
	}
	return &View{repo: repo, every: safetyRefresh, poke: make(chan struct{}, 1)}
}

// Poke asks for a refresh now; it never blocks.
func (v *View) Poke() {
	select {
	case v.poke <- struct{}{}:
	default:
	}
}

// Run refreshes until ctx ends.
func (v *View) Run(ctx context.Context) {
	t := time.NewTicker(v.every)
	defer t.Stop()
	v.refreshLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.poke:
		case <-t.C:
		}
		v.refreshLogged(ctx)
	}
}

func (v *View) refreshLogged(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := v.Refresh(c); err != nil && time.Since(v.errLog) > 10*time.Second {
		v.errLog = time.Now()
		log.Printf("alarms: cannot refresh the alarm list: %v (the last known list stays on screen)", err)
	}
}

// Refresh reads the outstanding alarms. On error the previous list is kept.
func (v *View) Refresh(ctx context.Context) error {
	defs, err := v.repo.ListAlarmDefs(ctx)
	if err != nil {
		return err
	}
	occs, err := v.repo.ActiveOccurrences(ctx)
	if err != nil {
		return err
	}
	byID := make(map[int64]store.AlarmDef, len(defs))
	for _, d := range defs {
		byID[d.ID] = d
	}
	entries := make([]Entry, 0, len(occs))
	for _, o := range occs {
		if d, ok := byID[o.AlarmID]; ok {
			entries = append(entries, Entry{Occ: o, Def: d})
		}
	}
	sortEntries(entries)

	v.mu.Lock()
	changed := !sameEntries(v.entries, entries)
	v.entries = entries
	v.mu.Unlock()
	if changed {
		v.version.Add(1)
	}
	return nil
}

// Snapshot returns the list in display order.
func (v *View) Snapshot() []Entry {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return append([]Entry(nil), v.entries...)
}

// Get returns one outstanding alarm.
func (v *View) Get(occ int64) (Entry, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, e := range v.entries {
		if e.Occ.ID == occ {
			return e, true
		}
	}
	return Entry{}, false
}

// Version increases whenever the list changes.
func (v *View) Version() uint64 { return v.version.Load() }

// Counts summarises the list by state.
type Counts struct{ Unack, Acked, Returned, Shelved int }

// NeedsAck is how many alarms wait for the operator: unacknowledged ones, and
// those that returned to normal but were never acknowledged.
func (c Counts) NeedsAck() int { return c.Unack + c.Returned }

func (v *View) Counts() Counts {
	var c Counts
	for _, e := range v.Snapshot() {
		switch e.Occ.State {
		case store.StateUnack:
			c.Unack++
		case store.StateAcked:
			c.Acked++
		case store.StateRTNUN:
			c.Returned++
		case store.StateShlvd:
			c.Shelved++
		}
	}
	return c
}

func stateRank(s string) int {
	switch s {
	case store.StateUnack:
		return 0
	case store.StateRTNUN:
		return 1
	case store.StateAcked:
		return 2
	}
	return 3 // SHLVD
}

func priorityRank(p string) int {
	switch p {
	case store.PriorityCritical:
		return 0
	case store.PriorityMajor:
		return 1
	}
	return 2
}

// sortEntries puts what needs the operator first: unacknowledged, then
// returned-to-normal-unacknowledged, then acknowledged, then shelved; inside a
// group by priority, then newest first.
func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if ra, rb := stateRank(a.Occ.State), stateRank(b.Occ.State); ra != rb {
			return ra < rb
		}
		if pa, pb := priorityRank(a.Def.Priority), priorityRank(b.Def.Priority); pa != pb {
			return pa < pb
		}
		if !a.Occ.ActivatedAt.Equal(b.Occ.ActivatedAt) {
			return a.Occ.ActivatedAt.After(b.Occ.ActivatedAt)
		}
		return a.Occ.ID > b.Occ.ID
	})
}

func sameEntries(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Occ.ID != y.Occ.ID || x.Occ.State != y.Occ.State || x.Occ.ProcessConditionActive != y.Occ.ProcessConditionActive ||
			!ptrTimeEq(x.Occ.ShelvedExpiry, y.Occ.ShelvedExpiry) || !ptrTimeEq(x.Occ.ReturnedToNormal, y.Occ.ReturnedToNormal) ||
			!ptrTimeEq(x.Occ.AckedAt, y.Occ.AckedAt) || x.Def.Priority != y.Def.Priority || x.Def.Message != y.Def.Message {
			return false
		}
	}
	return true
}

func ptrTimeEq(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
