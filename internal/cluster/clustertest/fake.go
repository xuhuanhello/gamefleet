// Package clustertest provides an in-memory stand-in for one cluster.
package clustertest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
)

// Fake behaves like a cluster with a fixed number of Ready GameServers.
type Fake struct {
	mu          sync.Mutex
	name        string
	ready       int
	contention  int
	err         error
	rooms       map[string]cluster.Room
	allocations int
	servers     int // game servers allocated from the Ready ones
	capacity    int
	logQuery    cluster.LogQuery
	retiring    map[string]bool
	ended       map[string][]string
	view        View

	fleets      map[string]*managed
	autoscalers map[string]Autoscaler
	secrets     map[string]cluster.Secret
	deleted     []string // nodes deleted with DeleteNode
}

type managed struct {
	spec  cluster.FleetSpec
	fleet cluster.ManagedFleet
}

// Autoscaler is what ApplyAutoscaler set for a Fleet.
type Autoscaler struct {
	Deployment          int64
	Buffer, MaxReplicas int
}

// View is what the console's read methods return besides rooms.
type View struct {
	Nodes  []cluster.Node
	Pods   []cluster.Pod
	Fleets []cluster.Fleet
	Events []cluster.Event
	// Logs maps "namespace/pod/container" to the log text.
	Logs     map[string]string
	Usage    cluster.Usage
	UsageErr error
	// EventsErr and PodsErr fail Events and Pods alone.
	EventsErr, PodsErr error
}

// Namespace is the fake's game namespace.
const Namespace = "games"

// New returns a fake cluster with the given number of Ready GameServers.
func New(name string, ready int) *Fake {
	return &Fake{name: name, ready: ready, rooms: map[string]cluster.Room{}, capacity: 1, retiring: map[string]bool{}, ended: map[string][]string{},
		fleets: map[string]*managed{}, autoscalers: map[string]Autoscaler{}, secrets: map[string]cluster.Secret{}}
}

// Fail makes every call return err; Fail(nil) restores normal behaviour.
func (f *Fake) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Contend makes the next n allocations report contention.
func (f *Fake) Contend(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contention = n
}

// Allocations reports how many matches were allocated.
func (f *Fake) Allocations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allocations
}

func (f *Fake) Allocate(_ context.Context, a cluster.Allocation) (cluster.Room, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.err != nil:
		return cluster.Room{}, f.err
	case f.contention > 0:
		f.contention--
		return cluster.Room{}, cluster.ErrContention
	}
	match := cluster.Match{ID: a.MatchID, MatchInfo: cluster.MatchInfo{Players: a.Players, Channel: a.Channel, Compat: a.Compat}}
	// Like Agones with the rooms list: a running game server of the Fleet with
	// a.MinAvailable places left first, the fullest one, then a Ready one.
	var best *cluster.Room
	for id, room := range f.rooms {
		if room.Fleet == a.Fleet && room.State == "Allocated" && room.Free() >= a.MinAvailable() && !f.retiring[id] &&
			(best == nil || room.Free() < best.Free() || room.Free() == best.Free() && room.ID < best.ID) {
			r := room
			best = &r
		}
	}
	if best == nil {
		if f.ready == 0 {
			return cluster.Room{}, cluster.ErrNoCapacity
		}
		f.ready--
		f.servers++
		best = &cluster.Room{
			ID:       fmt.Sprintf("%s-%d", a.Fleet, f.servers),
			Cluster:  f.name,
			State:    "Allocated",
			Address:  "203.0.113.10",
			Port:     7000 + f.servers,
			Fleet:    a.Fleet,
			Capacity: f.capacity,
		}
	}
	f.allocations++
	best.Matches = append(best.Matches, match)
	f.rooms[best.ID] = *best
	room := *best
	room.MatchID, room.Players = a.MatchID, a.Players
	return room, nil
}

// SetCapacity sets how many matches each new game server runs; the default is 1.
func (f *Fake) SetCapacity(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacity = n
}

// ReleaseMatch is the game server removing a match that is over from its
// rooms list. GameFleet's note about it stays until ForgetMatches.
func (f *Fake) ReleaseMatch(room, matchID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rooms[room]
	if !ok {
		return
	}
	for i, m := range r.Matches {
		if m.ID == matchID {
			r.Matches = append(r.Matches[:i:i], r.Matches[i+1:]...)
			r.Stale = append(r.Stale, matchID)
		}
	}
	f.rooms[room] = r
}

// Retiring reports whether MarkRetiring took the game server out of allocation.
func (f *Fake) Retiring(room string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retiring[room]
}

func (f *Fake) FindByMatch(_ context.Context, matchID string) (*cluster.Room, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	for _, room := range f.rooms {
		if m, ok := room.Match(matchID); ok {
			room.MatchID, room.Players = m.ID, m.Players
			return &room, nil
		}
	}
	return nil, nil
}

func (f *Fake) ForgetMatches(_ context.Context, id string, matchIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	r, ok := f.rooms[id]
	if !ok {
		return nil
	}
	forget := map[string]bool{}
	for _, m := range matchIDs {
		forget[m] = true
	}
	stale := r.Stale[:0:0]
	for _, m := range r.Stale {
		if !forget[m] {
			stale = append(stale, m)
		}
	}
	r.Stale = stale
	f.rooms[id] = r
	return nil
}

func (f *Fake) EndMatch(_ context.Context, id, matchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if _, ok := f.rooms[id]; ok {
		f.ended[id] = append(f.ended[id], matchID)
	}
	return nil
}

// Ended lists the matches EndMatch asked the game server to end.
func (f *Fake) Ended(room string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ended[room]...)
}

func (f *Fake) MarkRetiring(_ context.Context, id string, retiring bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if retiring {
		f.retiring[id] = true
	} else {
		delete(f.retiring, id)
	}
	if r, ok := f.rooms[id]; ok {
		r.Retiring = retiring
		f.rooms[id] = r
	}
	return nil
}

func (f *Fake) Get(_ context.Context, id string) (*cluster.Room, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if room, ok := f.rooms[id]; ok {
		return &room, nil
	}
	return nil, nil
}

func (f *Fake) Shutdown(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.rooms, id)
	return nil
}

// SetView sets what the console's read methods return.
func (f *Fake) SetView(v View) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.view = v
}

// View returns what the console's read methods return.
func (f *Fake) View() View {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view
}

// AddRoom adds a GameServer in any state.
func (f *Fake) AddRoom(room cluster.Room) {
	f.mu.Lock()
	defer f.mu.Unlock()
	room.Cluster = f.name
	f.rooms[room.ID] = room
}

func (f *Fake) Namespace() string { return Namespace }

func (f *Fake) Nodes(context.Context) ([]cluster.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.view.Nodes), f.err
}

func (f *Fake) Pods(_ context.Context, node string) ([]cluster.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.view.PodsErr != nil {
		return nil, f.view.PodsErr
	}
	pods := []cluster.Pod{}
	for _, p := range f.view.Pods {
		if node == "" || p.Node == node {
			pods = append(pods, p)
		}
	}
	return pods, nil
}

func (f *Fake) Pod(_ context.Context, namespace, name string) (*cluster.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	for _, p := range f.view.Pods {
		if p.Namespace == namespace && p.Name == name {
			return &p, nil
		}
	}
	return nil, nil
}

func (f *Fake) Fleets(context.Context) ([]cluster.Fleet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view.Fleets, f.err
}

func (f *Fake) Rooms(context.Context) ([]cluster.Room, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	rooms := []cluster.Room{}
	for _, r := range f.rooms {
		rooms = append(rooms, r)
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].ID < rooms[j].ID })
	return rooms, nil
}

func (f *Fake) Events(_ context.Context, namespace, object string) ([]cluster.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.view.EventsErr != nil {
		return nil, f.view.EventsErr
	}
	events := []cluster.Event{}
	for _, e := range f.view.Events {
		if (namespace == "" || e.Namespace == namespace) && (object == "" || e.Name == object) {
			events = append(events, e)
		}
	}
	return events, nil
}

// Log returns the log the view has under "namespace/pod/container", or
// under that and "#previous" for the previous run. It keeps the newest
// q.Lines lines.
func (f *Fake) Log(_ context.Context, namespace, pod string, q cluster.LogQuery) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logQuery = q
	key := namespace + "/" + pod + "/" + q.Container
	if q.Previous {
		key += "#previous"
	}
	lines := strings.SplitAfter(f.view.Logs[key], "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if q.Lines > 0 && len(lines) > q.Lines {
		lines = lines[len(lines)-q.Lines:]
	}
	return strings.Join(lines, ""), f.err
}

// LogQuery returns what the last read of a log asked for.
func (f *Fake) LogQuery() cluster.LogQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logQuery
}

func (f *Fake) Usage(context.Context) (cluster.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return cluster.Usage{}, f.err
	}
	return f.view.Usage, f.view.UsageErr
}

// ManagedFleet returns the Fleet GameFleet created with this name and its spec.
func (f *Fake) ManagedFleet(name string) (cluster.ManagedFleet, cluster.FleetSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.fleets[name]
	if !ok {
		return cluster.ManagedFleet{}, cluster.FleetSpec{}, false
	}
	return m.fleet, m.spec, true
}

// SetAllocated sets how many matches run on a managed Fleet.
func (f *Fake) SetAllocated(name string, allocated int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.fleets[name]; ok {
		m.fleet.Allocated = allocated
	}
}

// Autoscaler returns the autoscaler of a Fleet.
func (f *Fake) Autoscaler(fleet string) (Autoscaler, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.autoscalers[fleet]
	return a, ok
}

func (f *Fake) ManagedFleets(context.Context) ([]cluster.ManagedFleet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	fleets := []cluster.ManagedFleet{}
	for _, m := range f.fleets {
		fleets = append(fleets, m.fleet)
	}
	sort.Slice(fleets, func(i, j int) bool { return fleets[i].Name < fleets[j].Name })
	return fleets, nil
}

func (f *Fake) CreateFleet(_ context.Context, spec cluster.FleetSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if _, ok := f.fleets[spec.Name]; !ok {
		f.fleets[spec.Name] = &managed{spec: spec, fleet: cluster.ManagedFleet{
			Name: spec.Name, Deployment: spec.Deployment, Replicas: spec.Replicas, Current: spec.Replicas, Ready: spec.Replicas,
		}}
	}
	return nil
}

func (f *Fake) ScaleFleet(_ context.Context, name string, replicas int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	m, ok := f.fleets[name]
	if !ok {
		return fmt.Errorf("no fleet %s", name)
	}
	m.fleet.Replicas = replicas
	m.fleet.Ready = max(replicas-m.fleet.Allocated, 0)
	return nil
}

func (f *Fake) DeleteFleet(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.fleets, name)
	// As in a cluster, the Fleet's game servers go with it, matches and all.
	for id, r := range f.rooms {
		if r.Fleet == name {
			delete(f.rooms, id)
		}
	}
	return nil
}

func (f *Fake) ApplyAutoscaler(_ context.Context, fleet string, deployment int64, buffer, maxReplicas int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.autoscalers[fleet] = Autoscaler{Deployment: deployment, Buffer: buffer, MaxReplicas: maxReplicas}
	return nil
}

func (f *Fake) DeleteAutoscaler(_ context.Context, fleet string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.autoscalers, fleet)
	return nil
}

func (f *Fake) Name() string { return f.name }

func (f *Fake) Check(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *Fake) node(name string) *cluster.Node {
	for i := range f.view.Nodes {
		if f.view.Nodes[i].Name == name {
			return &f.view.Nodes[i]
		}
	}
	return nil
}

func (f *Fake) RetireNode(_ context.Context, name string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	n := f.node(name)
	if n == nil {
		return fmt.Errorf("no node %s", name)
	}
	n.Unschedulable, n.Retiring = true, at
	return nil
}

func (f *Fake) NameNode(_ context.Context, name, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	n := f.node(name)
	if n == nil {
		return fmt.Errorf("no node %s", name)
	}
	n.Given = title
	return nil
}

func (f *Fake) KeepNode(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	n := f.node(name)
	if n == nil {
		return fmt.Errorf("no node %s", name)
	}
	n.Unschedulable, n.Retiring = false, time.Time{}
	return nil
}

func (f *Fake) DeleteNode(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	nodes := f.view.Nodes[:0]
	for _, n := range f.view.Nodes {
		if n.Name != name {
			nodes = append(nodes, n)
		}
	}
	f.view.Nodes = nodes
	f.deleted = append(f.deleted, name)
	return nil
}

// DeletedNodes lists the nodes removed with DeleteNode.
func (f *Fake) DeletedNodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *Fake) ApplySecret(_ context.Context, s cluster.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.secrets[s.Name] = s
	return nil
}

// Secret returns a Secret written with ApplySecret.
func (f *Fake) Secret(name string) (cluster.Secret, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.secrets[name]
	return s, ok
}

var _ cluster.API = (*Fake)(nil)
