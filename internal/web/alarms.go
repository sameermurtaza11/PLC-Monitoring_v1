package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/alarms"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// Alarm Management (operator view): the list of outstanding alarms, the
// acknowledge / shelve actions and the history log. Who may do what is decided
// centrally in auth.Rules: every logged-in user acknowledges; supervisors and
// administrators shelve and unshelve.

func init() {
	alarmFuncs := template.FuncMap{
		"stateLabel": stateLabel,
		"stateClass": stateClass,
		"eventLabel": eventLabel,
		"dur":        durText,
		"durSec":     func(s *float64) string { return secText(s) },
		"alarmValue": alarmValue,
		"eventValue": eventValue,
		"rowStatus":  rowStatus,
		"liveValue":  liveValue,
		"canAck":     func(state string) bool { return state == store.StateUnack || state == store.StateRTNUN },
		"respLine":   respLine,
		"since":      func(t time.Time) string { return durText(time.Since(t)) },
		"inState":    func(a, b string) bool { return a == b },
	}
	for k, v := range alarmFuncs {
		funcs[k] = v
	}
}

// ---------------------------------------------------------------------
// Presentation helpers
// ---------------------------------------------------------------------

func stateLabel(s string) string {
	switch s {
	case store.StateUnack:
		return "Unacknowledged"
	case store.StateAcked:
		return "Acknowledged"
	case store.StateRTNUN:
		return "Returned to normal – unacknowledged"
	case store.StateShlvd:
		return "Shelved"
	case store.StateNorm:
		return "Normal"
	}
	return s
}

func stateClass(s string) string {
	switch s {
	case store.StateUnack:
		return "bad"
	case store.StateRTNUN:
		return "warn"
	case store.StateAcked:
		return "info"
	}
	return "none"
}

func eventLabel(e string) string {
	switch e {
	case "ACTIVATED":
		return "Activated"
	case "ACKNOWLEDGED":
		return "Acknowledged"
	case "RETURNED_TO_NORMAL":
		return "Returned to normal"
	case "SHELVED":
		return "Shelved"
	case "UNSHELVED":
		return "Unshelved"
	}
	return e
}

// durText writes a duration the way an operator reads it: "45 s", "3 min 05 s", "2 h 10 min".
func durText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		return fmt.Sprintf("%d min %02d s", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%d h %02d min", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%d d %d h", s/86400, s%86400/3600)
}

func secText(s *float64) string {
	if s == nil {
		return "—"
	}
	return durText(time.Duration(*s * float64(time.Second)))
}

// alarmValue formats a process value of an alarm's PV: the state text for a
// digital input, the number with its unit for an analog one.
func alarmValue(d store.AlarmDef, v *float64) string {
	if v == nil {
		return "—"
	}
	if d.Digital {
		if *v >= 0.5 {
			return d.State1Text
		}
		return d.State0Text
	}
	return strconv.FormatFloat(*v, 'f', 2, 64) + " " + d.PVUnit
}

// eventValue formats the process value recorded with a history event ("—"
// when none was captured, e.g. an automatic shelf expiry).
func eventValue(e store.HistoryEvent) string {
	if e.Value == nil {
		return "—"
	}
	if e.Digital {
		if *e.Value >= 0.5 {
			return e.State1Text
		}
		return e.State0Text
	}
	return strconv.FormatFloat(*e.Value, 'f', 2, 64) + " " + e.Unit
}

func liveValue(d store.AlarmDef, live *store.LatestRow) string {
	if live == nil || live.Value == nil {
		return "—"
	}
	s := alarmValue(d, live.Value)
	if live.Quality != nil && *live.Quality != store.QualityGood {
		return s + " (" + *live.Quality + ")"
	}
	return s
}

// respInfo is the outcome of comparing the operator's response with the limit.
type respInfo struct{ Text, Class string }

// respLine compares the operator's response with the alarm's maximum
// allowable response time (rationalization).
func respLine(d store.AlarmDef, o store.Occurrence) respInfo {
	if d.MaxResponseTimeS == nil {
		return respInfo{"not defined", "none"}
	}
	limit := time.Duration(*d.MaxResponseTimeS) * time.Second
	switch {
	case o.State == store.StateUnack:
		if el := time.Since(o.ActivatedAt); el > limit {
			return respInfo{"limit " + durText(limit) + " – exceeded, unacknowledged for " + durText(el), "bad"}
		}
		return respInfo{"limit " + durText(limit) + " – " + durText(time.Since(o.ActivatedAt)) + " elapsed", "none"}
	case o.AckedAt != nil:
		took := o.AckedAt.Sub(o.ActivatedAt)
		if took > limit {
			return respInfo{"limit " + durText(limit) + " – acknowledged after " + durText(took) + " (exceeded)", "warn"}
		}
		return respInfo{"limit " + durText(limit) + " – acknowledged after " + durText(took), "ok"}
	}
	return respInfo{"limit " + durText(limit), "none"}
}

// ---------------------------------------------------------------------
// Rows for the list
// ---------------------------------------------------------------------

type alarmRow struct {
	E    alarms.Entry
	Live *store.LatestRow
}

func (s *Server) alarmRows() []alarmRow {
	if s.alarmView == nil {
		return nil
	}
	entries := s.alarmView.Snapshot()
	rows := make([]alarmRow, len(entries))
	for i, e := range entries {
		rows[i].E = e
		if s.live != nil {
			if l, ok := s.live.Get(e.Def.PVID); ok {
				l := l
				rows[i].Live = &l
			}
		}
	}
	return rows
}

func (s *Server) renderAlarmRows() (string, error) {
	return s.render("alarm_mgmt_rows.html", s.alarmRows())
}

// unackCount is the number shown in the menu badge: alarms waiting for an
// acknowledgement (see alarms.Counts.NeedsAck).
func (s *Server) unackCount() int {
	if s.alarmView == nil {
		return 0
	}
	return s.alarmView.Counts().NeedsAck()
}

func (s *Server) renderAlarmSummary() (string, error) {
	var c alarms.Counts
	if s.alarmView != nil {
		c = s.alarmView.Counts()
	}
	return s.render("alarm_summary.html", c)
}

// ---------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------

func (s *Server) alarmsPage(c *gin.Context) {
	var counts alarms.Counts
	if s.alarmView != nil {
		counts = s.alarmView.Counts()
	}
	c.HTML(http.StatusOK, "alarms.html", s.pageData(c, "alarms", gin.H{"Counts": counts}))
}

func (s *Server) alarmRowsPartial(c *gin.Context) {
	c.HTML(http.StatusOK, "alarm_mgmt_rows.html", s.alarmRows())
}

// ---------------------------------------------------------------------
// Detail modal ("More")
// ---------------------------------------------------------------------

// alarmDetail is everything the modal shows about one occurrence.
type alarmDetail struct {
	Gone      bool
	Occ       store.Occurrence
	Def       store.AlarmDef
	Live      *store.LatestRow
	Events    []store.HistoryEvent
	CanShelve bool
	MaxShelve string // '' = no site limit
}

func (s *Server) canShelve(u *auth.User) bool { return u != nil && u.Role.Allows(auth.Config) }

// detailOf collects the data of an occurrence. events may be passed in when
// the caller already has them (the push loop re-reads them only on change).
func (s *Server) detailOf(c *gin.Context, id int64, user *auth.User, events []store.HistoryEvent) (alarmDetail, error) {
	d := alarmDetail{CanShelve: s.canShelve(user)}
	if s.maxShelve > 0 {
		d.MaxShelve = durText(s.maxShelve)
	}
	ctx := c.Request.Context()
	if e, ok := s.alarmView.Get(id); ok {
		d.Occ, d.Def = e.Occ, e.Def
	} else {
		// Not in the list: completed, or the list is a moment behind.
		o, err := s.store.GetOccurrence(ctx, id)
		switch {
		case err == nil:
			d.Occ = o
			if d.Def, err = s.store.GetAlarmDef(ctx, o.AlarmID); err != nil {
				return d, err
			}
		case errors.Is(err, store.ErrOccurrenceGone):
			d.Gone = true
		default:
			return d, err
		}
	}
	if events == nil {
		var err error
		if events, err = s.store.OccurrenceEvents(ctx, id); err != nil {
			return d, err
		}
	}
	d.Events = events
	if d.Gone {
		d.Occ.ID = id
		if len(events) == 0 {
			return d, store.ErrOccurrenceGone
		}
		d.Def, _ = s.store.GetAlarmDef(ctx, events[0].AlarmID)
	}
	if s.live != nil {
		if l, ok := s.live.Get(d.Def.PVID); ok {
			d.Live = &l
		}
	}
	return d, nil
}

func (s *Server) alarmDetailPage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "invalid alarm id")
		return
	}
	if c.GetHeader("HX-Request") == "" {
		c.Redirect(http.StatusFound, "/alarms?alarm="+strconv.FormatInt(id, 10))
		return
	}
	if s.alarmView == nil {
		c.String(http.StatusServiceUnavailable, "alarms are not available")
		return
	}
	d, err := s.detailOf(c, id, auth.FromContext(c), nil)
	if errors.Is(err, store.ErrOccurrenceGone) {
		c.String(http.StatusNotFound, "Alarm occurrence not found")
		return
	}
	if err != nil {
		log.Printf("web: alarm %d detail: %v", id, err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "alarm_detail.html", d)
}

// ---------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------

func alarmMsg(c *gin.Context, kind, text string) {
	payload, _ := json.Marshal(map[string]any{"alarmMsg": map[string]string{"kind": kind, "text": text}})
	c.Header("HX-Trigger", string(payload))
	c.Status(http.StatusNoContent)
}

// valueNow is the current good process value of an alarm's PV, recorded in the
// history with the operator's action; nil when it is not known (no live value,
// or the reading is not trustworthy).
func (s *Server) valueNow(occ int64) *float64 {
	if s.alarmView == nil || s.live == nil {
		return nil
	}
	e, ok := s.alarmView.Get(occ)
	if !ok {
		return nil
	}
	l, ok := s.live.Get(e.Def.PVID)
	if !ok || l.Value == nil || l.Quality == nil || *l.Quality != store.QualityGood {
		return nil
	}
	v := *l.Value
	return &v
}

func (s *Server) afterAlarmAction() {
	if s.alarmView != nil {
		s.alarmView.Poke()
	}
}

func (s *Server) occurrenceID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		alarmMsg(c, "error", "Invalid alarm.")
		return 0, false
	}
	return id, true
}

// actionError turns a store error into the operator's message.
func actionError(c *gin.Context, what string, err error) {
	var ce *store.ConstraintError
	switch {
	case errors.Is(err, store.ErrOccurrenceGone):
		alarmMsg(c, "error", "This alarm is no longer outstanding (another user may have handled it). The list has been updated.")
	case errors.Is(err, store.ErrWrongAlarmState):
		alarmMsg(c, "error", "Cannot "+what+": the alarm changed state in the meantime. The list has been updated.")
	case errors.As(err, &ce):
		alarmMsg(c, "error", "Rejected by the database: "+ce.Message)
	default:
		log.Printf("web: alarm %s: %v", what, err)
		alarmMsg(c, "error", "Could not "+what+" – the database did not accept it. Try again.")
	}
}

func (s *Server) alarmAck(c *gin.Context) {
	id, ok := s.occurrenceID(c)
	if !ok {
		return
	}
	if err := s.store.Acknowledge(c.Request.Context(), id, actorOf(c), s.valueNow(id)); err != nil {
		s.afterAlarmAction()
		actionError(c, "acknowledge", err)
		return
	}
	s.afterAlarmAction()
	alarmMsg(c, "ok", "Alarm acknowledged.")
}

// Shelving durations the user can choose from; the amount is typed by the user.
var shelveUnits = map[string]time.Duration{"minutes": time.Minute, "hours": time.Hour, "days": 24 * time.Hour}

func (s *Server) alarmShelve(c *gin.Context) {
	id, ok := s.occurrenceID(c)
	if !ok {
		return
	}
	reason := strings.TrimSpace(c.PostForm("reason"))
	amount, err := strconv.Atoi(strings.TrimSpace(c.PostForm("amount")))
	unit, unitOK := shelveUnits[c.PostForm("unit")]
	switch {
	case reason == "":
		alarmMsg(c, "error", "Enter the reason for shelving.")
		return
	case len(reason) > 500:
		alarmMsg(c, "error", "The reason is too long (500 characters at most).")
		return
	case err != nil || amount < 1 || !unitOK:
		alarmMsg(c, "error", "Choose how long the alarm is shelved: a whole number (1 or more) and a unit.")
		return
	}
	d := time.Duration(amount) * unit
	if amount > 100000 || d <= 0 { // overflow guard
		alarmMsg(c, "error", "That shelving time is not valid.")
		return
	}
	if s.maxShelve > 0 && d > s.maxShelve {
		alarmMsg(c, "error", "The longest allowed shelving time is "+durText(s.maxShelve)+".")
		return
	}
	if err := s.store.Shelve(c.Request.Context(), id, time.Now().Add(d), reason, actorOf(c), s.valueNow(id)); err != nil {
		s.afterAlarmAction()
		actionError(c, "shelve", err)
		return
	}
	s.afterAlarmAction()
	alarmMsg(c, "ok", "Alarm shelved for "+durText(d)+".")
}

func (s *Server) alarmUnshelve(c *gin.Context) {
	id, ok := s.occurrenceID(c)
	if !ok {
		return
	}
	if err := s.store.Unshelve(c.Request.Context(), id, actorOf(c), s.valueNow(id)); err != nil {
		s.afterAlarmAction()
		actionError(c, "unshelve", err)
		return
	}
	s.afterAlarmAction()
	alarmMsg(c, "ok", "Alarm returned to the alarm list.")
}

// ---------------------------------------------------------------------
// History log and statistics
// ---------------------------------------------------------------------

var historyRanges = []struct{ Key, Label string }{
	{"1h", "Last hour"}, {"24h", "Last 24 hours"}, {"7d", "Last 7 days"}, {"30d", "Last 30 days"}, {"all", "All"},
}

var historySince = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

const historyLimit = 500

func (s *Server) alarmHistory(c *gin.Context) {
	rng := c.DefaultQuery("range", "24h")
	since := time.Unix(0, 0)
	if d, ok := historySince[rng]; ok {
		since = time.Now().Add(-d)
	} else {
		rng = "all"
	}
	f := store.HistoryFilter{Since: since, Priority: c.Query("priority"), Event: c.Query("event"), Limit: historyLimit}
	if f.Priority != "" && !inList(store.AlarmPriorities, f.Priority) {
		f.Priority = ""
	}
	if f.Event != "" && !inList([]string{"ACTIVATED", "ACKNOWLEDGED", "RETURNED_TO_NORMAL", "SHELVED", "UNSHELVED"}, f.Event) {
		f.Event = ""
	}
	f.AlarmID, _ = strconv.ParseInt(c.Query("alarm"), 10, 64)

	ctx := c.Request.Context()
	events, total, err := s.store.AlarmEvents(ctx, f)
	if err == nil {
		var stats store.AlarmStats
		var defs []store.AlarmDef
		if stats, err = s.store.AlarmStatistics(ctx, since); err == nil {
			if defs, err = s.store.ListAlarmDefs(ctx); err == nil {
				c.HTML(http.StatusOK, "alarm_history.html", s.pageData(c, "alarm_history", gin.H{
					"Events": events, "Total": total, "Limit": historyLimit, "Stats": stats, "Defs": defs,
					"Range": rng, "Ranges": historyRanges, "Priority": f.Priority, "Priorities": store.AlarmPriorities,
					"AlarmID": f.AlarmID, "Event": f.Event,
					"EventTypes": []string{"ACTIVATED", "ACKNOWLEDGED", "RETURNED_TO_NORMAL", "SHELVED", "UNSHELVED"},
				}))
				return
			}
		}
	}
	log.Printf("web: alarm history: %v", err)
	c.String(http.StatusInternalServerError, "database error")
}

func inList(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Alarm state on the dashboard
// ---------------------------------------------------------------------

// pvRow is a dashboard row: the live values of a PV plus its active alarm, if
// any. Alarm limits are not part of pv_metadata; they come from the alarm tables.
type pvRow struct {
	store.LatestRow
	Alarm *store.AlarmDef // the most severe active (not shelved) alarm of this PV
}

func severityRank(d *store.AlarmDef) int {
	r := 0
	switch d.Priority {
	case store.PriorityCritical:
		r = 20
	case store.PriorityMajor:
		r = 10
	}
	if d.AlarmType == store.AlarmHIHI || d.AlarmType == store.AlarmLOLO {
		r += 5
	}
	return r
}

// activeAlarmsByPV maps PV → its most severe alarm whose process condition is
// abnormal and that is not shelved (a shelved alarm is suppressed).
func (s *Server) activeAlarmsByPV() map[int]*store.AlarmDef {
	if s.alarmView == nil {
		return nil
	}
	out := map[int]*store.AlarmDef{}
	for _, e := range s.alarmView.Snapshot() {
		if !e.Occ.ProcessConditionActive || e.Occ.State == store.StateShlvd {
			continue
		}
		d := e.Def
		if cur, ok := out[d.PVID]; !ok || severityRank(&d) > severityRank(cur) {
			out[d.PVID] = &d
		}
	}
	return out
}

func (s *Server) rowView(r store.LatestRow) pvRow {
	return pvRow{LatestRow: r, Alarm: s.activeAlarmsByPV()[r.ID]}
}

func (s *Server) pvRowViews() []pvRow {
	rows := s.live.Snapshot()
	active := s.activeAlarmsByPV()
	out := make([]pvRow, len(rows))
	for i, r := range rows {
		out[i] = pvRow{LatestRow: r, Alarm: active[r.ID]}
	}
	return out
}

// rowStatus: a communication / data problem first, then the PV's active alarm.
func rowStatus(r pvRow) Status {
	st := statusOf(r.LatestRow)
	if st.Class != "ok" || r.Alarm == nil {
		return st
	}
	text, class := "ALARM", "warn"
	if r.Alarm.AlarmType != store.AlarmDiscrete {
		text = r.Alarm.AlarmType
	}
	if r.Alarm.Priority == store.PriorityCritical {
		class = "bad"
	}
	return Status{text, class}
}

// limitOf returns the setpoint of an active-evaluable analog alarm of that type.
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
