package web

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/alarms"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/live"
	"PLC_Monitoring/internal/store"
)

func fp(v float64) *float64 { return &v }

func testServer(t *testing.T) *Server {
	t.Helper()
	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	return &Server{tmpl: tmpl}
}

func sampleDef() store.AlarmDef {
	resp := 60
	return store.AlarmDef{ID: 7, PVID: 1, PLCName: "PLC-1", PVName: "PT101_PRESSURE", PVUnit: "PSI", PVMin: 0, PVMax: 100,
		AlarmType: store.AlarmHI, Setpoint: fp(80), SetpointUnit: "PSI", Deadband: 2, OnDelayS: 3, OffDelayS: 3,
		Priority: store.PriorityMajor, AlarmClass: "PROCESS", Message: "High pressure",
		OperatorAction: "Reduce load", ConsequencesOfInaction: "Relief valve lifts", ProbableCause: "Blocked line",
		MaxResponseTimeS: &resp, Rationale: "Needs action within minutes", RationalizationStatus: store.RationApproved,
		ApprovedByName: "adm", ApprovedAt: ptrTime(time.Now()), Enabled: true, UpdatedAt: time.Now(), UpdatedByName: "sup"}
}

func ptrTime(t time.Time) *time.Time { return &t }

// Every alarm template must run with realistic data in every state: a template
// error would otherwise only show up on a live page.
func TestAlarmTemplatesRenderInEveryState(t *testing.T) {
	s := testServer(t)
	def := sampleDef()
	now := time.Now()
	q := store.QualityGood
	live := &store.LatestRow{Value: fp(85.5), Quality: &q}
	for _, state := range []string{store.StateUnack, store.StateAcked, store.StateRTNUN, store.StateShlvd} {
		occ := store.Occurrence{ID: 11, AlarmID: 7, State: state, PVValue: fp(81.25), ActivatedAt: now.Add(-90 * time.Second),
			ProcessConditionActive: state != store.StateRTNUN}
		switch state {
		case store.StateAcked:
			occ.AckedAt, occ.AckedByName = ptrTime(now.Add(-30*time.Second)), "op"
		case store.StateRTNUN:
			occ.ReturnedToNormal = ptrTime(now.Add(-10 * time.Second))
		case store.StateShlvd:
			occ.ShelvedAt, occ.ShelvedByName, occ.ShelvedExpiry, occ.ShelveReason = ptrTime(now), "sup", ptrTime(now.Add(time.Hour)), "repair"
		}
		events := []store.HistoryEvent{{ID: 1, OccurrenceID: 11, Event: "ACTIVATED", Next: "UNACK", Value: fp(81.25), TS: now, Details: "value 81.25"}}
		for _, canShelve := range []bool{false, true} {
			d := alarmDetail{Occ: occ, Def: def, Live: live, Events: events, CanShelve: canShelve, MaxShelve: "8 h 00 min"}
			html, err := s.render("alarm_detail.html", d)
			if err != nil {
				t.Fatalf("%s canShelve=%v: %v", state, canShelve, err)
			}
			for _, want := range []string{"PT101_PRESSURE", "High pressure", "Reduce load", "Relief valve lifts", "Blocked line", "≥ 80 PSI", "Approved by adm"} {
				if !strings.Contains(html, want) {
					t.Errorf("%s: detail lacks %q", state, want)
				}
			}
			hasAck := strings.Contains(html, `/alarm/11/ack`)
			if want := state == store.StateUnack || state == store.StateRTNUN; hasAck != want {
				t.Errorf("%s: Acknowledge button present = %v, want %v", state, hasAck, want)
			}
			hasShelve := strings.Contains(html, `/alarm/11/shelve`)
			hasUnshelve := strings.Contains(html, `/alarm/11/unshelve`)
			if want := canShelve && state != store.StateShlvd; hasShelve != want {
				t.Errorf("%s canShelve=%v: shelve form present = %v, want %v", state, canShelve, hasShelve, want)
			}
			if want := canShelve && state == store.StateShlvd; hasUnshelve != want {
				t.Errorf("%s canShelve=%v: unshelve button present = %v, want %v", state, canShelve, hasUnshelve, want)
			}
			if hasShelve && !strings.Contains(html, "Site limit: 8 h 00 min") {
				t.Errorf("shelve form does not show the site limit")
			}
		}
	}

	// completed occurrence
	d := alarmDetail{Gone: true, Occ: store.Occurrence{ID: 11}, Def: def, Events: []store.HistoryEvent{{Event: "ACTIVATED", Next: "UNACK", TS: now}}}
	if html, err := s.render("alarm_detail.html", d); err != nil || !strings.Contains(html, "has completed") {
		t.Errorf("completed alarm: %v %q", err, html)
	}

	rows := []alarmRow{{E: alarms.Entry{Occ: store.Occurrence{ID: 11, State: store.StateUnack, PVValue: fp(81), ActivatedAt: now}, Def: def}, Live: live}}
	html, err := s.render("alarm_mgmt_rows.html", alarmRowsView{Rows: rows})
	if err != nil || !strings.Contains(html, "Acknowledge") || !strings.Contains(html, `hx-get="/alarm/11"`) || !strings.Contains(html, "More") {
		t.Errorf("rows: %v %q", err, html)
	}
	if html, _ := s.render("alarm_mgmt_rows.html", alarmRowsView{}); !strings.Contains(html, "No outstanding alarms") {
		t.Errorf("empty list: %q", html)
	}
	if html, err := s.render("alarm_summary.html", alarmSummary{Counts: alarms.Counts{Unack: 2}}); err != nil || !strings.Contains(html, "<strong>2</strong>") || strings.Contains(html, "Showing") {
		t.Errorf("summary: %v %q", err, html)
	}
}

func TestAlarmHistoryPageRenders(t *testing.T) {
	s := testServer(t)
	data := gin.H{
		"Page": "alarm_history", "Nav": navFor(nil), "Events": []store.HistoryEvent{{ID: 1, Event: "ACTIVATED", Next: "UNACK", TS: time.Now(), Value: fp(81), Priority: "Major", AlarmType: "HI", Message: "High", PLCName: "PLC-1", PVName: "PT", Unit: "PSI"}},
		"Total": 1, "Limit": 500, "Stats": store.AlarmStats{ByPriority: map[string]int{"Major": 1}, Activations: 1, Top: []store.FrequentAlarm{{AlarmID: 7, Activations: 1}}},
		"Defs": []store.AlarmDef{sampleDef()}, "Range": "24h", "Ranges": historyRanges, "Priority": "", "Priorities": store.AlarmPriorities,
		"AlarmID": int64(0), "Event": "", "EventTypes": []string{"ACTIVATED"},
	}
	html, err := s.render("alarm_history.html", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Statistics", "Most frequent alarms", "Event log", "Activated", "PT101_PRESSURE"} {
		if !strings.Contains(html, want) {
			t.Errorf("history page lacks %q", want)
		}
	}
}

func shelveRequest(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.POST("/alarm/:id/shelve", s.alarmShelve)
	req := httptest.NewRequest(http.MethodPost, "/alarm/5/shelve", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// The shelving time is chosen by the user and must be a valid, finite time
// inside the site limit; invalid requests never reach the database.
func TestShelvingInputIsValidatedBeforeTheDatabase(t *testing.T) {
	s := testServer(t) // no store: reaching the database would panic
	s.maxShelve = 8 * time.Hour
	for name, form := range map[string]url.Values{
		"no reason":          {"reason": {""}, "amount": {"10"}, "unit": {"minutes"}},
		"blank reason":       {"reason": {"   "}, "amount": {"10"}, "unit": {"minutes"}},
		"no amount":          {"reason": {"repair"}, "unit": {"minutes"}},
		"zero":               {"reason": {"repair"}, "amount": {"0"}, "unit": {"minutes"}},
		"negative":           {"reason": {"repair"}, "amount": {"-5"}, "unit": {"minutes"}},
		"not a number":       {"reason": {"repair"}, "amount": {"abc"}, "unit": {"minutes"}},
		"no unit":            {"reason": {"repair"}, "amount": {"10"}},
		"unknown unit":       {"reason": {"repair"}, "amount": {"10"}, "unit": {"weeks"}},
		"above the site max": {"reason": {"repair"}, "amount": {"9"}, "unit": {"hours"}},
		"huge":               {"reason": {"repair"}, "amount": {"99999999999"}, "unit": {"days"}},
		"reason too long":    {"reason": {strings.Repeat("x", 501)}, "amount": {"10"}, "unit": {"minutes"}},
	} {
		w := shelveRequest(t, s, form)
		trig := w.Header().Get("HX-Trigger")
		if w.Code != http.StatusNoContent || !strings.Contains(trig, `"kind":"error"`) {
			t.Errorf("%s: got %d %q, want a refusal message", name, w.Code, trig)
		}
	}
}

func TestDurationText(t *testing.T) {
	for in, want := range map[time.Duration]string{
		0: "0 s", 45 * time.Second: "45 s", 185 * time.Second: "3 min 05 s", 2*time.Hour + 10*time.Minute: "2 h 10 min",
		49 * time.Hour: "2 d 1 h", -time.Second: "0 s",
	} {
		if got := durText(in); got != want {
			t.Errorf("durText(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestResponseTimeLine(t *testing.T) {
	def := sampleDef() // limit 60 s
	now := time.Now()
	if r := respLine(def, store.Occurrence{State: store.StateUnack, ActivatedAt: now.Add(-2 * time.Minute)}); r.Class != "bad" {
		t.Errorf("unacknowledged past the limit: %+v", r)
	}
	if r := respLine(def, store.Occurrence{State: store.StateUnack, ActivatedAt: now.Add(-10 * time.Second)}); r.Class != "none" {
		t.Errorf("unacknowledged within the limit: %+v", r)
	}
	act := now.Add(-5 * time.Minute)
	if r := respLine(def, store.Occurrence{State: store.StateAcked, ActivatedAt: act, AckedAt: ptrTime(act.Add(20 * time.Second))}); r.Class != "ok" {
		t.Errorf("acknowledged in time: %+v", r)
	}
	if r := respLine(def, store.Occurrence{State: store.StateAcked, ActivatedAt: act, AckedAt: ptrTime(act.Add(5 * time.Minute))}); r.Class != "warn" {
		t.Errorf("acknowledged late: %+v", r)
	}
	def.MaxResponseTimeS = nil
	if r := respLine(def, store.Occurrence{State: store.StateUnack, ActivatedAt: act}); r.Text != "not defined" {
		t.Errorf("no limit: %+v", r)
	}
}

func TestMenuBadgeShowsAlarmsWaitingForAcknowledgement(t *testing.T) {
	s := testServer(t)
	base := gin.H{"Page": "alarms", "Nav": navFor(&auth.User{Role: auth.Operator}), "User": nil}
	for count, wantHidden := range map[int]bool{0: true, 3: false} {
		data := gin.H{}
		for k, v := range base {
			data[k] = v
		}
		data["Unack"] = count
		html, err := s.render("nav", data)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(html, `id="nav-badge"`) || !strings.Contains(html, ">"+strconv.Itoa(count)+"</span>") {
			t.Errorf("count %d: badge missing or wrong in %q", count, html)
		}
		if hidden := strings.Contains(html, `title="Alarms waiting for acknowledgement" hidden`); hidden != wantHidden {
			t.Errorf("count %d: hidden = %v, want %v", count, hidden, wantHidden)
		}
		if strings.Count(html, `id="nav-badge"`) != 1 {
			t.Errorf("the badge must appear once, on the Alarms link only")
		}
	}
	if got := (alarms.Counts{Unack: 2, Returned: 1, Acked: 5, Shelved: 4}).NeedsAck(); got != 3 {
		t.Errorf("NeedsAck = %d, want unacknowledged + returned-unacknowledged (3), not acknowledged or shelved", got)
	}
}

func TestEveryStreamCarriesTheBadgeAndPagesWithoutRowsGetTheirOwn(t *testing.T) {
	m := live.New(func(context.Context) ([]store.LatestRow, error) { return nil, nil }, nil, 5)
	ts := liveServer(t, m)
	for _, path := range []string{"/events", "/events?view=badge"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if got := next(t, readEvents(t, resp), "badge", 3*time.Second); strings.TrimSpace(got) != "0" {
			t.Errorf("%s: badge = %q, want 0", path, got)
		}
		resp.Body.Close()
	}
}

func TestOnlyApprovedAlarmsDrawTheGaugeBands(t *testing.T) {
	hi := sampleDef()
	lo := sampleDef()
	lo.AlarmType, lo.Setpoint = store.AlarmLO, fp(20)
	draft := sampleDef()
	draft.AlarmType, draft.Setpoint, draft.RationalizationStatus = store.AlarmLOLO, fp(5), store.RationDraft
	defs := []store.AlarmDef{hi, lo, draft}
	if v := limitOf(defs, store.AlarmHI); v == nil || *v != 80 {
		t.Errorf("HI limit = %v", v)
	}
	if v := limitOf(defs, store.AlarmLO); v == nil || *v != 20 {
		t.Errorf("LO limit = %v", v)
	}
	if limitOf(defs, store.AlarmLOLO) != nil {
		t.Error("a draft alarm must not draw a band")
	}
}

// ---------------------------------------------------------------------
// Alarm list filter
// ---------------------------------------------------------------------

func entry(id int64, plc, pv, typ, prio, class, state, msg string) alarms.Entry {
	return alarms.Entry{
		Occ: store.Occurrence{ID: id, State: state},
		Def: store.AlarmDef{ID: id, PLCName: plc, PVName: pv, AlarmType: typ, Priority: prio, AlarmClass: class, Message: msg},
	}
}

func filterOf(q string) alarmFilter {
	v, _ := url.ParseQuery(q)
	return parseAlarmFilter(v.Get)
}

func matchIDs(f alarmFilter, es []alarms.Entry) []int64 {
	var out []int64
	for _, e := range es {
		if f.Match(e) {
			out = append(out, e.Occ.ID)
		}
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAlarmFilterSelectsExactlyWhatWasAskedFor(t *testing.T) {
	es := []alarms.Entry{
		entry(1, "PLC-1", "PT101_PRESSURE", "HIHI", "Critical", "EQUIPMENT_PROTECTION", store.StateUnack, "PT101_PRESSURE very high"),
		entry(2, "PLC-1", "PT101_PRESSURE", "HI", "Major", "PROCESS", store.StateAcked, "PT101_PRESSURE high"),
		entry(3, "PLC-1", "XS101", "DISCRETE", "Major", "PROCESS", store.StateRTNUN, "XS101 alarm state"),
		entry(4, "PLC-2", "LT201", "LO", "Minor", "PROCESS", store.StateShlvd, "Tank low"),
		entry(5, "PLC-2", "LT201", "LOLO", "Critical", "PROCESS", store.StateUnack, "Tank very low"),
	}
	for name, c := range map[string]struct {
		query string
		want  []int64
	}{
		"no filter":             {"", []int64{1, 2, 3, 4, 5}},
		"priority Critical":     {"priority=Critical", []int64{1, 5}},
		"priority Major":        {"priority=Major", []int64{2, 3}},
		"priority Minor":        {"priority=Minor", []int64{4}},
		"type HI":               {"type=HI", []int64{2}},
		"type DISCRETE":         {"type=DISCRETE", []int64{3}},
		"state UNACK":           {"state=UNACK", []int64{1, 5}},
		"state RTNUN":           {"state=RTNUN", []int64{3}},
		"state ACKED":           {"state=ACKED", []int64{2}},
		"state SHLVD":           {"state=SHLVD", []int64{4}},
		"needs acknowledgement": {"state=NEEDS_ACK", []int64{1, 3, 5}},
		"class":                 {"class=EQUIPMENT_PROTECTION", []int64{1}},
		"plc":                   {"plc=PLC-2", []int64{4, 5}},
		"search pv (any case)":  {"q=xs101", []int64{3}},
		"search message":        {"q=VERY", []int64{1, 5}},
		"search plc":            {"q=plc-2", []int64{4, 5}},
		"two criteria (AND)":    {"priority=Critical&state=UNACK&plc=PLC-1", []int64{1}},
		"all four":              {"priority=Critical&type=LOLO&state=UNACK&plc=PLC-2", []int64{5}},
		"nothing matches":       {"priority=Minor&state=UNACK", nil},
		"unknown value ignored": {"priority=Urgent&type=XX&state=BOGUS&class=Z", []int64{1, 2, 3, 4, 5}},
	} {
		if got := matchIDs(filterOf(c.query), es); !sameIDs(got, c.want) {
			t.Errorf("%s (%q): got %v, want %v", name, c.query, got, c.want)
		}
	}
}

func TestAlarmFilterQueryRoundTripsAndIsBounded(t *testing.T) {
	f := filterOf("priority=Major&type=HI&state=NEEDS_ACK&class=PROCESS&plc=PLC-1&q=pump+1")
	if !f.Active() {
		t.Fatal("filter not active")
	}
	if g := filterOf(f.Query()); g != f {
		t.Errorf("round trip: %+v != %+v", g, f)
	}
	if (alarmFilter{}).Active() || (alarmFilter{}).Query() != "" {
		t.Error("an empty filter must be inactive with an empty query")
	}
	if long := filterOf("q=" + strings.Repeat("x", 500)); len(long.Q) != maxFilterText {
		t.Errorf("search text not bounded: %d", len(long.Q))
	}
}

func TestFilteredListAndCountsAreConsistent(t *testing.T) {
	s := testServer(t)
	rows := alarmRowsView{Filtered: true}
	html, err := s.render("alarm_mgmt_rows.html", rows)
	if err != nil || !strings.Contains(html, "No alarms match the filters") {
		t.Errorf("empty filtered list: %v %q", err, html)
	}
	html, err = s.render("alarm_summary.html", alarmSummary{Counts: alarms.Counts{Unack: 5}, Shown: 2, Total: 9, Filtered: true})
	if err != nil || !strings.Contains(html, "Showing <strong>2</strong> of <strong>9</strong>") {
		t.Errorf("summary: %v %q", err, html)
	}
}
