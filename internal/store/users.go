package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Roles stored in users.role (the database also enforces this list).
const (
	RoleAdmin      = "admin"
	RoleSupervisor = "supervisor"
	RoleOperator   = "operator"
)

var (
	ErrUserNotFound = errors.New("user not found")
	ErrLastAdmin    = errors.New("the last administrator cannot be deleted or demoted")
)

// User is one row of users. Password is filled only by GetUser and
// UserByUsername; lists never carry it.
type User struct {
	ID         int64
	Username   string
	Password   string
	Role       string
	CreatedOn  time.Time
	LastOnline *time.Time // nil = never logged in
}

// SessionUser is the identity behind a valid session.
type SessionUser struct {
	ID       int64
	Username string
	Role     string
}

// adminLockKey serialises every change that could remove an administrator,
// so two administrators demoting or deleting each other at the same moment
// cannot leave the system without one.
const adminLockKey = 7_265_003

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, username, role, created_on, last_online
		FROM users ORDER BY lower(username)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) {
		var u User
		return u, r.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedOn, &u.LastOnline)
	})
}

// GetUser returns one user including the stored password (administrators
// see it in the edit form). ErrUserNotFound if it does not exist.
func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, username, password, role, created_on, last_online
		FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Username, &u.Password, &u.Role, &u.CreatedOn, &u.LastOnline)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrUserNotFound
	}
	return u, err
}

// UserByUsername finds a user ignoring case. ErrUserNotFound if none.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, username, password, role, created_on, last_online
		FROM users WHERE lower(username) = lower($1)`, username).
		Scan(&u.ID, &u.Username, &u.Password, &u.Role, &u.CreatedOn, &u.LastOnline)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrUserNotFound
	}
	return u, err
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a user. A duplicate name (ignoring case) or an invalid
// role comes back as *ConstraintError.
func (s *Store) CreateUser(ctx context.Context, username, password, role string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (username, password, role) VALUES ($1, $2, $3) RETURNING id`,
		username, password, role).Scan(&id)
	return id, asConstraintError(err)
}

// UpdateUser changes the username and role, and the password only when
// `password` is not nil. Demoting the last administrator is refused.
func (s *Store) UpdateUser(ctx context.Context, id int64, username, role string, password *string) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, adminLockKey); err != nil {
			return err
		}
		var oldRole string
		if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&oldRole); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUserNotFound
			}
			return err
		}
		if oldRole == RoleAdmin && role != RoleAdmin {
			if err := requireAnotherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE users SET username = $2, role = $3, password = COALESCE($4, password)
			WHERE id = $1`, id, username, role, password)
		return err
	})
	return asConstraintError(err)
}

// DeleteUser removes a user and, through the foreign key, all their
// sessions. Deleting the last administrator is refused.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, adminLockKey); err != nil {
			return err
		}
		var role string
		if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&role); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUserNotFound
			}
			return err
		}
		if role == RoleAdmin {
			if err := requireAnotherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
		return err
	})
}

// requireAnotherAdmin returns ErrLastAdmin unless some administrator other
// than `id` exists.
func requireAnotherAdmin(ctx context.Context, tx pgx.Tx, id int64) error {
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND id <> $1`, id).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return ErrLastAdmin
	}
	return nil
}

// StartSession records a new session and the login time in one transaction.
// `replace` (the hash of the token the browser presented before logging in,
// if any) is deleted, so an old identifier cannot be carried into the
// authenticated session (session fixation). Expired sessions are cleaned up.
func (s *Store) StartSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time, replace []byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if len(replace) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, replace); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
			tokenHash, userID, expires); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET last_online = now() WHERE id = $1`, userID)
		return err
	})
}

// SessionUser returns the user of a live (unexpired) session. The role is
// read from users on every call, so a changed role or a deleted account
// takes effect immediately. ErrUserNotFound if there is no such session.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (SessionUser, error) {
	var u SessionUser
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.role
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, tokenHash).
		Scan(&u.ID, &u.Username, &u.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrUserNotFound
	}
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}
