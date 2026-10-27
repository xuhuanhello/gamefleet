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

// meeting holds the first lookup of a match until a second one arrives, or
// for a while: requests that overlap both look before either allocates,
// unless something keeps them apart.
type meeting struct {
	*clustertest.Fake
	mu     sync.Mutex
	looks  int
	second chan struct{}
}

func (m *meeting) FindByMatch(ctx context.Context, matchID string) (*cluster.Room, error) {
	m.mu.Lock()
	m.looks++
	n := m.looks
	m.mu.Unlock()
	switch n {
	case 1:
		select {
		case <-m.second:
		case <-time.After(300 * time.Millisecond):
		}
	case 2:
		close(m.second)
	}
	return m.Fake.FindByMatch(ctx, matchID)
}

// Requests for one match that overlap, such as a retry sent while a slow
// first request still runs, get the same room.
func TestOverlappingRequestsForOneMatchShareTheRoom(t *testing.T) {
	svc := newService(t, clustertest.New("us-1", 0), clustertest.New("sg-1", 0))
	svc.sites.Add(sites.Site{Name: "us-1", Region: "us", Cluster: &meeting{Fake: clustertest.New("us-1", 8), second: make(chan struct{})}})
	var results [2]Result
	var errs [2]error
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.Allocate(context.Background(), request("m-1", "us", "stable"))
		}()
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
	}
	if results[0].Room.ID != results[1].Room.ID || results[0].Created == results[1].Created {
		t.Fatalf("results = %+v; want one room, allocated once", results)
	}
}
