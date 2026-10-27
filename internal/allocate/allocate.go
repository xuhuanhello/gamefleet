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
	return &Service{router: router, sites: dir, retryWait: 25 * time.Millisecond, capacityWait: 300 * time.Millisecond}
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
		result, err := s.attempt(ctx, site, req, players)
		var wait time.Duration
		switch {
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
// there is none, all in one turn of the cluster.
func (s *Service) attempt(ctx context.Context, site sites.Site, req Request, players []byte) (Result, error) {
	done, err := s.turn(ctx, site.Name)
	if err != nil {
		return Result{}, err
	}
	defer done()
	existing, err := site.Cluster.FindByMatch(ctx, req.MatchID)
	if err != nil {
		return Result{}, err
	}
	if existing != nil {
		return Result{Room: *existing}, nil
	}
	target, err := s.router.Resolve(ctx, req.Game, req.Channel, req.Compat, req.Region)
	if err != nil {
		return Result{}, err
	}
	if target.Cluster != site.Name {
		return Result{}, fmt.Errorf("%w: the route for region %s names cluster %s, but %s serves it", ErrUnavailable, req.Region, target.Cluster, site.Name)
	}
	room, err := site.Cluster.Allocate(ctx, cluster.Allocation{
		Fleet:   target.Fleet,
		MatchID: req.MatchID,
		Game:    req.Game,
		Channel: req.Channel,
		Compat:  req.Compat,
		Players: players,
	})
	if err != nil {
		return Result{}, err
	}
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
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
