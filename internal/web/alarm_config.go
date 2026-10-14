package web

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/alarms"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// Alarm configuration (Configuration page). Who may use each route is decided
// centrally by auth.Rules: supervisors and administrators edit definitions,
// only administrators approve a rationalization.

func actorOf(c *gin.Context) store.Actor {
	u := auth.FromContext(c)
	if u == nil {
		return store.Actor{IP: c.ClientIP()}
	}
	return store.Actor{ID: u.ID, Username: u.Username, IP: c.ClientIP()}
}

func (s *Server) alarmList(c *gin.Context) {
	defs, err := s.store.ListAlarmDefs(c.Request.Context())
	if err != nil {
		log.Printf("web: list alarm definitions: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	active := defs[:0:0]
	for _, d := range defs {
		if !d.PVArchived { // alarms of a deleted PV are kept for the history, not configured
			active = append(active, d)
		}
	}
	c.HTML(http.StatusOK, "alarm_rows.html", gin.H{"Alarms": active, "IsAdmin": auth.FromContext(c).IsAdmin()})
}

func (s *Server) alarmForm(c *gin.Context) {
	f := form{Values: map[string]string{
		"enabled": "on", "deadband": "0", "on_delay_s": "0", "off_delay_s": "0",
	}, Errors: fieldErrors{}}
	var def *store.AlarmDef
	if idStr := c.Param("id"); idStr != "" {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		d, err := s.store.GetAlarmDef(c.Request.Context(), id)
		if err != nil {
			c.String(http.StatusNotFound, "Alarm not found")
			return
		}
		def = &d
		f.ID = int(d.ID)
		f.Values = map[string]string{
			"setpoint": optFtoa(d.Setpoint), "trigger_state": optItoa(d.TriggerState),
			"deadband": ftoa(d.Deadband), "on_delay_s": strconv.Itoa(d.OnDelayS), "off_delay_s": strconv.Itoa(d.OffDelayS),
			"priority": d.Priority, "alarm_class": d.AlarmClass, "alarm_message": d.Message,
			"operator_action": d.OperatorAction, "consequences_of_inaction": d.ConsequencesOfInaction,
			"probable_cause": d.ProbableCause, "max_response_time_s": optItoa(d.MaxResponseTimeS),
			"rationale": d.Rationale, "enabled": onOff(d.Enabled), "unit": d.PVUnit,
		}
	}
	s.renderAlarmForm(c, f, def)
}

func (s *Server) renderAlarmForm(c *gin.Context, f form, def *store.AlarmDef) {
	pvs, err := s.store.ListAlarmPVs(c.Request.Context())
	if err != nil {
		log.Printf("web: list PVs for alarms: %v", err)
	}
	c.HTML(http.StatusOK, "alarm_form.html", gin.H{
		"F": f, "Def": def, "PVs": pvs,
		"Types": store.AlarmTypes, "Priorities": store.AlarmPriorities, "Classes": store.AlarmClasses,
	})
}

func (s *Server) alarmSave(c *gin.Context) {
	f := readForm(c, "pv_id", "alarm_type", "setpoint", "trigger_state", "deadband", "on_delay_s", "off_delay_s",
		"priority", "alarm_class", "alarm_message", "operator_action", "consequences_of_inaction", "probable_cause",
		"max_response_time_s", "rationale", "enabled", "unit", "change_reason")
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	f.ID = int(id)

	in := store.AlarmInput{
		AlarmType: f.V("alarm_type"), Unit: f.V("unit"),
		Priority: f.V("priority"), AlarmClass: f.V("alarm_class"), Message: f.V("alarm_message"),
		OperatorAction: f.V("operator_action"), ConsequencesOfInaction: f.V("consequences_of_inaction"),
		ProbableCause: f.V("probable_cause"), Rationale: f.V("rationale"), Enabled: f.Checked("enabled"),
	}
	if id == 0 {
		pv, err := strconv.Atoi(f.V("pv_id"))
		if err != nil {
			f.Errors["pv_id"] = "Choose a PV"
		}
		in.PVID = pv
	}
	in.Setpoint = optFloatField(f, "setpoint")
	in.TriggerState = optIntField(f, "trigger_state")
	in.MaxResponseTimeS = optIntField(f, "max_response_time_s")
	in.Deadband = floatField(f, "deadband")
	in.OnDelayS = intField(f, "on_delay_s", 0, 3600)
	in.OffDelayS = intField(f, "off_delay_s", 0, 3600)

	if len(f.Errors) == 0 {
		_, reset, err := s.alarmCfg.Save(c.Request.Context(), actorOf(c), id, in, f.V("change_reason"))
		var ve *alarms.ValidationError
		switch {
		case err == nil:
			msg := "Alarm saved"
			if reset {
				msg = "Alarm saved. Its approval was withdrawn because rationalized values changed: it is not evaluated until an administrator approves it again."
			}
			s.reloadAlarms()
			payload, _ := json.Marshal(map[string]string{"configSaved": msg})
			c.Header("HX-Trigger", string(payload))
			c.Status(http.StatusNoContent)
			return
		case errors.As(err, &ve):
			for k, v := range ve.Fields {
				f.Errors[k] = v
			}
		case errors.Is(err, store.ErrAlarmExists):
			f.Errors["alarm_type"] = "This PV already has an alarm of this type — edit that one"
		case errors.Is(err, store.ErrAlarmNotFound):
			c.String(http.StatusNotFound, "Alarm not found (deleted?)")
			return
		default:
			var ce *store.ConstraintError
			if errors.As(err, &ce) {
				f.Errors["_"] = "Rejected by the database: " + ce.Message
			} else {
				log.Printf("web: save alarm: %v", err)
				f.Errors["_"] = "Could not save the alarm, try again"
			}
		}
	}
	var def *store.AlarmDef
	if id != 0 {
		if d, err := s.store.GetAlarmDef(c.Request.Context(), id); err == nil {
			def = &d
		}
	}
	s.renderAlarmForm(c, f, def)
}

// alarmApprove approves the rationalization of an alarm (administrators only).
func (s *Server) alarmApprove(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	err := s.alarmCfg.Approve(c.Request.Context(), actorOf(c), id)
	switch {
	case err == nil:
		s.reloadAlarms()
		c.Header("HX-Trigger", "configSaved")
		s.message(c, "ok", "Rationalization approved. The alarm is now evaluated if it is enabled.")
	case errors.Is(err, store.ErrNotRationalized):
		s.message(c, "error", "Cannot approve: "+strings.TrimPrefix(err.Error(), store.ErrNotRationalized.Error()+": "))
	case errors.Is(err, store.ErrAlarmNotFound):
		s.message(c, "error", "Alarm not found (deleted?).")
	default:
		log.Printf("web: approve alarm %d: %v", id, err)
		s.message(c, "error", "Could not approve, try again.")
	}
}

func optIntField(f form, name string) *int {
	if f.V(name) == "" {
		return nil
	}
	n, err := strconv.Atoi(f.V(name))
	if err != nil {
		f.Errors[name] = "Enter a whole number"
		return nil
	}
	return &n
}

// reloadAlarms makes the alarm engine read the definitions again now instead
// of at its next periodic refresh.
func (s *Server) reloadAlarms() {
	if s.alarmReload != nil {
		s.alarmReload()
	}
}
