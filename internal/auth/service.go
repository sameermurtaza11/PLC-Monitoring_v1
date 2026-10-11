// Package auth provides login, server-side sessions, and role-based access.
//
// The browser holds only a random session token in an HTTP-only cookie. The
// server stores its SHA-256 hash with the user id; the user's name and role
// are read from the users table on every request, so a changed role or a
// deleted account applies at once. No password is ever put in a session.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"PLC_Monitoring/internal/store"
)

// DefaultSessionLifetime is how long a login lasts.
const DefaultSessionLifetime = 12 * time.Hour

var (
	// ErrInvalidCredentials is the one answer for an unknown user and for a
	// wrong password, so the login form never reveals which was wrong.
	ErrInvalidCredentials = errors.New("Invalid username or password")
	ErrNoSession          = errors.New("not logged in")
)

// Repository is the part of the store the auth service needs.
// *store.Store implements it; tests use a fake.
type Repository interface {
	UserByUsername(ctx context.Context, username string) (store.User, error)
	StartSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time, replace []byte) error
	SessionUser(ctx context.Context, tokenHash []byte) (store.SessionUser, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error
}

// User is the logged-in user attached to a request.
type User struct {
	ID       int64
	Username string
	Role     Role
}

func (u *User) IsAdmin() bool { return u != nil && u.Role == Admin }

type Service struct {
	repo     Repository
	lifetime time.Duration
	now      func() time.Time
}

func NewService(repo Repository, lifetime time.Duration) *Service {
	if lifetime <= 0 {
		lifetime = DefaultSessionLifetime
	}
	return &Service{repo: repo, lifetime: lifetime, now: time.Now}
}

func (s *Service) Lifetime() time.Duration { return s.lifetime }

// dummyStored is compared when the username does not exist, so a login for
// an unknown user takes as long as one for a real user.
const dummyStored = "not-a-real-password-for-timing-only"

// Login checks the credentials and starts a new session. It returns the raw
// token for the cookie and its expiry. `presented` is the token the browser
// sent before logging in (if any); that old session is destroyed, and the new
// one gets a fresh identifier (session fixation protection). The last-login
// time is recorded in the same step; nothing else updates it.
func (s *Service) Login(ctx context.Context, username, password, presented string) (token string, expires time.Time, err error) {
	username = strings.TrimSpace(username)

	u, err := s.repo.UserByUsername(ctx, username)
	switch {
	case errors.Is(err, store.ErrUserNotFound):
		VerifyPassword(StorePassword(dummyStored), password)
		return "", time.Time{}, ErrInvalidCredentials
	case err != nil:
		return "", time.Time{}, fmt.Errorf("load user: %w", err)
	}
	if !VerifyPassword(u.Password, password) || !Role(u.Role).Valid() {
		return "", time.Time{}, ErrInvalidCredentials
	}

	token, hash, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	var replace []byte
	if presented != "" {
		replace = hashToken(presented)
	}
	expires = s.now().Add(s.lifetime)
	if err := s.repo.StartSession(ctx, hash, u.ID, expires, replace); err != nil {
		return "", time.Time{}, fmt.Errorf("start session: %w", err)
	}
	return token, expires, nil
}

// Authenticate returns the user behind a session token, or ErrNoSession.
func (s *Service) Authenticate(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	u, err := s.repo.SessionUser(ctx, hashToken(token))
	if errors.Is(err, store.ErrUserNotFound) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	role := Role(u.Role)
	if !role.Valid() {
		return nil, ErrNoSession
	}
	return &User{ID: u.ID, Username: u.Username, Role: role}, nil
}

// Logout destroys the session on the server.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.repo.DeleteSession(ctx, hashToken(token))
}

func newToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// csrfFor derives the CSRF token of a session: another site cannot read the
// session cookie, so it cannot compute this.
func csrfFor(token string) string {
	sum := sha256.Sum256([]byte("csrf:" + token))
	return fmt.Sprintf("%x", sum[:])
}

func csrfEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
