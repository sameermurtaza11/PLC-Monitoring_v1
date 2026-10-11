package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
)

// navItem is one link of the top menu.
type navItem struct {
	Key, Label, Href string
}

// navFor returns the menu links this visitor may open. The server still
// enforces access on every request; hiding a link is only a convenience.
func navFor(user *auth.User) []navItem {
	if user == nil {
		return nil
	}
	items := []navItem{{"dashboard", "Dashboard", "/"}}
	if user.Role.Allows(auth.Config) {
		items = append(items, navItem{"config", "Configuration", "/config"})
	}
	return items
}

// pageData is the data every full page gets: the logged-in user, the CSRF
// token and the menu.
func (s *Server) pageData(c *gin.Context, page string, extra gin.H) gin.H {
	user := auth.FromContext(c)
	h := gin.H{
		"Page":    page,
		"User":    user,
		"IsAdmin": user.IsAdmin(),
		"CSRF":    auth.CSRFToken(c),
		"Nav":     navFor(user),
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// denied answers a request the guard refused, in the form the caller expects:
// a redirect to the login page for a browser page, an HX-Redirect header for
// HTMX (so a polled fragment never fills with the login page when a session
// ends), a "not permitted" page for a logged-in user whose role may not.
func (s *Server) denied(c *gin.Context, d auth.Denial) {
	hx := c.GetHeader("HX-Request") != ""
	switch {
	case d.NeedLogin && hx:
		c.Header("HX-Redirect", loginURL(hxPath(c)))
		c.String(http.StatusUnauthorized, "login required")
	case d.NeedLogin && c.Request.Method == http.MethodGet:
		c.Redirect(http.StatusFound, loginURL(c.Request.URL.RequestURI()))
	case d.NeedLogin:
		c.String(http.StatusUnauthorized, "login required")
	case hx:
		c.String(http.StatusForbidden, "not permitted")
	default:
		c.HTML(http.StatusForbidden, "forbidden.html", s.pageData(c, "forbidden", nil))
	}
}

// hxPath is the page the browser is on when an htmx request is made.
func hxPath(c *gin.Context) string {
	if u, err := url.Parse(c.GetHeader("HX-Current-URL")); err == nil && u.Path != "" {
		return u.RequestURI()
	}
	return "/"
}

func loginURL(next string) string {
	if next = safeNext(next); next == "/" {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(next)
}

// safeNext returns a local path to go back to after login. Anything that
// could lead to another site (absolute URLs, //host, backslashes) becomes "/".
func safeNext(next string) string {
	if next == "" || len(next) > 500 || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") ||
		strings.HasPrefix(next, "/login") {
		return "/"
	}
	return next
}
