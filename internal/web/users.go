package web

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// User management (administrators only: enforced by auth.Rules for every
// route below). It follows the same HTMX flow as the PLC / PV forms.

const maxUsername = 100

// usernameRE: letters, digits and . _ - @ (so e-mail style names work).
var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

func (s *Server) userList(c *gin.Context) {
	users, err := s.store.ListUsers(c.Request.Context())
	if err != nil {
		log.Printf("web: list users: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "user_rows.html", gin.H{"Users": users, "Me": auth.FromContext(c)})
}

func (s *Server) userForm(c *gin.Context) {
	f := form{Values: map[string]string{"role": string(auth.Operator)}, Errors: fieldErrors{}}
	if idStr := c.Param("id"); idStr != "" {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		u, err := s.store.GetUser(c.Request.Context(), id)
		if err != nil {
			c.String(http.StatusNotFound, "User not found")
			return
		}
		f.ID = int(u.ID)
		// The administrator sees the current password here (Version 01).
		f.Values = map[string]string{"username": u.Username, "role": u.Role, "password": u.Password}
	}
	s.renderUserForm(c, f)
}

func (s *Server) renderUserForm(c *gin.Context, f form) {
	c.HTML(http.StatusOK, "user_form.html", gin.H{"F": f, "Roles": auth.Roles})
}

func (s *Server) userSave(c *gin.Context) {
	ctx := c.Request.Context()
	f := form{Values: map[string]string{}, Errors: fieldErrors{}}
	f.Values["username"] = strings.TrimSpace(c.PostForm("username"))
	f.Values["role"] = strings.TrimSpace(c.PostForm("role"))
	f.Values["password"] = c.PostForm("password") // not trimmed: spaces may be part of it
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	f.ID = int(id)

	username, role, password := f.Values["username"], f.Values["role"], f.Values["password"]

	switch n := utf8.RuneCountInString(username); {
	case username == "":
		f.Errors["username"] = "Required"
	case n < 3 || n > maxUsername:
		f.Errors["username"] = "Between 3 and 100 characters"
	case !usernameRE.MatchString(username):
		f.Errors["username"] = "Only letters, digits and . _ - @"
	}
	if !auth.Role(role).Valid() {
		f.Errors["role"] = "Choose Operator, Supervisor or Admin"
	}

	var newPassword *string // nil on update = keep the current one
	if id == 0 {
		if password == "" {
			f.Errors["password"] = "Required"
		} else if err := auth.ValidatePassword(password); err != nil {
			f.Errors["password"] = err.Error()
		}
		newPassword = &password
	} else if password != "" {
		current, err := s.store.GetUser(ctx, id)
		switch {
		case errors.Is(err, store.ErrUserNotFound):
			c.String(http.StatusNotFound, "User not found (already deleted?)")
			return
		case err != nil:
			log.Printf("web: load user %d: %v", id, err)
			f.Errors["_"] = "Could not check the user, try again"
		case password != current.Password:
			// a changed password must meet the policy; the unchanged one (shown
			// in this form) is kept as it is, even if it is an older, shorter one
			if err := auth.ValidatePassword(password); err != nil {
				f.Errors["password"] = err.Error()
			}
			stored := auth.StorePassword(password)
			newPassword = &stored
		}
	}

	if len(f.Errors) == 0 {
		var err error
		if id == 0 {
			_, err = s.store.CreateUser(ctx, username, auth.StorePassword(*newPassword), role)
		} else {
			err = s.store.UpdateUser(ctx, id, username, role, newPassword)
		}
		switch {
		case err == nil:
			payload, _ := json.Marshal(map[string]string{"configSaved": "User " + username + " saved"})
			c.Header("HX-Trigger", string(payload))
			c.Status(http.StatusNoContent)
			return
		case errors.Is(err, store.ErrLastAdmin):
			f.Errors["role"] = "This is the last administrator; it cannot be demoted"
		case errors.Is(err, store.ErrUserNotFound):
			c.String(http.StatusNotFound, "User not found (already deleted?)")
			return
		default:
			var ce *store.ConstraintError
			if errors.As(err, &ce) {
				switch ce.Constraint {
				case "users_username_key", "users_username_lower_idx":
					f.Errors["username"] = "This username is already taken (names are not case-sensitive)"
				case "users_role_check":
					f.Errors["role"] = "Choose Operator, Supervisor or Admin"
				default:
					f.Errors["_"] = "Rejected by the database: " + ce.Message
				}
			} else {
				log.Printf("web: save user: %v", err) // the message never contains the password
				f.Errors["_"] = "Could not save the user, try again"
			}
		}
	}
	s.renderUserForm(c, f)
}

func (s *Server) userDelete(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	me := auth.FromContext(c)
	err := s.store.DeleteUser(c.Request.Context(), id)
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		s.message(c, "error", "The last administrator cannot be deleted. Create another administrator first.")
	case errors.Is(err, store.ErrUserNotFound):
		s.message(c, "error", "User not found (already deleted?).")
	case err != nil:
		log.Printf("web: delete user %d: %v", id, err)
		s.message(c, "error", "Delete failed, try again.")
	case me != nil && me.ID == id:
		// the account is gone, and with it this session
		c.Header("HX-Redirect", "/login")
		s.message(c, "ok", "Your own account was deleted. You will be signed out.")
	default:
		c.Header("HX-Trigger", "configSaved")
		s.message(c, "ok", "User deleted.")
	}
}
