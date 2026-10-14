package auth

import "testing"

// Acknowledging is open to every logged-in user; shelving and unshelving are
// for supervisors and administrators only (the alarm philosophy decision).
func TestAlarmActionPermissions(t *testing.T) {
	want := map[string]Level{
		"GET /alarms": Authenticated, "GET /alarm/:id": Authenticated, "GET /alarms/history": Authenticated,
		"POST /alarm/:id/ack": Authenticated, "POST /alarm/:id/shelve": Config, "POST /alarm/:id/unshelve": Config,
	}
	for route, level := range want {
		if got, ok := Rules[route]; !ok || got != level {
			t.Errorf("%s: level %v (listed %v), want %v", route, got, ok, level)
		}
	}
	if Operator.Allows(Config) {
		t.Error("an operator must not be allowed to shelve")
	}
	if !Supervisor.Allows(Config) || !Admin.Allows(Config) {
		t.Error("supervisors and administrators must be allowed to shelve")
	}
}
