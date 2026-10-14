package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/store"
)

// Flow of every form (HTMX):
//
//	GET  …/new or …/:id/edit → form HTML swapped into the form dialog
//	POST …                   → invalid: same form + field errors (200)
//	                           valid:   204 + HX-Trigger "configSaved"
//	                                    (dialog closes, lists reload)
//	DELETE …                 → message HTML into #config-msg
//
// Validation happens twice on purpose: here (friendly per-field messages)
// and in PostgreSQL constraints (the guarantee).

type fieldErrors map[string]string

// form keeps what the user typed, so an invalid form is re-shown unchanged.
type form struct {
	ID     int               // 0 = new
	Values map[string]string // field name → typed value
	Errors fieldErrors
	PLCs   []store.PLCRecord // for the PV form's PLC select

	Defs    map[string]store.AlarmDef // stored alarms of the PV by type (PV form)
	IsAdmin bool                      // may approve alarms in the PV form
}

func (f form) V(name string) string     { return f.Values[name] }
func (f form) Checked(name string) bool { return f.Values[name] == "on" }

func (s *Server) configPage(c *gin.Context) {
	plcs, err := s.store.ListPLCs(c.Request.Context())
	if err != nil {
		log.Printf("web: list plcs: %v", err)
	}
	c.HTML(http.StatusOK, "config.html", s.pageData(c, "config", gin.H{"PLCs": plcs}))
}

// saved closes the dialog, refreshes the lists and reloads acquisition.
func (s *Server) saved(c *gin.Context, what string) {
	s.onConfigChange()
	payload, _ := json.Marshal(map[string]string{"configSaved": what})
	c.Header("HX-Trigger", string(payload))
	c.Status(http.StatusNoContent)
}

func (s *Server) message(c *gin.Context, class, text string) {
	c.HTML(http.StatusOK, "config_msg.html", gin.H{"Class": class, "Text": text})
}

// ---------------------------------------------------------------------
// PLC
// ---------------------------------------------------------------------

func (s *Server) plcList(c *gin.Context) {
	plcs, err := s.store.ListPLCs(c.Request.Context())
	if err != nil {
		log.Printf("web: list plcs: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "plc_rows.html", plcs)
}

func (s *Server) plcForm(c *gin.Context) {
	f := form{Values: map[string]string{
		"port": "502", "unit_id": "1", "scan_interval_ms": "1000", "timeout_ms": "3000", "enabled": "on",
	}}
	if idStr := c.Param("id"); idStr != "" {
		id, _ := strconv.Atoi(idStr)
		p, err := s.store.GetPLC(c.Request.Context(), id)
		if err != nil {
			c.String(http.StatusNotFound, "PLC not found")
			return
		}
		f.ID = p.ID
		f.Values = map[string]string{
			"plc_name": p.Name, "ip_address": p.IP, "port": strconv.Itoa(p.Port),
			"unit_id": strconv.Itoa(p.UnitID), "scan_interval_ms": strconv.Itoa(p.ScanIntervalMS),
			"timeout_ms": strconv.Itoa(p.TimeoutMS), "description": p.Description,
			"enabled": onOff(p.Enabled),
		}
	}
	c.HTML(http.StatusOK, "plc_form.html", f)
}

func (s *Server) plcSave(c *gin.Context) {
	f := readForm(c, "plc_name", "ip_address", "port", "unit_id", "scan_interval_ms", "timeout_ms", "description", "enabled")
	f.ID, _ = strconv.Atoi(c.Param("id"))

	p := store.PLCRecord{ID: f.ID, Enabled: f.Checked("enabled"), Description: f.V("description")}
	p.Name = requiredText(f, "plc_name", 64)
	p.IP = f.V("ip_address")
	if ip := net.ParseIP(p.IP); ip == nil || ip.To4() == nil {
		f.Errors["ip_address"] = "Enter a valid IPv4 address, e.g. 192.168.1.10"
	}
	p.Port = intField(f, "port", 1, 65535)
	p.UnitID = intField(f, "unit_id", 0, 255)
	p.ScanIntervalMS = intField(f, "scan_interval_ms", 100, 3_600_000)
	p.TimeoutMS = intField(f, "timeout_ms", 100, 60_000)

	if len(f.Errors) == 0 {
		_, err := s.store.SavePLC(c.Request.Context(), p)
		if err == nil {
			s.saved(c, "PLC "+p.Name+" saved")
			return
		}
		if !mapConstraint(err, f.Errors) {
			log.Printf("web: save plc: %v", err)
			f.Errors["_"] = "Could not save: " + err.Error()
		}
	}
	c.HTML(http.StatusOK, "plc_form.html", f)
}

func (s *Server) plcDelete(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	err := s.store.DeletePLC(c.Request.Context(), id)
	switch {
	case errors.Is(err, store.ErrInUse):
		s.message(c, "error", "This PLC still has PVs. Delete or move its PVs first, or just disable the PLC.")
	case errors.Is(err, pgx.ErrNoRows):
		s.message(c, "error", "PLC not found (already deleted?).")
	case err != nil:
		log.Printf("web: delete plc %d: %v", id, err)
		s.message(c, "error", "Delete failed: "+err.Error())
	default:
		s.onConfigChange()
		c.Header("HX-Trigger", "configSaved")
		s.message(c, "ok", "PLC deleted.")
	}
}

// ---------------------------------------------------------------------
// PV
// ---------------------------------------------------------------------

func (s *Server) pvList(c *gin.Context) {
	plcID, _ := strconv.Atoi(c.Query("plc"))
	pvs, err := s.store.ListPVs(c.Request.Context(), plcID)
	if err != nil {
		log.Printf("web: list pvs: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "pv_config_rows.html", pvs)
}

// ---------------------------------------------------------------------
// Form helpers
// ---------------------------------------------------------------------

func readForm(c *gin.Context, names ...string) form {
	f := form{Values: map[string]string{}, Errors: fieldErrors{}}
	for _, n := range names {
		f.Values[n] = strings.TrimSpace(c.PostForm(n))
	}
	return f
}

func requiredText(f form, name string, max int) string {
	v := f.V(name)
	switch {
	case v == "":
		f.Errors[name] = "Required"
	case len(v) > max:
		f.Errors[name] = fmt.Sprintf("Maximum %d characters", max)
	}
	return v
}

func intField(f form, name string, min, max int) int {
	n, err := strconv.Atoi(f.V(name))
	if err != nil {
		f.Errors[name] = "Enter a whole number"
		return 0
	}
	if n < min || n > max {
		f.Errors[name] = fmt.Sprintf("Must be between %d and %d", min, max)
	}
	return n
}

func floatField(f form, name string) float64 {
	v, err := strconv.ParseFloat(f.V(name), 64)
	if err != nil {
		f.Errors[name] = "Enter a number"
	}
	return v
}

// optFloatField: empty = not configured (NULL in the database).
func optFloatField(f form, name string) *float64 {
	if f.V(name) == "" {
		return nil
	}
	v := floatField(f, name)
	return &v
}

// mapConstraint puts a PostgreSQL constraint violation next to its field.
func mapConstraint(err error, errs fieldErrors) bool {
	var ce *store.ConstraintError
	if !errors.As(err, &ce) {
		return false
	}
	switch ce.Constraint {
	case "plc_metadata_plc_name_key":
		errs["plc_name"] = "A PLC with this name already exists"
	case "pv_unique_register":
		errs["register_address"] = "This register is already used by another PV on this PLC"
	case "pv_unique_name":
		errs["pv_name"] = "This PV name already exists on this PLC"
	case "pv_metadata_plc_id_fkey":
		errs["plc_id"] = "Selected PLC no longer exists"
	default:
		errs["_"] = "Rejected by database: " + ce.Message
	}
	return true
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return ""
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func optItoa(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func optFtoa(v *float64) string {
	if v == nil {
		return ""
	}
	return ftoa(*v)
}
