package web

import (
	"bufio"
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/live"
	"PLC_Monitoring/internal/store"
)

// liveServer is a Server with only what /events needs: no database, no login.
func liveServer(t *testing.T, m *live.Module) *httptest.Server {
	t.Helper()
	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	srv := &Server{live: m, tmpl: tmpl}
	r := gin.New()
	r.GET("/events", srv.events)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

func sample(v float64, ts time.Time) bus.Sample {
	return bus.Sample{PLCName: "PLC-1", ScanIntervalMS: 1000, Quality: store.QualityGood, Raw: 1, Value: v, TS: ts,
		PV: store.PV{ID: 1, Name: "Tank level", RegisterAddress: 30001, Min: 0, Max: 100, Unit: "%"}}
}

// events reads SSE messages of a connection.
func readEvents(t *testing.T, resp *http.Response) <-chan [2]string {
	out := make(chan [2]string, 100)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data += strings.TrimPrefix(line, "data: ") + "\n"
			case line == "" && ev != "":
				out <- [2]string{ev, data}
				ev, data = "", ""
			}
		}
	}()
	return out
}

func next(t *testing.T, ch <-chan [2]string, event string, d time.Duration) string {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed while waiting for %q", event)
			}
			if e[0] == event {
				return e[1]
			}
		case <-deadline:
			t.Fatalf("no %q event within %v", event, d)
		}
	}
}

func TestEventsPushesTheInitialStateAndEveryChangeWithoutTheDatabase(t *testing.T) {
	m := live.New(func(context.Context) ([]store.LatestRow, error) { t.Error("database was read"); return nil, nil }, nil, 5)
	m.Apply(sample(10, time.Now()))
	ts := liveServer(t, m)

	resp, err := http.Get(ts.URL + "/events?pv=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	ch := readEvents(t, resp)

	if rows := next(t, ch, "rows", 3*time.Second); !strings.Contains(rows, "Tank level") || !strings.Contains(rows, "10.00") {
		t.Errorf("initial rows = %q", rows)
	}
	if lv := next(t, ch, "live", 3*time.Second); !strings.Contains(lv, "10.00") {
		t.Errorf("initial live block = %q", lv)
	}

	m.Apply(sample(77.25, time.Now().Add(time.Second)))
	if rows := next(t, ch, "rows", 3*time.Second); !strings.Contains(rows, "77.25") {
		t.Errorf("pushed rows = %q, want the new value", rows)
	}
	if lv := next(t, ch, "live", 3*time.Second); !strings.Contains(lv, "77.25") {
		t.Errorf("pushed live block = %q, want the new value", lv)
	}
}

func TestEventsMarksAValueStaleWhenNothingNewArrives(t *testing.T) {
	m := live.New(func(context.Context) ([]store.LatestRow, error) { return nil, nil }, nil, 5)
	m.Apply(sample(10, time.Now().Add(-time.Minute))) // last sample is a minute old
	ts := liveServer(t, m)
	resp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if rows := next(t, readEvents(t, resp), "rows", 3*time.Second); !strings.Contains(rows, "STALE") {
		t.Errorf("rows = %q, want STALE", rows)
	}
}

func TestEventsRefusesMoreClientsThanTheLimitAndFreesTheSlot(t *testing.T) {
	m := live.New(func(context.Context) ([]store.LatestRow, error) { return nil, nil }, nil, 1)
	ts := liveServer(t, m)

	first, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	next(t, readEvents(t, first), "rows", 3*time.Second)

	second, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second client: %d, want 503", second.StatusCode)
	}
	if m.Stats().Rejected != 1 {
		t.Errorf("rejection not counted: %+v", m.Stats())
	}

	first.Body.Close() // browser closes the page
	deadline := time.Now().Add(3 * time.Second)
	for m.Stats().Clients != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed: %+v", m.Stats())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEventsRejectsABadPVId(t *testing.T) {
	ts := liveServer(t, live.New(func(context.Context) ([]store.LatestRow, error) { return nil, nil }, nil, 5))
	resp, err := http.Get(ts.URL + "/events?pv=abc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400", resp.StatusCode)
	}
}
