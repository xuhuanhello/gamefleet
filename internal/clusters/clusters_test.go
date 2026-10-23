package clusters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

type fixture struct {
	svc   *Service
	store *store.Store
	keys  *auth.Keys
	dir   *sites.Directory
	// fakes are the clusters connect hands out, by name.
	fakes map[string]*clustertest.Fake
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "gamefleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{store: db, keys: auth.NewKeys(db), dir: sites.New(), fakes: map[string]*clustertest.Fake{},
		now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	connect := func(c store.Cluster) (cluster.API, error) {
		fake, ok := f.fakes[c.Name]
		if !ok {
			fake = clustertest.New(c.Name, 0)
			f.fakes[c.Name] = fake
		}
		return fake, nil
	}
	f.svc = New(db, f.dir, f.keys, connect, "fleet.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.svc.now = func() time.Time { return f.now }
	return f
}

func connection(name, region string) *Connection {
	ca := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	return &Connection{Name: name, Region: region, Server: "https://203.0.113.7:6443", CABase64: ca, Token: "sa-token",
		Namespace: Namespace, JoinURL: "https://203.0.113.7:6443", JoinToken: "agent-token"}
}

func TestJoinAClusterWithACode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.svc.SetVar(ctx, "billiards", "NAKAMA_URL", "https://api.example.com", "admin"); err != nil {
		t.Fatal(err)
	}
	j, err := f.svc.StartJoin(ctx, "sg-1", "sg", false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.svc.InstallCommand(j), "https://fleet.example.com/install/cluster.sh") ||
		!strings.HasSuffix(f.svc.InstallCommand(j), "--code "+j.Code) {
		t.Errorf("install command = %s", f.svc.InstallCommand(j))
	}
	// A cluster whose first machine only runs the control plane says so.
	if other, _ := f.svc.StartJoin(ctx, "us-2", "us", true, "admin"); !strings.HasSuffix(f.svc.InstallCommand(other), " --control-plane-only") {
		t.Errorf("control plane only: %s", f.svc.InstallCommand(other))
	}
	for _, stage := range []string{StageK3s, StageAgones} {
		if err := f.svc.Report(ctx, j.Code, "203.0.113.7", Report{Stage: stage}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := f.svc.JoinByID(j.ID); got.Stage != StageAgones || got.IP != "203.0.113.7" {
		t.Fatalf("join = %+v", got)
	}

	// The firewall still blocks 6443: GameFleet says so and the script retries.
	f.fakes["sg-1"] = clustertest.New("sg-1", 0)
	f.fakes["sg-1"].Fail(cluster.ErrUnavailable)
	err = f.svc.Report(ctx, j.Code, "203.0.113.7", Report{Stage: StageConnect, Connection: connection("sg-1", "sg")})
	if err == nil || !strings.Contains(err.Error(), "6443") {
		t.Fatalf("unreachable cluster: %v", err)
	}
	if got, _ := f.svc.JoinByID(j.ID); got.Stage != StageConnect || !strings.Contains(got.Message, "6443") {
		t.Fatalf("join after a failed connect = %+v", got)
	}
	f.fakes["sg-1"].Fail(nil)
	if err := f.svc.Report(ctx, j.Code, "203.0.113.7", Report{Stage: StageConnect, Connection: connection("sg-1", "sg")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.JoinByID(j.ID); got.Stage != StageDone {
		t.Fatalf("join = %+v", got)
	}
	site, ok := f.dir.Get("sg-1")
	if !ok || site.Region != "sg" {
		t.Fatalf("directory = %+v", f.dir.All())
	}

	// The cluster pulls with its own key and has the game's settings.
	pull, ok := f.fakes["sg-1"].Secret(cluster.PullSecretName)
	if !ok || pull.Type != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("pull secret = %+v", pull)
	}
	var config struct {
		Auths map[string]struct{ Password string } `json:"auths"`
	}
	json.Unmarshal(pull.Data[".dockerconfigjson"], &config)
	caller, ok := f.keys.Check(ctx, config.Auths["fleet.example.com"].Password)
	if !ok || caller.Name != "pull-sg-1" || !caller.Has(auth.ScopePull) || caller.Has(auth.ScopePush) {
		t.Fatalf("pull key = %+v, %v", caller, ok)
	}
	if runtime, ok := f.fakes["sg-1"].Secret("billiards-runtime"); !ok || string(runtime.Data["NAKAMA_URL"]) != "https://api.example.com" {
		t.Fatalf("runtime secret = %+v", runtime)
	}

	// The code is spent; reporting again changes nothing.
	if err := f.svc.Report(ctx, j.Code, "203.0.113.7", Report{Stage: StageConnect, Connection: connection("sg-1", "sg")}); err != nil {
		t.Fatalf("report after done: %v", err)
	}
	// A second cluster may not take the region.
	if _, err := f.svc.StartJoin(ctx, "sg-2", "sg", false, "admin"); err == nil {
		t.Fatal("two clusters for one region")
	}
	// The stored cluster loads again after a restart.
	f.dir.Set(nil)
	if err := f.svc.Load(ctx); err != nil || len(f.dir.All()) != 1 {
		t.Fatalf("load: %v, %+v", err, f.dir.All())
	}
}

func TestJoinCodesExpireAndBindTheName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, bad := range [][2]string{{"SG", "sg"}, {"sg-1", "Sg"}, {"", "sg"}} {
		if _, err := f.svc.StartJoin(ctx, bad[0], bad[1], false, "admin"); err == nil {
			t.Errorf("StartJoin(%q, %q) accepted", bad[0], bad[1])
		}
	}
	j, _ := f.svc.StartJoin(ctx, "sg-1", "sg", false, "admin")
	if err := f.svc.Report(ctx, "guess", "1.2.3.4", Report{Stage: StageK3s}); !errors.Is(err, ErrBadCode) {
		t.Errorf("unknown code: %v", err)
	}
	if err := f.svc.Report(ctx, j.Code, "1.2.3.4", Report{Stage: StageConnect, Connection: connection("eu-1", "eu")}); err == nil {
		t.Error("a code for sg-1 added eu-1")
	}
	f.now = f.now.Add(JoinCodeTTL + time.Minute)
	if err := f.svc.Report(ctx, j.Code, "1.2.3.4", Report{Stage: StageK3s}); !errors.Is(err, ErrBadCode) {
		t.Errorf("expired code: %v", err)
	}
}

func TestRetireAMachine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	us := clustertest.New("us-1", 0)
	us.SetView(clustertest.View{Nodes: []cluster.Node{
		{Name: "mgr", Ready: true, Roles: []string{"control-plane"}},
		{Name: "worker", Ready: true},
	}})
	us.AddRoom(cluster.Room{ID: "billiards-1-a", State: "Allocated", Node: "worker", Capacity: 50, Matches: []cluster.Match{{ID: "m-1"}}})
	us.AddRoom(cluster.Room{ID: "billiards-1-b", State: "Ready", Node: "worker", Capacity: 50})
	us.AddRoom(cluster.Room{ID: "billiards-1-c", State: "Allocated", Node: "worker", Capacity: 50})
	f.dir.Add(sites.Site{Name: "us-1", Region: "us", Cluster: us})

	if err := f.svc.Retire(ctx, "us-1", "mgr", "admin"); err == nil {
		t.Fatal("the control plane was retired")
	}
	if err := f.svc.Retire(ctx, "us-1", "worker", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rooms, _ := us.Rooms(ctx)
	if len(rooms) != 1 || rooms[0].ID != "billiards-1-a" || !us.Retiring("billiards-1-a") || len(us.DeletedNodes()) != 0 {
		t.Fatalf("with a match running: rooms %+v, deleted %v", rooms, us.DeletedNodes())
	}
	// Changing one's mind works until the machine is gone.
	if err := f.svc.Keep(ctx, "us-1", "worker", "admin"); err != nil {
		t.Fatal(err)
	}
	if nodes, _ := us.Nodes(ctx); nodes[1].Unschedulable || !nodes[1].Retiring.IsZero() || us.Retiring("billiards-1-a") {
		t.Fatalf("kept node = %+v", nodes[1])
	}
	if err := f.svc.Retire(ctx, "us-1", "worker", "admin"); err != nil {
		t.Fatal(err)
	}
	us.ReleaseMatch("billiards-1-a", "m-1")
	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := us.DeletedNodes(); len(got) != 1 || got[0] != "worker" {
		t.Fatalf("deleted = %v", got)
	}

	var log []store.Entry
	_ = f.store.View(ctx, func(tx *store.Tx) error {
		var err error
		log, err = tx.AuditLog(0, 10)
		return err
	})
	var actions []string
	for _, e := range log {
		actions = append(actions, e.Actor+" "+e.Action)
	}
	want := "gamefleet node.remove|admin node.retire|admin node.keep|admin node.retire"
	if strings.Join(actions, "|") != want {
		t.Fatalf("audit = %v", actions)
	}
}

func TestRuntimeVars(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	us := clustertest.New("us-1", 0)
	f.dir.Add(sites.Site{Name: "us-1", Region: "us", Cluster: us})
	for _, bad := range [][2]string{{"Billiards", "A"}, {"billiards", "nakama_url"}, {"billiards", "1A"}} {
		if err := f.svc.SetVar(ctx, bad[0], bad[1], "v", "admin"); err == nil {
			t.Errorf("SetVar(%q, %q) accepted", bad[0], bad[1])
		}
	}
	f.svc.SetVar(ctx, "billiards", "NAKAMA_URL", "https://api.example.com", "admin")
	f.svc.SetVar(ctx, "billiards", "NAKAMA_HTTP_KEY", "secret", "admin")
	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s, ok := us.Secret("billiards-runtime")
	if !ok || len(s.Data) != 2 || string(s.Data["NAKAMA_HTTP_KEY"]) != "secret" {
		t.Fatalf("secret = %+v", s)
	}
	if err := f.svc.DeleteVar(ctx, "billiards", "NOPE", "admin"); err == nil {
		t.Error("deleting a missing variable")
	}
}
