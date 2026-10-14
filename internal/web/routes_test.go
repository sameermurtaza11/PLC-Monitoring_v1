package web

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
)

func init() { gin.SetMode(gin.TestMode) }

func newTestRouter() *gin.Engine { return NewRouter(nil, func() {}, Options{}) }

// Every route the application registers must be listed in auth.Rules, and every
// rule must belong to a real route. A forgotten route would otherwise be closed
// to everyone but administrators; this makes the mistake visible.
func TestEveryRouteHasAnAccessRule(t *testing.T) {
	r := newTestRouter() // also proves every template parses

	registered := map[string]bool{}
	for _, ri := range r.Routes() {
		key := ri.Method + " " + ri.Path
		registered[key] = true
		if _, ok := auth.Rules[key]; !ok {
			t.Errorf("route %q has no access rule — add it to auth.Rules", key)
		}
	}
	var stale []string
	for key := range auth.Rules {
		if !registered[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("access rule %q matches no route (renamed or removed?)", key)
	}
}

func TestProtectedPagesRedirectAnonymousVisitorsToLogin(t *testing.T) {
	r := newTestRouter()
	for _, path := range []string{"/", "/config", "/pv/1", "/pv/1/history", "/partials/pv-rows", "/config/users", "/config/plcs"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusFound || w.Header().Get("Location") == "" || w.Header().Get("Location")[:6] != "/login" {
			t.Errorf("GET %s: %d → %q, want a redirect to /login", path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestPublicRoutesStayOpen(t *testing.T) {
	r := newTestRouter()
	for _, path := range []string{"/healthz", "/login", "/static/app.css", "/static/htmx.min.js"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, w.Code)
		}
	}
}

func TestLoginPageHasTheThreeFieldsAndNoPrefilledCredentials(t *testing.T) {
	w := httptest.NewRecorder()
	newTestRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	body := w.Body.String()
	for _, want := range []string{`name="username"`, `name="password"`, `type="password"`, `>Login</button>`} {
		if !contains(body, want) {
			t.Errorf("login page lacks %s", want)
		}
	}
}

func TestStateChangingRequestsAreRefusedWithoutALogin(t *testing.T) {
	r := newTestRouter()
	for _, c := range []struct{ method, path string }{
		{"POST", "/config/plcs"}, {"POST", "/config/pvs/3"}, {"POST", "/config/pvs/3/delete"},
		{"POST", "/config/users"}, {"DELETE", "/config/users/2"}, {"POST", "/logout"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestHTMXRequestsGetALoginRedirectHeaderNotAPageFragment(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/partials/pv-rows", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:8080/pv/3?range=1h")
	w := httptest.NewRecorder()
	newTestRouter().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	if got := w.Header().Get("HX-Redirect"); got != "/login?next=%2Fpv%2F3%3Frange%3D1h" {
		t.Errorf("HX-Redirect = %q, want a login URL that returns to the current page", got)
	}
}

func TestSafeNextOnlyAllowsLocalPaths(t *testing.T) {
	cases := map[string]string{
		"": "/", "/config": "/config", "/pv/3?range=1h": "/pv/3?range=1h",
		"https://evil.example/x": "/", "//evil.example/x": "/", "/\\evil.example": "/",
		"javascript:alert(1)": "/", "config": "/", "/login": "/", "/login?next=/x": "/",
		"/ok\r\nSet-Cookie: a=b": "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	if got := loginURL("/config?x=1"); got != "/login?next=%2Fconfig%3Fx%3D1" {
		t.Errorf("loginURL = %q", got)
	}
	if got := loginURL("/"); got != "/login" {
		t.Errorf("loginURL(/) = %q", got)
	}
}

func TestMenuHidesConfigurationFromOperators(t *testing.T) {
	names := func(u *auth.User) []string {
		var out []string
		for _, it := range navFor(u) {
			out = append(out, it.Label)
		}
		return out
	}
	if got := names(nil); len(got) != 0 {
		t.Errorf("anonymous menu = %v", got)
	}
	if got := names(&auth.User{Role: auth.Operator}); len(got) != 3 || got[0] != "Dashboard" || got[1] != "Alarms" || got[2] != "Alarm History" {
		t.Errorf("operator menu = %v, want Dashboard, Alarms, Alarm History (no Configuration)", got)
	}
	for _, role := range []auth.Role{auth.Supervisor, auth.Admin} {
		if got := names(&auth.User{Role: role}); len(got) != 4 || got[3] != "Configuration" {
			t.Errorf("%s menu = %v", role, got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
