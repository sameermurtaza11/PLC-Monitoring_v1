package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"PLC_Monitoring/internal/store"
)

// fakeRepo is an in-memory Repository.
type fakeRepo struct {
	users    map[string]store.User // key: lower-case username
	sessions map[string]fakeSession
	started  int
}

type fakeSession struct {
	userID  int64
	expires time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users: map[string]store.User{
			"anna": {ID: 1, Username: "Anna", Password: StorePassword("correct-horse"), Role: store.RoleOperator},
			"sam":  {ID: 2, Username: "sam", Password: StorePassword("secret-pass-1"), Role: store.RoleAdmin},
			"bad":  {ID: 3, Username: "bad", Password: StorePassword("whatever-123"), Role: "janitor"}, // not a valid role
		},
		sessions: map[string]fakeSession{},
	}
}

func (f *fakeRepo) UserByUsername(_ context.Context, name string) (store.User, error) {
	if u, ok := f.users[strings.ToLower(name)]; ok {
		return u, nil
	}
	return store.User{}, store.ErrUserNotFound
}

func (f *fakeRepo) StartSession(_ context.Context, hash []byte, userID int64, expires time.Time, replace []byte) error {
	f.started++
	if len(replace) > 0 {
		delete(f.sessions, string(replace))
	}
	f.sessions[string(hash)] = fakeSession{userID, expires}
	return nil
}

func (f *fakeRepo) SessionUser(_ context.Context, hash []byte) (store.SessionUser, error) {
	s, ok := f.sessions[string(hash)]
	if !ok || !s.expires.After(time.Now()) {
		return store.SessionUser{}, store.ErrUserNotFound
	}
	for _, u := range f.users {
		if u.ID == s.userID {
			return store.SessionUser{ID: u.ID, Username: u.Username, Role: u.Role}, nil
		}
	}
	return store.SessionUser{}, store.ErrUserNotFound
}

func (f *fakeRepo) DeleteSession(_ context.Context, hash []byte) error {
	delete(f.sessions, string(hash))
	return nil
}

func TestValidLoginStartsASessionForTheUser(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, time.Hour)

	token, expires, err := svc.Login(context.Background(), "  ANNA ", "correct-horse", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if d := time.Until(expires); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("session lifetime = %v, want about 1 h", d)
	}
	for k := range repo.sessions {
		if k == token {
			t.Fatal("the raw token must not be stored, only its hash")
		}
	}
	u, err := svc.Authenticate(context.Background(), token)
	if err != nil || u.Username != "Anna" || u.Role != Operator || u.ID != 1 {
		t.Fatalf("authenticate: %+v, %v", u, err)
	}
}

func TestInvalidCredentialsGiveOneAnswer(t *testing.T) {
	svc := NewService(newFakeRepo(), time.Hour)
	for _, c := range []struct{ user, pass string }{
		{"anna", "wrong"},           // right user, wrong password
		{"nobody", "correct-horse"}, // unknown user
		{"anna", ""},
		{"", ""},
		{"bad", "whatever-123"}, // right password, but the stored role is not a valid role
	} {
		_, _, err := svc.Login(context.Background(), c.user, c.pass, "")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%q/%q: got %v, want ErrInvalidCredentials", c.user, c.pass, err)
		}
	}
	if ErrInvalidCredentials.Error() != "Invalid username or password" {
		t.Errorf("the message must not say which part was wrong: %q", ErrInvalidCredentials)
	}
}

func TestUsernameMatchingIsCaseInsensitive(t *testing.T) {
	svc := NewService(newFakeRepo(), time.Hour)
	for _, name := range []string{"anna", "ANNA", "Anna", " aNnA "} {
		if _, _, err := svc.Login(context.Background(), name, "correct-horse", ""); err != nil {
			t.Errorf("login as %q: %v", name, err)
		}
	}
	// the password itself is case-sensitive
	if _, _, err := svc.Login(context.Background(), "anna", "Correct-Horse", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("password must be case-sensitive: %v", err)
	}
}

func TestLoginReplacesThePresentedSessionAgainstFixation(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, time.Hour)

	// An attacker plants a known token; a session for it exists on the server.
	old, _, err := svc.Login(context.Background(), "anna", "correct-horse", "")
	if err != nil {
		t.Fatal(err)
	}
	// The victim logs in while presenting that token.
	fresh, _, err := svc.Login(context.Background(), "sam", "secret-pass-1", old)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("the session identifier must change at login")
	}
	if _, err := svc.Authenticate(context.Background(), old); !errors.Is(err, ErrNoSession) {
		t.Errorf("the old identifier must stop working, got %v", err)
	}
	if u, err := svc.Authenticate(context.Background(), fresh); err != nil || u.Username != "sam" {
		t.Errorf("new session: %+v, %v", u, err)
	}
}

func TestLogoutDestroysTheSession(t *testing.T) {
	svc := NewService(newFakeRepo(), time.Hour)
	token, _, _ := svc.Login(context.Background(), "anna", "correct-horse", "")
	if err := svc.Logout(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v, want ErrNoSession after logout", err)
	}
}

func TestExpiredEmptyAndUnknownTokensAreRejected(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, time.Hour)
	token, _, _ := svc.Login(context.Background(), "anna", "correct-horse", "")
	for k, s := range repo.sessions { // age the session
		s.expires = time.Now().Add(-time.Minute)
		repo.sessions[k] = s
	}
	for _, tok := range []string{token, "", "not-a-token"} {
		if _, err := svc.Authenticate(context.Background(), tok); !errors.Is(err, ErrNoSession) {
			t.Errorf("token %q: got %v, want ErrNoSession", tok, err)
		}
	}
}

func TestSessionsCarryNoPassword(t *testing.T) {
	// The session row holds a token hash, a user id and an expiry — and nothing else.
	repo := newFakeRepo()
	svc := NewService(repo, time.Hour)
	svc.Login(context.Background(), "anna", "correct-horse", "")
	for k, s := range repo.sessions {
		if strings.Contains(k, "correct-horse") {
			t.Error("password found in the session key")
		}
		if s.userID != 1 {
			t.Errorf("unexpected session content %+v", s)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := map[string]bool{
		"":                       false,
		"short":                  false,
		"1234567":                false,
		"12345678":               true,
		"correct horse battery":  true,
		strings.Repeat("x", 128): true,
		strings.Repeat("x", 129): false,
	}
	for p, ok := range cases {
		if err := ValidatePassword(p); (err == nil) != ok {
			t.Errorf("ValidatePassword(%d chars) = %v, want ok=%v", len(p), err, ok)
		}
	}
}

func TestVerifyPassword(t *testing.T) {
	stored := StorePassword("Pass-Word-1")
	if !VerifyPassword(stored, "Pass-Word-1") {
		t.Error("right password rejected")
	}
	for _, wrong := range []string{"pass-word-1", "Pass-Word-", "Pass-Word-1 ", ""} {
		if VerifyPassword(stored, wrong) {
			t.Errorf("%q accepted", wrong)
		}
	}
}

func TestRolesAndLevels(t *testing.T) {
	type c struct {
		role  Role
		level Level
		want  bool
	}
	for _, x := range []c{
		{Admin, AdminOnly, true}, {Supervisor, AdminOnly, false}, {Operator, AdminOnly, false},
		{Admin, Config, true}, {Supervisor, Config, true}, {Operator, Config, false},
		{Admin, Authenticated, true}, {Supervisor, Authenticated, true}, {Operator, Authenticated, true},
		{Role("janitor"), Authenticated, false}, {Role(""), Config, false},
		{Operator, Public, true},
	} {
		if got := x.role.Allows(x.level); got != x.want {
			t.Errorf("%q.Allows(%d) = %v, want %v", x.role, x.level, got, x.want)
		}
	}
}
