package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/alarms"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/plc"
	"PLC_Monitoring/internal/store"
)

// PV configuration with its alarms in ONE form. The data stays in two tables
// (pv_metadata, alarm_metadata); store.SavePVWithAlarms writes both in one
// transaction. Alarm fields are named a_<TYPE>_<field> (TYPE: HIHI, HI, LO,
// LOLO or DISCRETE) and the toggle is alarms_on.

var alarmFields = []string{"on", "setpoint", "priority", "class", "deadband", "on_delay", "off_delay", "message",
	"max_resp", "action", "conseq", "cause", "rationale", "normal"}

func akey(alarmType, field string) string { return "a_" + alarmType + "_" + field }

// allAlarmTypes: every type, for reading the form (only the ones that fit the PV type are used).
var allAlarmTypes = []string{store.AlarmHIHI, store.AlarmHI, store.AlarmLO, store.AlarmLOLO, store.AlarmDiscrete}

// Starting values for an alarm type that has no definition yet.
var alarmDefaults = map[string]struct {
	Priority, Class     string
	OnDelay, OffDelay   string
	SetpointPercentOfPV float64 // suggested limit, as a fraction of the PV range (filled in by the page script)
}{
	store.AlarmHIHI:     {store.PriorityCritical, "EQUIPMENT_PROTECTION", "2", "5", 0.90},
	store.AlarmHI:       {store.PriorityMajor, "PROCESS", "10", "10", 0.80},
	store.AlarmLO:       {store.PriorityMinor, "PROCESS", "10", "10", 0.20},
	store.AlarmLOLO:     {store.PriorityMajor, "PROCESS", "2", "5", 0.10},
	store.AlarmDiscrete: {store.PriorityMajor, "PROCESS", "10", "5", 0},
}

func typeLabel(t string) string {
	switch t {
	case store.AlarmHIHI:
		return "High-high (HIHI)"
	case store.AlarmHI:
		return "High (HI)"
	case store.AlarmLO:
		return "Low (LO)"
	case store.AlarmLOLO:
		return "Low-low (LOLO)"
	}
	return "Discrete"
}

// alarmCard is what one alarm block of the form renders.
type alarmCard struct {
	F    form
	T    string
	Def  *store.AlarmDef
	Pct  float64
	Prio []string
	Cls  []string
}

func (c alarmCard) V(field string) string         { return c.F.Values[akey(c.T, field)] }
func (c alarmCard) Err(field string) string       { return c.F.Errors[akey(c.T, field)] }
func (c alarmCard) On() bool                      { return c.F.Values[akey(c.T, "on")] == "on" }
func (c alarmCard) Name(field string) string      { return akey(c.T, field) }
func (c alarmCard) Label() string                 { return typeLabel(c.T) }
func (c alarmCard) Digital() bool                 { return c.T == store.AlarmDiscrete }
func (c alarmCard) Selected(field, v string) bool { return c.V(field) == v }

type cardStatus struct{ Text, Class string }

// Status is the state of the stored definition, for the chip in the card header.
func (c alarmCard) Status() cardStatus {
	switch {
	case c.Def == nil:
		return cardStatus{"new", "none"}
	case !c.Def.Enabled:
		return cardStatus{"switched off", "none"}
	case c.Def.RationalizationStatus == store.RationApproved:
		return cardStatus{"approved", "ok"}
	}
	return cardStatus{"draft – not evaluated until approved", "warn"}
}

func init() {
	funcs["alarmCard"] = func(f form, t string) alarmCard {
		c := alarmCard{F: f, T: t, Prio: store.AlarmPriorities, Cls: store.AlarmClasses, Pct: alarmDefaults[t].SetpointPercentOfPV}
		if d, ok := f.Defs[t]; ok {
			c.Def = &d
		}
		return c
	}
	funcs["analogTypes"] = func() []string { return alarms.AnalogTypes }
	funcs["typeLabel"] = typeLabel
}

// defaultAlarmValues fills the alarm fields that no stored definition provides.
func defaultAlarmValues(f *form) {
	for _, t := range allAlarmTypes {
		d := alarmDefaults[t]
		for field, v := range map[string]string{"priority": d.Priority, "class": d.Class, "on_delay": d.OnDelay, "off_delay": d.OffDelay} {
			if _, ok := f.Values[akey(t, field)]; !ok {
				f.Values[akey(t, field)] = v
			}
		}
	}
}

// fillAlarmValues loads the stored definitions of the PV into the form. The
// toggle is on when at least one alarm is in service.
func fillAlarmValues(f *form, defs []store.AlarmDef) {
	f.Defs = map[string]store.AlarmDef{}
	anyOn := false
	for _, d := range defs {
		t := d.AlarmType
		f.Defs[t] = d
		set := func(field, v string) { f.Values[akey(t, field)] = v }
		set("on", onOff(d.Enabled))
		set("setpoint", optFtoa(d.Setpoint))
		set("priority", d.Priority)
		set("class", d.AlarmClass)
		set("deadband", ftoa(d.Deadband))
		set("on_delay", strconv.Itoa(d.OnDelayS))
		set("off_delay", strconv.Itoa(d.OffDelayS))
		set("message", d.Message)
		set("max_resp", optItoa(d.MaxResponseTimeS))
		set("action", d.OperatorAction)
		set("conseq", d.ConsequencesOfInaction)
		set("cause", d.ProbableCause)
		set("rationale", d.Rationale)
		if d.TriggerState != nil {
			set("normal", strconv.Itoa(1-*d.TriggerState)) // the other state is the abnormal one
		}
		anyOn = anyOn || d.Enabled
	}
	f.Values["alarms_on"] = onOff(anyOn)
}

func (s *Server) pvForm(c *gin.Context) {
	f := form{Values: map[string]string{
		"pv_min": "0", "pv_max": "100", "deadband": "0", "enabled": "on",
		"state0_text": "OFF", "state1_text": "ON",
		"plc_id": c.Query("plc"),
	}, Errors: fieldErrors{}}
	if idStr := c.Param("id"); idStr != "" {
		id, _ := strconv.Atoi(idStr)
		r, err := s.store.GetPV(c.Request.Context(), id)
		if err != nil {
			c.String(http.StatusNotFound, "PV not found")
			return
		}
		f.ID = r.ID
		f.Values = map[string]string{
			"plc_id": strconv.Itoa(r.PLCID), "register_address": strconv.Itoa(r.RegisterAddress),
			"pv_name": r.Name, "system_name": r.SystemName, "equipment": r.Equipment,
			"description": r.Description, "unit": r.Unit,
			"pv_min": ftoa(r.Min), "pv_max": ftoa(r.Max),
			"deadband": ftoa(r.Deadband), "enabled": onOff(r.Enabled),
			"state0_text": r.State0Text, "state1_text": r.State1Text,
		}
		defs, err := s.store.ListAlarmDefsOfPV(c.Request.Context(), id)
		if err != nil {
			log.Printf("web: pv %d alarms: %v", id, err)
		}
		fillAlarmValues(&f, defs)
	}
	s.renderPVForm(c, f)
}

func (s *Server) renderPVForm(c *gin.Context, f form) {
	plcs, err := s.store.ListPLCs(c.Request.Context())
	if err != nil {
		log.Printf("web: list plcs: %v", err)
	}
	f.PLCs = plcs
	if f.Defs == nil && f.ID != 0 { // an invalid submit: show the stored status of each alarm
		if defs, err := s.store.ListAlarmDefsOfPV(c.Request.Context(), f.ID); err == nil {
			f.Defs = map[string]store.AlarmDef{}
			for _, d := range defs {
				f.Defs[d.AlarmType] = d
			}
		}
	}
	defaultAlarmValues(&f)
	f.IsAdmin = auth.FromContext(c).IsAdmin()
	c.HTML(http.StatusOK, "pv_form.html", f)
}

// alarmFormKeys are the form fields of every alarm type.
func alarmFormKeys() []string {
	keys := []string{"alarms_on", "change_reason", "approve"}
	for _, t := range allAlarmTypes {
		for _, field := range alarmFields {
			keys = append(keys, akey(t, field))
		}
	}
	return keys
}

// parseAlarmSpecs reads the alarms that fit the PV type. Numbers are parsed
// only for alarms that are switched on; anything wrong is put on its field.
func parseAlarmSpecs(f form, digital bool) []store.AlarmSpec {
	var specs []store.AlarmSpec
	for _, t := range alarms.TypesFor(digital) {
		sp := store.AlarmSpec{Type: t, Enabled: f.Checked(akey(t, "on"))}
		if sp.Enabled {
			in := &sp.Input
			in.AlarmType = t
			in.Priority, in.AlarmClass = f.V(akey(t, "priority")), f.V(akey(t, "class"))
			in.Message = f.V(akey(t, "message"))
			in.OperatorAction, in.ConsequencesOfInaction = f.V(akey(t, "action")), f.V(akey(t, "conseq"))
			in.ProbableCause, in.Rationale = f.V(akey(t, "cause")), f.V(akey(t, "rationale"))
			in.OnDelayS = intField(f, akey(t, "on_delay"), 0, 3600)
			in.OffDelayS = intField(f, akey(t, "off_delay"), 0, 3600)
			in.MaxResponseTimeS = optIntField(f, akey(t, "max_resp"))
			if t == store.AlarmDiscrete {
				switch f.V(akey(t, "normal")) {
				case "0":
					one := 1
					in.TriggerState = &one // normal 0 → alarm when 1
				case "1":
					zero := 0
					in.TriggerState = &zero
				default:
					f.Errors[akey(t, "normal")] = "Choose the normal state"
				}
			} else {
				in.Setpoint = optFloatField(f, akey(t, "setpoint"))
				if in.Setpoint == nil && f.Errors[akey(t, "setpoint")] == "" {
					f.Errors[akey(t, "setpoint")] = "Required"
				}
				in.Deadband = floatField(f, akey(t, "deadband"))
			}
		}
		specs = append(specs, sp)
	}
	return specs
}

// alarmFieldKey maps a field name of alarms.Validate to the form field.
var alarmFieldKey = map[string]string{
	"setpoint": "setpoint", "setpoint_unit": "setpoint", "deadband": "deadband", "priority": "priority", "alarm_class": "class",
	"alarm_message": "message", "on_delay_s": "on_delay", "off_delay_s": "off_delay", "max_response_time_s": "max_resp",
	"operator_action": "action", "consequences_of_inaction": "conseq", "probable_cause": "cause", "rationale": "rationale",
	"trigger_state": "normal", "alarm_type": "setpoint",
}

func (s *Server) pvSave(c *gin.Context) {
	names := append([]string{"plc_id", "register_address", "pv_name", "system_name", "equipment",
		"description", "unit", "pv_min", "pv_max", "deadband", "enabled",
		"state0_text", "state1_text"}, alarmFormKeys()...)
	f := readForm(c, names...)
	f.ID, _ = strconv.Atoi(c.Param("id"))

	r, alarmsOn, specs, approve := buildPVSave(f, auth.FromContext(c).IsAdmin())

	if len(f.Errors) == 0 {
		actor := actorOf(c)
		res, err := s.store.SavePVWithAlarms(c.Request.Context(), r, alarmsOn, specs, approve, f.V("change_reason"), actor)
		var ce *store.ConstraintError
		switch {
		case err == nil:
			s.reloadAlarms()
			s.saved(c, pvSavedMessage(r.Name, alarmsOn, res))
			return
		case errors.Is(err, store.ErrNotRationalized):
			f.Errors["_"] = "Cannot approve: " + err.Error()
		case errors.Is(err, store.ErrAlarmExists):
			f.Errors["_"] = "An alarm of this type already exists for this PV — reload the form and try again"
		case errors.Is(err, pgx.ErrNoRows):
			c.String(http.StatusNotFound, "PV not found (deleted?)")
			return
		case errors.As(err, &ce) && mapConstraint(err, f.Errors):
		default:
			log.Printf("web: save pv with alarms: %v", err)
			f.Errors["_"] = "Could not save: " + err.Error() + " — nothing was changed"
		}
	}
	s.renderPVForm(c, f)
}

// buildPVSave turns the submitted form into the PV and its alarms, validating
// both. Everything wrong is put on its field in f.Errors; nothing is saved here.
func buildPVSave(f form, isAdmin bool) (r store.PVRecord, alarmsOn bool, specs []store.AlarmSpec, approve bool) {
	r = store.PVRecord{Enabled: f.Checked("enabled")}
	r.ID = f.ID
	r.PLCID = intField(f, "plc_id", 1, 1<<31-1)
	if _, ok := f.Errors["plc_id"]; ok {
		f.Errors["plc_id"] = "Select a PLC"
	}
	r.RegisterAddress = intField(f, "register_address", 0, 1<<31-1)
	if _, ok := f.Errors["register_address"]; !ok {
		if _, _, err := plc.ParseAddress(r.RegisterAddress); err != nil {
			f.Errors["register_address"] = "Use 10001–19999 (digital input), 30001–39999 (input register) or 40001–49999 (holding register)"
		}
	}
	r.Name = requiredText(f, "pv_name", 64)
	r.SystemName, r.Equipment, r.Description, r.Unit =
		f.V("system_name"), f.V("equipment"), f.V("description"), f.V("unit")
	if r.IsDigital() {
		// Digital input: value is 0/1. Scaling is fixed to 0…1 so trends and
		// "% of range" comparisons work; analog-only fields are cleared.
		r.Min, r.Max, r.Deadband, r.Unit = 0, 1, 0, ""
		r.State0Text = requiredText(f, "state0_text", 32)
		r.State1Text = requiredText(f, "state1_text", 32)
	} else {
		r.State0Text, r.State1Text = "OFF", "ON"
		r.Min = floatField(f, "pv_min")
		r.Max = floatField(f, "pv_max")
		if f.Errors["pv_min"] == "" && f.Errors["pv_max"] == "" && r.Max <= r.Min {
			f.Errors["pv_max"] = "PV max must be greater than PV min"
		}
		r.Deadband = floatField(f, "deadband")
		if f.Errors["deadband"] == "" && r.Deadband < 0 {
			f.Errors["deadband"] = "Deadband cannot be negative"
		}
	}

	// --- alarms ---
	alarmsOn = f.Checked("alarms_on")
	approve = alarmsOn && f.Checked("approve") && isAdmin
	if alarmsOn {
		specs = parseAlarmSpecs(f, r.IsDigital())
		enabled := 0
		for _, sp := range specs {
			if sp.Enabled {
				enabled++
			}
		}
		if enabled == 0 {
			f.Errors["alarms_on"] = "Switch on at least one alarm, or switch Enable Alarms off"
		}
		// Check the limits against the PV as it will be saved (needs a valid range).
		if f.Errors["pv_min"] == "" && f.Errors["pv_max"] == "" {
			pv := store.AlarmPV{ID: f.ID, Name: r.Name, Unit: r.Unit, Min: r.Min, Max: r.Max, Digital: r.IsDigital()}
			for key, msg := range alarms.ValidatePVAlarms(pv, specs, approve) {
				typ, field, _ := strings.Cut(key, ".")
				form := akey(typ, alarmFieldKey[field])
				if _, taken := f.Errors[form]; !taken {
					f.Errors[form] = msg
				}
			}
		}
	}

	return r, alarmsOn, specs, approve
}

// Digital reports whether the register typed in the form is a digital input.
func (f form) Digital() bool {
	n, _ := strconv.Atoi(f.V("register_address"))
	return n >= 10001 && n <= 19999
}

// pvSavedMessage tells the user what was saved, alarm by alarm.
func pvSavedMessage(name string, alarmsOn bool, res store.PVSaveResult) string {
	var parts []string
	for _, ch := range res.Changes {
		switch ch.Action {
		case "unchanged":
			continue
		case "created":
			parts = append(parts, ch.Type+" created")
		case "updated":
			parts = append(parts, ch.Type+" updated")
		case "enabled":
			parts = append(parts, ch.Type+" switched on")
		case "disabled":
			p := ch.Type + " switched off"
			if ch.Ended > 0 {
				p += fmt.Sprintf(" (%d active occurrence(s) ended)", ch.Ended)
			}
			parts = append(parts, p)
		}
		last := len(parts) - 1
		switch {
		case ch.Approved:
			parts[last] += ", approved"
		case ch.ResetToDraft:
			parts[last] += " (approval withdrawn)"
		}
	}
	msg := "PV " + name + " saved"
	if len(parts) > 0 {
		msg += ". Alarms: " + strings.Join(parts, "; ")
	} else if !alarmsOn {
		msg += ". Alarms off"
	}
	for _, ch := range res.Changes {
		if ch.Action == "created" && !ch.Approved {
			msg += ". New alarms are drafts: an administrator must approve them in Alarm Configuration before they are evaluated"
			break
		}
	}
	return msg
}

// ---------------------------------------------------------------------
// Deleting a PV
// ---------------------------------------------------------------------

func (s *Server) pvDeleteForm(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	s.renderPVDelete(c, id, "")
}

func (s *Server) renderPVDelete(c *gin.Context, id int, problem string) {
	impact, err := s.store.PVDeletionImpact(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "PV not found (already deleted?)")
		return
	}
	if err != nil {
		log.Printf("web: delete impact of pv %d: %v", id, err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "pv_delete.html", gin.H{
		"Impact": impact, "Problem": problem,
		"Archives":                  impact.RetainsRecords(false), // as things stand
		"ArchivesEvenWithoutValues": impact.RetainsRecords(true),  // after deleting the stored values too
	})
}

// pvDelete deletes a PV with its alarm configurations after an explicit
// confirmation (see store.DeletePVWithAlarms for what is kept).
func (s *Server) pvDelete(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if c.PostForm("confirm") != "yes" {
		s.renderPVDelete(c, id, "Tick the box to confirm that you want to delete this PV.")
		return
	}
	res, err := s.store.DeletePVWithAlarms(c.Request.Context(), id, c.PostForm("delete_history") == "on", actorOf(c))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		s.message(c, "error", "PV not found (already deleted?).")
	case err != nil:
		log.Printf("web: delete pv %d: %v", id, err)
		s.renderPVDelete(c, id, "Could not delete: "+err.Error()+" — nothing was changed.")
	default:
		s.reloadAlarms()
		s.onConfigChange()
		payload, _ := json.Marshal(map[string]string{"configSaved": pvDeletedMessage(res)})
		c.Header("HX-Trigger", string(payload))
		c.Status(http.StatusNoContent)
	}
}

func pvDeletedMessage(r store.PVDeleteResult) string {
	msg := "PV " + r.PVName + " deleted"
	if r.AlarmDefs > 0 {
		msg += fmt.Sprintf(" together with %d alarm configuration(s)", r.AlarmDefs)
	}
	if r.OccurrencesEnded > 0 {
		msg += fmt.Sprintf("; %d active alarm(s) ended", r.OccurrencesEnded)
	}
	if r.Erased {
		return msg + ". Nothing else referred to it, so it was erased completely."
	}
	var kept []string
	if r.AlarmEvents > 0 {
		kept = append(kept, fmt.Sprintf("%d alarm history event(s)", r.AlarmEvents))
	}
	if r.Outstanding > 0 {
		kept = append(kept, fmt.Sprintf("%d alarm(s) still waiting for acknowledgement", r.Outstanding))
	}
	if r.ValueHistoryKept > 0 {
		kept = append(kept, fmt.Sprintf("%d stored value(s)", r.ValueHistoryKept))
	}
	if r.ValueHistoryDeleted > 0 {
		msg += fmt.Sprintf("; %d stored value(s) deleted", r.ValueHistoryDeleted)
	}
	return msg + ". It is archived, not erased, because these records must be kept: " + strings.Join(kept, ", ") + "."
}
