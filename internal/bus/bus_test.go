package bus

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"PLC_Monitoring/internal/store"
)

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *logs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func smp(pv int) Sample {
	return Sample{PV: store.PV{ID: pv}, Quality: store.QualityGood, TS: time.Now()}
}

func TestEverySubscriberReceivesEverySample(t *testing.T) {
	b := New((&logs{}).logf)
	a := b.Subscribe("a", 10, DropOldest)
	c := b.Subscribe("c", 10, DropOldest)
	for i := 1; i <= 5; i++ {
		b.Publish(smp(i))
	}
	for _, s := range []*Subscription{a, c} {
		for i := 1; i <= 5; i++ {
			got := <-s.C()
			if got.PV.ID != i || got.Seq != uint64(i) {
				t.Fatalf("%s: got pv %d seq %d, want %d", s.name, got.PV.ID, got.Seq, i)
			}
		}
	}
}

func TestPublishNeverBlocksEvenWithAStuckConsumer(t *testing.T) {
	b := New((&logs{}).logf)
	b.Subscribe("stuck", 4, DropOldest) // nobody ever reads it

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100_000; i++ {
			b.Publish(smp(i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a consumer that does not read: the acquisition loop would stall")
	}
}

func TestAStuckConsumerDoesNotAffectTheOthers(t *testing.T) {
	b := New((&logs{}).logf)
	b.Subscribe("stuck", 2, DropOldest)
	healthy := b.Subscribe("healthy", 1000, DropOldest)
	for i := 1; i <= 500; i++ {
		b.Publish(smp(i))
	}
	for i := 1; i <= 500; i++ {
		if got := <-healthy.C(); got.PV.ID != i {
			t.Fatalf("healthy consumer lost or reordered a sample: got %d, want %d", got.PV.ID, i)
		}
	}
	if d := healthy.Stats().Dropped; d != 0 {
		t.Errorf("healthy consumer dropped %d samples because of the stuck one", d)
	}
}

func TestDropOldestKeepsTheNewestAndCountsEveryDrop(t *testing.T) {
	l := &logs{}
	b := New(l.logf)
	s := b.Subscribe("live", 3, DropOldest)
	for i := 1; i <= 10; i++ {
		b.Publish(smp(i))
	}
	st := s.Stats()
	if st.Dropped != 7 || st.Published != 10 || st.Queued != 3 {
		t.Fatalf("stats = %+v, want dropped 7, published 10, queued 3", st)
	}
	for _, want := range []int{8, 9, 10} { // the newest three survive
		if got := <-s.C(); got.PV.ID != want {
			t.Errorf("got %d, want %d", got.PV.ID, want)
		}
	}
	if st.LastDrop.IsZero() {
		t.Error("the time of the last drop must be recorded")
	}
}

func TestDropNewestKeepsWhatIsQueued(t *testing.T) {
	b := New((&logs{}).logf)
	s := b.Subscribe("x", 3, DropNewest)
	for i := 1; i <= 10; i++ {
		b.Publish(smp(i))
	}
	if st := s.Stats(); st.Dropped != 7 {
		t.Fatalf("dropped = %d, want 7", st.Dropped)
	}
	for _, want := range []int{1, 2, 3} {
		if got := <-s.C(); got.PV.ID != want {
			t.Errorf("got %d, want %d", got.PV.ID, want)
		}
	}
}

func TestDropsAreNeverSilent(t *testing.T) {
	l := &logs{}
	b := New(l.logf)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	b.Subscribe("storage", 1, DropOldest)

	b.Publish(smp(1))
	b.Publish(smp(2)) // first drop: logged at once
	got := l.all()
	if len(got) != 1 || !strings.Contains(got[0], `"storage"`) || !strings.Contains(got[0], "dropped") {
		t.Fatalf("the first drop must be logged immediately, got %v", got)
	}

	for i := 3; i <= 20; i++ { // a burst within the log interval: counted, not logged line by line
		b.Publish(smp(i))
	}
	if n := len(l.all()); n != 1 {
		t.Errorf("%d log lines for a burst, want the log rate-limited to 1", n)
	}

	now = now.Add(dropLogInterval + time.Second)
	b.Publish(smp(21)) // after the interval: a summary of everything dropped since
	got = l.all()
	if len(got) != 2 || !strings.Contains(got[1], "19 sample(s) dropped") {
		t.Fatalf("expected a summary of the 19 drops since the first line, got %v", got)
	}
	if d := b.Stats()[0].Dropped; d != 20 {
		t.Errorf("total dropped = %d, want 20", d)
	}
}

func TestCloseLetsConsumersDrainThenEnds(t *testing.T) {
	b := New((&logs{}).logf)
	s := b.Subscribe("x", 10, DropOldest)
	b.Publish(smp(1))
	b.Publish(smp(2))
	b.Close()
	b.Publish(smp(3)) // after Close: ignored, no panic

	var got []int
	for x := range s.C() {
		got = append(got, x.PV.ID)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("drained %v, want [1 2]", got)
	}
	b.Close() // idempotent
}

func TestConcurrentPublishersGetUniqueIncreasingSequenceNumbers(t *testing.T) {
	b := New((&logs{}).logf)
	s := b.Subscribe("x", 100_000, DropOldest)
	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				b.Publish(smp(i))
			}
		}()
	}
	wg.Wait()
	b.Close()
	seen := map[uint64]bool{}
	for x := range s.C() {
		if seen[x.Seq] {
			t.Fatalf("sequence number %d given twice", x.Seq)
		}
		seen[x.Seq] = true
	}
	if len(seen) != 8000 {
		t.Errorf("received %d samples, want 8000", len(seen))
	}
}

func TestSequenceGapsRevealDrops(t *testing.T) {
	b := New((&logs{}).logf)
	s := b.Subscribe("x", 2, DropOldest)
	for i := 1; i <= 6; i++ {
		b.Publish(smp(i))
	}
	first, second := <-s.C(), <-s.C()
	if first.Seq != 5 || second.Seq != 6 {
		t.Fatalf("seq = %d, %d", first.Seq, second.Seq)
	}
	// a consumer comparing with the last seq it saw (0) learns that 4 were lost before it
	if first.Seq-0-1 != 4 {
		t.Error("a Seq gap must show how many samples were dropped before this consumer")
	}
}
