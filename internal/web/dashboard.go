package web

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"PLC_Monitoring/internal/store"
)

// ---------------------------------------------------------------------
// Dashboard search
// ---------------------------------------------------------------------

// pvFilter narrows the dashboard table. The search text is split into words;
// every word has to appear in at least one of: PV name, system, equipment,
// PLC name, description, register address or unit (case-insensitive).
type pvFilter struct{ Q string }

func parsePVFilter(get func(string) string) pvFilter {
	q := strings.TrimSpace(get("q"))
	if len(q) > maxFilterText {
		q = q[:maxFilterText]
	}
	return pvFilter{Q: q}
}

func (f pvFilter) Active() bool { return f.Q != "" }

// Query is the filter as URL parameters, without a leading "?".
func (f pvFilter) Query() string {
	if f.Q == "" {
		return ""
	}
	return url.Values{"q": {f.Q}}.Encode()
}

func (f pvFilter) Match(r store.LatestRow) bool {
	if f.Q == "" {
		return true
	}
	hay := strings.ToLower(strings.Join([]string{
		r.Name, r.SystemName, r.Equipment, r.PLCName, r.Description, strconv.Itoa(r.RegisterAddress), r.Unit,
	}, "\n"))
	for _, word := range strings.Fields(strings.ToLower(f.Q)) {
		if !strings.Contains(hay, word) {
			return false
		}
	}
	return true
}

// pvSummary is the line above the dashboard table.
type pvSummary struct {
	Shown, Total int
	Filtered     bool
}

// pvRowsView is what the dashboard rows template renders.
type pvRowsView struct {
	Rows     []pvRow
	Filtered bool
}

// ---------------------------------------------------------------------
// Alarm state of a PV (Status column, gauge bands)
// ---------------------------------------------------------------------

// pvAlarm is an alarm whose process condition is abnormal, with its state.
type pvAlarm struct {
	Def   store.AlarmDef
	State string // UNACK | ACKED | SHLVD
}

// pvRow is a dashboard row: the live values of a PV plus its alarm, if any.
// Alarm limits are not part of pv_metadata; they come from the alarm tables.
type pvRow struct {
	store.LatestRow
	Alarm *pvAlarm // the alarm that decides the Status column
}

func typeSeverity(t string) int {
	if t == store.AlarmHIHI || t == store.AlarmLOLO {
		return 2
	}
	return 1
}

func priorityOrder(p string) int {
	switch p {
	case store.PriorityCritical:
		return 0
	case store.PriorityMajor:
		return 1
	}
	return 2
}

// alarmBetter says whether a should be shown instead of b when a PV has
// several abnormal alarms. Order of importance:
//  1. not shelved before shelved (a shelved alarm is suppressed),
//  2. HIHI / LOLO before HI / LO / DISCRETE,
//  3. higher priority (Critical, Major, Minor),
//  4. unacknowledged before acknowledged,
//  5. the one that activated last.
func alarmBetter(a, b pvAlarm, activatedA, activatedB time.Time) bool {
	if sa, sb := a.State == store.StateShlvd, b.State == store.StateShlvd; sa != sb {
		return !sa
	}
	if ta, tb := typeSeverity(a.Def.AlarmType), typeSeverity(b.Def.AlarmType); ta != tb {
		return ta > tb
	}
	if pa, pb := priorityOrder(a.Def.Priority), priorityOrder(b.Def.Priority); pa != pb {
		return pa < pb
	}
	if ua, ub := a.State == store.StateUnack, b.State == store.StateUnack; ua != ub {
		return ua
	}
	return activatedA.After(activatedB)
}

// activeAlarmsByPV maps each PV to the alarm that decides its status: from the
// outstanding alarms whose process condition is still abnormal (the same list
// the Alarm Management page shows).
func (s *Server) activeAlarmsByPV() map[int]*pvAlarm {
	if s.alarmView == nil {
		return nil
	}
	out := map[int]*pvAlarm{}
	at := map[int]time.Time{}
	for _, e := range s.alarmView.Snapshot() {
		if !e.Occ.ProcessConditionActive {
			continue
		}
		cand := pvAlarm{Def: e.Def, State: e.Occ.State}
		cur, ok := out[e.Def.PVID]
		if !ok || alarmBetter(cand, *cur, e.Occ.ActivatedAt, at[e.Def.PVID]) {
			c := cand
			out[e.Def.PVID] = &c
			at[e.Def.PVID] = e.Occ.ActivatedAt
		}
	}
	return out
}

func (s *Server) rowView(r store.LatestRow) pvRow {
	return pvRow{LatestRow: r, Alarm: s.activeAlarmsByPV()[r.ID]}
}

// pvRowViews returns the PVs that pass the filter, in dashboard order.
func (s *Server) pvRowViews(f pvFilter) pvRowsView {
	active := s.activeAlarmsByPV()
	v := pvRowsView{Filtered: f.Active()}
	for _, r := range s.live.Snapshot() {
		if f.Match(r) {
			v.Rows = append(v.Rows, pvRow{LatestRow: r, Alarm: active[r.ID]})
		}
	}
	return v
}

func (s *Server) pvSummaryOf(f pvFilter) pvSummary {
	sum := pvSummary{Filtered: f.Active()}
	for _, r := range s.live.Snapshot() {
		sum.Total++
		if f.Match(r) {
			sum.Shown++
		}
	}
	return sum
}

// rowStatus: a communication / data problem first, then the PV's alarm; else Normal.
func rowStatus(r pvRow) Status {
	st := statusOf(r.LatestRow)
	if st.Class != "ok" || r.Alarm == nil {
		return st
	}
	a := r.Alarm
	text := "ALARM"
	if a.Def.AlarmType != store.AlarmDiscrete {
		text = a.Def.AlarmType
	}
	out := Status{Text: text, Class: "warn", Title: a.Def.Message + " · " + a.Def.Priority + " · " + stateLabel(a.State)}
	switch {
	case a.State == store.StateShlvd:
		out.Text, out.Class = text+" (shelved)", "none"
	case a.Def.Priority == store.PriorityCritical:
		out.Class = "bad"
	}
	out.Flash = a.State == store.StateUnack
	return out
}

// limitOf returns the setpoint of an evaluable analog alarm of that type.
func limitOf(defs []store.AlarmDef, typ string) *float64 {
	for _, d := range defs {
		if d.AlarmType == typ && d.Evaluable() && d.Setpoint != nil {
			v := *d.Setpoint
			return &v
		}
	}
	return nil
}

// discreteState returns the abnormal state of an evaluable discrete alarm.
func discreteState(defs []store.AlarmDef) *int {
	for _, d := range defs {
		if d.AlarmType == store.AlarmDiscrete && d.Evaluable() && d.TriggerState != nil {
			v := *d.TriggerState
			return &v
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// PV alarms table (in the PV detail dialog)
// ---------------------------------------------------------------------

// pvAlarmRow is one configured alarm of a PV with what is outstanding for it.
type pvAlarmRow struct {
	Def  store.AlarmDef
	Occs []store.Occurrence // outstanding occurrences, oldest first
}

// Current is the occurrence that describes the alarm now: the one whose
// condition is abnormal, else the newest outstanding one.
func (r pvAlarmRow) Current() *store.Occurrence {
	var cur *store.Occurrence
	for i := range r.Occs {
		o := &r.Occs[i]
		if o.ProcessConditionActive {
			return o
		}
		cur = o
	}
	return cur
}

// Waiting is how many outstanding occurrences besides the current one wait for
// an acknowledgement.
func (r pvAlarmRow) Waiting() int {
	n := 0
	cur := r.Current()
	for i := range r.Occs {
		if &r.Occs[i] != cur && r.Occs[i].State == store.StateRTNUN {
			n++
		}
	}
	return n
}

func (s *Server) pvAlarmRows(defs []store.AlarmDef) []pvAlarmRow {
	rows := make([]pvAlarmRow, len(defs))
	for i, d := range defs {
		rows[i].Def = d
	}
	if s.alarmView == nil {
		return rows
	}
	for _, e := range s.alarmView.Snapshot() {
		for i := range rows {
			if rows[i].Def.ID == e.Def.ID {
				rows[i].Occs = append(rows[i].Occs, e.Occ)
			}
		}
	}
	for i := range rows {
		sort.Slice(rows[i].Occs, func(a, b int) bool { return rows[i].Occs[a].ActivatedAt.Before(rows[i].Occs[b].ActivatedAt) })
	}
	return rows
}

// ---------------------------------------------------------------------
// Global alarm ticker
// ---------------------------------------------------------------------

// maxTickerItems bounds the ticker: the oldest active alarms are kept (they
// have waited longest), the rest is counted.
const maxTickerItems = 100

type tickerItem struct {
	OccID    int64
	Time     time.Time
	PLC, PV  string
	Type     string
	Cond     string
	Message  string
	Priority string
	State    string
}

// tickerData is what the ticker shows: the alarms that are active now
// (unacknowledged or acknowledged, not shelved), oldest event first, and how
// many returned-to-normal alarms still wait for an acknowledgement.
type tickerData struct {
	Items    []tickerItem
	More     int // active alarms beyond maxTickerItems
	Returned int
}

func (s *Server) tickerData() tickerData {
	var t tickerData
	if s.alarmView == nil {
		return t
	}
	for _, e := range s.alarmView.Snapshot() {
		switch {
		case e.Occ.State == store.StateRTNUN:
			t.Returned++
		case e.Occ.ProcessConditionActive && (e.Occ.State == store.StateUnack || e.Occ.State == store.StateAcked):
			t.Items = append(t.Items, tickerItem{
				OccID: e.Occ.ID, Time: e.Occ.ActivatedAt, PLC: e.Def.PLCName, PV: e.Def.PVName, Type: e.Def.AlarmType,
				Cond: e.Def.Trigger(), Message: e.Def.Message, Priority: e.Def.Priority, State: e.Occ.State,
			})
		}
	}
	sort.SliceStable(t.Items, func(i, j int) bool {
		if !t.Items[i].Time.Equal(t.Items[j].Time) {
			return t.Items[i].Time.Before(t.Items[j].Time)
		}
		return t.Items[i].OccID < t.Items[j].OccID
	})
	if len(t.Items) > maxTickerItems {
		t.More = len(t.Items) - maxTickerItems
		t.Items = t.Items[:maxTickerItems]
	}
	return t
}

// tickTime writes an event time compactly: the clock time today, with the date
// when it is older.
func tickTime(t time.Time) string {
	t = t.Local()
	if t.YearDay() == time.Now().YearDay() && t.Year() == time.Now().Year() {
		return t.Format("15:04:05")
	}
	return t.Format("01-02 15:04:05")
}
