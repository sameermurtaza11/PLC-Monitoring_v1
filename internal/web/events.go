package web

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

const (
	pushTick      = 500 * time.Millisecond // how often a connection looks for news
	pushRefresh   = time.Second            // re-render at least this often: a value that stops arriving turns STALE
	pushKeepAlive = 15 * time.Second
	pushWriteWait = 5 * time.Second // a browser that does not accept data for this long is disconnected
)

// render executes a named template into a string.
func (s *Server) render(name string, data any) (string, error) {
	var b bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// frame is one named block of HTML pushed to the browser.
type frame struct{ event, html string }

// events is the pushed-updates stream (Server-Sent Events): the browser opens it
// once and receives HTML blocks whenever they change — no polling, no database
// query per browser.
//
//	/events                         dashboard rows ("rows")
//	/events?pv=ID                   + the live block of that PV ("live")
//	/events?view=alarms             alarm list ("rows") and its counts ("summary");
//	                                optional filter: priority, type, state, class, plc, q
//	/events?view=alarms&alarm=OCC   + that alarm's "detail" and "actions"
//	/events?view=badge              only the menu badge ("badge") — for pages without live rows
//
// Every stream also carries "badge": the number of alarms waiting for an acknowledgement.
//
// Each connection reads the live module's memory itself, so a slow browser can
// only delay its own connection: it is conflated to the newest state (nothing
// queues up), and one that stops reading is disconnected after pushWriteWait.
func (s *Server) events(c *gin.Context) {
	if s.live == nil {
		c.String(http.StatusServiceUnavailable, "live data is not available")
		return
	}
	view := c.Query("view")
	idParam := c.Query("pv")
	if view == "alarms" {
		idParam = c.Query("alarm")
	}
	var id int64
	if idParam != "" {
		n, err := strconv.ParseInt(idParam, 10, 64)
		if err != nil || n <= 0 {
			c.String(http.StatusBadRequest, "invalid id")
			return
		}
		id = n
	}

	user := auth.FromContext(c)
	filter := parseAlarmFilter(c.Query)
	var produce func(listChanged bool) []frame
	var version func() [2]uint64
	switch view {
	case "badge":
		version = func() [2]uint64 { return [2]uint64{0, s.alarmVersion()} }
		produce = func(bool) []frame { return []frame{s.badgeFrame()} }
	case "", "pv":
		version = func() [2]uint64 { return [2]uint64{s.live.Version(), s.alarmVersion()} }
		produce = func(bool) []frame { return append(s.pvFrames(int(id)), s.badgeFrame()) }
	case "alarms":
		if s.alarmView == nil {
			c.String(http.StatusServiceUnavailable, "alarms are not available")
			return
		}
		version = func() [2]uint64 { return [2]uint64{s.live.Version(), s.alarmView.Version()} }
		var events []store.HistoryEvent // re-read only when the alarm list changed
		produce = func(listChanged bool) []frame {
			if listChanged {
				events = nil
			}
			return s.alarmFrames(c, id, user, &events, filter)
		}
	default:
		c.String(http.StatusBadRequest, "unknown view")
		return
	}
	if !s.live.Attach() {
		c.String(http.StatusServiceUnavailable, "too many live connections")
		return
	}
	defer s.live.Detach()

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	rc := http.NewResponseController(c.Writer)

	write := func(text string) bool {
		rc.SetWriteDeadline(time.Now().Add(pushWriteWait))
		if _, err := c.Writer.WriteString(text); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(f frame) bool {
		var b strings.Builder
		b.WriteString("event: " + f.event + "\n")
		for _, line := range strings.Split(f.html, "\n") {
			b.WriteString("data: " + strings.TrimRight(line, "\r") + "\n")
		}
		b.WriteString("\n")
		return write(b.String())
	}

	last := map[string]string{}
	var lastVer [2]uint64
	var lastRender, lastWrite time.Time
	first := true
	push := func() bool {
		ver := version()
		if !first && ver == lastVer && time.Since(lastRender) < pushRefresh {
			return true
		}
		listChanged := first || ver[1] != lastVer[1]
		lastVer, lastRender, first = ver, time.Now(), false
		for _, f := range produce(listChanged) {
			if last[f.event] == f.html {
				continue
			}
			if !send(f) {
				return false
			}
			last[f.event], lastWrite = f.html, time.Now()
		}
		return true
	}

	if !write("retry: 2000\n\n") || !push() {
		return
	}
	ticker := time.NewTicker(pushTick)
	defer ticker.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
			if !push() {
				if c.Request.Context().Err() == nil {
					s.live.SlowClient()
				}
				return
			}
			if time.Since(lastWrite) >= pushKeepAlive {
				if !write(": keep-alive\n\n") {
					return
				}
				lastWrite = time.Now()
			}
		}
	}
}

// pvFrames: dashboard rows and, for an open PV detail, its live block.
func (s *Server) pvFrames(pvID int) []frame {
	var out []frame
	if rows, err := s.render("pv_rows.html", s.pvRowViews()); err == nil {
		out = append(out, frame{"rows", rows})
	}
	if pvID != 0 {
		if row, ok := s.live.Get(pvID); ok {
			if html, err := s.render("pv_live.html", s.rowView(row)); err == nil {
				out = append(out, frame{"live", html})
			}
		}
	}
	return out
}

// alarmFrames: the alarm list, its counts and, for an open alarm detail, the
// changing part of the modal and its action buttons.
func (s *Server) alarmFrames(c *gin.Context, occ int64, user *auth.User, events *[]store.HistoryEvent, f alarmFilter) []frame {
	out := []frame{s.badgeFrame()}
	if rows, err := s.renderAlarmRows(f); err == nil {
		out = append(out, frame{"rows", rows})
	}
	if sum, err := s.renderAlarmSummary(f); err == nil {
		out = append(out, frame{"summary", sum})
	}
	if occ == 0 {
		return out
	}
	d, err := s.detailOf(c, occ, user, *events)
	if err != nil {
		return out
	}
	*events = d.Events
	if dyn, err := s.render("alarm_dyn.html", d); err == nil {
		out = append(out, frame{"detail", dyn})
	}
	if act, err := s.render("alarm_actions.html", d); err == nil {
		out = append(out, frame{"actions", act})
	}
	return out
}

func (s *Server) alarmVersion() uint64 {
	if s.alarmView == nil {
		return 0
	}
	return s.alarmView.Version()
}

// badgeFrame carries the number for the menu badge.
func (s *Server) badgeFrame() frame { return frame{"badge", strconv.Itoa(s.unackCount())} }
