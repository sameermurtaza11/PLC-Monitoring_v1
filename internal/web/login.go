package web

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
)

// loginPage shows the login form. A user who is already logged in goes
// straight to the dashboard.
func (s *Server) loginPage(c *gin.Context) {
	if auth.FromContext(c) != nil {
		c.Redirect(http.StatusFound, safeNext(c.Query("next")))
		return
	}
	s.renderLogin(c, http.StatusOK, "", c.Query("next"))
}

func (s *Server) login(c *gin.Context) {
	username := c.PostForm("username")
	next := c.PostForm("next")
	presented, _ := c.Cookie(auth.CookieName) // an old identifier is destroyed at login

	token, expires, err := s.auth.Login(c.Request.Context(), username, c.PostForm("password"), presented)
	switch {
	case err == nil:
		auth.SetCookie(c, s.cookie, token, expires)
		log.Printf("web: login %q from %s", username, c.ClientIP()) // never the password
		c.Redirect(http.StatusSeeOther, safeNext(next))
	case errors.Is(err, auth.ErrInvalidCredentials):
		log.Printf("web: failed login for %q from %s", username, c.ClientIP())
		s.renderLogin(c, http.StatusUnauthorized, err.Error(), next)
	default:
		log.Printf("web: login error: %v", err)
		s.renderLogin(c, http.StatusInternalServerError, "Login is unavailable, please try again.", next)
	}
}

// logout destroys the session on the server and in the browser.
func (s *Server) logout(c *gin.Context) {
	if token, err := c.Cookie(auth.CookieName); err == nil {
		if err := s.auth.Logout(c.Request.Context(), token); err != nil {
			log.Printf("web: logout: %v", err)
		}
	}
	auth.ClearCookie(c, s.cookie)
	c.Redirect(http.StatusSeeOther, "/login")
}

func (s *Server) renderLogin(c *gin.Context, status int, msg, next string) {
	if next = safeNext(next); next == "/" {
		next = ""
	}
	c.HTML(status, "login.html", s.pageData(c, "login", gin.H{"Error": msg, "Next": next}))
}
