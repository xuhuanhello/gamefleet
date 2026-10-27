// Package allocate turns a match from the game backend into a room. It asks
// the router which Fleet serves the match's channel and compat version in its
// region, then allocates a GameServer from that Fleet.
package allocate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/sites"
)

// Errors returned by the service. Their text is the error code the API sends.
var (
	ErrInvalid       = errors.New("invalid_request")
	ErrRouteNotFound = errors.New("route_not_found")
	ErrNoCapacity    = errors.New("no_capacity")
	ErrUnavailable   = errors.New("cluster_unavailable")

	// errFull: the route was found full a moment ago, so the cluster was not
	// asked.
	errFull = errors.New("route full")
)

const (
	// FullFor is how long a route counts as full after Agones found no game
	// server with room for it. A new match on it in that time is answered
	// no_capacity at once, without waiting for the cluster's turn: when more
	// matches come than the game servers hold, each one that waited its turn
	// only to hear the same slowed all the others, until the game backend
	// gave up on them before their turn came. A place that frees up waits at
	// most this long.
	FullFor = 2 * time.Second
	// unsureFor is how long a match whose allocation got no answer is never
	// turned away. Nakama gives up on a match after 30 seconds.
	unsureFor = time.Minute
)

// Request asks for a room for one match.
type Request struct {
	MatchID string   `json:"matchId"`
	Game    string   `json:"game"`
	Region  string   `json:"region"`
	Channel string   `json:"channel"`
	Compat  string   `json:"compat"`
	Players []Player `json:"players"`
}

// Player is one seat in the match. GameFleet passes players through to the
// GameServer and the console; it never decides who may play.
type Player struct {
	UserID string `json:"userId"`
	Seat   int    `json:"seat"`
}

// Result is the room for the match. Created is false when the match already
// had a room, which makes retries safe.
type Result struct {
	Room    cluster.Room
	Created bool
}

// Router picks the Fleet for a match; release.Service is the real one.
type Router interface {
	Resolve(ctx context.Context, game, channel, compat, region string) (release.Target, error)
}

// Service allocates and looks up rooms across all clusters.
type Service struct {
	router Router
	sites  *sites.Directory
	// How long to wait before trying again after contention, and after
	// finding no game server with room.
	retryWait, capacityWait time.Duration
	// One allocation at a time in each cluster, by cluster name. Agones gives
	// a game server at most one new match per allocation batch, so matches
	// allocated together spread over game servers, or find none and fail.
	turns sync.Map
	// queued, when set, is called when an allocation starts to wait for a
	// cluster's turn that another holds. For tests.
	queued func()

	now     func() time.Time
	fullFor time.Duration
	mu      sync.Mutex
	// full: the routes Agones found full lately, by routeKey.
	full map[string]fullRoute
	// unsure: until when each match whose allocation was sent but got no
	// answer is looked for in the cluster rather than turned away.
	unsure map[string]time.Time
}

// fullRoute is a route Agones found full: until when it counts as full, and
// the cluster's game servers as they were then.
type fullRoute struct {
	until time.Time
	rooms []cluster.Room
}

var (
	// matchIDChars matches what may follow AnnotationMatchPrefix in the name
	// of an annotation; validMatchID also limits its length.
	matchIDChars = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`)
	nameValue    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// roomName matches Kubernetes object names, which GameServer names are.
	roomName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// New builds the service from the router and the clusters.
func New(router Router, dir *sites.Directory) *Service {
	return &Service{router: router, sites: dir, retryWait: 25 * time.Millisecond, capacityWait: 300 * time.Millisecond,
		now: time.Now, fullFor: FullFor, full: map[string]fullRoute{}, unsure: map[string]time.Time{}}
}

// Allocate returns the room for the match, allocating one only if the match
// has none yet. It looks for the match in its region's cluster before it
// reads the routes, so a retry finds its room even after the route moved or
// went; and it looks while it holds the cluster's turn, so requests for one
// match that overlap get one room.
func (s *Service) Allocate(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	// One cluster serves each region (clusters.Service refuses a second).
	site, ok := s.sites.InRegion(req.Region)
	if !ok {
		return Result{}, ErrRouteNotFound
	}
	players, err := json.Marshal(req.Players)
	if err != nil {
		return Result{}, err
	}
	contended, full := 0, 0
	for {
		// A new match on a full route is turned away; but not by the request
		// that found it full, which looks again after its waits below.
		result, err := s.attempt(ctx, site, req, players, full == 0)
		var wait time.Duration
		switch {
		case errors.Is(err, errFull):
			return Result{}, ErrNoCapacity
		case errors.Is(err, cluster.ErrContention) && contended < 2:
			contended++
			wait = time.Duration(contended) * s.retryWait
		// Agones may not know yet of a game server that has just become Ready.
		case errors.Is(err, cluster.ErrNoCapacity) && full < 2:
			full++
			wait = time.Duration(full) * s.capacityWait
		case errors.Is(err, release.ErrNoRoute):
			return Result{}, ErrRouteNotFound
		case err != nil:
			return Result{}, mapError(err)
		default:
			return result, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// attempt looks for the match's room in the cluster, and allocates one when
// there is none, all in one turn of the cluster. When turnAway is set and the
// route counts as full, it returns errFull without asking the cluster, before
// it waits for the turn and again after, as the route may have been found
// full meanwhile.
func (s *Service) attempt(ctx context.Context, site sites.Site, req Request, players []byte, turnAway bool) (Result, error) {
	if turnAway && s.isFull(req) {
		return Result{}, errFull
	}
	done, err := s.turn(ctx, site.Name)
	if err != nil {
		return Result{}, err
	}
	defer done()
	if turnAway && s.isFull(req) {
		return Result{}, errFull
	}
	rooms, err := site.Cluster.Rooms(ctx)
	if err != nil {
		return Result{}, err
	}
	if existing := cluster.FindMatch(rooms, req.MatchID); existing != nil {
		return Result{Room: *existing}, nil
	}
	target, err := s.router.Resolve(ctx, req.Game, req.Channel, req.Compat, req.Region)
	if err != nil {
		return Result{}, err
	}
	if target.Cluster != site.Name {
		return Result{}, fmt.Errorf("%w: the route for region %s names cluster %s, but %s serves it", ErrUnavailable, req.Region, target.Cluster, site.Name)
	}
	// A game server whose notes near what Kubernetes allows takes no new
	// match; this allocation would add one note. Every allocation in the
	// cluster comes through here, one at a time, so no game server gets more
	// than one note past the check.
	for _, r := range rooms {
		if r.Fleet == target.Fleet && r.Notes > cluster.FullNotes && !r.Retiring {
			if err := site.Cluster.MarkRetiring(ctx, r.ID, true); err != nil {
				return Result{}, err
			}
		}
	}
	room, err := site.Cluster.Allocate(ctx, cluster.Allocation{
		Fleet:         target.Fleet,
		MatchID:       req.MatchID,
		Game:          req.Game,
		Channel:       req.Channel,
		Compat:        req.Compat,
		Players:       players,
		Rooms:         target.Rooms,
		RoomsCapacity: target.RoomsCapacity,
	})
	if err != nil {
		switch {
		case errors.Is(err, cluster.ErrNoCapacity):
			s.markFull(req, rooms)
		case !errors.Is(err, cluster.ErrContention):
			// The allocation may still go through after the answer was lost,
			// and later than another one sees the cluster.
			s.markUnsure(req.MatchID)
		}
		return Result{}, err
	}
	s.clearFull(req)
	room.Build = strconv.FormatInt(target.Build, 10)
	return Result{Room: room, Created: true}, nil
}

// turn waits until no other allocation runs in the cluster. Call the function
// it returns when done.
func (s *Service) turn(ctx context.Context, cluster string) (func(), error) {
	v, _ := s.turns.LoadOrStore(cluster, make(chan struct{}, 1))
	turn := v.(chan struct{})
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	default:
	}
	if s.queued != nil {
		s.queued()
	}
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// routeKey names what Resolve picks the Fleet by. Compat holds no control
// characters (validate), so the key is unambiguous.
func routeKey(req Request) string {
	return req.Game + "\n" + req.Channel + "\n" + req.Compat + "\n" + req.Region
}

// isFull reports whether the route counts as full and the match was in none
// of the cluster's game servers when it was found full. GameFleet alone
// allocates in the cluster, one allocation at a time, and one that goes
// through ends the route's time as full: so a match that was not there has
// no room yet, unless its allocation was sent and got no answer, and may
// have gone through since. One that was there, asked about again because an
// answer got lost or GameFleet restarted, is looked for as usual.
func (s *Service) isFull(req Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	f, ok := s.full[routeKey(req)]
	if !ok || !now.Before(f.until) || now.Before(s.unsure[req.MatchID]) {
		return false
	}
	return cluster.FindMatch(f.rooms, req.MatchID) == nil
}

// markUnsure notes a match whose allocation got no answer, and forgets the
// ones noted long enough.
func (s *Service) markUnsure(matchID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, until := range s.unsure {
		if !now.Before(until) {
			delete(s.unsure, id)
		}
	}
	s.unsure[matchID] = now.Add(unsureFor)
}

// markFull starts the route's time as full, with the game servers the
// allocation saw, and forgets the routes whose time is over.
func (s *Service) markFull(req Request, rooms []cluster.Room) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, f := range s.full {
		if !now.Before(f.until) {
			delete(s.full, key)
		}
	}
	s.full[routeKey(req)] = fullRoute{until: now.Add(s.fullFor), rooms: rooms}
}

// clearFull ends the route's time as full: an allocation on it went through.
func (s *Service) clearFull(req Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.full, routeKey(req))
}

// Room returns the room with this id from whichever cluster has it, or nil.
func (s *Service) Room(ctx context.Context, id string) (*cluster.Room, error) {
	if !roomName.MatchString(id) {
		return nil, fmt.Errorf("%w: roomId", ErrInvalid)
	}
	return s.find(func(c cluster.API) (*cluster.Room, error) { return c.Get(ctx, id) })
}

// RoomByMatch returns the room allocated for the match, or nil.
func (s *Service) RoomByMatch(ctx context.Context, matchID string) (*cluster.Room, error) {
	if !validMatchID(matchID) {
		return nil, fmt.Errorf("%w: matchId", ErrInvalid)
	}
	return s.find(func(c cluster.API) (*cluster.Room, error) { return c.FindByMatch(ctx, matchID) })
}

// EndMatch asks the game server running the match to end it, and returns
// that game server, or nil when no cluster runs the match.
func (s *Service) EndMatch(ctx context.Context, matchID string) (*cluster.Room, error) {
	room, err := s.RoomByMatch(ctx, matchID)
	if err != nil || room == nil {
		return nil, err
	}
	site, ok := s.sites.Get(room.Cluster)
	if !ok {
		return nil, fmt.Errorf("%w: cluster %s is not configured", ErrUnavailable, room.Cluster)
	}
	if err := site.Cluster.EndMatch(ctx, room.ID, matchID); err != nil {
		return nil, mapError(err)
	}
	return room, nil
}

// Shutdown ends the room. It reports false when no cluster has the room.
func (s *Service) Shutdown(ctx context.Context, id string) (bool, error) {
	if !roomName.MatchString(id) {
		return false, fmt.Errorf("%w: roomId", ErrInvalid)
	}
	var failed error
	for _, site := range s.sites.All() {
		c := site.Cluster
		room, err := c.Get(ctx, id)
		if err != nil {
			failed = err
			continue
		}
		if room == nil {
			continue
		}
		if err := c.Shutdown(ctx, id); err != nil {
			return false, mapError(err)
		}
		return true, nil
	}
	if failed != nil {
		return false, mapError(failed)
	}
	return false, nil
}

// find asks every cluster in turn. A cluster that fails only matters when no
// other cluster has the room.
func (s *Service) find(lookup func(cluster.API) (*cluster.Room, error)) (*cluster.Room, error) {
	var failed error
	for _, site := range s.sites.All() {
		room, err := lookup(site.Cluster)
		if err != nil {
			failed = err
			continue
		}
		if room != nil {
			return room, nil
		}
	}
	if failed != nil {
		return nil, mapError(failed)
	}
	return nil, nil
}

func mapError(err error) error {
	switch {
	case errors.Is(err, cluster.ErrNoCapacity), errors.Is(err, cluster.ErrContention):
		return ErrNoCapacity
	case errors.Is(err, cluster.ErrUnavailable):
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	default:
		return err
	}
}

func (r Request) validate() error {
	invalid := func(field string) error { return fmt.Errorf("%w: %s", ErrInvalid, field) }
	switch {
	case !validMatchID(r.MatchID):
		return invalid("matchId")
	case !nameValue.MatchString(r.Game):
		return invalid("game")
	case !nameValue.MatchString(r.Region):
		return invalid("region")
	case !nameValue.MatchString(r.Channel):
		return invalid("channel")
	case !printable(r.Compat, 128):
		return invalid("compat")
	case len(r.Players) == 0 || len(r.Players) > 16:
		return invalid("players")
	}
	seats := map[int]bool{}
	users := map[string]bool{}
	for _, p := range r.Players {
		if !printable(p.UserID, 128) || users[p.UserID] {
			return invalid("players.userId")
		}
		if p.Seat < 0 || p.Seat > 15 || seats[p.Seat] {
			return invalid("players.seat")
		}
		users[p.UserID] = true
		seats[p.Seat] = true
	}
	// The match's note on its game server holds the players, the channel and
	// the compat version, and has to fit next to the game server's other
	// matches.
	players, err := json.Marshal(r.Players)
	if err != nil {
		return err
	}
	if cluster.NoteSize(cluster.Allocation{MatchID: r.MatchID, Channel: r.Channel, Compat: r.Compat, Players: players}) > cluster.MaxMatchNote {
		return invalid("players")
	}
	return nil
}

// validMatchID reports whether the ID can name the match's annotation.
func validMatchID(id string) bool {
	return len(id) <= cluster.MaxMatchIDLength && matchIDChars.MatchString(id)
}

func printable(s string, max int) bool {
	return s != "" && len(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}
