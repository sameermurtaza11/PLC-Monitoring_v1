// Package acquisition reads PLCs and stores the results.
//
// Layout:
//
//	Manager  — reads plc_metadata/pv_metadata, runs one Worker per enabled PLC,
//	           restarts a Worker when its configuration changes in the DB.
//	Worker   — owns one Modbus connection, scans its PVs on a timer,
//	           writes each scan to PostgreSQL.
//
// Each PLC runs in its own goroutine, so a dead PLC only affects itself.
package acquisition

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/store"
)

type Manager struct {
	store   *store.Store
	bus     *bus.Bus // every worker publishes its samples here
	reload  time.Duration
	mu      sync.Mutex       // guards workers (Run changes it, Stats reads it)
	workers map[int]*running // key: plc_id
	kick    chan struct{}    // Reload() → sync now instead of waiting for the ticker
}

type running struct {
	worker      *worker
	cancel      context.CancelFunc
	done        chan struct{}
	fingerprint string
	name        string
}

// NewManager creates the manager. The store is used only to read the PLC and
// PV configuration; samples go to the bus, never to the database from here.
func NewManager(s *store.Store, reload time.Duration, b *bus.Bus) *Manager {
	return &Manager{store: s, bus: b, reload: reload, workers: map[int]*running{}, kick: make(chan struct{}, 1)}
}

// Run blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.sync(ctx)

	ticker := time.NewTicker(m.reload)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			for id := range m.workers {
				m.stop(id)
			}
			log.Println("acquisition: stopped")
			return
		case <-ticker.C:
			m.sync(ctx)
		case <-m.kick:
			m.sync(ctx)
		}
	}
}

// Reload asks the manager to re-read the configuration now (called by the
// web UI after a PLC/PV is saved). Never blocks.
func (m *Manager) Reload() {
	select {
	case m.kick <- struct{}{}:
	default: // a reload is already pending
	}
}

// sync makes the running workers match the database.
func (m *Manager) sync(ctx context.Context) {
	plcs, err := m.store.LoadAcquisitionConfig(ctx)
	if err != nil {
		// Keep the current workers running with their last known config.
		log.Printf("acquisition: config reload failed, keeping current config: %v", err)
		return
	}

	seen := map[int]bool{}
	for _, p := range plcs {
		seen[p.ID] = true
		fp := fingerprint(p)

		if w, ok := m.workers[p.ID]; ok {
			if w.fingerprint == fp {
				continue
			}
			log.Printf("acquisition: [%s] configuration changed, restarting worker", p.Name)
			m.stop(p.ID)
		}
		m.start(ctx, p, fp)
	}

	for id, w := range m.workers {
		if !seen[id] {
			log.Printf("acquisition: [%s] removed or disabled, stopping worker", w.name)
			m.stop(id)
		}
	}
}

func (m *Manager) start(parent context.Context, p store.PLC, fp string) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	w := newWorker(p, m.bus)
	m.mu.Lock()
	m.workers[p.ID] = &running{worker: w, cancel: cancel, done: done, fingerprint: fp, name: p.Name}
	m.mu.Unlock()
	go func() {
		defer close(done)
		w.run(ctx)
	}()
}

func (m *Manager) stop(id int) {
	w := m.workers[id]
	w.cancel()
	<-w.done
	m.mu.Lock()
	delete(m.workers, id)
	m.mu.Unlock()
}

// Stats returns the scan statistics of the running workers (safe to call
// from any goroutine).
func (m *Manager) Stats() []WorkerStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WorkerStats, 0, len(m.workers))
	for _, r := range m.workers {
		out = append(out, r.worker.Stats())
	}
	return out
}

// fingerprint changes whenever any field of the PLC or any of its PVs changes.
// JSON is used (not %v) because it follows pointers such as AlarmHigh.
func fingerprint(p store.PLC) string {
	b, _ := json.Marshal(p)
	return string(b)
}
