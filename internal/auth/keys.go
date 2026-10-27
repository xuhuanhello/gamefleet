package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Scopes an API key can carry, see docs/api.md.
const (
	// ScopePush lets CI push images and register builds. It can pull too.
	ScopePush = "push"
	// ScopePull lets cluster nodes pull images.
	ScopePull = "pull"
	// ScopeAllocate lets the game backend read routes, allocate rooms and look rooms up.
	ScopeAllocate = "allocate"
	// ScopeGM lets a GM backend look rooms up and shut them down.
	ScopeGM = "gm"
	// ScopeLogs lets a person or an AI agent read the clusters' logs and
	// events and GameFleet's own log, with secrets and personal data hidden.
	// It changes nothing.
	ScopeLogs = "logs"
)

// Scopes lists every scope in the order the console shows them.
var Scopes = []string{ScopePush, ScopePull, ScopeAllocate, ScopeGM, ScopeLogs}

var (
	// ErrInvalidKey means a key's name, scopes or value is not acceptable.
	ErrInvalidKey = errors.New("key 不对")

	keyName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Caller is whoever presented a valid API key.
type Caller struct {
	Name   string
	Scopes []string
}

// Has reports whether the caller holds any of the scopes.
func (c Caller) Has(scopes ...string) bool {
	for _, s := range scopes {
		if slices.Contains(c.Scopes, s) {
			return true
		}
	}
	return false
}

// Actor is how the audit log names the caller.
func (c Caller) Actor() string { return "key:" + c.Name }

// Keys checks and manages API keys. Only their SHA-256 is stored: keys are
// 256 random bits, so a plain hash is enough.
type Keys struct {
	store *store.Store
	// When each key was last presented, by name, since GameFleet started.
	used sync.Map
}

// NewKeys returns the keys kept in the store.
func NewKeys(s *store.Store) *Keys { return &Keys{store: s} }

// Check returns the caller whose key this is.
func (k *Keys) Check(ctx context.Context, key string) (Caller, bool) {
	if key == "" || len(key) > 256 {
		return Caller{}, false
	}
	sum := sha256.Sum256([]byte(key))
	var found store.Key
	var now time.Time
	err := k.store.View(ctx, func(tx *store.Tx) error {
		var err error
		found, err = tx.KeyByHash(sum[:])
		now = tx.Now()
		return err
	})
	if err != nil || found.Expired(now) {
		return Caller{}, false
	}
	k.used.Store(found.Name, now)
	return Caller{Name: found.Name, Scopes: found.Scopes}, true
}

// List returns every key, without values, with when it was last used.
func (k *Keys) List(ctx context.Context) ([]store.Key, error) {
	var keys []store.Key
	err := k.store.View(ctx, func(tx *store.Tx) error {
		var err error
		keys, err = tx.Keys()
		return err
	})
	for i := range keys {
		if at, ok := k.used.Load(keys[i].Name); ok {
			keys[i].LastUsed = at.(time.Time)
		}
	}
	return keys, err
}

// Create makes a new key that works until expires, or for ever if it is
// zero, and returns its value, which is never shown again.
func (k *Keys) Create(ctx context.Context, name string, scopes []string, expires time.Time, actor string) (string, error) {
	raw := make([]byte, 32)
	rand.Read(raw)
	value := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(value))
	if err := checkKey(name, scopes); err != nil {
		return "", err
	}
	return value, k.addHash(ctx, name, scopes, sum[:], expires, actor)
}

// Add stores a key whose value was made elsewhere, for example a registry
// password that callers already hold. The value needs at least 32 characters.
func (k *Keys) Add(ctx context.Context, name string, scopes []string, value, actor string) error {
	if err := checkKey(name, scopes); err != nil {
		return err
	}
	if len(value) < 32 || len(value) > 256 || strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("%w：key 要 32 到 256 个字符，不能有空白", ErrInvalidKey)
	}
	sum := sha256.Sum256([]byte(value))
	return k.addHash(ctx, name, scopes, sum[:], time.Time{}, actor)
}

// AddHash stores a key known only by its SHA-256 in hex, for a key whose
// value stays on another machine.
func (k *Keys) AddHash(ctx context.Context, name string, scopes []string, sum, actor string) error {
	raw, err := hex.DecodeString(sum)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("%w：SHA-256 要 64 个十六进制字符", ErrInvalidKey)
	}
	if err := checkKey(name, scopes); err != nil {
		return err
	}
	return k.addHash(ctx, name, scopes, raw, time.Time{}, actor)
}

func (k *Keys) addHash(ctx context.Context, name string, scopes []string, sum []byte, expires time.Time, actor string) error {
	scopes = sorted(scopes)
	err := k.store.Update(ctx, func(tx *store.Tx) error {
		if !expires.IsZero() && !expires.After(tx.Now()) {
			return fmt.Errorf("%w：过期时间要在将来", ErrInvalidKey)
		}
		if _, err := tx.InsertKey(name, sum, scopes, expires, actor); err != nil {
			return err
		}
		detail := strings.Join(scopes, " ")
		if !expires.IsZero() {
			detail += "，" + expires.UTC().Format("2006-01-02 15:04 UTC") + " 过期"
		}
		return tx.Audit(actor, "key.create", name, detail)
	})
	if errors.Is(err, store.ErrDuplicate) {
		return fmt.Errorf("%w：已经有同名或同值的 key", ErrInvalidKey)
	}
	return err
}

// Revoke deletes the key. Callers using it are refused from then on.
func (k *Keys) Revoke(ctx context.Context, name, actor string) error {
	err := k.store.Update(ctx, func(tx *store.Tx) error {
		if err := tx.DeleteKey(name); err != nil {
			return err
		}
		return tx.Audit(actor, "key.revoke", name, "")
	})
	if err == nil {
		k.used.Delete(name)
	}
	return err
}

func checkKey(name string, scopes []string) error {
	if !keyName.MatchString(name) {
		return fmt.Errorf("%w：名字只能用小写字母、数字和横线", ErrInvalidKey)
	}
	if len(scopes) == 0 {
		return fmt.Errorf("%w：至少选一个权限", ErrInvalidKey)
	}
	for _, s := range scopes {
		if !slices.Contains(Scopes, s) {
			return fmt.Errorf("%w：没有 %q 这个权限", ErrInvalidKey, s)
		}
	}
	return nil
}

// sorted puts scopes in the order of Scopes and drops repeats.
func sorted(scopes []string) []string {
	out := []string{}
	for _, s := range Scopes {
		if slices.Contains(scopes, s) {
			out = append(out, s)
		}
	}
	return out
}
