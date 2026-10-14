package auth

// Rules is the one place that says who may open what. The key is
// "METHOD /gin/route/pattern". A route that is not listed is closed to
// everyone except administrators (DefaultLevel), and a test fails if the
// application has a route that is missing here.
var Rules = map[string]Level{
	// public
	"GET /login":             Public,
	"POST /login":            Public,
	"GET /healthz":           Public,
	"GET /favicon.ico":       Public,
	"GET /static/*filepath":  Public,
	"HEAD /static/*filepath": Public,

	// any logged-in user: dashboard, PV detail, history
	"POST /logout":          Authenticated,
	"GET /":                 Authenticated,
	"GET /partials/pv-rows": Authenticated,
	"GET /pv/:id":           Authenticated,
	"GET /events":           Authenticated, // pushed live updates (SSE)
	"GET /pv/:id/history":   Authenticated,

	// supervisors and administrators: PLC / PV configuration
	"GET /config":               Config,
	"GET /config/plcs":          Config,
	"GET /config/plcs/new":      Config,
	"GET /config/plcs/:id/edit": Config,
	"POST /config/plcs":         Config,
	"POST /config/plcs/:id":     Config,
	"DELETE /config/plcs/:id":   Config,
	"GET /config/pvs":           Config,
	"GET /config/pvs/new":       Config,
	"GET /config/pvs/:id/edit":  Config,
	"POST /config/pvs":          Config,
	"POST /config/pvs/:id":      Config,
	"DELETE /config/pvs/:id":    Config,

	// alarm configuration: supervisors and administrators edit, administrators approve
	"GET /config/alarms":              Config,
	"GET /config/alarms/new":          Config,
	"GET /config/alarms/:id/edit":     Config,
	"POST /config/alarms":             Config,
	"POST /config/alarms/:id":         Config,
	"POST /config/alarms/:id/approve": AdminOnly,

	// administrators only: health of the sample pipeline
	"GET /status/pipeline": AdminOnly,

	// administrators only: user management
	"GET /config/users":          AdminOnly,
	"GET /config/users/new":      AdminOnly,
	"GET /config/users/:id/edit": AdminOnly,
	"POST /config/users":         AdminOnly,
	"POST /config/users/:id":     AdminOnly,
	"DELETE /config/users/:id":   AdminOnly,
}

// DefaultLevel applies to a route missing from Rules.
const DefaultLevel = AdminOnly

// LevelFor returns the level a route needs and whether it was listed.
func LevelFor(method, route string) (Level, bool) {
	if l, ok := Rules[method+" "+route]; ok {
		return l, true
	}
	return DefaultLevel, false
}
