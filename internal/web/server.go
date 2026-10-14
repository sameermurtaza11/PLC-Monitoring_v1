// Package web is the Gin HTTP server: pages, HTMX partials and static files.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/alarms"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/live"
	"PLC_Monitoring/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Options are the login/session settings (see config.Config).
type Options struct {
	CookieSecure    bool          // Secure flag of the session cookie (HTTPS deployments)
	SameSiteStrict  bool          // SameSite=Strict instead of Lax
	SessionLifetime time.Duration // 0 = auth.DefaultSessionLifetime
	// Pipeline returns the health of the sample pipeline (queues, drops,
	// scan statistics) for GET /status/pipeline. May be nil.
	Pipeline func() any
	// Live is the live-data module: dashboard rows and the PV detail are served
	// from its memory and pushed to browsers. May be nil (then the database is read).
	Live *live.Module
	// AlarmView is the list of outstanding alarms (alarm page); MaxShelve is the
	// longest time an alarm may be shelved (0 = no site limit: the user still
	// has to choose a finite time); AlarmReload tells the alarm engine to read
	// the alarm definitions again after a configuration change.
	AlarmView   *alarms.View
	MaxShelve   time.Duration
	AlarmReload func()
}

type Server struct {
	store          *store.Store
	auth           *auth.Service
	cookie         auth.CookieOptions
	pipeline       func() any
	live           *live.Module
	alarmView      *alarms.View
	maxShelve      time.Duration
	alarmReload    func()
	tmpl           *template.Template
	alarmCfg       *alarms.ConfigService
	onConfigChange func() // tells acquisition to reload PLC/PV config now
}

func NewRouter(s *store.Store, onConfigChange func(), opt Options) *gin.Engine {
	cookie := auth.CookieOptions{Secure: opt.CookieSecure, SameSite: http.SameSiteLaxMode}
	if opt.SameSiteStrict {
		cookie.SameSite = http.SameSiteStrictMode
	}
	srv := &Server{
		store: s, auth: auth.NewService(s, opt.SessionLifetime), cookie: cookie, pipeline: opt.Pipeline, live: opt.Live, alarmView: opt.AlarmView, maxShelve: opt.MaxShelve, alarmReload: opt.AlarmReload, alarmCfg: alarms.NewConfigService(s),
		onConfigChange: onConfigChange,
	}

	r := gin.New()
	// auth.Load identifies the user; auth.Guard then applies auth.Rules to EVERY
	// route (a route without a rule is closed to non-administrators).
	r.Use(gin.Recovery(), requestLogger(), auth.Load(srv.auth), auth.Guard(srv.denied))

	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	r.SetHTMLTemplate(tmpl)
	srv.tmpl = tmpl

	static, _ := fs.Sub(staticFS, "static")
	r.StaticFS("/static", http.FS(static))

	r.GET("/", srv.dashboard)
	r.GET("/partials/pv-rows", srv.pvRows)
	r.GET("/pv/:id", srv.pvDetail)
	r.GET("/events", srv.events) // pushed live updates (Server-Sent Events)
	// Alarm management: every logged-in user sees and acknowledges; supervisors
	// and administrators shelve (auth.Rules)
	r.GET("/alarms", srv.alarmsPage)
	r.GET("/partials/alarm-rows", srv.alarmRowsPartial)
	r.GET("/alarms/history", srv.alarmHistory)
	r.GET("/alarm/:id", srv.alarmDetailPage)
	r.GET("/pv/:id/history", srv.pvHistory)
	// Configuration (CRUD for plc_metadata / pv_metadata)
	r.GET("/config", srv.configPage)
	r.GET("/config/plcs", srv.plcList)
	r.GET("/config/plcs/new", srv.plcForm)
	r.GET("/config/plcs/:id/edit", srv.plcForm)
	csrf := auth.RequireCSRF() // pages send the token in the X-CSRF-Token header (see config.html)
	r.POST("/config/plcs", csrf, srv.plcSave)
	r.POST("/config/plcs/:id", csrf, srv.plcSave)
	r.DELETE("/config/plcs/:id", csrf, srv.plcDelete)
	r.GET("/config/pvs", srv.pvList)
	r.GET("/config/pvs/new", srv.pvForm)
	r.GET("/config/pvs/:id/edit", srv.pvForm)
	r.POST("/config/pvs", csrf, srv.pvSave)
	r.POST("/config/pvs/:id", csrf, srv.pvSave)
	r.DELETE("/config/pvs/:id", csrf, srv.pvDelete)

	// Alarm configuration (supervisors and administrators; approval: administrators)
	r.GET("/config/alarms", srv.alarmList)
	r.GET("/config/alarms/new", srv.alarmForm)
	r.GET("/config/alarms/:id/edit", srv.alarmForm)
	r.POST("/config/alarms", csrf, srv.alarmSave)
	r.POST("/config/alarms/:id", csrf, srv.alarmSave)
	r.POST("/config/alarms/:id/approve", csrf, srv.alarmApprove)
	r.POST("/alarm/:id/ack", csrf, srv.alarmAck)
	r.POST("/alarm/:id/shelve", csrf, srv.alarmShelve)
	r.POST("/alarm/:id/unshelve", csrf, srv.alarmUnshelve)

	// User management (administrators only)
	r.GET("/config/users", srv.userList)
	r.GET("/config/users/new", srv.userForm)
	r.GET("/config/users/:id/edit", srv.userForm)
	r.POST("/config/users", csrf, srv.userSave)
	r.POST("/config/users/:id", csrf, srv.userSave)
	r.DELETE("/config/users/:id", csrf, srv.userDelete)

	// Login, logout (public / any logged-in user)
	r.GET("/login", srv.loginPage)
	r.POST("/login", srv.login)
	r.POST("/logout", auth.RequireCSRF(), srv.logout)

	r.GET("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/status/pipeline", srv.pipelineStatus)
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	return r
}

// ---------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------

func (s *Server) dashboard(c *gin.Context) {
	c.HTML(http.StatusOK, "dashboard.html", s.pageData(c, "dashboard", nil))
}

// pvRows returns the <tr> rows once, when the dashboard opens (the initial
// values); afterwards the page is updated by the pushed /events stream. The
// values come from the live module's memory, not from pv_latest.
func (s *Server) pvRows(c *gin.Context) {
	if s.live != nil {
		c.HTML(http.StatusOK, "pv_rows.html", s.live.Snapshot())
		return
	}
	rows, err := s.store.LatestValues(c.Request.Context())
	if err != nil {
		log.Printf("web: latest values: %v", err)
		c.HTML(http.StatusOK, "pv_rows_error.html", err.Error())
		return
	}
	c.HTML(http.StatusOK, "pv_rows.html", rows)
}

// latestOf returns one PV's current values: from the live module when there is
// one, else from the database. pgx.ErrNoRows if the PV does not exist.
func (s *Server) latestOf(c *gin.Context, id int) (store.LatestRow, error) {
	if s.live != nil {
		if row, ok := s.live.Get(id); ok {
			return row, nil
		}
		return store.LatestRow{}, pgx.ErrNoRows
	}
	return s.store.LatestValue(c.Request.Context(), id)
}

func (s *Server) pvDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.String(http.StatusBadRequest, "invalid PV id")
		return
	}
	// Opened directly (not by HTMX from the dashboard): show it inside the
	// styled dashboard instead of a bare HTML fragment.
	if c.GetHeader("HX-Request") == "" {
		c.Redirect(http.StatusFound, "/?pv="+strconv.Itoa(id))
		return
	}
	row, err := s.latestOf(c, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "PV not found")
		return
	}
	if err != nil {
		log.Printf("web: pv %d: %v", id, err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	options, err := s.store.PVOptions(c.Request.Context())
	if err != nil {
		log.Printf("web: pv options: %v", err)
	}
	c.HTML(http.StatusOK, "pv_detail.html", gin.H{
		"PV":      row,
		"Options": options,
		"Ranges":  rangeOrder,
	})
}

// ---------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------

// Status is what the "Status" column shows.
type Status struct {
	Text  string
	Class string // CSS class: ok | warn | bad | none
}

// statusOf decides a PV's status. Order matters: comm problems first,
// then stale data, then alarms.
func statusOf(r store.LatestRow) Status {
	if r.TS == nil || r.Quality == nil {
		return Status{"No data", "none"}
	}
	if *r.Quality != store.QualityGood {
		return Status{*r.Quality, "bad"}
	}
	// No update for 3 scan intervals (min 5 s) → the value can't be trusted.
	staleAfter := max(3*time.Duration(r.ScanIntervalMS)*time.Millisecond, 5*time.Second)
	if time.Since(*r.TS) > staleAfter {
		return Status{"STALE", "bad"}
	}
	if r.IsDigital() {
		if r.Value != nil && r.AlarmState != nil && int(*r.Value) == *r.AlarmState {
			return Status{"ALARM", "warn"}
		}
		return Status{"Normal", "ok"}
	}
	if r.Value != nil && r.AlarmHigh != nil && *r.Value >= *r.AlarmHigh {
		return Status{"HIGH", "warn"}
	}
	if r.Value != nil && r.AlarmLow != nil && *r.Value <= *r.AlarmLow {
		return Status{"LOW", "warn"}
	}
	return Status{"Normal", "ok"}
}

// fieldRef lets a template pass (form, field name) to the "field_error" block.
type fieldRef struct {
	Errors fieldErrors
	Name   string
}

var funcs = template.FuncMap{
	"fe":     func(f form, name string) fieldRef { return fieldRef{f.Errors, name} },
	"status": statusOf,
	// alarms
	"priorityClass": func(p string) string {
		switch p {
		case store.PriorityCritical:
			return "bad"
		case store.PriorityMajor:
			return "warn"
		}
		return "none"
	},
	// users
	"roleLabel": func(r string) string { return auth.Role(r).Label() },
	"roleClass": func(r string) string {
		switch auth.Role(r) {
		case auth.Admin:
			return "bad"
		case auth.Supervisor:
			return "warn"
		}
		return "none"
	},
	"dt": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") },
	"lastOnline": func(t *time.Time) string {
		if t == nil {
			return "Never"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	// display: the value as an operator reads it — state text for digital
	// inputs, 2 decimals for analog values.
	"display": func(r store.LatestRow) string {
		if r.Value == nil {
			return "—"
		}
		if r.IsDigital() {
			return r.StateText(*r.Value)
		}
		return strconv.FormatFloat(*r.Value, 'f', 2, 64)
	},
	// kind: short type tag for tables.
	"kind": func(addr int) string {
		switch {
		case addr >= 10001 && addr <= 19999:
			return "DI"
		case addr >= 30001 && addr <= 39999:
			return "AI"
		default:
			return "HR"
		}
	},
	"deref":  func(v *float64) float64 { return *v },
	"deref2": func(v *int) int { return *v },
	"alarmStateText": func(pv store.PV) string {
		if pv.AlarmState == nil {
			return "—"
		}
		return "when " + pv.StateText(float64(*pv.AlarmState))
	},
	"num": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return strconv.FormatFloat(*v, 'f', 2, 64)
	},
	"int": func(v *int) string {
		if v == nil {
			return "—"
		}
		return strconv.Itoa(*v)
	},
	"f": func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) },
	"time": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Local().Format("2006-01-02 15:04:05.000")
	},
	"ts":   func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05.000") },
	"fmt2": func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
	"rawValue": func(v *float64) string { // for data-* attributes: "" when no value
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'f', -1, 64)
	},
	"orDash": func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	},
	"optNum": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprint(*v)
	},
}

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		// Skip the 2-second polling noise unless it fails.
		p := c.Request.URL.Path
		if c.Writer.Status() < 400 && (p == "/partials/pv-rows" || strings.HasSuffix(p, "/history")) {
			return
		}
		log.Printf("web: %d %s %s (%v)", c.Writer.Status(), c.Request.Method, c.Request.URL.Path, time.Since(start).Round(time.Millisecond))
	}
}
