package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Integration test against a REAL PostgreSQL database. Skipped unless
// PLC_TEST_DATABASE_URL is set, and it refuses to run on a database whose
// name does not contain "scratch" or "test" (it empties the users table).
//
//	$env:PLC_TEST_DATABASE_URL = "postgres://postgres@localhost:5432/plc_users_scratch"
//	go test ./internal/store/ -run Integration -v
func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	url := os.Getenv("PLC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PLC_TEST_DATABASE_URL not set; skipping database integration test")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(name, "scratch") && !strings.Contains(name, "test") {
		t.Fatalf("refusing to run on database %q: its name must contain 'scratch' or 'test'", name)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM users`); err != nil { // cascades to sessions
		t.Fatalf("empty users: %v", err)
	}
	return s, ctx
}

func TestIntegrationUsers(t *testing.T) {
	s, ctx := openTestStore(t)

	adminID, err := s.CreateUser(ctx, "Boss", "admin-pass-1", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	opID, err := s.CreateUser(ctx, "olga", "operator-pass", RoleOperator)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("duplicate usernames are rejected, ignoring case", func(t *testing.T) {
		for _, name := range []string{"Boss", "boss", "BOSS"} {
			_, err := s.CreateUser(ctx, name, "another-pass-1", RoleOperator)
			var ce *ConstraintError
			if !errors.As(err, &ce) {
				t.Fatalf("CreateUser(%q): %v, want a constraint error", name, err)
			}
			if ce.Constraint != "users_username_key" && ce.Constraint != "users_username_lower_idx" {
				t.Errorf("constraint = %q", ce.Constraint)
			}
		}
	})

	t.Run("the database only accepts the three roles", func(t *testing.T) {
		_, err := s.CreateUser(ctx, "janitor", "janitor-pass-1", "janitor")
		var ce *ConstraintError
		if !errors.As(err, &ce) || ce.Constraint != "users_role_check" {
			t.Fatalf("got %v, want users_role_check", err)
		}
	})

	t.Run("lookup ignores case and the list carries no password", func(t *testing.T) {
		u, err := s.UserByUsername(ctx, "bOsS")
		if err != nil || u.ID != adminID || u.Password != "admin-pass-1" {
			t.Fatalf("got %+v, %v", u, err)
		}
		list, err := s.ListUsers(ctx)
		if err != nil || len(list) != 2 {
			t.Fatalf("%v, %d users", err, len(list))
		}
		for _, x := range list {
			if x.Password != "" {
				t.Error("ListUsers must not return passwords")
			}
			if x.LastOnline != nil {
				t.Error("last_online must be NULL until the first login")
			}
		}
	})

	t.Run("update keeps the password unless a new one is given", func(t *testing.T) {
		if err := s.UpdateUser(ctx, opID, "olga2", RoleSupervisor, nil); err != nil {
			t.Fatal(err)
		}
		u, _ := s.GetUser(ctx, opID)
		if u.Username != "olga2" || u.Role != RoleSupervisor || u.Password != "operator-pass" {
			t.Fatalf("after update without password: %+v", u)
		}
		np := "new-password-9"
		if err := s.UpdateUser(ctx, opID, "olga2", RoleSupervisor, &np); err != nil {
			t.Fatal(err)
		}
		u, _ = s.GetUser(ctx, opID)
		if u.Password != np {
			t.Fatalf("password not changed: %+v", u)
		}
	})

	t.Run("renaming to a taken name is rejected", func(t *testing.T) {
		err := s.UpdateUser(ctx, opID, "BOSS", RoleSupervisor, nil)
		var ce *ConstraintError
		if !errors.As(err, &ce) {
			t.Fatalf("got %v, want a constraint error", err)
		}
	})

	t.Run("login time is recorded by StartSession and nowhere else", func(t *testing.T) {
		before, _ := s.GetUser(ctx, opID)
		if before.LastOnline != nil {
			t.Fatal("expected NULL before login")
		}
		if err := s.StartSession(ctx, []byte("hash-1"), opID, time.Now().Add(time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		after, _ := s.GetUser(ctx, opID)
		if after.LastOnline == nil || time.Since(*after.LastOnline) > time.Minute {
			t.Fatalf("last_online = %v, want about now", after.LastOnline)
		}
		stamp := *after.LastOnline
		for i := 0; i < 3; i++ { // looking the session up (page requests) must not move it
			if _, err := s.SessionUser(ctx, []byte("hash-1")); err != nil {
				t.Fatal(err)
			}
		}
		again, _ := s.GetUser(ctx, opID)
		if !again.LastOnline.Equal(stamp) {
			t.Error("last_online changed on a normal request")
		}
	})

	t.Run("a session shows the CURRENT role and dies with its account or at expiry", func(t *testing.T) {
		su, err := s.SessionUser(ctx, []byte("hash-1"))
		if err != nil || su.Role != RoleSupervisor || su.Username != "olga2" {
			t.Fatalf("%+v, %v", su, err)
		}
		if err := s.UpdateUser(ctx, opID, "olga2", RoleOperator, nil); err != nil { // demoted while logged in
			t.Fatal(err)
		}
		if su, _ = s.SessionUser(ctx, []byte("hash-1")); su.Role != RoleOperator {
			t.Errorf("role in session = %q, want the new role at once", su.Role)
		}
		if err := s.StartSession(ctx, []byte("old"), opID, time.Now().Add(-time.Minute), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SessionUser(ctx, []byte("old")); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("expired session accepted: %v", err)
		}
	})

	t.Run("starting a session replaces the one presented before (fixation)", func(t *testing.T) {
		if err := s.StartSession(ctx, []byte("hash-2"), opID, time.Now().Add(time.Hour), []byte("hash-1")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SessionUser(ctx, []byte("hash-1")); !errors.Is(err, ErrUserNotFound) {
			t.Error("the presented session must be destroyed at login")
		}
		if _, err := s.SessionUser(ctx, []byte("hash-2")); err != nil {
			t.Errorf("new session: %v", err)
		}
	})

	t.Run("logout deletes the session", func(t *testing.T) {
		if err := s.DeleteSession(ctx, []byte("hash-2")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SessionUser(ctx, []byte("hash-2")); !errors.Is(err, ErrUserNotFound) {
			t.Error("session still valid after logout")
		}
	})

	t.Run("the last administrator can be neither demoted nor deleted", func(t *testing.T) {
		if err := s.UpdateUser(ctx, adminID, "Boss", RoleSupervisor, nil); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("demote: %v, want ErrLastAdmin", err)
		}
		if err := s.DeleteUser(ctx, adminID); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("delete: %v, want ErrLastAdmin", err)
		}
		u, _ := s.GetUser(ctx, adminID)
		if u.Role != RoleAdmin {
			t.Fatal("the administrator was changed anyway")
		}
		// renaming or changing the password of the last admin is fine
		np := "boss-new-pass"
		if err := s.UpdateUser(ctx, adminID, "Boss", RoleAdmin, &np); err != nil {
			t.Errorf("editing the last admin without demoting: %v", err)
		}
	})

	t.Run("with a second administrator the first can be demoted or deleted", func(t *testing.T) {
		second, err := s.CreateUser(ctx, "deputy", "deputy-pass-1", RoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateUser(ctx, adminID, "Boss", RoleSupervisor, nil); err != nil {
			t.Fatalf("demote with another admin present: %v", err)
		}
		// now deputy is the last one
		if err := s.DeleteUser(ctx, second); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("delete the new last admin: %v, want ErrLastAdmin", err)
		}
		if err := s.UpdateUser(ctx, adminID, "Boss", RoleAdmin, nil); err != nil { // promote again
			t.Fatal(err)
		}
		if err := s.DeleteUser(ctx, second); err != nil {
			t.Fatalf("delete with another admin present: %v", err)
		}
	})

	t.Run("two administrators removing each other at once leave exactly one", func(t *testing.T) {
		a, _ := s.CreateUser(ctx, "twin_a", "twin-pass-aaa", RoleAdmin)
		b, _ := s.CreateUser(ctx, "twin_b", "twin-pass-bbb", RoleAdmin)
		// demote everyone else so that only the twins (and Boss) are admins; remove Boss as admin too
		if err := s.UpdateUser(ctx, adminID, "Boss", RoleOperator, nil); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, id := range []int64{a, b} {
			wg.Add(1)
			go func(i int, id int64) {
				defer wg.Done()
				<-start
				errs[i] = s.DeleteUser(ctx, id)
			}(i, id)
		}
		close(start)
		wg.Wait()
		ok, refused := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case errors.Is(e, ErrLastAdmin):
				refused++
			default:
				t.Errorf("unexpected error: %v", e)
			}
		}
		var admins int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if admins != 1 || ok != 1 || refused != 1 {
			t.Errorf("admins left = %d (deleted %d, refused %d), want 1 / 1 / 1", admins, ok, refused)
		}
	})

	t.Run("deleting a user removes their sessions", func(t *testing.T) {
		u, _ := s.CreateUser(ctx, "temp", "temp-pass-123", RoleOperator)
		if err := s.StartSession(ctx, []byte("hash-t"), u, time.Now().Add(time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SessionUser(ctx, []byte("hash-t")); !errors.Is(err, ErrUserNotFound) {
			t.Error("a deleted user's session must stop working")
		}
		if err := s.DeleteUser(ctx, u); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("second delete: %v, want ErrUserNotFound", err)
		}
	})

	_ = fmt.Sprint // keep fmt imported for failure messages above
}
