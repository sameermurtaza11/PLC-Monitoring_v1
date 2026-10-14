package web

import (
	"net/url"
	"strconv"
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

// ---------------------------------------------------------------------
// Pagination (alarm list and event log)
// ---------------------------------------------------------------------

var pageSizes = []int{25, 50, 100}

// pageSpec is the page asked for: 1-based page number and rows per page.
type pageSpec struct{ Page, Size int }

// parsePage reads page and size from the query; anything invalid falls back
// to page 1 and the default size.
func parsePage(get func(string) string, defaultSize int) pageSpec {
	p := pageSpec{Page: 1, Size: defaultSize}
	if n, err := strconv.Atoi(get("page")); err == nil && n >= 1 && n < 1_000_000 {
		p.Page = n
	}
	if n, err := strconv.Atoi(get("size")); err == nil {
		for _, ok := range pageSizes {
			if n == ok {
				p.Size = n
			}
		}
	}
	return p
}

// pagerView describes the pager under a table.
type pagerView struct {
	Page, Pages, Size, Total, From, To int
	Sizes                              []int
	Base                               string       // query of the other parameters (history links)
	Hidden                             []pagerParam // the same parameters as form fields (history size selector)
}

type pagerParam struct{ Name, Value string }

// newPager clamps the requested page to the pages that exist.
func newPager(p pageSpec, total int) pagerView {
	v := pagerView{Size: p.Size, Total: total, Sizes: pageSizes, Pages: 1}
	if total > 0 {
		v.Pages = (total + p.Size - 1) / p.Size
	}
	v.Page = min(max(p.Page, 1), v.Pages)
	if total > 0 {
		v.From = (v.Page-1)*p.Size + 1
		v.To = min(v.Page*p.Size, total)
	}
	return v
}

func (v pagerView) Offset() int   { return (v.Page - 1) * v.Size }
func (v pagerView) HasPrev() bool { return v.Page > 1 }
func (v pagerView) HasNext() bool { return v.Page < v.Pages }
func (v pagerView) Multi() bool   { return v.Pages > 1 }

// Window lists the page numbers to show: the first, the last and the pages
// around the current one; 0 stands for a gap ("…").
func (v pagerView) Window() []int {
	shown := func(n int) bool { return n == 1 || n == v.Pages || (n >= v.Page-2 && n <= v.Page+2) }
	var out []int
	last := 0
	for n := 1; n <= v.Pages; n++ {
		// a gap of a single page is not worth a "…": show that page
		if !shown(n) && !(shown(n-1) && shown(n+1)) {
			continue
		}
		if last != 0 && n != last+1 {
			out = append(out, 0)
		}
		out = append(out, n)
		last = n
	}
	return out
}

// Link is the history page address for page n, keeping the other parameters.
func (v pagerView) Link(n int) string {
	q := v.Base
	if q != "" {
		q += "&"
	}
	return "?" + q + "page=" + strconv.Itoa(n) + "&size=" + strconv.Itoa(v.Size)
}
