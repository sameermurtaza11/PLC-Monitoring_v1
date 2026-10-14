package acquisition

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/plc"
	"PLC_Monitoring/internal/store"
)

const maxBackoff = 30 * time.Second

// reader is the part of a PLC connection a worker uses. *plc.Client is the
// real one; tests use a fake.
type reader interface {
	Connect() error
	Connected() bool
	Close()
	ReadBlock(b plc.Block) ([]uint16, error)
}

// WorkerStats is a snapshot of one PLC worker.
type WorkerStats struct {
	PLC         string
	Scans       uint64        // completed scan cycles
	Overruns    uint64        // scans that took longer than the scan interval
	MissedTicks uint64        // scan ticks that could not run because the previous scan was still busy
	LastScan    time.Duration // duration of the last scan
	Published   uint64        // samples handed to the bus
	Connected   bool
}

// worker owns one PLC connection and scans it on a timer. It only READS the
// PLC and PUBLISHES samples on the bus: it never waits for the database, the
// alarm engine or the browser, so a slow consumer cannot delay the next scan.
type worker struct {
	cfg    store.PLC
	bus    *bus.Bus
	client reader
	blocks []plc.Block

	backoff  time.Duration
	nextDial time.Time
	commDown bool // to log connection loss once, not every scan

	scans, overruns, missed, published atomic.Uint64
	lastScanNS                         atomic.Int64
	connected                          atomic.Bool
	lastOverrunLog                     time.Time
}

func newWorker(cfg store.PLC, b *bus.Bus) *worker {
	return newWorkerWith(cfg, b, plc.NewClient(cfg.IP, cfg.Port, cfg.UnitID, time.Duration(cfg.TimeoutMS)*time.Millisecond))
}

func newWorkerWith(cfg store.PLC, b *bus.Bus, client reader) *worker {
	w := &worker{cfg: cfg, bus: b, client: client, backoff: time.Second}

	addrs := make([]int, len(cfg.PVs))
	for i, pv := range cfg.PVs {
		addrs[i] = pv.RegisterAddress
	}
	blocks, invalid := plc.BuildBlocks(addrs)
	for i, err := range invalid {
		log.Printf("acquisition: [%s] PV %s skipped: %v", cfg.Name, cfg.PVs[i].Name, err)
	}
	w.blocks = blocks
	return w
}

func (w *worker) logf(format string, args ...any) {
	log.Printf("acquisition: ["+w.cfg.Name+"] "+format, args...)
}

// Stats returns a snapshot; safe to call from any goroutine.
func (w *worker) Stats() WorkerStats {
	return WorkerStats{
		PLC: w.cfg.Name, Scans: w.scans.Load(), Overruns: w.overruns.Load(), MissedTicks: w.missed.Load(),
		LastScan: time.Duration(w.lastScanNS.Load()), Published: w.published.Load(), Connected: w.connected.Load(),
	}
}

func (w *worker) run(ctx context.Context) {
	defer w.client.Close()

	if len(w.cfg.PVs) == 0 {
		w.logf("no enabled PVs configured, idle")
		<-ctx.Done()
		return
	}

	interval := time.Duration(w.cfg.ScanIntervalMS) * time.Millisecond
	w.logf("started: %s:%d unit %d, %d PVs in %d read(s), every %v",
		w.cfg.IP, w.cfg.Port, w.cfg.UnitID, len(w.cfg.PVs), len(w.blocks), interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		start := time.Now()
		w.scan(ctx)
		w.recordScan(time.Since(start), interval)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recordScan keeps the scan statistics and reports a scan that took longer
// than the interval: the ticks that passed meanwhile are missed scans.
func (w *worker) recordScan(took, interval time.Duration) {
	w.scans.Add(1)
	w.lastScanNS.Store(int64(took))
	if took <= interval {
		return
	}
	missed := uint64(took / interval)
	w.overruns.Add(1)
	w.missed.Add(missed)
	if now := time.Now(); w.lastOverrunLog.IsZero() || now.Sub(w.lastOverrunLog) >= 30*time.Second {
		w.lastOverrunLog = now
		w.logf("scan took %v, longer than the %v interval: about %d scan(s) missed (slow PLC or network); %d overruns so far",
			took.Round(time.Millisecond), interval, missed, w.overruns.Load())
	}
}

func (w *worker) publish(s bus.Sample) {
	w.bus.Publish(s)
	w.published.Add(1)
}

// sampleOf starts a sample of one PV with the metadata every consumer needs.
func (w *worker) sampleOf(pv store.PV, ts time.Time) bus.Sample {
	return bus.Sample{PLCID: w.cfg.ID, PLCName: w.cfg.Name, ScanIntervalMS: w.cfg.ScanIntervalMS, PV: pv, TS: ts}
}

func (w *worker) failed(pv store.PV, ts time.Time, quality, msg string) bus.Sample {
	s := w.sampleOf(pv, ts)
	s.Quality, s.Error = quality, msg
	return s
}

// scan reads all blocks once and publishes one sample per PV.
func (w *worker) scan(ctx context.Context) {
	if !w.ensureConnected() {
		// Still unreachable: every PV of this PLC is COMM_FAIL (consumers show
		// it), and an alarm engine must not read this as a normal value.
		now := time.Now().UTC()
		for _, pv := range w.cfg.PVs {
			w.publish(w.failed(pv, now, store.QualityCommFail, "PLC not reachable"))
		}
		return
	}

	for bi, b := range w.blocks {
		values, err := w.client.ReadBlock(b)
		ts := time.Now().UTC() // timestamp taken when the PLC answered

		if err != nil {
			quality := store.QualityReadFail
			if !plc.IsModbusException(err) {
				// Network-level failure: drop the socket and reconnect next scan.
				quality = store.QualityCommFail
				w.client.Close()
				w.connected.Store(false)
				w.logf("connection lost: %v", err)
				w.commDown = true
			} else {
				w.logf("read %s %d+%d failed: %v", b.Kind, b.Start, b.Count, err)
			}
			for _, rest := range w.blocks[bi:] { // this block and (if comm lost) the rest
				for _, idx := range rest.Items {
					w.publish(w.failed(w.cfg.PVs[idx], ts, quality, err.Error()))
				}
				if quality == store.QualityReadFail {
					break // only this block failed; continue with the next
				}
			}
			if quality == store.QualityCommFail {
				break
			}
			continue
		}

		for _, idx := range b.Items {
			pv := w.cfg.PVs[idx]
			_, off, _ := plc.ParseAddress(pv.RegisterAddress)
			raw := values[off-b.Start]
			value := float64(raw) // digital input: 0/1, no scaling
			if !pv.IsDigital() {
				value = Scale(raw, pv.Min, pv.Max)
			}
			s := w.sampleOf(pv, ts)
			s.Quality, s.Raw, s.Value = store.QualityGood, raw, value
			w.publish(s)
		}
	}
}

// ensureConnected returns true if the client is connected, dialling with
// exponential backoff (1s, 2s, 4s … 30s) while the PLC is unreachable.
func (w *worker) ensureConnected() bool {
	if w.client.Connected() {
		w.connected.Store(true)
		return true
	}
	w.connected.Store(false)
	if time.Now().Before(w.nextDial) {
		return false
	}
	if err := w.client.Connect(); err != nil {
		if !w.commDown {
			w.logf("cannot connect to %s:%d: %v", w.cfg.IP, w.cfg.Port, err)
		}
		w.commDown = true
		w.nextDial = time.Now().Add(w.backoff)
		w.backoff = min(w.backoff*2, maxBackoff)
		return false
	}
	if w.commDown {
		w.logf("connection restored")
	} else {
		w.logf("connected")
	}
	w.commDown = false
	w.backoff = time.Second
	w.connected.Store(true)
	return true
}
