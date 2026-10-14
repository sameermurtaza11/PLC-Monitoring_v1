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

	"PLC_Monitoring/internal/alarms"
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

type fixedView struct {
	defs []store.AlarmDef
	occs []store.Occurrence
}

func (f fixedView) ListAlarmDefs(context.Context) ([]store.AlarmDef, error) { return f.defs, nil }
func (f fixedView) ActiveOccurrences(context.Context) ([]store.Occurrence, error) {
	return f.occs, nil
}

// The pushed alarm list honours the filter in the stream URL, and the counts
// line says how many of the outstanding alarms are shown.
func TestAlarmStreamIsFilteredByTheQuery(t *testing.T) {
	d := func(id int64, pv, prio, typ string) store.AlarmDef {
		return store.AlarmDef{ID: id, PVID: int(id), PLCName: "PLC-1", PVName: pv, Priority: prio, AlarmType: typ, Message: pv + " msg", Digital: typ == "DISCRETE"}
	}
	repo := fixedView{
		defs: []store.AlarmDef{d(1, "CRIT_PV", store.PriorityCritical, "HIHI"), d(2, "MAJOR_PV", store.PriorityMajor, "HI"), d(3, "MINOR_PV", store.PriorityMinor, "LO")},
		occs: []store.Occurrence{
			{ID: 11, AlarmID: 1, State: store.StateUnack, ActivatedAt: time.Now()},
			{ID: 12, AlarmID: 2, State: store.StateAcked, ActivatedAt: time.Now()},
			{ID: 13, AlarmID: 3, State: store.StateUnack, ActivatedAt: time.Now()},
		},
	}
	view := alarms.NewView(repo, 0)
	if err := view.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := live.New(func(context.Context) ([]store.LatestRow, error) { return nil, nil }, nil, 5)
	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	srv := &Server{live: m, tmpl: tmpl, alarmView: view}
	r := gin.New()
	r.GET("/events", srv.events)
	ts := httptest.NewServer(r)
	defer ts.Close()

	read := func(query string) (rows, summary string) {
		resp, err := http.Get(ts.URL + "/events?view=alarms" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		ch := readEvents(t, resp)
		return next(t, ch, "rows", 3*time.Second), next(t, ch, "summary", 3*time.Second)
	}

	rows, sum := read("")
	for _, pv := range []string{"CRIT_PV", "MAJOR_PV", "MINOR_PV"} {
		if !strings.Contains(rows, pv) {
			t.Errorf("unfiltered list lacks %s", pv)
		}
	}
	if strings.Contains(sum, "Showing") {
		t.Errorf("unfiltered summary must not say Showing: %q", sum)
	}

	rows, sum = read("&priority=Critical")
	if !strings.Contains(rows, "CRIT_PV") || strings.Contains(rows, "MAJOR_PV") || strings.Contains(rows, "MINOR_PV") {
		t.Errorf("priority=Critical rows = %q", rows)
	}
	if !strings.Contains(sum, "Showing <strong>1</strong> of <strong>3</strong>") {
		t.Errorf("summary = %q", sum)
	}

	rows, _ = read("&state=NEEDS_ACK&type=LO")
	if !strings.Contains(rows, "MINOR_PV") || strings.Contains(rows, "CRIT_PV") || strings.Contains(rows, "MAJOR_PV") {
		t.Errorf("state+type rows = %q", rows)
	}

	rows, _ = read("&q=nothing-like-this")
	if !strings.Contains(rows, "No alarms match the filters") {
		t.Errorf("empty result rows = %q", rows)
	}
}
