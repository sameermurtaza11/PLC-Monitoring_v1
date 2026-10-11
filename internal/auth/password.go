package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Password length policy.
const (
	MinPasswordLen = 8
	MaxPasswordLen = 128
)

var ErrPasswordPolicy = fmt.Errorf("password must be %d to %d characters", MinPasswordLen, MaxPasswordLen)

// ValidatePassword checks a NEW password against the policy.
func ValidatePassword(p string) error {
	n := utf8.RuneCountInString(p)
	if n < MinPasswordLen || n > MaxPasswordLen {
		return ErrPasswordPolicy
	}
	return nil
}

// ---------------------------------------------------------------------
// The only place that knows how a password is stored.
//
// Version 01 stores the password as PLAIN TEXT (by decision, to be changed
// later). To start storing hashes: make StorePassword return the hash
// (for example bcrypt), make VerifyPassword compare against it, and migrate
// the existing rows. Nothing else in the application needs to change.
// ---------------------------------------------------------------------

// StorePassword turns a password into the value kept in users.password.
func StorePassword(plain string) string { return plain }

// VerifyPassword reports whether `plain` is the password behind `stored`,
// in constant time.
func VerifyPassword(stored, plain string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(plain)) == 1
}

// ErrBlankPassword is returned when a required password is empty.
var ErrBlankPassword = errors.New("password is required")
