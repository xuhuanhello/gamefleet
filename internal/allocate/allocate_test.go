package allocate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/sites"
)

const compat = "dm-v7-closed-frame-v3-cached-0"

// routes maps "game channel compat region" to a target, like release.Service
// with build 142 on stable in both regions and 143 on canary in us.
type routes map[string]release.Target

func (r routes) Resolve(_ context.Context, game, channel, compat, region string) (release.Target, error) {
	if t, ok := r[game+" "+channel+" "+compat+" "+region]; ok {
		return t, nil
	}
	return release.Target{}, release.ErrNoRoute
}

func newService(t *testing.T, us, sg *clustertest.Fake) *Service {
	t.Helper()
	svc := New(routes{
		"billiards stable " + compat + " us": {Cluster: "us-1", Fleet: "billiards-142", Build: 142},
		"billiards stable " + compat + " sg": {Cluster: "sg-1", Fleet: "billiards-142", Build: 142},
		"billiards canary " + compat + " us": {Cluster: "us-1", Fleet: "billiards-143", Build: 143},
	}, sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: us}, sites.Site{Name: "sg-1", Region: "sg", Cluster: sg}))
	svc.retryWait, svc.capacityWait = 0, 0
	return svc
}

func request(matchID, region, channel string) Request {
	return Request{
		MatchID: matchID, Game: "billiards", Region: region, Channel: channel, Compat: compat,
		Players: []Player{{UserID: "u-1", Seat: 0}, {UserID: "u-2", Seat: 1}},
	}
}

func TestAllocateUsesTheClusterOfTheRegion(t *testing.T) {
	us, sg := clustertest.New("us-1", 1), clustertest.New("sg-1", 1)
	svc := newService(t, us, sg)

	result, err := svc.Allocate(context.Background(), request("m-1", "sg", "stable"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Room.Cluster != "sg-1" || result.Room.Fleet != "billiards-142" {
		t.Fatalf("result = %+v", result)
	}
	if us.Allocations() != 0 || sg.Allocations() != 1 {
		t.Fatalf("allocations us=%d sg=%d", us.Allocations(), sg.Allocations())
	}
	var players []Player
	if err := json.Unmarshal(result.Room.Players, &players); err != nil || len(players) != 2 {
		t.Fatalf("players = %s", result.Room.Players)
	}
}

func TestAllocateTwiceForOneMatchReturnsTheSameRoom(t *testing.T) {
	us := clustertest.New("us-1", 5)
	svc := newService(t, us, clustertest.New("sg-1", 0))

	first, err := svc.Allocate(context.Background(), request("m-1", "us", "stable"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Allocate(context.Background(), request("m-1", "us", "stable"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Room.ID != first.Room.ID || us.Allocations() != 1 {
		t.Fatalf("first = %+v, second = %+v, allocations = %d", first, second, us.Allocations())
	}
}

func TestAllocateFollowsTheChannelRoute(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 1), clustertest.New("sg-1", 1))
	result, err := svc.Allocate(context.Background(), request("m-1", "us", "canary"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Room.Fleet != "billiards-143" || result.Room.Build != "143" {
		t.Fatalf("fleet = %s, build = %s, want the canary fleet", result.Room.Fleet, result.Room.Build)
	}
}

func TestAllocateWithoutRoute(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 1), clustertest.New("sg-1", 1))
	unknownCompat := request("m-1", "us", "stable")
	unknownCompat.Compat = "dm-v8"
	cases := map[string]Request{
		"unknown compat":              unknownCompat,
		"channel not deployed in sg":  request("m-2", "sg", "canary"),
		"region without any cluster":  request("m-3", "eu", "stable"),
		"channel that does not exist": request("m-4", "us", "beta"),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Allocate(context.Background(), req); !errors.Is(err, ErrRouteNotFound) {
				t.Fatalf("err = %v, want ErrRouteNotFound", err)
			}
		})
	}
}

func TestAllocateWithoutReadyServers(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 0), clustertest.New("sg-1", 0))
	if _, err := svc.Allocate(context.Background(), request("m-1", "us", "stable")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
}

func TestAllocateRetriesContentionTwice(t *testing.T) {
	us := clustertest.New("us-1", 2)
	svc := newService(t, us, clustertest.New("sg-1", 0))

	us.Contend(2)
	if _, err := svc.Allocate(context.Background(), request("m-1", "us", "stable")); err != nil {
		t.Fatalf("two contentions then success: %v", err)
	}
	us.Contend(3)
	if _, err := svc.Allocate(context.Background(), request("m-2", "us", "stable")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("three contentions: err = %v, want ErrNoCapacity", err)
	}
}

func TestAllocateWhenTheClusterIsDown(t *testing.T) {
	us := clustertest.New("us-1", 1)
	us.Fail(cluster.ErrUnavailable)
	svc := newService(t, us, clustertest.New("sg-1", 1))
	if _, err := svc.Allocate(context.Background(), request("m-1", "us", "stable")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestAllocateRejectsMalformedRequests(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 9), clustertest.New("sg-1", 9))
	cases := map[string]func(*Request){
		"match id with spaces": func(r *Request) { r.MatchID = "match 1" },
		"match id too long":    func(r *Request) { r.MatchID = strings.Repeat("a", cluster.MaxMatchIDLength+1) },
		"uppercase game":       func(r *Request) { r.Game = "Billiards" },
		"empty compat":         func(r *Request) { r.Compat = "" },
		"no players":           func(r *Request) { r.Players = nil },
		"same seat twice":      func(r *Request) { r.Players[1].Seat = 0 },
		"same player twice":    func(r *Request) { r.Players[1].UserID = "u-1" },
		"negative seat":        func(r *Request) { r.Players[0].Seat = -1 },
		// Sixteen players with the longest IDs make a note larger than a
		// game server has room for.
		"too long a note": func(r *Request) {
			r.Players = nil
			for i := range 16 {
				r.Players = append(r.Players, Player{UserID: fmt.Sprintf("%0128d", i), Seat: i})
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := request("m-1", "us", "stable")
			mutate(&req)
			if _, err := svc.Allocate(context.Background(), req); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// The longest match ID still names its annotation: Kubernetes takes 63
// characters after "gamefleet.io/", and "match." takes 6 of them.
func TestAllocateTakesTheLongestMatchID(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 1), clustertest.New("sg-1", 1))
	id := strings.Repeat("a", cluster.MaxMatchIDLength)
	if len(strings.TrimPrefix(cluster.AnnotationMatchPrefix, "gamefleet.io/")+id) != 63 {
		t.Fatalf("MaxMatchIDLength = %d does not fill the 63 characters of an annotation name", cluster.MaxMatchIDLength)
	}
	if _, err := svc.Allocate(context.Background(), request(id, "us", "stable")); err != nil {
		t.Fatal(err)
	}
}

func TestRoomLookupsSearchEveryCluster(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 1), clustertest.New("sg-1", 1))
	ctx := context.Background()
	result, err := svc.Allocate(ctx, request("m-1", "sg", "stable"))
	if err != nil {
		t.Fatal(err)
	}

	if room, err := svc.Room(ctx, result.Room.ID); err != nil || room == nil || room.Cluster != "sg-1" {
		t.Fatalf("Room = %+v, %v", room, err)
	}
	if room, err := svc.RoomByMatch(ctx, "m-1"); err != nil || room == nil || room.ID != result.Room.ID {
		t.Fatalf("RoomByMatch = %+v, %v", room, err)
	}
	if room, err := svc.Room(ctx, "missing-room"); err != nil || room != nil {
		t.Fatalf("Room(missing) = %+v, %v", room, err)
	}
	if _, err := svc.Room(ctx, "Bad_Name"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Room(bad name) err = %v", err)
	}
}

func TestShutdownEndsTheRoom(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 1), clustertest.New("sg-1", 1))
	ctx := context.Background()
	result, err := svc.Allocate(ctx, request("m-1", "us", "stable"))
	if err != nil {
		t.Fatal(err)
	}

	if found, err := svc.Shutdown(ctx, result.Room.ID); err != nil || !found {
		t.Fatalf("Shutdown = %v, %v", found, err)
	}
	if room, _ := svc.Room(ctx, result.Room.ID); room != nil {
		t.Fatalf("room still exists: %+v", room)
	}
	if found, err := svc.Shutdown(ctx, result.Room.ID); err != nil || found {
		t.Fatalf("second Shutdown = %v, %v", found, err)
	}
}

func TestMatchesShareAGameServerUntilItIsFull(t *testing.T) {
	us := clustertest.New("us-1", 1)
	us.SetCapacity(2)
	svc := newService(t, us, clustertest.New("sg-1", 0))
	ctx := context.Background()

	first, err := svc.Allocate(ctx, request("m-1", "us", "stable"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Allocate(ctx, request("m-2", "us", "stable"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Room.ID != first.Room.ID || second.Room.MatchID != "m-2" {
		t.Fatalf("first = %+v, second = %+v", first.Room, second.Room)
	}
	if _, err := svc.Allocate(ctx, request("m-3", "us", "stable")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("a full game server took a third match: %v", err)
	}
	again, err := svc.Allocate(ctx, request("m-1", "us", "stable"))
	if err != nil || again.Created || again.Room.ID != first.Room.ID || again.Room.MatchID != "m-1" {
		t.Fatalf("asking again = %+v, %v", again, err)
	}
}

// A deployment's game servers take as many matches as it lets them, which
// may be fewer than their rooms lists hold.
func TestAGameServerTakesTheMatchesItsDeploymentAllows(t *testing.T) {
	us := clustertest.New("us-1", 2)
	us.SetCapacity(cluster.MaxRooms)
	svc := New(routes{
		"billiards stable " + compat + " us": {Cluster: "us-1", Fleet: "billiards-142", Build: 142, Rooms: 2, RoomsCapacity: cluster.MaxRooms},
	}, sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: us}))
	svc.retryWait, svc.capacityWait = 0, 0
	ctx := context.Background()

	var servers []string
	for _, id := range []string{"m-1", "m-2", "m-3"} {
		result, err := svc.Allocate(ctx, request(id, "us", "stable"))
		if err != nil {
			t.Fatal(err)
		}
		servers = append(servers, result.Room.ID)
	}
	if servers[0] != servers[1] || servers[2] == servers[0] {
		t.Fatalf("game servers = %v, want two matches on the first and the third on another", servers)
	}
}

// overlapping counts the allocations that run at the same time.
type overlapping struct {
	*clustertest.Fake
	mu            sync.Mutex
	running, most int
}

func (o *overlapping) Allocate(ctx context.Context, a cluster.Allocation) (cluster.Room, error) {
	o.mu.Lock()
	o.running++
	o.most = max(o.most, o.running)
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.running--
		o.mu.Unlock()
	}()
	time.Sleep(time.Millisecond)
	return o.Fake.Allocate(ctx, a)
}

func TestAllocationsInOneClusterTakeTurns(t *testing.T) {
	us := &overlapping{Fake: clustertest.New("us-1", 1)}
	us.SetCapacity(50)
	svc := newService(t, us.Fake, clustertest.New("sg-1", 0))
	svc.sites = sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: us})

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Allocate(context.Background(), request(fmt.Sprintf("m-%d", i), "us", "stable"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if us.most != 1 {
		t.Fatalf("%d allocations ran at once", us.most)
	}
}

// A retry finds its match's room even when the route is gone: promoting a
// canary deletes the canary route, maybe between a request and its retry.
func TestRetryFindsTheRoomAfterItsRouteIsGone(t *testing.T) {
	us := clustertest.New("us-1", 1)
	canary := "billiards canary " + compat + " us"
	r := routes{canary: {Cluster: "us-1", Fleet: "billiards-143", Build: 143}}
	svc := New(r, sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: us}))
	ctx := context.Background()
	first, err := svc.Allocate(ctx, request("m-1", "us", "canary"))
	if err != nil {
		t.Fatal(err)
	}
	delete(r, canary)
	again, err := svc.Allocate(ctx, request("m-1", "us", "canary"))
	if err != nil || again.Created || again.Room.ID != first.Room.ID {
		t.Fatalf("retry after the route went = %+v, %v; want room %s", again, err, first.Room.ID)
	}
	if _, err := svc.Allocate(ctx, request("m-2", "us", "canary")); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("a new match on the deleted route: err = %v, want ErrRouteNotFound", err)
	}
}

// held tells the test as each lookup of a match starts, and holds the first
// until the test lets it go.
type held struct {
	*clustertest.Fake
	mu      sync.Mutex
	n       int
	looks   chan int
	release chan struct{}
}

func (h *held) Rooms(ctx context.Context) ([]cluster.Room, error) {
	h.mu.Lock()
	h.n++
	n := h.n
	h.mu.Unlock()
	h.looks <- n
	if n == 1 {
		<-h.release
	}
	return h.Fake.Rooms(ctx)
}

// Requests for one match that overlap, such as a retry sent while a slow
// first request still runs, get the same room.
func TestOverlappingRequestsForOneMatchShareTheRoom(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 0), clustertest.New("sg-1", 0))
	h := &held{Fake: clustertest.New("us-1", 8), looks: make(chan int, 2), release: make(chan struct{})}
	svc.sites.Add(sites.Site{Name: "us-1", Region: "us", Cluster: h})
	var results [2]Result
	var errs [2]error
	var wg sync.WaitGroup
	run := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.Allocate(context.Background(), request("m-1", "us", "stable"))
		}()
	}
	// The first request looks for the match, and is held there.
	run(0)
	if n := <-h.looks; n != 1 {
		t.Fatalf("lookup %d came first", n)
	}
	// The second one would look now too, but has to wait for the cluster's
	// turn. Give it the time to get there either way, then let the first go.
	run(1)
	select {
	case <-h.looks:
		t.Error("the second request looked for the match while the first held the cluster's turn")
	case <-time.After(200 * time.Millisecond):
	}
	close(h.release)
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
	}
	if results[0].Room.ID != results[1].Room.ID || !results[0].Created || results[1].Created {
		t.Fatalf("results = %+v; want one room, allocated by the first request", results)
	}
}

// A game server whose notes near what Kubernetes allows is taken out of
// allocation before the next match: allocation itself checks, as the loop
// may not pass before more matches come.
func TestAllocationPassesByAGameServerFullOfNotes(t *testing.T) {
	us := clustertest.New("us-1", 1)
	us.SetCapacity(50)
	svc := newService(t, us, clustertest.New("sg-1", 0))
	us.AddRoom(cluster.Room{ID: "full", Fleet: "billiards-142", State: "Allocated", Capacity: 50,
		Matches: []cluster.Match{{ID: "m-0"}}, Notes: cluster.FullNotes + 1})
	result, err := svc.Allocate(context.Background(), request("m-1", "us", "stable"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Room.ID == "full" || !us.Retiring("full") {
		t.Fatalf("allocated to %s; full retiring %v", result.Room.ID, us.Retiring("full"))
	}
}

// gated is a cluster with no room while shut. It counts what it is asked, and
// when hold is set the next allocation closes held and waits for hold.
type gated struct {
	*clustertest.Fake
	mu         sync.Mutex
	shut       bool
	asked      int
	hold, held chan struct{}
}

func (g *gated) Rooms(ctx context.Context) ([]cluster.Room, error) {
	g.mu.Lock()
	g.asked++
	g.mu.Unlock()
	return g.Fake.Rooms(ctx)
}

func (g *gated) Allocate(ctx context.Context, a cluster.Allocation) (cluster.Room, error) {
	g.mu.Lock()
	g.asked++
	shut, hold, held := g.shut, g.hold, g.held
	g.hold = nil
	g.mu.Unlock()
	if hold != nil {
		close(held)
		<-hold
	}
	if shut {
		return cluster.Room{}, cluster.ErrNoCapacity
	}
	return g.Fake.Allocate(ctx, a)
}

func (g *gated) set(shut bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shut = shut
}

func (g *gated) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked
}

// gatedService serves us from g, on a clock the test moves.
func gatedService(t *testing.T, g *gated) (*Service, *time.Time) {
	t.Helper()
	svc := newService(t, clustertest.New("us-1", 0), clustertest.New("sg-1", 0))
	svc.sites = sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: g})
	clock := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return clock }
	return svc, &clock
}

func TestANewMatchOnAFullRouteIsTurnedAwayAtOnce(t *testing.T) {
	g := &gated{Fake: clustertest.New("us-1", 5), shut: true}
	svc, clock := gatedService(t, g)
	ctx := context.Background()

	if _, err := svc.Allocate(ctx, request("m-1", "us", "stable")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("m-1: err = %v, want ErrNoCapacity", err)
	}
	asked := g.count()
	if _, err := svc.Allocate(ctx, request("m-2", "us", "stable")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("m-2: err = %v, want ErrNoCapacity", err)
	}
	if g.count() != asked {
		t.Fatalf("a new match on a full route asked the cluster %d times", g.count()-asked)
	}

	// Another route has game servers of its own.
	g.set(false)
	if _, err := svc.Allocate(ctx, request("m-3", "us", "canary")); err != nil {
		t.Fatalf("canary while stable is full: %v", err)
	}
	if _, err := svc.Allocate(ctx, request("m-4", "us", "stable")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("m-4 within FullFor: err = %v, want ErrNoCapacity", err)
	}
	*clock = clock.Add(FullFor)
	if _, err := svc.Allocate(ctx, request("m-5", "us", "stable")); err != nil {
		t.Fatalf("m-5 after FullFor: %v", err)
	}
}

// A retry finds the room its match got even while the route counts as full,
// and an allocation that goes through ends the route's time as full.
func TestAMatchAskedAboutBeforeIsLookedForWhileItsRouteIsFull(t *testing.T) {
	g := &gated{Fake: clustertest.New("us-1", 1)}
	svc, _ := gatedService(t, g)
	ctx := context.Background()

	first, err := svc.Allocate(ctx, request("m-1", "us", "stable"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Allocate(ctx, request("m-2", "us", "stable")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("m-2: err = %v, want ErrNoCapacity", err)
	}
	again, err := svc.Allocate(ctx, request("m-1", "us", "stable"))
	if err != nil || again.Created || again.Room.ID != first.Room.ID {
		t.Fatalf("m-1 again = %+v, %v; want its room %s", again, err, first.Room.ID)
	}

	// Game servers became Ready, and m-2's retry got one; m-3, new, need not
	// wait out FullFor for the other.
	g.Fake = clustertest.New("us-1", 2)
	if _, err := svc.Allocate(ctx, request("m-2", "us", "stable")); err != nil {
		t.Fatalf("m-2 again: %v", err)
	}
	if _, err := svc.Allocate(ctx, request("m-3", "us", "stable")); err != nil {
		t.Fatalf("m-3 after an allocation went through: %v", err)
	}
}

// Matches that wait for the cluster's turn while another finds the route full
// are turned away when their turn comes.
func TestMatchesWaitingForTheTurnAreTurnedAwayWhenTheRouteIsFound(t *testing.T) {
	g := &gated{Fake: clustertest.New("us-1", 0), shut: true, hold: make(chan struct{}), held: make(chan struct{})}
	svc, _ := gatedService(t, g)
	ctx := context.Background()
	hold := g.hold

	errs := make(chan error, 2)
	go func() {
		_, err := svc.Allocate(ctx, request("m-1", "us", "stable"))
		errs <- err
	}()
	<-g.held
	go func() {
		_, err := svc.Allocate(ctx, request("m-2", "us", "stable"))
		errs <- err
	}()
	time.Sleep(20 * time.Millisecond) // m-2 waits for the turn
	close(hold)
	for range 2 {
		if err := <-errs; !errors.Is(err, ErrNoCapacity) {
			t.Fatalf("err = %v, want ErrNoCapacity", err)
		}
	}
	// m-1 looked three times, a list and an allocation each; m-2 not at all.
	if g.count() != 6 {
		t.Fatalf("the cluster was asked %d times, want 6", g.count())
	}
}
