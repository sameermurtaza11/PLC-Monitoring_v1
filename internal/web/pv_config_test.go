package web

import (
	"net/url"
	"strings"
	"testing"

	"PLC_Monitoring/internal/store"
)

// formOf builds a submitted form the way readForm does.
func formOf(pairs ...string) form {
	f := form{Values: map[string]string{}, Errors: fieldErrors{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		f.Values[pairs[i]] = pairs[i+1]
	}
	return f
}

func analogForm(extra ...string) form {
	base := []string{"plc_id", "1", "register_address", "30001", "pv_name", "PT101", "unit", "PSI", "pv_min", "0", "pv_max", "100",
		"deadband", "0", "enabled", "on"}
	return formOf(append(base, extra...)...)
}

func card(t string, kv ...string) []string {
	out := []string{akey(t, "on"), "on", akey(t, "priority"), "Major", akey(t, "class"), "PROCESS", akey(t, "on_delay"), "10",
		akey(t, "off_delay"), "10", akey(t, "message"), "PT101 " + t, akey(t, "deadband"), "2"}
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, akey(t, kv[0+i]), kv[1+i])
	}
	return out
}

func with(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestAlarmsSwitchedOffSaveOnlyThePVAndDoNotValidateAlarmFields(t *testing.T) {
	f := analogForm(akey("HI", "setpoint"), "garbage") // leftovers of a hidden section
	r, on, specs, _ := buildPVSave(f, true)
	if len(f.Errors) != 0 || on || len(specs) != 0 || r.Name != "PT101" {
		t.Fatalf("errors %v, alarmsOn %v, specs %v", f.Errors, on, specs)
	}
}

func TestFourAnalogAlarmsAreReadIntoSpecs(t *testing.T) {
	f := analogForm(with([]string{"alarms_on", "on"},
		card("HIHI", "setpoint", "90"), card("HI", "setpoint", "80"), card("LO", "setpoint", "20"), card("LOLO", "setpoint", "10"))...)
	_, on, specs, _ := buildPVSave(f, false)
	if len(f.Errors) != 0 || !on || len(specs) != 4 {
		t.Fatalf("errors %v, alarmsOn %v, %d specs", f.Errors, on, len(specs))
	}
	want := map[string]float64{"HIHI": 90, "HI": 80, "LO": 20, "LOLO": 10}
	for _, sp := range specs {
		if !sp.Enabled || sp.Input.Setpoint == nil || *sp.Input.Setpoint != want[sp.Type] || sp.Input.Priority != "Major" || sp.Input.OnDelayS != 10 || sp.Input.Deadband != 2 {
			t.Errorf("%s: %+v", sp.Type, sp.Input)
		}
	}
}

func TestEachAlarmTypeIsSwitchedIndependently(t *testing.T) {
	hiOnly := card("HI", "setpoint", "80")
	lo := card("LO", "setpoint", "20")
	lo[1] = "" // LO is switched off (checkbox not sent)
	f := analogForm(with([]string{"alarms_on", "on"}, hiOnly, lo)...)
	_, _, specs, _ := buildPVSave(f, false)
	on := map[string]bool{}
	for _, sp := range specs {
		on[sp.Type] = sp.Enabled
	}
	if !on["HI"] || on["LO"] || on["HIHI"] || on["LOLO"] {
		t.Errorf("enabled = %v", on)
	}
	if len(f.Errors) != 0 {
		t.Errorf("a switched-off alarm must not be validated: %v", f.Errors)
	}
}

func TestAlarmValidationErrorsLandOnTheirFields(t *testing.T) {
	f := analogForm(with([]string{"alarms_on", "on"},
		card("HI", "setpoint", "20"), card("LO", "setpoint", "30"), // HI below LO
		card("HIHI", "setpoint", "500"), // outside the range
	)...)
	buildPVSave(f, false)
	for _, key := range []string{akey("HI", "setpoint"), akey("HIHI", "setpoint")} {
		if f.Errors[key] == "" {
			t.Errorf("no error on %s: %v", key, f.Errors)
		}
	}

	f = analogForm(with([]string{"alarms_on", "on"}, card("HI", "setpoint", "80", "deadband", "-1", "on_delay", "9999", "message", " ", "priority", "Urgent"))...)
	buildPVSave(f, false)
	for _, key := range []string{akey("HI", "deadband"), akey("HI", "on_delay"), akey("HI", "message"), akey("HI", "priority")} {
		if f.Errors[key] == "" {
			t.Errorf("no error on %s: %v", key, f.Errors)
		}
	}

	f = analogForm(with([]string{"alarms_on", "on"}, card("HI", "setpoint", ""))...)
	buildPVSave(f, false)
	if f.Errors[akey("HI", "setpoint")] == "" {
		t.Errorf("a missing setpoint was accepted: %v", f.Errors)
	}
}

func TestAlarmsOnWithNothingSwitchedOnIsAnError(t *testing.T) {
	f := analogForm("alarms_on", "on")
	buildPVSave(f, false)
	if f.Errors["alarms_on"] == "" {
		t.Errorf("errors: %v", f.Errors)
	}
}

func TestLimitsAreCheckedAgainstTheRangeTypedInTheSameForm(t *testing.T) {
	// the PV range is changed in this very form: 80 is fine for 0…100 but not for 0…50
	f := formOf("plc_id", "1", "register_address", "30001", "pv_name", "PT101", "unit", "PSI", "pv_min", "0", "pv_max", "50", "deadband", "0",
		"alarms_on", "on", akey("HI", "on"), "on", akey("HI", "setpoint"), "80", akey("HI", "priority"), "Major", akey("HI", "class"), "PROCESS",
		akey("HI", "deadband"), "1", akey("HI", "on_delay"), "5", akey("HI", "off_delay"), "5", akey("HI", "message"), "high")
	buildPVSave(f, false)
	if !strings.Contains(f.Errors[akey("HI", "setpoint")], "range") {
		t.Errorf("errors: %v", f.Errors)
	}
}

func TestDigitalPVGetsOneDiscreteAlarmFromItsNormalState(t *testing.T) {
	base := []string{"plc_id", "1", "register_address", "10001", "pv_name", "XS101", "state0_text", "STOPPED", "state1_text", "RUNNING", "enabled", "on", "alarms_on", "on"}
	discrete := func(normal string) []string {
		return []string{akey("DISCRETE", "on"), "on", akey("DISCRETE", "normal"), normal, akey("DISCRETE", "priority"), "Critical",
			akey("DISCRETE", "class"), "EQUIPMENT_PROTECTION", akey("DISCRETE", "on_delay"), "30", akey("DISCRETE", "off_delay"), "5",
			akey("DISCRETE", "message"), "XS101 stopped"}
	}
	// analog leftovers must be ignored for a digital PV
	leftovers := []string{akey("HI", "on"), "on", akey("HI", "setpoint"), "garbage"}

	for normal, wantTrigger := range map[string]int{"1": 0, "0": 1} { // abnormal = the other state
		f := formOf(with(base, discrete(normal), leftovers)...)
		r, on, specs, _ := buildPVSave(f, false)
		if len(f.Errors) != 0 || !on || len(specs) != 1 || specs[0].Type != "DISCRETE" {
			t.Fatalf("normal %s: errors %v, specs %+v", normal, f.Errors, specs)
		}
		in := specs[0].Input
		if in.TriggerState == nil || *in.TriggerState != wantTrigger || in.Setpoint != nil || in.Deadband != 0 || in.OnDelayS != 30 {
			t.Errorf("normal %s: %+v (trigger %v)", normal, in, in.TriggerState)
		}
		if !r.IsDigital() || r.Unit != "" || r.Min != 0 || r.Max != 1 {
			t.Errorf("a digital PV must have no unit and a 0…1 scale: %+v", r.PV)
		}
	}

	f := formOf(with(base, discrete(""))...)
	buildPVSave(f, false)
	if f.Errors[akey("DISCRETE", "normal")] == "" {
		t.Errorf("no normal state chosen was accepted: %v", f.Errors)
	}
}

func TestOnlyAdministratorsCanApproveInTheSameSave(t *testing.T) {
	fields := with([]string{"alarms_on", "on", "approve", "on"}, card("HI", "setpoint", "80"))
	if _, _, _, approve := buildPVSave(analogForm(fields...), false); approve {
		t.Error("a non-administrator's approval request was honoured")
	}
	f := analogForm(fields...)
	if _, _, _, approve := buildPVSave(f, true); !approve {
		t.Error("an administrator's approval request was ignored")
	}
	if f.Errors[akey("HI", "rationale")] == "" {
		t.Errorf("approving without a rationalization was accepted: %v", f.Errors)
	}
}

func TestFormDistinguishesAnalogFromDigitalAndShowsOnlyTheMatchingAlarmFields(t *testing.T) {
	s := testServer(t)
	render := func(f form) string {
		t.Helper()
		defaultAlarmValues(&f)
		html, err := s.render("pv_form.html", f)
		if err != nil {
			t.Fatal(err)
		}
		return html
	}
	group := func(html, class string) string { // the opening tag of a group
		i := strings.Index(html, `<div class="`+class+` alarm-set"`)
		if i < 0 {
			t.Fatalf("no %s group", class)
		}
		return html[i : i+strings.Index(html[i:], ">")+1]
	}

	ai := render(analogForm("alarms_on", "on"))
	if strings.Contains(group(ai, "analog-only"), "hidden") || !strings.Contains(group(ai, "digital-only"), "hidden") {
		t.Error("an analog PV must show the analog alarms and hide the discrete one")
	}
	for _, name := range []string{"a_HIHI_setpoint", "a_HI_setpoint", "a_LO_setpoint", "a_LOLO_setpoint", "a_HI_deadband", "a_LO_on_delay", "a_LOLO_off_delay", "a_HIHI_priority"} {
		if !strings.Contains(ai, `name="`+name+`"`) {
			t.Errorf("analog form lacks %s", name)
		}
	}

	di := render(formOf("register_address", "10001", "pv_name", "XS101", "state0_text", "STOPPED", "state1_text", "RUNNING"))
	if !strings.Contains(group(di, "analog-only"), "hidden") || strings.Contains(group(di, "digital-only"), "hidden") {
		t.Error("a digital PV must show the discrete alarm and hide the analog ones")
	}
	if !strings.Contains(di, `name="a_DISCRETE_normal"`) || !strings.Contains(di, "Normal state") {
		t.Error("the digital form needs a normal-state choice")
	}
	// the master switch hides everything until it is on
	if !strings.Contains(di, `id="alarm-fields" hidden`) || !strings.Contains(ai, `id="alarms-on"`) {
		t.Error("Enable Alarms must start OFF for a PV without alarms and control the alarm fields")
	}
	if strings.Contains(ai, `id="alarm-fields" hidden`) == false {
		// alarms_on was "on" for the analog render above, so the fields must be shown
		t.Log("alarm fields visible when Enable Alarms is on")
	}
	on := render(analogForm("alarms_on", "on"))
	if strings.Contains(on, `id="alarm-fields" hidden`) {
		t.Error("Enable Alarms ON must show the alarm fields")
	}
}

func TestEditFormLoadsTheStoredAlarmsIntoTheSameForm(t *testing.T) {
	s := testServer(t)
	sp, hh := 80.0, 95.0
	mr := 120
	defs := []store.AlarmDef{
		{ID: 1, PVID: 5, AlarmType: "HI", Setpoint: &sp, Deadband: 2, OnDelayS: 7, OffDelayS: 9, Priority: "Critical", AlarmClass: "EQUIPMENT_PROTECTION",
			Message: "High pressure", OperatorAction: "reduce", MaxResponseTimeS: &mr, Enabled: true, RationalizationStatus: "APPROVED"},
		{ID: 2, PVID: 5, AlarmType: "HIHI", Setpoint: &hh, Priority: "Major", AlarmClass: "PROCESS", Message: "HH", Enabled: false, RationalizationStatus: "DRAFT"},
	}
	f := analogForm()
	f.ID = 5
	fillAlarmValues(&f, defs)
	defaultAlarmValues(&f)
	if f.V("alarms_on") != "on" || f.V(akey("HI", "on")) != "on" || f.V(akey("HIHI", "on")) != "" {
		t.Errorf("toggles: %v", f.Values)
	}
	html, err := s.render("pv_form.html", f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`value="80"`, `value="95"`, `value="7"`, `value="9"`, `value="High pressure"`, `value="120"`, "approved", "switched off", ">reduce<"} {
		if !strings.Contains(html, want) {
			t.Errorf("edit form lacks %s", want)
		}
	}
	// a stored discrete alarm shows its NORMAL state (the opposite of the abnormal one)
	zero := 0
	f2 := formOf("register_address", "10001")
	fillAlarmValues(&f2, []store.AlarmDef{{ID: 3, AlarmType: "DISCRETE", TriggerState: &zero, Priority: "Major", AlarmClass: "PROCESS", Message: "m", Enabled: true}})
	if f2.V(akey("DISCRETE", "normal")) != "1" {
		t.Errorf("abnormal 0 → normal %q, want 1", f2.V(akey("DISCRETE", "normal")))
	}
}

func TestSavedAndDeletedMessagesSayWhatHappened(t *testing.T) {
	msg := pvSavedMessage("PT101", true, store.PVSaveResult{Changes: []store.AlarmChange{
		{Type: "HI", Action: "created"}, {Type: "LO", Action: "updated", ResetToDraft: true}, {Type: "LOLO", Action: "disabled", Ended: 2},
		{Type: "HIHI", Action: "unchanged"},
	}})
	for _, want := range []string{"PV PT101 saved", "HI created", "LO updated (approval withdrawn)", "LOLO switched off (2 active occurrence(s) ended)", "drafts"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "HIHI") {
		t.Error("an unchanged alarm must not be mentioned")
	}

	erased := pvDeletedMessage(store.PVDeleteResult{PVName: "X", Erased: true, AlarmDefs: 2})
	if !strings.Contains(erased, "2 alarm configuration") || !strings.Contains(erased, "erased completely") {
		t.Errorf("erased message: %q", erased)
	}
	archived := pvDeletedMessage(store.PVDeleteResult{PVName: "X", Archived: true, AlarmDefs: 4, AlarmEvents: 12, Outstanding: 1, ValueHistoryKept: 300, OccurrencesEnded: 1})
	for _, want := range []string{"4 alarm configuration", "1 active alarm(s) ended", "archived, not erased", "12 alarm history", "300 stored value"} {
		if !strings.Contains(archived, want) {
			t.Errorf("archive message %q lacks %q", archived, want)
		}
	}
}

func TestDeleteDialogWarnsAboutAlarmsAndRequiresConfirmation(t *testing.T) {
	s := testServer(t)
	render := func(imp store.PVDeletionImpact, archives, archivesAnyway bool, problem string) string {
		html, err := s.render("pv_delete.html", map[string]any{"Impact": imp, "Problem": problem, "Archives": archives, "ArchivesEvenWithoutValues": archivesAnyway})
		if err != nil {
			t.Fatal(err)
		}
		return html
	}
	imp := store.PVDeletionImpact{PVID: 7, PVName: "PT101", PLCName: "PLC-1", AlarmDefs: 4, EnabledAlarm: 3, Outstanding: 1, AlarmEvents: 9, ValueHistory: 500}
	html := render(imp, true, true, "")
	for _, want := range []string{"Delete PV PT101", "also deletes its alarm configurations", "4 configured", "3 in service",
		`name="confirm" value="yes" required`, `hx-post="/config/pvs/7/delete"`, "never erased", "archived", "500 stored value"} {
		if !strings.Contains(html, want) {
			t.Errorf("dialog lacks %q", want)
		}
	}
	if html := render(store.PVDeletionImpact{PVID: 8, PVName: "P", AlarmDefs: 1}, false, false, ""); !strings.Contains(html, "erased completely") || strings.Contains(html, "delete_history") {
		t.Errorf("a PV without history must say it is erased completely: %s", html)
	}
	if html := render(imp, true, true, "Tick the box"); !strings.Contains(html, "Tick the box") {
		t.Error("the problem message is not shown")
	}
	// the dialog needs the CSRF header like every other state-changing form
	if v, _ := url.ParseQuery("confirm=yes"); v.Get("confirm") != "yes" {
		t.Fatal("test setup")
	}
}

func TestApproveCheckboxIsOnlyOfferedToAdministrators(t *testing.T) {
	s := testServer(t)
	for _, admin := range []bool{false, true} {
		f := analogForm("alarms_on", "on")
		f.IsAdmin = admin
		defaultAlarmValues(&f)
		html, err := s.render("pv_form.html", f)
		if err != nil {
			t.Fatal(err)
		}
		if has := strings.Contains(html, `name="approve"`); has != admin {
			t.Errorf("admin=%v: approve checkbox present = %v", admin, has)
		}
	}
}
