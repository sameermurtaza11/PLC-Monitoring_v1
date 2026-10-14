// Package live is the live-data module: it keeps the latest sample of every PV
// in memory, fed straight from the sample bus, and serves it to the web layer
// (dashboard rows, PV detail, server-pushed updates).
//
// It never reads pv_latest to follow the process: the database is only asked
// for the PV configuration and the last known values when the module starts
// (and after a configuration change), so a page that opens right after a
// restart already shows values.
package live

import (
	"context"
	"log"
	"sort"
	"sync"
	"sync/atomic"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/store"
)

// Source returns every enabled PV with its last known value
// (store.Store.LatestValues).
type Source func(ctx context.Context) ([]store.LatestRow, error)

// Stats describe the module; Ignored/Clients/Rejected make every refusal visible.
type Stats struct {
	Received uint64 // samples taken from the bus
	Ignored  uint64 // out-of-order samples (older than the one already held)
	PVs      int    // PVs currently held
	Version  uint64 // increases with every change
	Clients  int64  // browsers currently connected for pushed updates
	Rejected uint64 // connections refused because the client limit was reached
	Slow     uint64 // clients disconnected because they did not read in time
}

type Module struct {
	sub    *bus.Subscription
	source Source

	mu   sync.RWMutex
	rows map[int]store.LatestRow

	version               atomic.Uint64
	received, ignored     atomic.Uint64
	clients               atomic.Int64
	rejected, slowDropped atomic.Uint64
	maxClients            int64
}

// New creates the module. sub may be nil (tests that only call Refresh/Apply).
func New(source Source, sub *bus.Subscription, maxClients int) *Module {
	return &Module{sub: sub, source: source, rows: map[int]store.LatestRow{}, maxClients: int64(maxClients)}
}

// Refresh loads the PV configuration and last known values. A value already
// received from the bus is kept when it is newer than the stored one; PVs that
// are no longer enabled disappear. Call it at start-up and after every
// configuration change.
func (m *Module) Refresh(ctx context.Context) error {
	rows, err := m.source(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	next := make(map[int]store.LatestRow, len(rows))
	for _, r := range rows {
		if cur, ok := m.rows[r.ID]; ok && cur.TS != nil && (r.TS == nil || cur.TS.After(*r.TS)) {
			cur.PV, cur.PLCName, cur.ScanIntervalMS = r.PV, r.PLCName, r.ScanIntervalMS // new configuration, live value
			r = cur
		}
		next[r.ID] = r
	}
	m.rows = next
	m.mu.Unlock()
	m.version.Add(1)
	return nil
}

// Run consumes the bus subscription until the bus is closed and drained.
func (m *Module) Run() {
	for s := range m.sub.C() {
		m.Apply(s)
	}
}

// Apply merges one sample. A failed read keeps the last value (as pv_latest
// does) and only changes quality, error and time.
func (m *Module) Apply(s bus.Sample) {
	m.received.Add(1)
	m.mu.Lock()
	row, ok := m.rows[s.PV.ID]
	if ok && row.TS != nil && s.TS.Before(*row.TS) {
		m.mu.Unlock()
		m.ignored.Add(1)
		return
	}
	row.PV, row.PLCName, row.ScanIntervalMS = s.PV, s.PLCName, s.ScanIntervalMS
	ts, q := s.TS, s.Quality
	row.TS, row.Quality = &ts, &q
	if s.Good() {
		raw, val := int(s.Raw), s.Value
		row.Raw, row.Value, row.Error, row.LastGoodTS = &raw, &val, "", &ts
	} else {
		row.Error = s.Error
	}
	m.rows[s.PV.ID] = row // pointed-to values are never modified, so copies are safe to share
	m.mu.Unlock()
	m.version.Add(1)
}

// Snapshot returns all PVs ordered like the dashboard (PLC name, register).
func (m *Module) Snapshot() []store.LatestRow {
	m.mu.RLock()
	out := make([]store.LatestRow, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].PLCName != out[j].PLCName {
			return out[i].PLCName < out[j].PLCName
		}
		return out[i].RegisterAddress < out[j].RegisterAddress
	})
	return out
}

// Get returns one PV.
func (m *Module) Get(id int) (store.LatestRow, bool) {
	m.mu.RLock()
	r, ok := m.rows[id]
	m.mu.RUnlock()
	return r, ok
}

// Version changes whenever any value changes; clients compare it to decide
// whether to push.
func (m *Module) Version() uint64 { return m.version.Load() }

// Attach registers a pushed-updates client. It returns false (and counts the
// refusal) when the client limit is reached. Every true must be matched by Detach.
func (m *Module) Attach() bool {
	if m.clients.Add(1) > m.maxClients {
		m.clients.Add(-1)
		if m.rejected.Add(1) == 1 {
			log.Printf("live: client limit of %d reached, refusing new connections", m.maxClients)
		}
		return false
	}
	return true
}

func (m *Module) Detach() { m.clients.Add(-1) }

// SlowClient counts a client that was disconnected for not reading.
func (m *Module) SlowClient() { m.slowDropped.Add(1) }

func (m *Module) Stats() Stats {
	m.mu.RLock()
	n := len(m.rows)
	m.mu.RUnlock()
	return Stats{
		Received: m.received.Load(), Ignored: m.ignored.Load(), PVs: n, Version: m.version.Load(),
		Clients: m.clients.Load(), Rejected: m.rejected.Load(), Slow: m.slowDropped.Load(),
	}
}
