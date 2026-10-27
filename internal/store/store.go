// Package store keeps what Kubernetes does not know: clusters, API keys,
// builds, deployments, routes, game server settings and the audit log. Everything is in one SQLite file
// that only GameFleet opens. The rules for changing these records live in the
// packages that use them; this package only reads and writes rows.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrNotFound means the record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrDuplicate means a name, key or live deployment already exists.
	ErrDuplicate = errors.New("already exists")
)

// Deployment states, see docs/releases.md.
const (
	StateNew      = "new"      // deployed, never routed: keeps its buffer
	StateServing  = "serving"  // a route points at its build: keeps its buffer
	StateDraining = "draining" // no route points at it any more: scaled to 0
	StateDeleted  = "deleted"  // its Fleet is gone or going
)

// Key is a machine credential. Only the SHA-256 of the key is stored.
type Key struct {
	ID        int64
	Name      string
	Scopes    []string
	Created   time.Time
	CreatedBy string
}

// Build is one image that CI pushed and registered.
type Build struct {
	ID     int64
	Game   string // the repository name, for example billiards
	Digest string // sha256:… of the manifest or index that was pushed
	Tag    string
	Compat string // label io.gamefleet.compat
	Labels map[string]string
	// Port and Protocol come from the image's EXPOSE.
	Port         int
	Protocol     string
	Note         string
	Registered   time.Time
	RegisteredBy string
}

// Version is the build's org.opencontainers.image.version label, or its tag.
func (b Build) Version() string {
	if v := b.Labels["org.opencontainers.image.version"]; v != "" {
		return v
	}
	return b.Tag
}

// Resources are a game server container's CPU (millicores) and memory (MiB).
type Resources struct {
	CPURequest    int
	CPULimit      int
	MemoryRequest int
	MemoryLimit   int
}

// Deployment is one build running as one Fleet in one cluster.
type Deployment struct {
	ID          int64
	BuildID     int64
	Cluster     string
	Fleet       string
	Buffer      int
	MaxReplicas int
	// Rooms is how many matches one game server runs at once.
	Rooms     int
	Resources Resources
	State     string
	Since     time.Time // when it entered State
	Created   time.Time
	CreatedBy string
}

// Route sends one (game, channel, compat) to a build.
type Route struct {
	Game    string
	Channel string
	Compat  string
	BuildID int64
	// PreviousBuildID is where the route pointed before, 0 for none. Rolling
	// back points the route there again.
	PreviousBuildID int64
	Updated         time.Time
	UpdatedBy       string
}

// Entry is one line of the audit log.
type Entry struct {
	ID     int64
	At     time.Time
	Actor  string // console user, "key:<name>" or "gamefleet"
	Action string
	Target string
	Detail string
}

// Store is an open database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens or creates the database at path and brings its schema up to date.
func Open(path string) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: GameFleet writes rarely, and a single writer never
	// waits on itself.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Backup writes a consistent copy of the database to path, which must not
// exist yet. GameFleet keeps running meanwhile.
func (s *Store) Backup(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return err
	}
	// It holds every cluster's token: for GameFleet's user only.
	return os.Chmod(path, 0o600)
}

// SetClock replaces the clock; tests use it.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Now is the store's clock.
func (s *Store) Now() time.Time { return s.now() }

// schema lists the migrations in order. PRAGMA user_version counts how many
// have run. Only ever append.
var schema = []string{`
CREATE TABLE api_keys (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	sha256     BLOB NOT NULL UNIQUE,
	scopes     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL
);
CREATE TABLE builds (
	id            INTEGER PRIMARY KEY,
	game          TEXT NOT NULL,
	digest        TEXT NOT NULL,
	tag           TEXT NOT NULL,
	compat        TEXT NOT NULL,
	labels        TEXT NOT NULL,
	port          INTEGER NOT NULL,
	protocol      TEXT NOT NULL,
	note          TEXT NOT NULL,
	registered_at INTEGER NOT NULL,
	registered_by TEXT NOT NULL,
	UNIQUE (game, digest)
);
CREATE TABLE deployments (
	id             INTEGER PRIMARY KEY,
	build_id       INTEGER NOT NULL REFERENCES builds (id),
	cluster        TEXT NOT NULL,
	fleet          TEXT NOT NULL,
	buffer         INTEGER NOT NULL,
	max_replicas   INTEGER NOT NULL,
	cpu_request    INTEGER NOT NULL,
	cpu_limit      INTEGER NOT NULL,
	memory_request INTEGER NOT NULL,
	memory_limit   INTEGER NOT NULL,
	state          TEXT NOT NULL CHECK (state IN ('new', 'serving', 'draining', 'deleted')),
	state_since    INTEGER NOT NULL,
	created_at     INTEGER NOT NULL,
	created_by     TEXT NOT NULL
);
CREATE UNIQUE INDEX deployments_live ON deployments (cluster, build_id) WHERE state <> 'deleted';
CREATE TABLE routes (
	game              TEXT NOT NULL,
	channel           TEXT NOT NULL,
	compat            TEXT NOT NULL,
	build_id          INTEGER NOT NULL REFERENCES builds (id),
	previous_build_id INTEGER REFERENCES builds (id),
	updated_at        INTEGER NOT NULL,
	updated_by        TEXT NOT NULL,
	PRIMARY KEY (game, channel, compat)
);
CREATE TABLE audit_log (
	id     INTEGER PRIMARY KEY,
	at     INTEGER NOT NULL,
	actor  TEXT NOT NULL,
	action TEXT NOT NULL,
	target TEXT NOT NULL,
	detail TEXT NOT NULL
);
`, `
CREATE TABLE clusters (
	name       TEXT PRIMARY KEY,
	region     TEXT NOT NULL UNIQUE,
	server     TEXT NOT NULL,
	ca         TEXT NOT NULL,
	token      TEXT NOT NULL,
	namespace  TEXT NOT NULL,
	join_url   TEXT NOT NULL,
	join_token TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL
);
CREATE TABLE runtime_vars (
	game       TEXT NOT NULL,
	name       TEXT NOT NULL,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	updated_by TEXT NOT NULL,
	PRIMARY KEY (game, name)
);
`, `
-- Game servers run several matches each. Deployments made before ran one.
ALTER TABLE deployments ADD COLUMN rooms INTEGER NOT NULL DEFAULT 1;
`, `
-- Every game that ever had a runtime setting: its Secret is written, empty
-- once its last setting is gone, whether or not it has a build. The games
-- whose settings were all deleted before come from the audit log, where a
-- setting's target is "<game> <name>".
CREATE TABLE runtime_games (game TEXT PRIMARY KEY);
INSERT OR IGNORE INTO runtime_games (game) SELECT game FROM runtime_vars;
INSERT OR IGNORE INTO runtime_games (game)
	SELECT substr(target, 1, instr(target, ' ') - 1) FROM audit_log
	WHERE action IN ('runtime.set', 'runtime.delete') AND instr(target, ' ') > 1;
`}

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(schema) {
		return fmt.Errorf("schema version %d is newer than this GameFleet (%d)", version, len(schema))
	}
	for i := version; i < len(schema); i++ {
		if _, err := tx.Exec(schema[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(schema))); err != nil {
		return err
	}
	return tx.Commit()
}

// Tx is one transaction. Reads see the transaction's own writes.
type Tx struct {
	tx  *sql.Tx
	ctx context.Context
	now time.Time
}

// Now is the time the transaction started; every write in it uses this time.
func (t *Tx) Now() time.Time { return t.now }

// Update runs fn in a transaction and commits it when fn returns nil.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&Tx{tx: tx, ctx: ctx, now: s.now()}); err != nil {
		return err
	}
	return tx.Commit()
}

// View runs fn in a read-only transaction.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(&Tx{tx: tx, ctx: ctx, now: s.now()})
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms) }

// --- API keys

// KeyByHash returns the key whose SHA-256 is sum.
func (t *Tx) KeyByHash(sum []byte) (Key, error) {
	return scanKey(t.tx.QueryRowContext(t.ctx,
		`SELECT id, name, scopes, created_at, created_by FROM api_keys WHERE sha256 = ?`, sum))
}

// Keys lists all keys by name.
func (t *Tx) Keys() ([]Key, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT id, name, scopes, created_at, created_by FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanKey)
}

// InsertKey stores a key. Names and hashes are unique.
func (t *Tx) InsertKey(name string, sum []byte, scopes []string, by string) (Key, error) {
	res, err := t.tx.ExecContext(t.ctx,
		`INSERT INTO api_keys (name, sha256, scopes, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		name, sum, strings.Join(scopes, " "), millis(t.now), by)
	if err != nil {
		return Key{}, duplicate(err)
	}
	id, _ := res.LastInsertId()
	return Key{ID: id, Name: name, Scopes: scopes, Created: t.now, CreatedBy: by}, nil
}

// DeleteKey removes the key with this name.
func (t *Tx) DeleteKey(name string) error {
	return expectOne(t.tx.ExecContext(t.ctx, `DELETE FROM api_keys WHERE name = ?`, name))
}

func scanKey(row scanner) (Key, error) {
	var k Key
	var scopes string
	var created int64
	if err := row.Scan(&k.ID, &k.Name, &scopes, &created, &k.CreatedBy); err != nil {
		return Key{}, notFound(err)
	}
	k.Scopes = strings.Fields(scopes)
	k.Created = fromMillis(created)
	return k, nil
}

// --- Builds

const buildColumns = `id, game, digest, tag, compat, labels, port, protocol, note, registered_at, registered_by`

// Build returns the build with this number.
func (t *Tx) Build(id int64) (Build, error) {
	return scanBuild(t.tx.QueryRowContext(t.ctx, `SELECT `+buildColumns+` FROM builds WHERE id = ?`, id))
}

// BuildByDigest returns the game's build with this digest.
func (t *Tx) BuildByDigest(game, digest string) (Build, error) {
	return scanBuild(t.tx.QueryRowContext(t.ctx,
		`SELECT `+buildColumns+` FROM builds WHERE game = ? AND digest = ?`, game, digest))
}

// Builds lists builds, newest first. An empty game means every game.
func (t *Tx) Builds(game string, limit int) ([]Build, error) {
	rows, err := t.tx.QueryContext(t.ctx,
		`SELECT `+buildColumns+` FROM builds WHERE ? = '' OR game = ? ORDER BY id DESC LIMIT ?`, game, game, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanBuild)
}

// InsertBuild stores a build and fills in its number and registration time.
func (t *Tx) InsertBuild(b Build) (Build, error) {
	labels, err := json.Marshal(b.Labels)
	if err != nil {
		return Build{}, err
	}
	b.Registered = t.now
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO builds (game, digest, tag, compat, labels, port, protocol, note, registered_at, registered_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.Game, b.Digest, b.Tag, b.Compat, string(labels), b.Port, b.Protocol, b.Note, millis(b.Registered), b.RegisteredBy)
	if err != nil {
		return Build{}, duplicate(err)
	}
	b.ID, _ = res.LastInsertId()
	return b, nil
}

// SetBuildNote replaces the build's note.
func (t *Tx) SetBuildNote(id int64, note string) error {
	return expectOne(t.tx.ExecContext(t.ctx, `UPDATE builds SET note = ? WHERE id = ?`, note, id))
}

func scanBuild(row scanner) (Build, error) {
	var b Build
	var labels string
	var registered int64
	if err := row.Scan(&b.ID, &b.Game, &b.Digest, &b.Tag, &b.Compat, &labels, &b.Port, &b.Protocol, &b.Note,
		&registered, &b.RegisteredBy); err != nil {
		return Build{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(labels), &b.Labels); err != nil {
		return Build{}, fmt.Errorf("build %d: labels: %w", b.ID, err)
	}
	b.Registered = fromMillis(registered)
	return b, nil
}

// --- Deployments

const deploymentColumns = `id, build_id, cluster, fleet, buffer, max_replicas, rooms, cpu_request, cpu_limit,
	memory_request, memory_limit, state, state_since, created_at, created_by`

// Deployment returns the deployment with this id.
func (t *Tx) Deployment(id int64) (Deployment, error) {
	return scanDeployment(t.tx.QueryRowContext(t.ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id = ?`, id))
}

// Deployments lists deployments that are not deleted, oldest first.
func (t *Tx) Deployments() ([]Deployment, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE state <> 'deleted' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDeployment)
}

// DeploymentsOf lists every deployment of a build, deleted ones too, newest first.
func (t *Tx) DeploymentsOf(buildID int64) ([]Deployment, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE build_id = ? ORDER BY id DESC`, buildID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDeployment)
}

// LastDeployment returns the newest deployment of the game in the cluster,
// deleted ones included. Its settings are the defaults for the next one.
func (t *Tx) LastDeployment(game, cluster string) (Deployment, error) {
	return scanDeployment(t.tx.QueryRowContext(t.ctx, `SELECT `+prefixed("d.", deploymentColumns)+`
		FROM deployments d JOIN builds b ON b.id = d.build_id
		WHERE b.game = ? AND d.cluster = ? ORDER BY d.id DESC LIMIT 1`, game, cluster))
}

// InsertDeployment stores a deployment and fills in its id and times.
func (t *Tx) InsertDeployment(d Deployment) (Deployment, error) {
	d.Created, d.Since = t.now, t.now
	r := d.Resources
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO deployments (build_id, cluster, fleet, buffer, max_replicas, rooms,
		cpu_request, cpu_limit, memory_request, memory_limit, state, state_since, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.BuildID, d.Cluster, d.Fleet, d.Buffer, d.MaxReplicas, d.Rooms, r.CPURequest, r.CPULimit, r.MemoryRequest, r.MemoryLimit,
		d.State, millis(d.Since), millis(d.Created), d.CreatedBy)
	if err != nil {
		return Deployment{}, duplicate(err)
	}
	d.ID, _ = res.LastInsertId()
	return d, nil
}

// SetDeploymentState moves the deployment to state as of the transaction's time.
func (t *Tx) SetDeploymentState(id int64, state string) error {
	return expectOne(t.tx.ExecContext(t.ctx,
		`UPDATE deployments SET state = ?, state_since = ? WHERE id = ?`, state, millis(t.now), id))
}

// SetDeploymentBuffer changes how many idle game servers it keeps and its ceiling.
func (t *Tx) SetDeploymentBuffer(id int64, buffer, maxReplicas int) error {
	return expectOne(t.tx.ExecContext(t.ctx,
		`UPDATE deployments SET buffer = ?, max_replicas = ? WHERE id = ?`, buffer, maxReplicas, id))
}

func scanDeployment(row scanner) (Deployment, error) {
	var d Deployment
	var since, created int64
	r := &d.Resources
	if err := row.Scan(&d.ID, &d.BuildID, &d.Cluster, &d.Fleet, &d.Buffer, &d.MaxReplicas, &d.Rooms,
		&r.CPURequest, &r.CPULimit, &r.MemoryRequest, &r.MemoryLimit, &d.State, &since, &created, &d.CreatedBy); err != nil {
		return Deployment{}, notFound(err)
	}
	d.Since, d.Created = fromMillis(since), fromMillis(created)
	return d, nil
}

// --- Routes

const routeColumns = `game, channel, compat, build_id, COALESCE(previous_build_id, 0), updated_at, updated_by`

// Routes lists routes by game, channel and compat. An empty game means every game.
func (t *Tx) Routes(game string) ([]Route, error) {
	rows, err := t.tx.QueryContext(t.ctx,
		`SELECT `+routeColumns+` FROM routes WHERE ? = '' OR game = ? ORDER BY game, channel, compat`, game, game)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRoute)
}

// Route returns one route.
func (t *Tx) Route(game, channel, compat string) (Route, error) {
	return scanRoute(t.tx.QueryRowContext(t.ctx,
		`SELECT `+routeColumns+` FROM routes WHERE game = ? AND channel = ? AND compat = ?`, game, channel, compat))
}

// PutRoute creates or replaces a route as of the transaction's time.
func (t *Tx) PutRoute(r Route) error {
	var previous any
	if r.PreviousBuildID != 0 {
		previous = r.PreviousBuildID
	}
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO routes (game, channel, compat, build_id, previous_build_id, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (game, channel, compat) DO UPDATE SET build_id = excluded.build_id,
			previous_build_id = excluded.previous_build_id, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		r.Game, r.Channel, r.Compat, r.BuildID, previous, millis(t.now), r.UpdatedBy)
	return err
}

// DeleteRoute removes a route.
func (t *Tx) DeleteRoute(game, channel, compat string) error {
	return expectOne(t.tx.ExecContext(t.ctx,
		`DELETE FROM routes WHERE game = ? AND channel = ? AND compat = ?`, game, channel, compat))
}

func scanRoute(row scanner) (Route, error) {
	var r Route
	var updated int64
	if err := row.Scan(&r.Game, &r.Channel, &r.Compat, &r.BuildID, &r.PreviousBuildID, &updated, &r.UpdatedBy); err != nil {
		return Route{}, notFound(err)
	}
	r.Updated = fromMillis(updated)
	return r, nil
}

// --- Clusters

// Cluster is how GameFleet reaches one K3s cluster and how machines join it.
type Cluster struct {
	Name      string
	Region    string
	Server    string // https://host:6443
	CA        string // PEM
	Token     string // GameFleet's ServiceAccount token
	Namespace string // where game servers run
	// JoinURL and JoinToken are what a new machine needs to join as an agent.
	JoinURL   string
	JoinToken string
	Created   time.Time
	CreatedBy string
}

const clusterColumns = `name, region, server, ca, token, namespace, join_url, join_token, created_at, created_by`

// Clusters lists the clusters by name.
func (t *Tx) Clusters() ([]Cluster, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT `+clusterColumns+` FROM clusters ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanCluster)
}

// InsertCluster stores a cluster. Names and regions are unique.
func (t *Tx) InsertCluster(c Cluster) (Cluster, error) {
	c.Created = t.now
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO clusters (`+clusterColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Region, c.Server, c.CA, c.Token, c.Namespace, c.JoinURL, c.JoinToken, millis(c.Created), c.CreatedBy)
	if err != nil {
		return Cluster{}, duplicate(err)
	}
	return c, nil
}

// UpdateCluster replaces how GameFleet reaches a stored cluster: its server,
// CA, token, namespace and what new machines join with.
func (t *Tx) UpdateCluster(c Cluster) error {
	return expectOne(t.tx.ExecContext(t.ctx, `UPDATE clusters SET server = ?, ca = ?, token = ?, namespace = ?, join_url = ?, join_token = ?
		WHERE name = ?`, c.Server, c.CA, c.Token, c.Namespace, c.JoinURL, c.JoinToken, c.Name))
}

func scanCluster(row scanner) (Cluster, error) {
	var c Cluster
	var created int64
	if err := row.Scan(&c.Name, &c.Region, &c.Server, &c.CA, &c.Token, &c.Namespace, &c.JoinURL, &c.JoinToken,
		&created, &c.CreatedBy); err != nil {
		return Cluster{}, notFound(err)
	}
	c.Created = fromMillis(created)
	return c, nil
}

// --- Runtime settings of game servers

// Var is one environment variable of a game's servers. Values can be
// secret: the console never shows them again.
type Var struct {
	Game, Name, Value string
	Updated           time.Time
	UpdatedBy         string
}

// Vars lists the variables of one game by name, or of every game when game is empty.
func (t *Tx) Vars(game string) ([]Var, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT game, name, value, updated_at, updated_by FROM runtime_vars
		WHERE ? = '' OR game = ? ORDER BY game, name`, game, game)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row scanner) (Var, error) {
		var v Var
		var updated int64
		if err := row.Scan(&v.Game, &v.Name, &v.Value, &updated, &v.UpdatedBy); err != nil {
			return Var{}, err
		}
		v.Updated = fromMillis(updated)
		return v, nil
	})
}

// RuntimeGames lists the games whose game servers load runtime settings, by
// name: every game with a build, or that ever had a variable.
func (t *Tx) RuntimeGames() ([]string, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT game FROM builds UNION SELECT game FROM runtime_games ORDER BY game`)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row scanner) (string, error) {
		var game string
		err := row.Scan(&game)
		return game, err
	})
}

// PutVar creates or replaces a variable as of the transaction's time, and
// counts its game among RuntimeGames for good.
func (t *Tx) PutVar(v Var) error {
	if _, err := t.tx.ExecContext(t.ctx, `INSERT OR IGNORE INTO runtime_games (game) VALUES (?)`, v.Game); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO runtime_vars (game, name, value, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (game, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		v.Game, v.Name, v.Value, millis(t.now), v.UpdatedBy)
	return err
}

// DeleteVar removes a variable.
func (t *Tx) DeleteVar(game, name string) error {
	return expectOne(t.tx.ExecContext(t.ctx, `DELETE FROM runtime_vars WHERE game = ? AND name = ?`, game, name))
}

// --- Audit log

// Audit appends an entry at the transaction's time.
func (t *Tx) Audit(actor, action, target, detail string) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO audit_log (at, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`,
		millis(t.now), actor, action, target, detail)
	return err
}

// AuditLog lists entries newest first, starting below id before (0 for the newest).
func (t *Tx) AuditLog(before int64, limit int) ([]Entry, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT id, at, actor, action, target, detail FROM audit_log
		WHERE ? = 0 OR id < ? ORDER BY id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row scanner) (Entry, error) {
		var e Entry
		var at int64
		if err := row.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return Entry{}, err
		}
		e.At = fromMillis(at)
		return e, nil
	})
}

// --- helpers

type scanner interface{ Scan(dest ...any) error }

func collect[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func duplicate(err error) error {
	var e *sqlite.Error
	if errors.As(err, &e) && (e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return ErrDuplicate
	}
	return err
}

func expectOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func prefixed(prefix, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
