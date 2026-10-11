package auth

import "PLC_Monitoring/internal/store"

// Role is a user's role. The database allows exactly these three values.
type Role string

const (
	Admin      Role = store.RoleAdmin
	Supervisor Role = store.RoleSupervisor
	Operator   Role = store.RoleOperator
)

// Roles in the order they are offered in forms.
var Roles = []Role{Operator, Supervisor, Admin}

func (r Role) Valid() bool { return r == Admin || r == Supervisor || r == Operator }

// Label is the role as shown in menus ("Operator").
func (r Role) Label() string {
	switch r {
	case Admin:
		return "Admin"
	case Supervisor:
		return "Supervisor"
	case Operator:
		return "Operator"
	}
	return string(r)
}

// Level is what a route requires.
type Level int

const (
	// Public: no login (login page, health check, static files).
	Public Level = iota
	// Authenticated: any logged-in user (dashboard, PV detail, history).
	Authenticated
	// Config: supervisors and administrators (PLC / PV configuration).
	Config
	// AdminOnly: administrators (user management).
	AdminOnly
)

// Allows reports whether the role may use a route of this level.
func (r Role) Allows(l Level) bool {
	switch l {
	case Public:
		return true
	case Authenticated:
		return r.Valid()
	case Config:
		return r == Supervisor || r == Admin
	case AdminOnly:
		return r == Admin
	}
	return false
}
