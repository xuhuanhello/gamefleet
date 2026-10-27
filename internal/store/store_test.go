package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gamefleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReopenKeepsDataAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamefleet.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Tx) error {
		_, err := tx.InsertKey("ci", []byte("0123456789abcdef0123456789abcdef"), []string{"push"}, time.Time{}, "admin")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.View(ctx, func(tx *Tx) error {
		k, err := tx.KeyByHash([]byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			return err
		}
		if k.Name != "ci" || len(k.Scopes) != 1 || k.Scopes[0] != "push" || k.CreatedBy != "admin" {
			t.Errorf("key = %+v", k)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDuplicatesAndMissingRows(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	err := s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.InsertKey("ci", []byte("a"), []string{"push"}, time.Time{}, "admin"); err != nil {
			return err
		}
		if _, err := tx.InsertKey("ci", []byte("b"), []string{"push"}, time.Time{}, "admin"); !errors.Is(err, ErrDuplicate) {
			t.Errorf("same name: %v", err)
		}
		if _, err := tx.InsertKey("other", []byte("a"), []string{"push"}, time.Time{}, "admin"); !errors.Is(err, ErrDuplicate) {
			t.Errorf("same hash: %v", err)
		}
		if err := tx.DeleteKey("nobody"); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete missing: %v", err)
		}
		if _, err := tx.Build(9); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing build: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOnlyOneLiveDeploymentPerBuildAndCluster(t *testing.T) {
	s := open(t)
	err := s.Update(context.Background(), func(tx *Tx) error {
		b, err := tx.InsertBuild(Build{Game: "billiards", Digest: "sha256:1", Compat: "net7",
			Labels: map[string]string{"org.opencontainers.image.version": "42"}, Port: 7770, Protocol: "UDP"})
		if err != nil {
			return err
		}
		if b.ID != 1 || b.Version() != "42" {
			t.Errorf("build = %+v", b)
		}
		d := Deployment{BuildID: b.ID, Cluster: "us-1", Fleet: "billiards-1", Buffer: 2, MaxReplicas: 10, State: StateNew}
		first, err := tx.InsertDeployment(d)
		if err != nil {
			return err
		}
		if _, err := tx.InsertDeployment(d); !errors.Is(err, ErrDuplicate) {
			t.Errorf("second live deployment: %v", err)
		}
		if err := tx.SetDeploymentState(first.ID, StateDeleted); err != nil {
			return err
		}
		if _, err := tx.InsertDeployment(d); err != nil {
			t.Errorf("deploy again after delete: %v", err)
		}
		live, err := tx.Deployments()
		if err != nil {
			return err
		}
		all, err := tx.DeploymentsOf(b.ID)
		if err != nil {
			return err
		}
		if len(live) != 1 || len(all) != 2 {
			t.Errorf("live %d, all %d", len(live), len(all))
		}
		last, err := tx.LastDeployment("billiards", "us-1")
		if err != nil || last.ID != all[0].ID {
			t.Errorf("last = %+v, %v", last, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoutesKeepThePreviousBuild(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return at })
	ctx := context.Background()
	err := s.Update(ctx, func(tx *Tx) error {
		for _, digest := range []string{"sha256:1", "sha256:2"} {
			if _, err := tx.InsertBuild(Build{Game: "billiards", Digest: digest, Compat: "net7", Labels: map[string]string{}}); err != nil {
				return err
			}
		}
		if err := tx.PutRoute(Route{Game: "billiards", Channel: "stable", Compat: "net7", BuildID: 1, UpdatedBy: "admin"}); err != nil {
			return err
		}
		return tx.PutRoute(Route{Game: "billiards", Channel: "stable", Compat: "net7", BuildID: 2, PreviousBuildID: 1, UpdatedBy: "admin"})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.View(ctx, func(tx *Tx) error {
		r, err := tx.Route("billiards", "stable", "net7")
		if err != nil {
			return err
		}
		if r.BuildID != 2 || r.PreviousBuildID != 1 || !r.Updated.Equal(at) {
			t.Errorf("route = %+v", r)
		}
		routes, err := tx.Routes("")
		if err != nil || len(routes) != 1 {
			t.Errorf("routes = %v, %v", routes, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogPages(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	err := s.Update(ctx, func(tx *Tx) error {
		for _, action := range []string{"one", "two", "three"} {
			if err := tx.Audit("admin", action, "x", ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.View(ctx, func(tx *Tx) error {
		page, err := tx.AuditLog(0, 2)
		if err != nil {
			return err
		}
		if len(page) != 2 || page[0].Action != "three" || page[1].Action != "two" {
			t.Errorf("first page = %+v", page)
		}
		rest, err := tx.AuditLog(page[1].ID, 2)
		if err != nil {
			return err
		}
		if len(rest) != 1 || rest[0].Action != "one" {
			t.Errorf("second page = %+v", rest)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClustersAndVars(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	err := s.Update(ctx, func(tx *Tx) error {
		c := Cluster{Name: "us-1", Region: "us", Server: "https://10.0.0.1:6443", CA: "pem", Token: "t", Namespace: "games",
			JoinURL: "https://10.0.0.1:6443", JoinToken: "j", CreatedBy: "admin"}
		if _, err := tx.InsertCluster(c); err != nil {
			return err
		}
		c.Name = "us-2"
		if _, err := tx.InsertCluster(c); !errors.Is(err, ErrDuplicate) {
			t.Errorf("second cluster for a region: %v", err)
		}
		if err := tx.PutVar(Var{Game: "billiards", Name: "NAKAMA_URL", Value: "a", UpdatedBy: "admin"}); err != nil {
			return err
		}
		if err := tx.PutVar(Var{Game: "billiards", Name: "NAKAMA_URL", Value: "b", UpdatedBy: "admin"}); err != nil {
			return err
		}
		if err := tx.DeleteVar("billiards", "NOPE"); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete missing var: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.View(ctx, func(tx *Tx) error {
		clusters, err := tx.Clusters()
		if err != nil || len(clusters) != 1 || clusters[0].JoinToken != "j" {
			t.Errorf("clusters = %+v, %v", clusters, err)
		}
		vars, err := tx.Vars("billiards")
		if err != nil || len(vars) != 1 || vars[0].Value != "b" {
			t.Errorf("vars = %+v, %v", vars, err)
		}
		return nil
	})
}

func runtimeGames(t *testing.T, s *Store) []string {
	t.Helper()
	var games []string
	if err := s.View(context.Background(), func(tx *Tx) error {
		var err error
		games, err = tx.RuntimeGames()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return games
}

// A game stays among those with runtime settings once it had one, also after
// its last variable is deleted: its Secret must be emptied, build or not.
func TestAGameKeepsItsRuntimeSecretAfterItsLastVariable(t *testing.T) {
	s := open(t)
	err := s.Update(context.Background(), func(tx *Tx) error {
		if err := tx.PutVar(Var{Game: "chess", Name: "A", Value: "1", UpdatedBy: "admin"}); err != nil {
			return err
		}
		return tx.DeleteVar("chess", "A")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeGames(t, s); !slices.Equal(got, []string{"chess"}) {
		t.Fatalf("runtime games = %v", got)
	}
}

// Opening a database of an earlier GameFleet finds every game that had
// runtime settings: those with variables, and, from the audit log, those
// whose variables were all deleted before.
func TestUpgradeFindsEveryGameThatHadRuntimeSettings(t *testing.T) {
	s := upgradeFrom(t, "CREATE TABLE runtime_games",
		`INSERT INTO runtime_vars (game, name, value, updated_at, updated_by) VALUES ('billiards', 'A', '1', 0, 'admin')`,
		`INSERT INTO audit_log (at, actor, action, target, detail) VALUES
			(0, 'admin', 'runtime.set', 'chess A', ''), (0, 'admin', 'runtime.delete', 'chess A', ''),
			(0, 'admin', 'runtime.delete', 'go B', ''), (0, 'admin', 'route.set', 'poker stable net7', '')`)
	if got := runtimeGames(t, s); !slices.Equal(got, []string{"billiards", "chess", "go"}) {
		t.Fatalf("runtime games = %v", got)
	}
}

// Opening a database of an earlier GameFleet leaves each deployment able to
// run as many matches per game server as before, and no more: its game
// servers' rooms lists hold that many.
func TestUpgradeKeepsWhatEachGameServerHolds(t *testing.T) {
	s := upgradeFrom(t, "rooms_capacity",
		`INSERT INTO builds (id, game, digest, tag, compat, labels, port, protocol, note, registered_at, registered_by)
			VALUES (1, 'billiards', 'sha256:aaa', 'v1', 'net7', '{}', 7770, 'UDP', '', 0, 'ci')`,
		`INSERT INTO deployments (build_id, cluster, fleet, buffer, max_replicas, rooms, cpu_request, cpu_limit,
			memory_request, memory_limit, state, state_since, created_at, created_by)
			VALUES (1, 'us-1', 'billiards-1', 1, 10, 50, 250, 2000, 256, 1024, 'serving', 0, 0, 'admin')`)
	var d Deployment
	err := s.View(context.Background(), func(tx *Tx) error {
		var err error
		d, err = tx.Deployment(1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Rooms != 50 || d.RoomsCapacity != 50 {
		t.Fatalf("rooms = %d of %d, want 50 of 50", d.Rooms, d.RoomsCapacity)
	}
}

// upgradeFrom opens a database that the GameFleet before the migration
// containing text left, with rows the statements add.
func upgradeFrom(t *testing.T, migration string, statements ...string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gamefleet.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	before := slices.IndexFunc(schema, func(m string) bool { return strings.Contains(m, migration) })
	if before < 0 {
		t.Fatalf("no migration contains %q", migration)
	}
	for _, m := range schema[:before] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range append([]string{fmt.Sprintf("PRAGMA user_version = %d", before)}, statements...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A daily backup copies the database through a connection of its own, while
// GameFleet keeps its connection; it checks the copy, and keeps the newest
// DailyBackups of them.
func TestDailyBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gamefleet.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Tx) error { return tx.Audit("admin", "test", "x", "") }); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	start := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	var last string
	for i := range DailyBackups + 3 {
		last, err = DailyBackup(ctx, path, backups, start.Add(time.Duration(i)*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
	}
	if filepath.Base(last) != "gamefleet-daily-20261112-030000.db" {
		t.Fatalf("newest backup = %s", last)
	}
	// An update's backup is not one of the daily ones and stays.
	if err := os.WriteFile(filepath.Join(backups, "gamefleet-2.1.0-20261027-101010.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DailyBackup(ctx, path, backups, start.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(backups)
	var daily, other int
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "gamefleet-daily-") && strings.HasSuffix(e.Name(), ".db"):
			daily++
		case strings.HasPrefix(e.Name(), "."):
			t.Errorf("a half-written backup is left: %s", e.Name())
		default:
			other++
		}
	}
	if daily != DailyBackups || other != 1 {
		t.Fatalf("%d daily backups and %d others, want %d and 1", daily, other, DailyBackups)
	}
	// The copy is a database with what GameFleet wrote.
	copied, err := Open(last)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var log []Entry
	copied.View(ctx, func(tx *Tx) error { log, err = tx.AuditLog(0, 10); return err })
	if len(log) != 1 || log[0].Action != "test" {
		t.Fatalf("audit log of the copy = %+v", log)
	}
	if info, err := os.Stat(last); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, %v; want 0600", info.Mode(), err)
	}
	newest, at, err := NewestDailyBackup(backups)
	if err != nil || filepath.Base(newest) != "gamefleet-daily-20261126-030000.db" || at.IsZero() {
		t.Fatalf("newest = %s %v %v", newest, at, err)
	}
}

func TestLastOffsite(t *testing.T) {
	dir := t.TempDir()
	if at, err := LastOffsite(dir); err != nil || !at.IsZero() {
		t.Fatalf("no marker: %v, %v", at, err)
	}
	when := time.Date(2026, 10, 27, 4, 50, 0, 0, time.UTC)
	path := filepath.Join(dir, OffsiteMarker)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, when, when)
	if at, err := LastOffsite(dir); err != nil || !at.Equal(when) {
		t.Fatalf("marker: %v, %v", at, err)
	}
}
