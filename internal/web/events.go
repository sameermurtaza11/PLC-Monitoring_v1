package web

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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

// events is the pushed-updates stream (Server-Sent Events): the browser opens it
// once and receives the dashboard rows ("rows") and, with ?pv=ID, the live block
// of that PV ("live") whenever they change — no polling, no database query.
//
// Each connection reads the live module's memory itself, so a slow browser can
// only delay its own connection: it is conflated to the newest state (nothing
// queues up), and one that stops reading is disconnected after pushWriteWait.
func (s *Server) events(c *gin.Context) {
	if s.live == nil {
		c.String(http.StatusServiceUnavailable, "live data is not available")
		return
	}
	pvID := 0
	if v := c.Query("pv"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid PV id")
			return
		}
		pvID = id
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

	write := func(s2 string) bool {
		rc.SetWriteDeadline(time.Now().Add(pushWriteWait))
		if _, err := c.Writer.WriteString(s2); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(event, html string) bool {
		var b strings.Builder
		b.WriteString("event: " + event + "\n")
		for _, line := range strings.Split(html, "\n") {
			b.WriteString("data: " + strings.TrimRight(line, "\r") + "\n")
		}
		b.WriteString("\n")
		return write(b.String())
	}

	var lastRows, lastLive string
	var lastVer uint64
	var lastRender, lastWrite time.Time
	push := func() bool {
		ver := s.live.Version()
		if ver == lastVer && time.Since(lastRender) < pushRefresh {
			return true
		}
		lastVer, lastRender = ver, time.Now()
		rows, err := s.render("pv_rows.html", s.live.Snapshot())
		if err == nil && rows != lastRows {
			if !send("rows", rows) {
				return false
			}
			lastRows, lastWrite = rows, time.Now()
		}
		if pvID != 0 {
			if row, ok := s.live.Get(pvID); ok {
				if html, err := s.render("pv_live.html", row); err == nil && html != lastLive {
					if !send("live", html) {
						return false
					}
					lastLive, lastWrite = html, time.Now()
				}
			}
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
