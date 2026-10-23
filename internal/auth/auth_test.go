package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/config"
)

func TestTOTPMatchesRFC6238(t *testing.T) {
	secret := []byte("12345678901234567890")
	// RFC 6238 appendix B gives eight digits; authenticator apps show the last six.
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924"} {
		if got := totp(secret, unix/totpStep); got != want {
			t.Errorf("totp at %d = %s, want %s", unix, got, want)
		}
	}
}

type fixture struct {
	auth   *Auth
	secret []byte
	clock  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	secret := NewTOTPSecret()
	a, err := New([]config.User{{Name: "admin", PasswordHash: hash, TOTPSecret: secret}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{auth: a, clock: time.Unix(1_800_000_000, 0)}
	f.secret, _ = secretEncoding.DecodeString(secret)
	a.now = func() time.Time { return f.clock }
	return f
}

func (f *fixture) code(offset time.Duration) string {
	return totp(f.secret, f.clock.Add(offset).Unix()/totpStep)
}

func TestLoginStartsASession(t *testing.T) {
	f := newFixture(t)
	token, err := f.auth.Login("admin", "correct horse battery staple", f.code(0))
	if err != nil {
		t.Fatal(err)
	}
	if user, ok := f.auth.User(token); !ok || user != "admin" {
		t.Fatalf("User = %q, %v", user, ok)
	}
	if _, ok := f.auth.User("forged"); ok {
		t.Fatal("an unknown token has a session")
	}

	f.clock = f.clock.Add(SessionTTL)
	if _, ok := f.auth.User(token); ok {
		t.Fatal("the session outlived its TTL")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	f := newFixture(t)
	token, err := f.auth.Login("admin", "correct horse battery staple", f.code(0))
	if err != nil {
		t.Fatal(err)
	}
	f.auth.Logout(token)
	if _, ok := f.auth.User(token); ok {
		t.Fatal("the session survived logout")
	}
}

func TestLoginRejectsWrongCredentials(t *testing.T) {
	f := newFixture(t)
	cases := map[string]struct{ name, password, code string }{
		"unknown user":    {"root", "correct horse battery staple", f.code(0)},
		"wrong password":  {"admin", "Tr0ub4dor&3", f.code(0)},
		"wrong code":      {"admin", "correct horse battery staple", "000000"},
		"expired code":    {"admin", "correct horse battery staple", f.code(-2 * totpStep * time.Second)},
		"code from later": {"admin", "correct horse battery staple", f.code(2 * totpStep * time.Second)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.auth.Login(tc.name, tc.password, tc.code); !errors.Is(err, ErrDenied) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestLoginAllowsClockDriftButNoReplay(t *testing.T) {
	f := newFixture(t)
	if _, err := f.auth.Login("admin", "correct horse battery staple", f.code(-totpStep*time.Second)); err != nil {
		t.Fatalf("previous step: %v", err)
	}
	if _, err := f.auth.Login("admin", "correct horse battery staple", f.code(-totpStep*time.Second)); !errors.Is(err, ErrDenied) {
		t.Fatalf("replayed code: %v", err)
	}
	if _, err := f.auth.Login("admin", "correct horse battery staple", f.code(totpStep*time.Second)); err != nil {
		t.Fatalf("next step: %v", err)
	}
}

func TestOTPAuthURI(t *testing.T) {
	got := OTPAuthURI("GameFleet", "admin", "JBSWY3DPEHPK3PXP")
	if want := "otpauth://totp/GameFleet:admin?issuer=GameFleet&secret=JBSWY3DPEHPK3PXP"; got != want {
		t.Fatalf("uri = %s\nwant  %s", got, want)
	}
}
