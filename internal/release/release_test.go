package release

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/registry"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// images is a registry with images by tag.
type images map[string]registry.Image

func (i images) Inspect(_ context.Context, repo, ref string) (registry.Image, error) {
	for tag, img := range i {
		if ref == tag || ref == img.Digest {
			return img, nil
		}
	}
	return registry.Image{}, registry.ErrNotFound
}

func image(digest, compat, version string) registry.Image {
	return registry.Image{Digest: digest, Port: 7770, Protocol: "UDP", Labels: map[string]string{
		LabelCompat: compat, LabelVersion: version, LabelRevision: "abc",
	}}
}

type fixture struct {
	svc   *Service
	store *store.Store
	us    *clustertest.Fake
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "gamefleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fixture{store: s, us: clustertest.New("us-1", 0), now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s.SetClock(func() time.Time { return f.now })
	reg := images{
		"141": image("sha256:aaa", "net7", "141"),
		"142": image("sha256:bbb", "net7", "142"),
		"bad": {Digest: "sha256:ccc", Port: 7770, Protocol: "UDP", Labels: map[string]string{LabelCompat: "net7"}},
	}
	f.svc = New(s, reg, "fleet.example.com", sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: f.us}), 24*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

func (f *fixture) reconcile(t *testing.T) {
	t.Helper()
	if err := f.svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) register(t *testing.T, tag string) store.Build {
	t.Helper()
	b, _, err := f.svc.Register(context.Background(), Registration{Repository: "billiards", Tag: tag}, "key:ci")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fixture) deploy(t *testing.T, b store.Build) store.Deployment {
	t.Helper()
	req := DeployRequest{BuildID: b.ID, Cluster: "us-1", Buffer: 2, MaxReplicas: 10, Rooms: 50, Resources: Defaults.Resources}
	d, err := f.svc.Deploy(context.Background(), req, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fixture) state(t *testing.T, id int64) string {
	t.Helper()
	var d store.Deployment
	err := f.store.View(context.Background(), func(tx *store.Tx) error {
		var err error
		d, err = tx.Deployment(id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return d.State
}

func (f *fixture) resolve(t *testing.T, channel string) string {
	t.Helper()
	target, err := f.svc.Resolve(context.Background(), "billiards", channel, "net7", "us")
	if errors.Is(err, ErrNoRoute) {
		return "none"
	}
	if err != nil {
		t.Fatal(err)
	}
	return target.Fleet
}

func TestRegister(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, created, err := f.svc.Register(ctx, Registration{Repository: "billiards", Tag: "141", Note: "first"}, "key:ci")
	if err != nil || !created {
		t.Fatalf("register: %v, created %v", err, created)
	}
	if b.ID != 1 || b.Compat != "net7" || b.Version() != "141" || b.Port != 7770 || b.RegisteredBy != "key:ci" {
		t.Errorf("build = %+v", b)
	}
	again, created, err := f.svc.Register(ctx, Registration{Repository: "billiards", Digest: "sha256:aaa"}, "key:ci")
	if err != nil || created || again.ID != b.ID {
		t.Errorf("registering again: %+v, created %v, %v", again, created, err)
	}
	cases := map[string]struct {
		r    Registration
		want error
	}{
		"no reference":      {Registration{Repository: "billiards"}, ErrInvalid},
		"missing image":     {Registration{Repository: "billiards", Tag: "999"}, registry.ErrNotFound},
		"missing labels":    {Registration{Repository: "billiards", Tag: "bad"}, registry.ErrUnusable},
		"tag is another":    {Registration{Repository: "billiards", Tag: "141", Digest: "sha256:bbb"}, ErrInvalid},
		"control character": {Registration{Repository: "billiards", Tag: "142", Note: "a\x00b"}, ErrInvalid},
	}
	for name, c := range cases {
		if _, _, err := f.svc.Register(ctx, c.r, "key:ci"); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// The whole release of section 8.2: canary, promotion, draining, reclaim and rollback.
func TestReleaseLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	b141 := f.register(t, "141")
	d141 := f.deploy(t, b141)
	if f.state(t, d141.ID) != store.StateNew || f.resolve(t, Stable) != "none" {
		t.Fatal("a deployment without a route must not get matches")
	}
	if err := f.svc.SetRoute(ctx, Stable, b141.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	fleet, spec, ok := f.us.ManagedFleet(d141.Fleet)
	if !ok || fleet.Deployment != d141.ID || spec.Image != "fleet.example.com/billiards@sha256:aaa" || spec.Port != 7770 || spec.Replicas != 2 || spec.Rooms != 50 {
		t.Fatalf("fleet = %+v, spec = %+v", fleet, spec)
	}
	if a, ok := f.us.Autoscaler(d141.Fleet); !ok || a.Buffer != 2 || a.MaxReplicas != 10 {
		t.Fatalf("autoscaler = %+v, %v", a, ok)
	}
	if f.state(t, d141.ID) != store.StateServing || f.resolve(t, Stable) != d141.Fleet {
		t.Fatal("stable should go to 141")
	}

	// 142 goes to testers first.
	b142 := f.register(t, "142")
	if err := f.svc.SetRoute(ctx, Canary, b142.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("routing to an undeployed build: %v", err)
	}
	d142 := f.deploy(t, b142)
	if err := f.svc.SetRoute(ctx, Canary, b142.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if f.resolve(t, Canary) != d142.Fleet || f.resolve(t, Stable) != d141.Fleet {
		t.Fatal("testers get 142, everyone else 141")
	}
	views, err := f.svc.Routes(ctx, "billiards")
	if err != nil || len(views) != 2 || views[0].Channel != Canary || views[0].Build != b142.ID || len(views[0].Regions) != 1 {
		t.Fatalf("routes = %+v, %v", views, err)
	}

	// Promotion: 141 drains, but a match is still running on it. The canary
	// ends: testers play with everyone again.
	if err := f.svc.Promote(ctx, "billiards", "net7", "admin"); err != nil {
		t.Fatal(err)
	}
	if f.state(t, d141.ID) != store.StateDraining || f.resolve(t, Stable) != d142.Fleet || f.resolve(t, Canary) != "none" {
		t.Fatal("after promotion stable goes to 142, 141 drains and the canary ends")
	}
	f.us.SetAllocated(d141.Fleet, 1)
	f.reconcile(t)
	fleet, _, _ = f.us.ManagedFleet(d141.Fleet)
	if _, ok := f.us.Autoscaler(d141.Fleet); ok || fleet.Replicas != 0 {
		t.Fatalf("draining fleet = %+v, autoscaler kept %v", fleet, ok)
	}
	if err := f.svc.Undeploy(ctx, d141.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("undeploy with a match running: %v", err)
	}

	// The match ends; the Fleet stays for the retention time, then goes.
	f.us.SetAllocated(d141.Fleet, 0)
	f.now = f.now.Add(23 * time.Hour)
	f.reconcile(t)
	if _, _, ok := f.us.ManagedFleet(d141.Fleet); !ok {
		t.Fatal("reclaimed before the retention time")
	}
	f.now = f.now.Add(2 * time.Hour)
	f.reconcile(t)
	if _, _, ok := f.us.ManagedFleet(d141.Fleet); ok || f.state(t, d141.ID) != store.StateDeleted {
		t.Fatal("not reclaimed after the retention time")
	}

	// Rolling back needs 141 deployed again.
	if err := f.svc.Rollback(ctx, "billiards", Stable, "net7", "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("rollback to a reclaimed build: %v", err)
	}
	again := f.deploy(t, b141)
	if again.State != store.StateNew {
		t.Fatalf("redeployed 141 is %s", again.State)
	}
	if err := f.svc.Rollback(ctx, "billiards", Stable, "net7", "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if f.resolve(t, Stable) != again.Fleet || f.state(t, again.ID) != store.StateServing || f.state(t, d142.ID) != store.StateDraining {
		t.Fatal("after rollback stable goes to 141 and 142, routed by nothing, drains")
	}
	// Testers go on with 142 while everyone else is back on 141.
	if err := f.svc.SetRoute(ctx, Canary, b142.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if f.resolve(t, Canary) != d142.Fleet || f.state(t, d142.ID) != store.StateServing {
		t.Fatal("canary on 142 again")
	}
	if _, ok := f.us.Autoscaler(again.Fleet); !ok {
		t.Fatal("the rolled-back fleet keeps no buffer")
	}

	// End the canary: 142 drains and can be deleted a minute later.
	if err := f.svc.Undeploy(ctx, d142.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("undeploying a routed build: %v", err)
	}
	if err := f.svc.DeleteRoute(ctx, "billiards", Canary, "net7", "admin"); err != nil {
		t.Fatal(err)
	}
	if f.state(t, d142.ID) != store.StateDraining || f.resolve(t, Canary) != "none" {
		t.Fatal("after ending the canary 142 drains")
	}
	f.now = f.now.Add(UndeployWait)
	if err := f.svc.Undeploy(ctx, d142.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.us.ManagedFleet(d142.Fleet); ok || f.state(t, d142.ID) != store.StateDeleted {
		t.Fatal("undeploy left the fleet")
	}

	var log []store.Entry
	_ = f.store.View(ctx, func(tx *store.Tx) error {
		log, err = tx.AuditLog(0, 100)
		return err
	})
	var actions []string
	for i := len(log) - 1; i >= 0; i-- {
		actions = append(actions, log[i].Actor+" "+log[i].Action)
	}
	want := []string{
		"key:ci build.register", "admin deployment.create", "admin route.set",
		"key:ci build.register", "admin deployment.create", "admin route.set",
		"admin route.promote", "admin route.delete", "gamefleet deployment.reclaim",
		"admin deployment.create", "admin route.rollback", "admin route.set",
		"admin route.delete", "admin deployment.delete",
	}
	if len(actions) != len(want) {
		t.Fatalf("audit log:\n%v\nwant\n%v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Errorf("audit[%d] = %s, want %s", i, actions[i], want[i])
		}
	}
}

func TestLeftoverFleetsGoOnceEmpty(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.us.CreateFleet(ctx, cluster.FleetSpec{Name: "billiards-9", Deployment: 99, Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	f.us.SetAllocated("billiards-9", 1)
	f.reconcile(t)
	fleet, _, ok := f.us.ManagedFleet("billiards-9")
	if !ok || fleet.Replicas != 0 {
		t.Fatalf("a leftover with a match must scale to 0 and stay: %+v %v", fleet, ok)
	}
	f.us.SetAllocated("billiards-9", 0)
	f.reconcile(t)
	if _, _, ok := f.us.ManagedFleet("billiards-9"); ok {
		t.Fatal("empty leftover kept")
	}
}

func TestDrainingStopsGameServersWithoutMatches(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b := f.register(t, "141")
	d := f.deploy(t, b)
	if err := f.svc.SetRoute(ctx, Stable, b.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.us.AddRoom(cluster.Room{ID: "busy", Fleet: d.Fleet, State: "Allocated", Capacity: 50,
		Matches: []cluster.Match{{ID: "m-1"}}, Stale: []string{"m-0"}})
	f.us.AddRoom(cluster.Room{ID: "empty", Fleet: d.Fleet, State: "Allocated", Capacity: 50, LastAllocated: f.now})
	f.us.AddRoom(cluster.Room{ID: "idle", Fleet: d.Fleet, State: "Ready", Capacity: 50})

	// While the build serves, an empty game server waits for the next match.
	f.reconcile(t)
	if room, _ := f.us.Get(ctx, "empty"); room == nil {
		t.Fatal("an empty game server of a serving build was stopped")
	}
	if room, _ := f.us.Get(ctx, "busy"); room == nil || len(room.Stale) != 0 {
		t.Fatalf("notes of finished matches kept: %+v", room)
	}

	// Draining, it stops taking matches and then goes; the one still running
	// a match stays.
	b2 := f.register(t, "142")
	f.deploy(t, b2)
	if err := f.svc.SetRoute(ctx, Stable, b2.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.reconcile(t)
	if room, _ := f.us.Get(ctx, "empty"); room != nil {
		t.Fatal("an empty game server of a draining build was kept")
	}
	if room, _ := f.us.Get(ctx, "busy"); room == nil {
		t.Fatal("a game server running a match was stopped")
	}
}

func TestServingStopsGameServersIdleForLong(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b := f.register(t, "141")
	d := f.deploy(t, b)
	if err := f.svc.SetRoute(ctx, Stable, b.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	room := func(id string, idle time.Duration, matches ...cluster.Match) cluster.Room {
		return cluster.Room{ID: id, Fleet: d.Fleet, State: "Allocated", Capacity: 50,
			LastAllocated: f.now.Add(-idle), Matches: matches}
	}
	f.us.AddRoom(room("recent", time.Minute))
	f.us.AddRoom(room("idle", IdleTimeout))
	f.us.AddRoom(room("raced", IdleTimeout))
	f.us.AddRoom(room("busy", time.Hour, cluster.Match{ID: "m-1"}))

	// First they stop taking matches, so that none is given one as it stops.
	f.reconcile(t)
	for id, want := range map[string]bool{"recent": false, "idle": true, "raced": true, "busy": false} {
		if r, _ := f.us.Get(ctx, id); r == nil || f.us.Retiring(id) != want {
			t.Fatalf("%s: retiring %v, want %v and kept", id, f.us.Retiring(id), want)
		}
	}

	// Allocation gave one a match just before that.
	raced := room("raced", 0, cluster.Match{ID: "m-2"})
	raced.Retiring = true
	f.us.AddRoom(raced)
	f.reconcile(t)
	if r, _ := f.us.Get(ctx, "idle"); r != nil {
		t.Fatal("an idle game server was kept")
	}
	for _, id := range []string{"recent", "raced", "busy"} {
		if r, _ := f.us.Get(ctx, id); r == nil {
			t.Fatalf("%s was stopped", id)
		}
	}

	// It goes once that match is over.
	f.us.ReleaseMatch("raced", "m-2")
	f.reconcile(t)
	if r, _ := f.us.Get(ctx, "raced"); r != nil {
		t.Fatal("a retiring game server was kept after its match")
	}
}

func TestDeployChecks(t *testing.T) {
	f := newFixture(t)
	b := f.register(t, "141")
	ctx := context.Background()
	base := DeployRequest{BuildID: b.ID, Cluster: "us-1", Buffer: 2, MaxReplicas: 10, Rooms: 50, Resources: Defaults.Resources}
	cases := map[string]func(*DeployRequest){
		"unknown cluster": func(r *DeployRequest) { r.Cluster = "sg-1" },
		"no buffer":       func(r *DeployRequest) { r.Buffer = 0 },
		"max below":       func(r *DeployRequest) { r.MaxReplicas = 1 },
		"limit below":     func(r *DeployRequest) { r.Resources.CPULimit = 100 },
		"tiny memory":     func(r *DeployRequest) { r.Resources.MemoryRequest = 1 },
		"too many rooms":  func(r *DeployRequest) { r.Rooms = cluster.MaxRooms + 1 },
	}
	for name, mutate := range cases {
		r := base
		mutate(&r)
		if _, err := f.svc.Deploy(ctx, r, "admin"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	missing := base
	missing.BuildID = 99
	if _, err := f.svc.Deploy(ctx, missing, "admin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing build: %v", err)
	}
	f.deploy(t, b)
	if _, err := f.svc.Deploy(ctx, base, "admin"); !errors.Is(err, ErrConflict) {
		t.Errorf("deploying twice: %v", err)
	}
	defaults, err := f.svc.DefaultsFor(ctx, "billiards", "us-1")
	if err != nil || defaults.Buffer != 2 {
		t.Errorf("defaults = %+v, %v", defaults, err)
	}
}

// A route moves only to a build deployed in every cluster that its current
// build serves: elsewhere that region's players would get no game server.
func TestRouteKeepsEveryCluster(t *testing.T) {
	f := newFixture(t)
	f.svc.sites.Add(sites.Site{Name: "sg-1", Region: "sg", Cluster: clustertest.New("sg-1", 0)})
	ctx := context.Background()
	deployTo := func(b store.Build, clusterName string) {
		t.Helper()
		req := DeployRequest{BuildID: b.ID, Cluster: clusterName, Buffer: 2, MaxReplicas: 10, Rooms: 50, Resources: Defaults.Resources}
		if _, err := f.svc.Deploy(ctx, req, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	old := f.register(t, "141")
	deployTo(old, "us-1")
	deployTo(old, "sg-1")
	if err := f.svc.SetRoute(ctx, Stable, old.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	next := f.register(t, "142")
	deployTo(next, "us-1")
	if err := f.svc.SetRoute(ctx, Canary, next.ID, "admin"); err != nil {
		t.Fatalf("a new canary route has nothing to keep: %v", err)
	}
	if err := f.svc.Promote(ctx, "billiards", "net7", "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("promoting a build missing from sg-1: err = %v, want ErrConflict", err)
	}
	if err := f.svc.SetRoute(ctx, Stable, next.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("pointing stable at a build missing from sg-1: err = %v, want ErrConflict", err)
	}
	var stable store.Route
	if err := f.store.View(ctx, func(tx *store.Tx) error {
		var err error
		stable, err = tx.Route("billiards", Stable, "net7")
		return err
	}); err != nil || stable.BuildID != old.ID {
		t.Fatalf("stable = %+v, %v; want it left on #%d", stable, err, old.ID)
	}
	deployTo(next, "sg-1")
	if err := f.svc.Promote(ctx, "billiards", "net7", "admin"); err != nil {
		t.Fatalf("promoting once sg-1 has the build: %v", err)
	}
}

// hooked runs a function once, the first time the Fleets are read: something
// that happens while a deletion is under way.
type hooked struct {
	*clustertest.Fake
	once   sync.Once
	during func()
}

func (h *hooked) ManagedFleets(ctx context.Context) ([]cluster.ManagedFleet, error) {
	fleets, err := h.Fake.ManagedFleets(ctx)
	h.once.Do(h.during)
	return fleets, err
}

// promoted deploys 141 and 142 and routes stable from 141 to 142, so that 141
// drains.
func promoted(t *testing.T, f *fixture) (d141, d142 store.Deployment) {
	t.Helper()
	ctx := context.Background()
	b141 := f.register(t, "141")
	d141 = f.deploy(t, b141)
	if err := f.svc.SetRoute(ctx, Stable, b141.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	b142 := f.register(t, "142")
	d142 = f.deploy(t, b142)
	if err := f.svc.SetRoute(ctx, Stable, b142.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	return d141, d142
}

// A deployment that just stopped serving cannot be deleted by hand yet: an
// allocation that read the route a moment ago may still be on its way.
func TestUndeployWaitsAfterServing(t *testing.T) {
	f := newFixture(t)
	d141, _ := promoted(t, f)
	ctx := context.Background()
	if err := f.svc.Undeploy(ctx, d141.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("undeploying at once: err = %v, want ErrConflict", err)
	}
	f.now = f.now.Add(UndeployWait)
	if err := f.svc.Undeploy(ctx, d141.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.us.ManagedFleet(d141.Fleet); ok || f.state(t, d141.ID) != store.StateDeleted {
		t.Fatal("undeploy left the fleet")
	}
}

// A rollback to a deployment that is being deleted by hand keeps it.
func TestUndeployLosesToARollback(t *testing.T) {
	f := newFixture(t)
	d141, _ := promoted(t, f)
	ctx := context.Background()
	f.us.AddRoom(cluster.Room{ID: "back", Fleet: d141.Fleet, State: "Ready", Capacity: 50})
	f.now = f.now.Add(UndeployWait)
	f.svc.sites.Add(sites.Site{Name: "us-1", Region: "us", Cluster: &hooked{Fake: f.us, during: func() {
		if err := f.svc.Rollback(ctx, "billiards", Stable, "net7", "admin"); err != nil {
			t.Error(err)
		}
	}}})
	if err := f.svc.Undeploy(ctx, d141.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("undeploy during a rollback: err = %v, want ErrConflict", err)
	}
	if _, _, ok := f.us.ManagedFleet(d141.Fleet); !ok || f.state(t, d141.ID) != store.StateServing {
		t.Fatal("the rolled-back deployment lost its fleet")
	}
	if room, _ := f.us.Get(ctx, "back"); room == nil {
		t.Fatal("the rolled-back deployment lost its game servers")
	}
}

// A rollback after the loop read the database, before it reclaimed the
// drained deployment, keeps the deployment and its Fleet.
func TestReclaimLosesToARollback(t *testing.T) {
	f := newFixture(t)
	d141, _ := promoted(t, f)
	ctx := context.Background()
	f.now = f.now.Add(25 * time.Hour)
	f.svc.sites.Add(sites.Site{Name: "us-1", Region: "us", Cluster: &hooked{Fake: f.us, during: func() {
		if err := f.svc.Rollback(ctx, "billiards", Stable, "net7", "admin"); err != nil {
			t.Error(err)
		}
	}}})
	f.reconcile(t)
	if _, _, ok := f.us.ManagedFleet(d141.Fleet); !ok || f.state(t, d141.ID) != store.StateServing {
		t.Fatal("the loop reclaimed a deployment a rollback had just routed to")
	}
}

// A draining Fleet's empty game server first stops taking matches, and stops
// on a later pass: a rollback may route a match to it in between.
func TestDrainingTakesEmptyGameServersOutOfAllocationFirst(t *testing.T) {
	f := newFixture(t)
	d141, _ := promoted(t, f)
	ctx := context.Background()
	f.us.AddRoom(cluster.Room{ID: "empty", Fleet: d141.Fleet, State: "Allocated", Capacity: 50, LastAllocated: f.now})
	f.reconcile(t)
	if r, _ := f.us.Get(ctx, "empty"); r == nil || !f.us.Retiring("empty") {
		t.Fatalf("after one pass: room %+v, retiring %v; want kept and out of allocation", r, f.us.Retiring("empty"))
	}
	f.reconcile(t)
	if r, _ := f.us.Get(ctx, "empty"); r != nil {
		t.Fatal("still empty on the next pass, it was kept")
	}
}

// A database restored from a backup hands out build and deployment IDs again.
// A Fleet that a lost deployment with such an ID left in the cluster is a
// leftover: the new deployment gets a Fleet of its own and serves from it.
func TestFleetOfAReusedIDIsALeftover(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.us.CreateFleet(ctx, cluster.FleetSpec{Name: "billiards-1", Deployment: 1, Build: 1, Image: "old", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	b := f.register(t, "141")
	d := f.deploy(t, b)
	if b.ID != 1 || d.ID != 1 {
		t.Fatalf("build %d, deployment %d: the test needs both to reuse ID 1", b.ID, d.ID)
	}
	if err := f.svc.SetRoute(ctx, Stable, b.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if d.Fleet == "billiards-1" || f.resolve(t, Stable) != d.Fleet {
		t.Fatalf("deployment fleet %s, stable resolves to %s: want a fleet of its own", d.Fleet, f.resolve(t, Stable))
	}
	if _, spec, ok := f.us.ManagedFleet(d.Fleet); !ok || spec.Image == "old" {
		t.Fatalf("the deployment's own fleet: %+v, %v", spec, ok)
	}
	if _, _, ok := f.us.ManagedFleet("billiards-1"); ok {
		t.Fatal("the leftover fleet of the lost deployment was kept")
	}
}

// Every deployment's Fleet has a name of its own, even of one build in one
// cluster.
func TestRedeployGetsAFleetOfItsOwn(t *testing.T) {
	f := newFixture(t)
	b := f.register(t, "141")
	first := f.deploy(t, b)
	if err := f.svc.Undeploy(context.Background(), first.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if again := f.deploy(t, b); again.Fleet == first.Fleet {
		t.Fatalf("both deployments are Fleet %s", again.Fleet)
	}
}

// A rollback and a route back again, after the loop read the database, start
// the deployment's retention over: the loop does not reclaim it on the old
// time.
func TestReclaimWaitsAgainAfterARollbackAndBack(t *testing.T) {
	f := newFixture(t)
	d141, d142 := promoted(t, f)
	ctx := context.Background()
	f.now = f.now.Add(25 * time.Hour)
	f.svc.sites.Add(sites.Site{Name: "us-1", Region: "us", Cluster: &hooked{Fake: f.us, during: func() {
		if err := f.svc.Rollback(ctx, "billiards", Stable, "net7", "admin"); err != nil {
			t.Error(err)
		}
		if err := f.svc.SetRoute(ctx, Stable, d142.BuildID, "admin"); err != nil {
			t.Error(err)
		}
	}}})
	f.reconcile(t)
	if _, _, ok := f.us.ManagedFleet(d141.Fleet); !ok || f.state(t, d141.ID) != store.StateDraining {
		t.Fatal("the loop reclaimed a deployment that had just drained again, on its old retention")
	}
}
