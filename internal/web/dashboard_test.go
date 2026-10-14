package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/alarms"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/live"
	"PLC_Monitoring/internal/store"
)

// ---------------------------------------------------------------------
// Status column
// ---------------------------------------------------------------------

func goodRow(id int, name string) store.LatestRow {
	q := store.QualityGood
	ts := time.Now()
	return store.LatestRow{PV: store.PV{ID: id, Name: name, SystemName: "Compressor", Equipment: "K-101", Description: "Discharge pressure",
		RegisterAddress: 30001, Unit: "PSI", Min: 0, Max: 100}, PLCName: "PLC-1", ScanIntervalMS: 1000,
		Quality: &q, TS: &ts, Value: fp(55), Raw: ip(36000), LastGoodTS: &ts}
}

func ip(v int) *int { return &v }

func pvAl(typ, prio, state string) *pvAlarm {
	return &pvAlarm{Def: store.AlarmDef{AlarmType: typ, Priority: prio, Message: typ + " message"}, State: state}
}

func TestStatusColumnShowsTheAlarmThatIsActive(t *testing.T) {
	row := goodRow(1, "P")
	for name, c := range map[string]struct {
		alarm       *pvAlarm
		text, class string
		flash       bool
	}{
		"no alarm → Normal":               {nil, "Normal", "ok", false},
		"HIHI unacknowledged":             {pvAl("HIHI", "Critical", "UNACK"), "HIHI", "bad", true},
		"HI acknowledged":                 {pvAl("HI", "Major", "ACKED"), "HI", "warn", false},
		"LO":                              {pvAl("LO", "Minor", "UNACK"), "LO", "warn", true},
		"LOLO critical":                   {pvAl("LOLO", "Critical", "ACKED"), "LOLO", "bad", false},
		"discrete":                        {pvAl("DISCRETE", "Major", "UNACK"), "ALARM", "warn", true},
		"shelved alarm is marked shelved": {pvAl("HI", "Major", "SHLVD"), "HI (shelved)", "none", false},
	} {
		got := rowStatus(pvRow{LatestRow: row, Alarm: c.alarm})
		if got.Text != c.text || got.Class != c.class || got.Flash != c.flash {
			t.Errorf("%s: got %+v, want text %q class %q flash %v", name, got, c.text, c.class, c.flash)
		}
	}
	// a communication or data problem is shown before an alarm
	bad := store.QualityCommFail
	row.Quality = &bad
	if got := rowStatus(pvRow{LatestRow: row, Alarm: pvAl("HIHI", "Critical", "UNACK")}); got.Text != store.QualityCommFail {
		t.Errorf("bad quality: %+v", got)
	}
	old := time.Now().Add(-time.Minute)
	row = goodRow(1, "P")
	row.TS = &old
	if got := rowStatus(pvRow{LatestRow: row, Alarm: pvAl("HI", "Major", "UNACK")}); got.Text != "STALE" {
		t.Errorf("stale data: %+v", got)
	}
}

// viewServer builds a server whose alarm list is the given outstanding alarms.
func viewServer(t *testing.T, defs []store.AlarmDef, occs []store.Occurrence) *Server {
	t.Helper()
	view := alarms.NewView(fixedView{defs: defs, occs: occs}, 0)
	if err := view.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	s.alarmView = view
	s.live = live.New(func(context.Context) ([]store.LatestRow, error) { return nil, nil }, nil, 5)
	return s
}

func adef(id int64, pv int, typ, prio string) store.AlarmDef {
	return store.AlarmDef{ID: id, PVID: pv, PLCName: "PLC-1", PVName: "PV" + string(rune('0'+pv)), AlarmType: typ, Priority: prio,
		Message: typ + " alarm", AlarmClass: "PROCESS", Digital: typ == "DISCRETE"}
}

func occ(id, alarm int64, state string, active bool, at time.Time) store.Occurrence {
	return store.Occurrence{ID: id, AlarmID: alarm, State: state, ProcessConditionActive: active, ActivatedAt: at}
}

// With several alarms on one PV the Status column follows one fixed order:
// not shelved, then HIHI/LOLO, then priority, then unacknowledged, then newest.
func TestStatusOfAPVWithSeveralAlarmsFollowsTheDocumentedOrder(t *testing.T) {
	now := time.Now()
	defs := []store.AlarmDef{
		adef(1, 1, "HIHI", "Critical"), adef(2, 1, "HI", "Major"), // PV1: both active → HIHI
		adef(3, 2, "HIHI", "Critical"), adef(4, 2, "HI", "Major"), // PV2: HIHI shelved → HI shows
		adef(5, 3, "HI", "Major"),                                 // PV3: only a returned-to-normal occurrence → Normal
		adef(6, 4, "HIHI", "Minor"), adef(7, 4, "HI", "Critical"), // PV4: type outranks priority → HIHI
		adef(8, 5, "HI", "Major"), // PV5: shelved only
		adef(9, 6, "DISCRETE", "Major"),
	}
	occs := []store.Occurrence{
		occ(11, 1, store.StateAcked, true, now), occ(12, 2, store.StateUnack, true, now),
		occ(13, 3, store.StateShlvd, true, now), occ(14, 4, store.StateUnack, true, now),
		occ(15, 5, store.StateRTNUN, false, now),
		occ(16, 6, store.StateUnack, true, now), occ(17, 7, store.StateAcked, true, now),
		occ(18, 8, store.StateShlvd, true, now),
		occ(19, 9, store.StateAcked, true, now),
	}
	s := viewServer(t, defs, occs)
	got := s.activeAlarmsByPV()
	want := map[int]string{1: "HIHI", 2: "HI", 4: "HIHI", 5: "HI", 6: "DISCRETE"}
	for pv, typ := range want {
		if got[pv] == nil || got[pv].Def.AlarmType != typ {
			t.Errorf("PV%d: got %+v, want %s", pv, got[pv], typ)
		}
	}
	if got[3] != nil {
		t.Errorf("PV3 has no abnormal alarm, must be Normal: %+v", got[3])
	}
	if got[5].State != store.StateShlvd {
		t.Errorf("PV5: a shelved alarm must be reported as shelved")
	}
	if got[2].State != store.StateUnack {
		t.Errorf("PV2 shows the unshelved HI (unacknowledged), got state %s", got[2].State)
	}
}

func TestEqualAlarmsPreferUnacknowledgedThenTheNewest(t *testing.T) {
	older, newer := time.Now().Add(-time.Hour), time.Now()
	a := pvAlarm{Def: store.AlarmDef{AlarmType: "HI", Priority: "Major"}, State: store.StateAcked}
	b := pvAlarm{Def: store.AlarmDef{AlarmType: "HI", Priority: "Major"}, State: store.StateUnack}
	if !alarmBetter(b, a, older, newer) || alarmBetter(a, b, newer, older) {
		t.Error("an unacknowledged alarm must win over an acknowledged one of equal rank")
	}
	b.State = store.StateAcked
	if !alarmBetter(b, a, newer, older) || alarmBetter(a, b, older, newer) {
		t.Error("with everything else equal the newest activation wins")
	}
}

// ---------------------------------------------------------------------
// Dashboard search
// ---------------------------------------------------------------------

func TestDashboardSearchMatchesEveryRelevantField(t *testing.T) {
	r := goodRow(1, "PT101_PRESSURE")
	r.SystemName, r.Equipment, r.PLCName, r.Description = "Air system", "Compressor K-101", "PLC-7", "Discharge header pressure"
	for q, want := range map[string]bool{
		"": true, "pt101": true, "AIR": true, "compressor": true, "k-101": true, "plc-7": true, "header": true,
		"30001": true, "psi": true, "air header": true, "air plc-7 pt101": true,
		"boiler": false, "air boiler": false, "plc-8": false,
	} {
		if got := (pvFilter{Q: q}).Match(r); got != want {
			t.Errorf("search %q: %v, want %v", q, got, want)
		}
	}
	if f := parsePVFilter(func(string) string { return "  " + strings.Repeat("x", 300) + "  " }); len(f.Q) != maxFilterText {
		t.Errorf("search text not bounded: %d", len(f.Q))
	}
	v, _ := url.ParseQuery((pvFilter{Q: "air header"}).Query())
	if (pvFilter{Q: v.Get("q")}) != (pvFilter{Q: "air header"}) {
		t.Error("filter does not survive its own query string")
	}
}

func TestDashboardRowsHaveTheRequestedColumnsInOrder(t *testing.T) {
	s := testServer(t)
	r := goodRow(1, "PT101_PRESSURE")
	r.SystemName, r.Equipment, r.Description = "Air system", "K-101", "Discharge header pressure"
	html, err := s.render("pv_rows.html", pvRowsView{Rows: []pvRow{{LatestRow: r, Alarm: pvAl("HIHI", "Critical", "UNACK")}}})
	if err != nil {
		t.Fatal(err)
	}
	cells := strings.Split(html, "<td")[1:]
	if len(cells) != 9 {
		t.Fatalf("%d cells, want 9:\n%s", len(cells), html)
	}
	for i, want := range []string{"Air system", "PLC-1", "K-101", "PT101_PRESSURE", "Discharge header pressure", "55.00", "HIHI", ":", "More"} {
		if !strings.Contains(cells[i], want) {
			t.Errorf("column %d = %q, want it to contain %q", i+1, cells[i], want)
		}
	}
	if !strings.Contains(cells[5], "PSI") {
		t.Errorf("value column lacks the engineering unit: %q", cells[5])
	}
	if strings.Contains(html, ">Raw<") {
		t.Error("the raw value column was removed from the table")
	}
	if html, _ := s.render("pv_rows.html", pvRowsView{Filtered: true}); !strings.Contains(html, "No PVs match the search") {
		t.Errorf("empty search result: %q", html)
	}
}

func TestDashboardStreamFollowsTheSearchAndKeepsPushingLiveValues(t *testing.T) {
	m := live.New(func(context.Context) ([]store.LatestRow, error) {
		a, b := goodRow(1, "PT101"), goodRow(2, "TT202")
		b.SystemName = "Cooling"
		return []store.LatestRow{a, b}, nil
	}, nil, 5)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	s.live = m
	ts := serverFor(t, s)

	resp, err := http.Get(ts.URL + "/events?q=cooling")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ch := readEvents(t, resp)
	rows := next(t, ch, "rows", 3*time.Second)
	if !strings.Contains(rows, "TT202") || strings.Contains(rows, "PT101") {
		t.Errorf("filtered rows = %q", rows)
	}
	if sum := next(t, ch, "summary", 3*time.Second); !strings.Contains(sum, "Showing <strong>1</strong> of <strong>2</strong>") {
		t.Errorf("summary = %q", sum)
	}
	// a new reading for a visible PV arrives while the search is active
	upd := goodRow(2, "TT202")
	upd.SystemName = "Cooling"
	upd.Value = fp(77.25)
	m.Apply(sampleOf(upd, 77.25, time.Now().Add(time.Second)))
	if rows := next(t, ch, "rows", 3*time.Second); !strings.Contains(rows, "77.25") || strings.Contains(rows, "PT101") {
		t.Errorf("live update under a search = %q", rows)
	}
}

// ---------------------------------------------------------------------
// Ticker
// ---------------------------------------------------------------------

func TestTickerListsActiveAlarmsOldestFirstAndExplainsTheRest(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	defs := []store.AlarmDef{adef(1, 1, "HI", "Major"), adef(2, 2, "LO", "Minor"), adef(3, 3, "HIHI", "Critical"),
		adef(4, 4, "HI", "Major"), adef(5, 5, "DISCRETE", "Major"), adef(6, 6, "LOLO", "Critical")}
	occs := []store.Occurrence{
		occ(21, 1, store.StateUnack, true, base.Add(30*time.Minute)), // newest active
		occ(22, 2, store.StateAcked, true, base.Add(10*time.Minute)),
		occ(23, 3, store.StateUnack, true, base),                     // oldest
		occ(24, 4, store.StateRTNUN, false, base),                    // not active: counted only
		occ(25, 5, store.StateShlvd, true, base),                     // shelved: suppressed
		occ(26, 6, store.StateUnack, true, base.Add(10*time.Minute)), // same time as 22: id order
	}
	s := viewServer(t, defs, occs)
	d := s.tickerData()
	var ids []int64
	for _, it := range d.Items {
		ids = append(ids, it.OccID)
	}
	if !sameIDs(ids, []int64{23, 22, 26, 21}) {
		t.Errorf("ticker order = %v, want oldest event first [23 22 26 21]", ids)
	}
	if d.Returned != 1 || d.More != 0 {
		t.Errorf("returned=%d more=%d", d.Returned, d.More)
	}
	html, err := s.render("ticker_items", d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(html, "PV3") > strings.Index(html, "PV1") {
		t.Errorf("ticker text is not in chronological order: %q", html)
	}
	for _, want := range []string{`href="/alarms?alarm=23"`, "HIHI", "HIHI alarm", "1 returned to normal"} {
		if !strings.Contains(html, want) {
			t.Errorf("ticker lacks %q", want)
		}
	}
	if strings.Contains(html, "PV5") {
		t.Error("a shelved alarm must not scroll in the ticker")
	}
}

func TestTickerIsBoundedAndHandlesNoAlarms(t *testing.T) {
	s := testServer(t) // no alarm list at all
	html, err := s.render("ticker_items", s.tickerData())
	if err != nil || !strings.Contains(html, "No active alarms") {
		t.Errorf("no alarms: %v %q", err, html)
	}

	var defs []store.AlarmDef
	var occs []store.Occurrence
	base := time.Now().Add(-time.Hour)
	for i := 1; i <= maxTickerItems+7; i++ {
		defs = append(defs, adef(int64(i), i, "HI", "Major"))
		occs = append(occs, occ(int64(1000+i), int64(i), store.StateAcked, true, base.Add(time.Duration(i)*time.Second)))
	}
	s = viewServer(t, defs, occs)
	d := s.tickerData()
	if len(d.Items) != maxTickerItems || d.More != 7 {
		t.Fatalf("items=%d more=%d", len(d.Items), d.More)
	}
	if d.Items[0].OccID != 1001 {
		t.Error("the oldest alarms must be the ones kept")
	}
	if html, _ := s.render("ticker_items", d); !strings.Contains(html, "+7 more") {
		t.Error("the ticker must say how many alarms are not shown")
	}
}

func TestEveryPageWithTheMenuCarriesTheTicker(t *testing.T) {
	s := viewServer(t, []store.AlarmDef{adef(1, 1, "HI", "Major")},
		[]store.Occurrence{occ(21, 1, store.StateUnack, true, time.Now())})
	u := &auth.User{Username: "op", Role: auth.Operator}
	data := pageDataFor(s, u)
	html, err := s.render("nav", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="alarm-ticker"`, `class="ticker-label"`, "PV1", `href="/alarms?alarm=21"`} {
		if !strings.Contains(html, want) {
			t.Errorf("menu lacks %s", want)
		}
	}
	// the login page (no user) must not show a ticker
	if html, _ := s.render("nav", map[string]any{"Page": "login", "Nav": navFor(nil), "User": nil}); strings.Contains(html, "alarm-ticker") {
		t.Error("the ticker must not appear on the login page")
	}
}

func TestEveryStreamCarriesTheTicker(t *testing.T) {
	s := viewServer(t, []store.AlarmDef{adef(1, 1, "HI", "Major")},
		[]store.Occurrence{occ(21, 1, store.StateUnack, true, time.Now())})
	ts := serverFor(t, s)
	for _, path := range []string{"/events", "/events?view=badge", "/events?view=alarms"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if got := next(t, readEvents(t, resp), "ticker", 3*time.Second); !strings.Contains(got, "PV1") {
			t.Errorf("%s: ticker = %q", path, got)
		}
		resp.Body.Close()
	}
}

// ---------------------------------------------------------------------
// PV detail
// ---------------------------------------------------------------------

func TestPVDetailShowsAllMetadataAlarmsAndEvents(t *testing.T) {
	now := time.Now()
	def := sampleDef()
	def.ID, def.PVID, def.Priority, def.Deadband, def.OnDelayS, def.OffDelayS = 7, 1, store.PriorityCritical, 2, 10, 5
	other := sampleDef()
	other.ID, other.AlarmType, other.Setpoint, other.RationalizationStatus = 8, store.AlarmLO, fp(20), store.RationDraft
	s := viewServer(t, []store.AlarmDef{def, other},
		[]store.Occurrence{occ(31, 7, store.StateUnack, true, now.Add(-time.Minute)), occ(32, 7, store.StateRTNUN, false, now.Add(-time.Hour))})
	row := goodRow(1, "PT101_PRESSURE")
	data := map[string]any{
		"PV": pvRow{LatestRow: row, Alarm: pvAl("HI", "Critical", "UNACK")}, "Options": []store.PVOption{}, "Ranges": rangeOrder,
		"Alarms": []store.AlarmDef{def, other}, "AlarmLow": fp(20), "AlarmHigh": fp(80), "AlarmState": (*int)(nil),
		"AlarmRows": s.pvAlarmRows([]store.AlarmDef{def, other}),
		"Info": store.PVInfo{PVID: 1, Enabled: true, CreatedAt: now, UpdatedAt: now, PLCName: "PLC-1", PLCEnabled: true,
			PLCAddress: "127.0.0.1:502", UnitID: 1, ScanIntervalMS: 1000, TimeoutMS: 2000, PLCDescription: "Compressor house"},
		"Events": []store.HistoryEvent{{Event: "ACTIVATED", Next: "UNACK", TS: now, Value: fp(81), Unit: "PSI", AlarmType: "HI", Message: "High pressure", Details: "value 81"}},
	}
	html, err := s.render("pv_detail.html", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Identification", "Scaling and storage", "PLC and communication", "Alarms", "Recent alarm events", "Trend and history",
		"Compressor", "K-101", "PT101_PRESSURE", "Discharge pressure", "30001", // identification
		"Raw value", "36000", "Engineering value", "55.00 PSI", "Data quality", "GOOD", "Last updated", // live
		"Minimum", "Maximum", "History deadband", // scaling
		"127.0.0.1:502", "Compressor house", "2000 ms", // PLC
		"≥ 80 PSI", "≤ 20 PSI", "2 PSI", "10 / 5 s", "Critical", // alarm configuration
		"Unacknowledged", "awaiting acknowledgement" /* the older RTNUN occurrence */, "Draft", "In service",
		"High pressure", // events
	} {
		if !strings.Contains(html, want) {
			t.Errorf("PV detail lacks %q", want)
		}
	}
	if got := strings.Count(html, "<tr>"); got < 5 {
		t.Errorf("too few table rows (%d)", got)
	}
	// every id the page script relies on is still there
	for _, id := range []string{`id="pv-gauge"`, `id="pv-alarms"`, `id="trend-form"`, `id="pv-trend"`, `id="history"`, `class="live-wrap"`} {
		if !strings.Contains(html, id) {
			t.Errorf("PV detail lacks %s", id)
		}
	}
}

func TestPVAlarmRowPicksTheOccurrenceThatDescribesTheAlarm(t *testing.T) {
	at := time.Now()
	r := pvAlarmRow{Occs: []store.Occurrence{
		{ID: 1, State: store.StateRTNUN, ActivatedAt: at.Add(-time.Hour)},
		{ID: 2, State: store.StateAcked, ProcessConditionActive: true, ActivatedAt: at},
		{ID: 3, State: store.StateRTNUN, ActivatedAt: at.Add(-2 * time.Hour)},
	}}
	if cur := r.Current(); cur == nil || cur.ID != 2 {
		t.Errorf("current = %+v, want the abnormal occurrence 2", cur)
	}
	if r.Waiting() != 2 {
		t.Errorf("waiting = %d, want the 2 older returned-to-normal occurrences", r.Waiting())
	}
	if (pvAlarmRow{}).Current() != nil {
		t.Error("an alarm with nothing outstanding has no current occurrence")
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func serverFor(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	r := gin.New()
	r.GET("/events", s.events)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// sampleOf is a good reading of the PV that row describes.
func sampleOf(row store.LatestRow, value float64, ts time.Time) bus.Sample {
	return bus.Sample{PLCName: row.PLCName, ScanIntervalMS: row.ScanIntervalMS, PV: row.PV, Quality: store.QualityGood,
		Raw: 1, Value: value, TS: ts}
}

func pageDataFor(s *Server, u *auth.User) gin.H {
	return gin.H{"Page": "dashboard", "User": u, "Nav": navFor(u), "CSRF": "x", "Unack": s.unackCount(), "Ticker": s.tickerData()}
}
