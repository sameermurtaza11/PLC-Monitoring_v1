// Package bus distributes every acquired PV sample from the acquisition
// module to its consumers (storage, live data, alarms), inside the process.
//
// The one rule: Publish NEVER blocks. A slow or stuck consumer must not slow
// the PLC scan loop, so each consumer has its own bounded queue and an
// explicit overload policy. Nothing is dropped silently: every dropped sample
// is counted per consumer, and the first drop and then a summary at most every
// few seconds are logged.
package bus

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"PLC_Monitoring/internal/store"
)

// Sample is one acquired reading of one PV (good or failed).
type Sample struct {
	// Seq numbers the samples of the bus in the order they were published.
	// A consumer that sees a gap in Seq knows samples were dropped before it.
	Seq uint64

	PLCID          int
	PLCName        string
	ScanIntervalMS int
	PV             store.PV // the PV's metadata as configured when it was read

	Raw     uint16    // raw register word (0/1 for a discrete input); valid when Quality is GOOD
	Value   float64   // engineering value (scaled); valid when Quality is GOOD
	Quality string    // store.QualityGood | QualityCommFail | QualityReadFail
	Error   string    // why the read failed (empty when good)
	TS      time.Time // acquisition time: when the PLC answered (or the read failed)
}

// Good reports whether the sample carries a trustworthy value.
func (s Sample) Good() bool { return s.Quality == store.QualityGood }

// Policy is what a subscription does when its queue is full.
type Policy int

const (
	// DropOldest discards the oldest queued sample to make room: the consumer
	// ends up with the NEWEST data (live view, alarm evaluation).
	DropOldest Policy = iota
	// DropNewest discards the incoming sample: the consumer keeps what it already has.
	DropNewest
)

func (p Policy) String() string {
	if p == DropNewest {
		return "drop newest"
	}
	return "drop oldest"
}

// Stats is a snapshot of one subscription.
type Stats struct {
	Name      string
	Policy    string
	Capacity  int
	Queued    int
	Published uint64 // samples offered to this subscription
	Dropped   uint64 // samples discarded because the queue was full
	LastDrop  time.Time
}

// Subscription is one consumer's view of the bus.
type Subscription struct {
	name   string
	policy Policy
	ch     chan Sample

	published atomic.Uint64
	dropped   atomic.Uint64

	mu              sync.Mutex
	lastDrop        time.Time
	lastLogged      time.Time
	droppedSinceLog uint64
}

// C delivers the samples. It is closed by Bus.Close once everything queued
// has been delivered.
func (s *Subscription) C() <-chan Sample { return s.ch }

func (s *Subscription) Stats() Stats {
	s.mu.Lock()
	last := s.lastDrop
	s.mu.Unlock()
	return Stats{
		Name: s.name, Policy: s.policy.String(), Capacity: cap(s.ch), Queued: len(s.ch),
		Published: s.published.Load(), Dropped: s.dropped.Load(), LastDrop: last,
	}
}

// Bus fans every published sample out to all subscriptions.
type Bus struct {
	mu     sync.RWMutex
	subs   []*Subscription
	closed bool
	seq    atomic.Uint64
	logf   func(format string, args ...any)
	now    func() time.Time
}

// New creates a bus. logf may be nil (the standard logger is used).
func New(logf func(format string, args ...any)) *Bus {
	if logf == nil {
		logf = log.Printf
	}
	return &Bus{logf: logf, now: time.Now}
}

// dropLogInterval limits drop messages: the first drop is logged at once, then
// a summary at most this often while drops continue.
const dropLogInterval = 5 * time.Second

// Subscribe adds a consumer. Call it before the first Publish.
func (b *Bus) Subscribe(name string, capacity int, policy Policy) *Subscription {
	if capacity < 1 {
		capacity = 1
	}
	s := &Subscription{name: name, policy: policy, ch: make(chan Sample, capacity)}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()
	return s
}

// Publish offers the sample to every subscription. It never blocks and never
// waits for a consumer. It returns the sequence number given to the sample.
func (b *Bus) Publish(smp Sample) uint64 {
	smp.Seq = b.seq.Add(1)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return smp.Seq
	}
	for _, s := range b.subs {
		s.published.Add(1)
		select {
		case s.ch <- smp:
			continue
		default:
		}
		// Queue full.
		switch s.policy {
		case DropNewest:
			s.recordDrop(b, 1)
		default: // DropOldest
			select {
			case <-s.ch: // make room by discarding the oldest queued sample
				s.recordDrop(b, 1)
			default: // the consumer just drained it
			}
			select {
			case s.ch <- smp:
			default: // still full (another publisher won the slot): count the new one instead
				s.recordDrop(b, 1)
			}
		}
	}
	return smp.Seq
}

func (s *Subscription) recordDrop(b *Bus, n uint64) {
	s.dropped.Add(n)
	now := b.now()
	s.mu.Lock()
	s.lastDrop = now
	s.droppedSinceLog += n
	logNow := s.lastLogged.IsZero() || now.Sub(s.lastLogged) >= dropLogInterval
	var pending uint64
	if logNow {
		s.lastLogged = now
		pending = s.droppedSinceLog
		s.droppedSinceLog = 0
	}
	s.mu.Unlock()
	if logNow {
		b.logf("bus: ERROR consumer %q cannot keep up: %d sample(s) dropped (%s, queue %d, %d dropped in total)",
			s.name, pending, s.policy, cap(s.ch), s.dropped.Load())
	}
}

// Stats returns a snapshot of every subscription.
func (b *Bus) Stats() []Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Stats, len(b.subs))
	for i, s := range b.subs {
		out[i] = s.Stats()
	}
	return out
}

// Close ends the bus after the publishers have stopped. Each subscription's
// channel is closed once its queued samples have been read, so consumers can
// drain and finish.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, s := range b.subs {
		close(s.ch)
	}
}
