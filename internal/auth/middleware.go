package auth

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// CookieName is the session cookie.
const CookieName = "plc_session"

const userKey = "auth.user"

// CookieOptions are the security settings of the session cookie.
//
// The cookie is always HTTP-only. Secure must be on when the application is
// served over HTTPS (it keeps the cookie off plain HTTP); it is off by
// default so local development over http://localhost still works.
type CookieOptions struct {
	Secure   bool
	SameSite http.SameSite // Lax (default) or Strict
}

func (o CookieOptions) sameSite() http.SameSite {
	if o.SameSite == http.SameSiteStrictMode {
		return http.SameSiteStrictMode
	}
	return http.SameSiteLaxMode
}

// SetCookie stores the session token in the browser.
func SetCookie(c *gin.Context, o CookieOptions, token string, expires time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: CookieName, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: o.Secure, SameSite: o.sameSite(),
	})
}

// ClearCookie removes the session cookie.
func ClearCookie(c *gin.Context, o CookieOptions) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: o.Secure, SameSite: o.sameSite(),
	})
}

// Load identifies the user of the request (if any). It never blocks a
// request; Guard decides.
func Load(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token, err := c.Cookie(CookieName); err == nil {
			if u, err := svc.Authenticate(c.Request.Context(), token); err == nil {
				c.Set(userKey, u)
			}
		}
		c.Next()
	}
}

// FromContext returns the logged-in user, or nil.
func FromContext(c *gin.Context) *User {
	u, _ := c.Get(userKey)
	user, _ := u.(*User)
	return user
}

// Denial describes a refused request.
type Denial struct {
	Status    int // 401: login needed; 403: logged in but the role may not
	NeedLogin bool
	Level     Level
	User      *User
}

// Guard applies Rules to every request, on the server. onDeny renders the
// refusal. It must run after Load.
func Guard(onDeny func(c *gin.Context, d Denial)) gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "" {
			return // no such route: gin answers 404
		}
		level, listed := LevelFor(c.Request.Method, route)
		if !listed {
			// A route nobody classified is closed to non-administrators.
			log.Printf("auth: route %s %s has no access rule; closed to non-administrators", c.Request.Method, route)
		}
		if d := Decide(level, FromContext(c)); d != nil {
			onDeny(c, *d)
			c.Abort()
		}
	}
}

// Decide returns nil if a user (nil = not logged in) may use a route of this level.
func Decide(level Level, user *User) *Denial {
	if level == Public {
		return nil
	}
	if user == nil {
		return &Denial{Status: http.StatusUnauthorized, NeedLogin: true, Level: level}
	}
	if !user.Role.Allows(level) {
		return &Denial{Status: http.StatusForbidden, Level: level, User: user}
	}
	return nil
}

// CSRFToken is the token a form or request must send back. It is derived from
// the session cookie.
func CSRFToken(c *gin.Context) string {
	token, err := c.Cookie(CookieName)
	if err != nil || token == "" {
		return ""
	}
	return csrfFor(token)
}

// RequireCSRF rejects state-changing requests (anything but GET/HEAD/OPTIONS)
// that do not carry the right token in the form field "csrf_token" or the
// header "X-CSRF-Token".
func RequireCSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return
		}
		want := CSRFToken(c)
		got := c.PostForm("csrf_token")
		if got == "" {
			got = c.GetHeader("X-CSRF-Token")
		}
		if want == "" || !csrfEqual(want, got) {
			c.String(http.StatusForbidden, "invalid or missing security token — reload the page and try again")
			c.Abort()
		}
	}
}
