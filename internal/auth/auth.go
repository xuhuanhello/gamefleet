// Package auth signs console users in. A user proves a password and a TOTP
// code from an authenticator app, then holds a session token in a cookie.
// Sessions live in memory, so restarting GameFleet signs everyone out.
package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/config"
)

// SessionTTL is how long a sign-in lasts.
const SessionTTL = 12 * time.Hour

const (
	iterations = 600_000 // OWASP's 2023 figure for PBKDF2-SHA256
	totpStep   = 30      // seconds
	totpDigits = 1_000_000
)

// ErrDenied means the name, password or code was wrong. The caller is not told which.
var ErrDenied = errors.New("sign-in denied")

var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Auth checks sign-ins and remembers sessions.
type Auth struct {
	users map[string]*user
	now   func() time.Time

	// checking lets one password check run at a time, which bounds the CPU a
	// flood of sign-in attempts can use.
	checking sync.Mutex

	mu       sync.Mutex
	sessions map[[32]byte]session
}

type user struct {
	name       string
	iterations int
	salt, key  []byte
	totp       []byte
	lastStep   int64 // a TOTP code works once
}

type session struct {
	user    string
	expires time.Time
}

// New prepares the configured users.
func New(users []config.User) (*Auth, error) {
	a := &Auth{users: map[string]*user{}, sessions: map[[32]byte]session{}, now: time.Now}
	for _, u := range users {
		parts := strings.Split(u.PasswordHash, "$")
		if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
			return nil, fmt.Errorf("user %s: unknown password hash", u.Name)
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("user %s: password hash: %w", u.Name, err)
		}
		salt, err := base64.RawStdEncoding.DecodeString(parts[2])
		if err != nil {
			return nil, fmt.Errorf("user %s: password salt: %w", u.Name, err)
		}
		key, err := base64.RawStdEncoding.DecodeString(parts[3])
		if err != nil {
			return nil, fmt.Errorf("user %s: password key: %w", u.Name, err)
		}
		secret, err := secretEncoding.DecodeString(u.TOTPSecret)
		if err != nil {
			return nil, fmt.Errorf("user %s: TOTP secret: %w", u.Name, err)
		}
		a.users[u.Name] = &user{name: u.Name, iterations: n, salt: salt, key: key, totp: secret}
	}
	return a, nil
}

// Login checks the password and the current TOTP code and starts a session.
// It returns the session token for the cookie.
func (a *Auth) Login(name, password, code string) (string, error) {
	a.checking.Lock()
	defer a.checking.Unlock()

	u := a.users[name]
	if u == nil {
		return "", ErrDenied
	}
	key, err := pbkdf2.Key(sha256.New, password, u.salt, u.iterations, len(u.key))
	if err != nil || subtle.ConstantTimeCompare(key, u.key) != 1 {
		return "", ErrDenied
	}
	// Accept the previous and next 30-second step too, for clock drift.
	now := a.now().Unix() / totpStep
	step := int64(-1)
	for s := now - 1; s <= now+1; s++ {
		if s > u.lastStep && subtle.ConstantTimeCompare([]byte(totp(u.totp, s)), []byte(code)) == 1 {
			step = s
		}
	}
	if step < 0 {
		return "", ErrDenied
	}
	u.lastStep = step

	token := rand.Text()
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, s := range a.sessions {
		if !a.now().Before(s.expires) {
			delete(a.sessions, k)
		}
	}
	a.sessions[sha256.Sum256([]byte(token))] = session{user: name, expires: a.now().Add(SessionTTL)}
	return token, nil
}

// User returns who holds the session token.
func (a *Auth) User(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[sha256.Sum256([]byte(token))]
	if !ok || !a.now().Before(s.expires) {
		return "", false
	}
	return s.user, true
}

// Logout ends the session.
func (a *Auth) Logout(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sha256.Sum256([]byte(token)))
}

// HashPassword makes the passwordHash of a config user.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// NewTOTPSecret makes the totpSecret of a config user: 160 random bits, as RFC 4226 recommends.
func NewTOTPSecret() string {
	secret := make([]byte, 20)
	rand.Read(secret)
	return secretEncoding.EncodeToString(secret)
}

// OTPAuthURI is what authenticator apps import, by QR code or by pasting it.
func OTPAuthURI(issuer, name, secret string) string {
	return (&url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + name,
		RawQuery: url.Values{"secret": {secret}, "issuer": {issuer}}.Encode(),
	}).String()
}

// Code is the code an authenticator app shows for the secret at that time.
func Code(secret string, at time.Time) (string, error) {
	key, err := secretEncoding.DecodeString(secret)
	if err != nil {
		return "", err
	}
	return totp(key, at.Unix()/totpStep), nil
}

// totp is the RFC 6238 code for one time step: HMAC-SHA1, six digits.
func totp(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%totpDigits)
}
