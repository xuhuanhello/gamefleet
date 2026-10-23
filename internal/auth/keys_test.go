package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xuhuanhello/gamefleet/internal/store"
)

func TestKeysByValueAndByHash(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gamefleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys := NewKeys(db)
	ctx := context.Background()

	value, err := keys.Create(ctx, "ci", []string{ScopePush, ScopePull, ScopePush}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := keys.Check(ctx, value); !ok || c.Name != "ci" || len(c.Scopes) != 2 || !c.Has(ScopePull) || c.Actor() != "key:ci" {
		t.Fatalf("check = %+v, %v", c, ok)
	}

	elsewhere := "a-key-that-stays-on-the-business-machine"
	sum := sha256.Sum256([]byte(elsewhere))
	if err := keys.AddHash(ctx, "nakama", []string{ScopeAllocate}, hex.EncodeToString(sum[:]), "gamefleet key"); err != nil {
		t.Fatal(err)
	}
	if c, ok := keys.Check(ctx, elsewhere); !ok || c.Name != "nakama" || c.Has(ScopePush) {
		t.Fatalf("hashed key: %+v, %v", c, ok)
	}
	if err := keys.AddHash(ctx, "short", []string{ScopeGM}, "abcd", "x"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("short hash: %v", err)
	}
	for name, scopes := range map[string][]string{"Upper": {ScopeGM}, "noscope": nil, "badscope": {"admin"}} {
		if err := keys.Add(ctx, name, scopes, elsewhere+"-other", "x"); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := keys.Add(ctx, "copy", []string{ScopeGM}, elsewhere, "x"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("same value twice: %v", err)
	}

	if err := keys.Revoke(ctx, "ci", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.Check(ctx, value); ok {
		t.Fatal("revoked key still works")
	}
	if _, ok := keys.Check(ctx, ""); ok {
		t.Fatal("empty key accepted")
	}
}
