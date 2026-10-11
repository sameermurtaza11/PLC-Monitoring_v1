package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// routerFor registers a dummy handler for EVERY rule in Rules and puts the
// guard in front, so the real rules table is exercised with each role.
func routerFor(user *User) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if user != nil {
			c.Set(userKey, user)
		}
	})
	r.Use(Guard(func(c *gin.Context, d Denial) { c.String(d.Status, "denied") }))
	for key := range Rules {
		method, path, _ := strings.Cut(key, " ")
		r.Handle(method, path, func(c *gin.Context) { c.String(200, "ok") })
	}
	return r
}

func request(r *gin.Engine, method, route string) int {
	path := strings.NewReplacer(":id", "7", "*filepath", "app.css").Replace(route)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Code
}

func TestEveryRouteAgainstEveryRole(t *testing.T) {
	roles := map[string]*User{
		"anonymous":  nil,
		"operator":   {ID: 1, Username: "o", Role: Operator},
		"supervisor": {ID: 2, Username: "s", Role: Supervisor},
		"admin":      {ID: 3, Username: "a", Role: Admin},
	}
	// what each role may open, per level
	allowed := map[string]map[Level]bool{
		"anonymous":  {Public: true},
		"operator":   {Public: true, Authenticated: true},
		"supervisor": {Public: true, Authenticated: true, Config: true},
		"admin":      {Public: true, Authenticated: true, Config: true, AdminOnly: true},
	}
	for name, u := range roles {
		r := routerFor(u)
		for key, level := range Rules {
			method, route, _ := strings.Cut(key, " ")
			got := request(r, method, route)
			want := 200
			switch {
			case allowed[name][level]:
			case u == nil:
				want = 401
			default:
				want = 403
			}
			if got != want {
				t.Errorf("%s %s: %d, want %d", name, key, got, want)
			}
		}
	}
}

func TestOperatorAndSupervisorCannotUseUserManagement(t *testing.T) {
	for _, role := range []Role{Operator, Supervisor} {
		r := routerFor(&User{ID: 1, Username: "x", Role: role})
		for _, c := range []struct{ m, p string }{
			{"GET", "/config/users"}, {"GET", "/config/users/new"}, {"GET", "/config/users/:id/edit"},
			{"POST", "/config/users"}, {"POST", "/config/users/:id"}, {"DELETE", "/config/users/:id"},
		} {
			if got := request(r, c.m, c.p); got != 403 {
				t.Errorf("%s %s %s: %d, want 403", role, c.m, c.p, got)
			}
		}
	}
}

func TestOperatorCannotOpenConfigurationButCanOpenTheDashboard(t *testing.T) {
	r := routerFor(&User{ID: 1, Username: "o", Role: Operator})
	if got := request(r, "GET", "/config"); got != 403 {
		t.Errorf("GET /config as operator: %d, want 403", got)
	}
	if got := request(r, "POST", "/config/plcs"); got != 403 {
		t.Errorf("POST /config/plcs as operator: %d, want 403", got)
	}
	for _, p := range []string{"/", "/pv/:id", "/pv/:id/history", "/partials/pv-rows"} {
		if got := request(r, "GET", p); got != 200 {
			t.Errorf("GET %s as operator: %d, want 200", p, got)
		}
	}
}

func TestUnlistedRoutesAreClosedToEveryoneButAdministrators(t *testing.T) {
	build := func(u *User) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			if u != nil {
				c.Set(userKey, u)
			}
		})
		r.Use(Guard(func(c *gin.Context, d Denial) { c.String(d.Status, "denied") }))
		r.GET("/new-feature", func(c *gin.Context) { c.String(200, "ok") })
		return r
	}
	for u, want := range map[*User]int{
		nil: 401, {Role: Operator}: 403, {Role: Supervisor}: 403, {Role: Admin}: 200,
	} {
		if got := request(build(u), "GET", "/new-feature"); got != want {
			t.Errorf("unlisted route as %v: %d, want %d", u, got, want)
		}
	}
}

func TestUnknownURLsGet404NotARedirect(t *testing.T) {
	r := routerFor(nil)
	if got := request(r, "GET", "/no/such/page"); got != 404 {
		t.Errorf("got %d, want 404", got)
	}
}

// --- CSRF ----------------------------------------------------------------

func csrfRouter() *gin.Engine {
	r := gin.New()
	r.POST("/act", RequireCSRF(), func(c *gin.Context) { c.String(200, "done") })
	r.GET("/read", RequireCSRF(), func(c *gin.Context) { c.String(200, "read") })
	return r
}

func post(r *gin.Engine, form url.Values, header, cookie string) int {
	req := httptest.NewRequest("POST", "/act", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if header != "" {
		req.Header.Set("X-CSRF-Token", header)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestCSRFProtectionOnStateChangingRequests(t *testing.T) {
	r := csrfRouter()
	const session = "session-token"
	good := csrfFor(session)

	if got := post(r, url.Values{}, "", session); got != 403 {
		t.Errorf("no token: %d, want 403", got)
	}
	if got := post(r, url.Values{"csrf_token": {"forged"}}, "", session); got != 403 {
		t.Errorf("wrong token: %d, want 403", got)
	}
	if got := post(r, url.Values{"csrf_token": {good}}, "", ""); got != 403 {
		t.Errorf("no session: %d, want 403", got)
	}
	if got := post(r, url.Values{"csrf_token": {good}}, "", session); got != 200 {
		t.Errorf("token in form: %d, want 200", got)
	}
	if got := post(r, url.Values{}, good, session); got != 200 {
		t.Errorf("token in header (htmx): %d, want 200", got)
	}
	if got := post(r, url.Values{"csrf_token": {good}}, "", "another-session"); got != 403 {
		t.Errorf("token from a different session: %d, want 403", got)
	}
	// reading is never blocked by it
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/read", nil))
	if w.Code != 200 {
		t.Errorf("GET: %d, want 200", w.Code)
	}
}

// --- cookie settings -------------------------------------------------------

func setCookie(o CookieOptions) *http.Cookie {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	SetCookie(c, o, "tok", time.Now().Add(time.Hour))
	return w.Result().Cookies()[0]
}

func TestSessionCookieIsHTTPOnlyAndScopedWithConfigurableSecureAndSameSite(t *testing.T) {
	local := setCookie(CookieOptions{}) // development over plain HTTP
	if !local.HttpOnly {
		t.Error("the session cookie must be HTTP-only")
	}
	if local.Secure {
		t.Error("Secure must be off by default so http://localhost works")
	}
	if local.SameSite != http.SameSiteLaxMode || local.Path != "/" || local.Expires.IsZero() {
		t.Errorf("default cookie: %+v", local)
	}

	prod := setCookie(CookieOptions{Secure: true, SameSite: http.SameSiteStrictMode})
	if !prod.Secure || prod.SameSite != http.SameSiteStrictMode || !prod.HttpOnly {
		t.Errorf("production cookie: %+v", prod)
	}
}

func TestClearCookieExpiresIt(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ClearCookie(c, CookieOptions{Secure: true})
	ck := w.Result().Cookies()[0]
	if ck.Value != "" || ck.MaxAge >= 0 || !ck.HttpOnly || !ck.Secure {
		t.Errorf("cleared cookie: %+v", ck)
	}
}
