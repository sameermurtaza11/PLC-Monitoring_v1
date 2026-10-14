package web

import (
	"net/url"
	"strings"

	"PLC_Monitoring/internal/alarms"
	"PLC_Monitoring/internal/store"
)

// alarmFilter narrows the alarm list. Every field is optional ("" = all); a
// value that is not one of the known choices is ignored, never an error.
type alarmFilter struct {
	Priority string // Minor | Major | Critical
	Type     string // HIHI | HI | LO | LOLO | DISCRETE
	State    string // UNACK | RTNUN | ACKED | SHLVD | NEEDS_ACK
	Class    string // PROCESS | EQUIPMENT_PROTECTION | ...
	PLC      string // PLC name
	Q        string // text in the PV name, alarm message or PLC name
}

// stateNeedsAck selects what waits for the operator: UNACK and RTNUN.
const stateNeedsAck = "NEEDS_ACK"

// filterStates are the choices of the State drop-down.
var filterStates = []struct{ Value, Label string }{
	{stateNeedsAck, "Needs acknowledgement"},
	{store.StateUnack, "Unacknowledged"},
	{store.StateRTNUN, "Returned to normal – unacknowledged"},
	{store.StateAcked, "Acknowledged"},
	{store.StateShlvd, "Shelved"},
}

const maxFilterText = 80

// parseAlarmFilter reads the filter from query parameters.
func parseAlarmFilter(get func(string) string) alarmFilter {
	f := alarmFilter{
		Priority: get("priority"), Type: get("type"), State: get("state"), Class: get("class"),
		PLC: strings.TrimSpace(get("plc")), Q: strings.TrimSpace(get("q")),
	}
	if !inList(store.AlarmPriorities, f.Priority) {
		f.Priority = ""
	}
	if !inList(store.AlarmTypes, f.Type) {
		f.Type = ""
	}
	if !inList(store.AlarmClasses, f.Class) {
		f.Class = ""
	}
	valid := f.State == stateNeedsAck
	for _, s := range filterStates {
		valid = valid || s.Value == f.State
	}
	if !valid {
		f.State = ""
	}
	if len(f.PLC) > maxFilterText {
		f.PLC = ""
	}
	if len(f.Q) > maxFilterText {
		f.Q = f.Q[:maxFilterText]
	}
	return f
}

// Active reports whether any criterion is set.
func (f alarmFilter) Active() bool { return f != alarmFilter{} }

// Query is the filter as URL parameters (only the set ones), without a leading "?".
func (f alarmFilter) Query() string {
	v := url.Values{}
	for k, x := range map[string]string{"priority": f.Priority, "type": f.Type, "state": f.State, "class": f.Class, "plc": f.PLC, "q": f.Q} {
		if x != "" {
			v.Set(k, x)
		}
	}
	return v.Encode()
}

// Match reports whether an outstanding alarm passes the filter. All set
// criteria must hold.
func (f alarmFilter) Match(e alarms.Entry) bool {
	d, o := e.Def, e.Occ
	switch {
	case f.Priority != "" && d.Priority != f.Priority,
		f.Type != "" && d.AlarmType != f.Type,
		f.Class != "" && d.AlarmClass != f.Class,
		f.PLC != "" && d.PLCName != f.PLC:
		return false
	}
	switch f.State {
	case "":
	case stateNeedsAck:
		if o.State != store.StateUnack && o.State != store.StateRTNUN {
			return false
		}
	default:
		if o.State != f.State {
			return false
		}
	}
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		if !strings.Contains(strings.ToLower(d.PVName), q) && !strings.Contains(strings.ToLower(d.Message), q) &&
			!strings.Contains(strings.ToLower(d.PLCName), q) {
			return false
		}
	}
	return true
}

// alarmSummary is what the counts line shows: totals by state (unfiltered, so
// the operator always sees everything outstanding) and how many rows the
// filter lets through.
type alarmSummary struct {
	Counts   alarms.Counts
	Shown    int
	Total    int
	Filtered bool
}

// alarmRowsView is what the rows template renders.
type alarmRowsView struct {
	Rows     []alarmRow
	Filtered bool
}
