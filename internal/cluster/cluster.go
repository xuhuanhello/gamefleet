// Package cluster reads one K3s cluster for the console, allocates and ends
// its Agones game servers, runs Fleets, retires nodes and writes the Secrets
// game servers need. It is the only code in GameFleet that talks to
// Kubernetes. It calls the Kubernetes API directly over HTTPS with a
// ServiceAccount token, so GameFleet needs neither a Kubernetes client library
// nor an agent on the hosts.
package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What GameFleet keeps on game servers. A game server runs several matches;
// the console and GM tools read who plays them, GameFleet itself never
// interprets players.
const (
	LabelGame = "gamefleet.io/game"
	// LabelRetiring marks the game servers on a machine being retired: they
	// take no new matches.
	LabelRetiring = "gamefleet.io/retiring"
	// ListRooms is the Agones list of the match IDs a game server runs.
	// Allocation adds one; the game server removes it when the match is over.
	ListRooms = "rooms"
	// AnnotationMatchPrefix followed by a match ID holds that match's
	// MatchInfo as JSON.
	AnnotationMatchPrefix = "gamefleet.io/match."
	// MaxMatchIDLength is the longest match ID that fits after
	// AnnotationMatchPrefix: the name of an annotation, after its
	// "gamefleet.io/", holds at most 63 characters, and "match." takes 6.
	MaxMatchIDLength = 57
	// AnnotationEndPrefix followed by a match ID asks the game server to end
	// that match; the value is when it was asked.
	AnnotationEndPrefix = "gamefleet.io/end."

	labelFleet = "agones.dev/fleet"
	// Agones sets this on every allocation, RFC 3339.
	annotationLastAllocated = "agones.dev/last-allocated"
)

var (
	// ErrNoCapacity means the Fleet had no Ready GameServer to allocate.
	ErrNoCapacity = errors.New("no ready game server")
	// ErrContention means Agones lost an allocation race; a retry may succeed.
	ErrContention = errors.New("allocation contention")
	// ErrUnavailable means the cluster could not be reached or refused GameFleet.
	ErrUnavailable = errors.New("cluster unavailable")

	errNotFound = errors.New("not found")
	errConflict = errors.New("already exists")
)

// Room is an Agones GameServer as GameFleet reports it: one game server
// process, running up to Capacity matches at once.
type Room struct {
	ID      string `json:"roomId"`
	Cluster string `json:"cluster"`
	State   string `json:"state"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	Fleet   string `json:"fleet"`
	Build   string `json:"build,omitempty"`
	// The match an answer is about: the one just allocated or looked up.
	MatchID string          `json:"matchId,omitempty"`
	Players json.RawMessage `json:"players,omitempty"`

	// Only the console shows these; they are not part of the API.
	Matches  []Match   `json:"-"`
	Capacity int       `json:"-"`
	Node     string    `json:"-"`
	Image    string    `json:"-"`
	Game     string    `json:"-"`
	Created  time.Time `json:"-"`
	// When Agones last gave it a match; zero if it never did.
	LastAllocated time.Time `json:"-"`
	// Retiring game servers take no new matches.
	Retiring bool `json:"-"`
	// Matches whose annotations (their note, or a request to end them) are
	// still there though the game server no longer runs them.
	Stale []string `json:"-"`
	// Notes is how much all its annotations take, keys and values.
	Notes int `json:"-"`
}

// Match is one match a game server runs.
type Match struct {
	ID string
	MatchInfo
}

// MatchInfo is what GameFleet notes about a match on its game server.
type MatchInfo struct {
	Players   json.RawMessage `json:"players"`
	Channel   string          `json:"channel"`
	Compat    string          `json:"compat"`
	Allocated time.Time       `json:"allocated"`
}

// Match returns the match with this ID if the game server runs it.
func (r Room) Match(id string) (Match, bool) {
	for _, m := range r.Matches {
		if m.ID == id {
			return m, true
		}
	}
	return Match{}, false
}

// Free is how many more matches the game server can take.
func (r Room) Free() int { return max(0, r.Capacity-len(r.Matches)) }

// Kubernetes keeps all the annotations of an object under 256 KiB, and a
// game server carries a note for each match it runs, one for each that is
// over until the loop forgets it, and one for each it is asked to end. So a
// match's note, key and value, is at most MaxMatchNote, and a game server
// runs at most MaxRooms matches at a time; TestNotesFitInAGameServer checks
// that these leave room for Agones' own annotations.
const (
	MaxMatchNote = 2 << 10
	MaxRooms     = 80
	// FullNotes is how much a game server's annotations may take before it
	// stops taking matches: notes of matches that are over pile up when
	// forgetting them keeps failing, or when its matches end and start
	// again faster than the loop passes. It finishes the matches it runs,
	// and is replaced once empty. A quarter of Kubernetes' 256 KiB stays
	// for what piles up meanwhile.
	FullNotes = 192 << 10
)

// NoteSize is how much the note of the allocation's match takes, key and
// value, at its longest.
func NoteSize(a Allocation) int {
	latest := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	info, _ := json.Marshal(MatchInfo{Players: a.Players, Channel: a.Channel, Compat: a.Compat, Allocated: latest})
	return len(AnnotationMatchPrefix) + len(a.MatchID) + len(info)
}

// Allocation describes the match that needs a room.
type Allocation struct {
	Fleet   string
	MatchID string
	Game    string
	Channel string
	Compat  string
	Players json.RawMessage
}

// Client talks to one cluster.
type Client struct {
	name      string
	server    string
	namespace string
	token     string
	http      *http.Client
}

// Connect returns a client for the Kubernetes API at server
// (https://host:6443) that trusts the cluster's CA certificate (PEM) and
// authenticates with a ServiceAccount token.
func Connect(name, server string, ca []byte, token, namespace string) (*Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("cluster %s: the CA holds no PEM certificate", name)
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: 4,
	}
	return &Client{
		name:      name,
		server:    strings.TrimRight(server, "/"),
		namespace: namespace,
		token:     strings.TrimSpace(token),
		http: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Allocate gives the match a place on one of the Fleet's game servers: one
// that already runs matches and has room, the fullest first so that idle
// game servers stay idle, otherwise a Ready one. It adds the match ID to the
// game server's rooms list and notes the match's players on it. Agones
// changes the list atomically, so two matches never take the same place.
func (c *Client) Allocate(ctx context.Context, a Allocation) (Room, error) {
	info, err := json.Marshal(MatchInfo{Players: a.Players, Channel: a.Channel, Compat: a.Compat, Allocated: time.Now().UTC()})
	if err != nil {
		return Room{}, err
	}
	selector := func(state string) map[string]any {
		return map[string]any{
			"matchLabels":      map[string]string{labelFleet: a.Fleet},
			"matchExpressions": []any{map[string]any{"key": LabelRetiring, "operator": "DoesNotExist"}},
			"gameServerState":  state,
			"lists":            map[string]any{ListRooms: map[string]any{"minAvailable": 1}},
		}
	}
	request := map[string]any{
		"apiVersion": "allocation.agones.dev/v1",
		"kind":       "GameServerAllocation",
		"spec": map[string]any{
			"selectors":  []any{selector("Allocated"), selector("Ready")},
			"priorities": []any{map[string]any{"type": "List", "key": ListRooms, "order": "Ascending"}},
			"lists":      map[string]any{ListRooms: map[string]any{"addValues": []string{a.MatchID}}},
			"metadata": map[string]any{
				"labels":      map[string]string{LabelGame: a.Game},
				"annotations": map[string]string{AnnotationMatchPrefix + a.MatchID: string(info)},
			},
		},
	}
	var response struct {
		Status struct {
			State          string       `json:"state"`
			GameServerName string       `json:"gameServerName"`
			Address        string       `json:"address"`
			Ports          []statusPort `json:"ports"`
		} `json:"status"`
	}
	path := apiPath("allocation.agones.dev", c.namespace, "gameserverallocations", "")
	if err := c.do(ctx, http.MethodPost, path, request, &response); err != nil {
		return Room{}, err
	}
	switch response.Status.State {
	case "Allocated":
		return Room{
			ID:      response.Status.GameServerName,
			Cluster: c.name,
			State:   "Allocated",
			Address: response.Status.Address,
			Port:    firstPort(response.Status.Ports),
			Fleet:   a.Fleet,
			MatchID: a.MatchID,
			Players: a.Players,
		}, nil
	case "UnAllocated":
		return Room{}, ErrNoCapacity
	case "Contention":
		return Room{}, ErrContention
	default:
		return Room{}, fmt.Errorf("%w: cluster %s returned allocation state %q", ErrUnavailable, c.name, response.Status.State)
	}
}

// FindByMatch returns the game server that runs the match, or nil. Should a
// retried allocation have placed the match twice, the oldest one counts.
func (c *Client) FindByMatch(ctx context.Context, matchID string) (*Room, error) {
	rooms, err := c.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	return FindMatch(rooms, matchID), nil
}

// FindMatch returns the room among rooms that runs the match, with the
// match's ID and players filled in, or nil.
func FindMatch(rooms []Room, matchID string) *Room {
	for _, room := range rooms {
		if m, ok := room.Match(matchID); ok {
			room.MatchID, room.Players = m.ID, m.Players
			return &room
		}
	}
	return nil
}

// ForgetMatches removes what GameFleet noted about matches the game server
// no longer runs.
func (c *Client) ForgetMatches(ctx context.Context, name string, matchIDs []string) error {
	annotations := map[string]any{}
	for _, id := range matchIDs {
		annotations[AnnotationMatchPrefix+id] = nil
		annotations[AnnotationEndPrefix+id] = nil
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": annotations}}
	err := c.send(ctx, http.MethodPatch, apiPath("agones.dev", c.namespace, "gameservers", name), "application/merge-patch+json", patch, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// EndMatch asks the game server to end one of its matches. The game server
// closes it as void and gives its place back as for any match that is over.
// A game server that is already gone has nothing to end.
func (c *Client) EndMatch(ctx context.Context, name, matchID string) error {
	at := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationEndPrefix + matchID: at}}}
	err := c.send(ctx, http.MethodPatch, apiPath("agones.dev", c.namespace, "gameservers", name), "application/merge-patch+json", patch, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// MarkRetiring stops the game server from taking new matches, or lets it
// take them again.
func (c *Client) MarkRetiring(ctx context.Context, name string, retiring bool) error {
	var value any
	if retiring {
		value = "true"
	}
	patch := map[string]any{"metadata": map[string]any{"labels": map[string]any{LabelRetiring: value}}}
	err := c.send(ctx, http.MethodPatch, apiPath("agones.dev", c.namespace, "gameservers", name), "application/merge-patch+json", patch, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// Get returns the GameServer with this name, or nil when it does not exist.
func (c *Client) Get(ctx context.Context, name string) (*Room, error) {
	var gs gameServer
	err := c.get(ctx, apiPath("agones.dev", c.namespace, "gameservers", name), nil, &gs)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	room := c.room(gs)
	return &room, nil
}

// Shutdown deletes the GameServer. Deleting one that is already gone succeeds.
func (c *Client) Shutdown(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, apiPath("agones.dev", c.namespace, "gameservers", name), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

type statusPort struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

type gameServer struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		State    string       `json:"state"`
		Address  string       `json:"address"`
		Ports    []statusPort `json:"ports"`
		NodeName string       `json:"nodeName"`
		Lists    map[string]struct {
			Capacity int      `json:"capacity"`
			Values   []string `json:"values"`
		} `json:"lists"`
	} `json:"status"`
}

type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	OwnerReferences   []struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Controller bool   `json:"controller"`
	} `json:"ownerReferences"`
}

func (c *Client) room(gs gameServer) Room {
	room := Room{
		ID:       gs.Metadata.Name,
		Cluster:  c.name,
		State:    gs.Status.State,
		Address:  gs.Status.Address,
		Port:     firstPort(gs.Status.Ports),
		Fleet:    gs.Metadata.Labels[labelFleet],
		Build:    gs.Metadata.Labels[LabelBuild],
		Node:     gs.Status.NodeName,
		Game:     gs.Metadata.Labels[LabelGame],
		Created:  gs.Metadata.CreationTimestamp,
		Retiring: gs.Metadata.Labels[LabelRetiring] != "",
	}
	if containers := gs.Spec.Template.Spec.Containers; len(containers) > 0 {
		room.Image = containers[0].Image
	}
	room.LastAllocated, _ = time.Parse(time.RFC3339Nano, gs.Metadata.Annotations[annotationLastAllocated])
	rooms := gs.Status.Lists[ListRooms]
	room.Capacity = rooms.Capacity
	running := map[string]bool{}
	for _, id := range rooms.Values {
		running[id] = true
		m := Match{ID: id}
		// A note that does not parse still leaves the match listed.
		_ = json.Unmarshal([]byte(gs.Metadata.Annotations[AnnotationMatchPrefix+id]), &m.MatchInfo)
		room.Matches = append(room.Matches, m)
	}
	for key, value := range gs.Metadata.Annotations {
		room.Notes += len(key) + len(value)
	}
	stale := map[string]bool{}
	for key := range gs.Metadata.Annotations {
		for _, prefix := range []string{AnnotationMatchPrefix, AnnotationEndPrefix} {
			if id, ok := strings.CutPrefix(key, prefix); ok && !running[id] && !stale[id] {
				stale[id] = true
				room.Stale = append(room.Stale, id)
			}
		}
	}
	sort.Strings(room.Stale)
	sort.SliceStable(room.Matches, func(i, j int) bool { return room.Matches[i].Allocated.Before(room.Matches[j].Allocated) })
	return room
}

func firstPort(ports []statusPort) int {
	if len(ports) == 0 {
		return 0
	}
	return ports[0].Port
}

// apiPath builds the path of a resource or one object of it. Group "" is the
// core API, namespace "" means every namespace or a cluster-wide resource.
func apiPath(group, namespace, resource, name string) string {
	p := "/api/v1"
	switch group {
	case "":
	case "metrics.k8s.io":
		p = "/apis/metrics.k8s.io/v1beta1"
	default:
		p = "/apis/" + group + "/v1"
	}
	if namespace != "" {
		p += "/namespaces/" + url.PathEscape(namespace)
	}
	p += "/" + resource
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

// listPage is how many objects GameFleet asks for at a time. A game server
// with many matches carries a note for each, so pages stay small.
const listPage = 100

// maxResponse is the most GameFleet reads of one response. A page of
// listPage game servers is far below it, notes included: Kubernetes keeps an
// object's annotations under 256 KiB.
const maxResponse = 64 << 20

// listAll reads every item of a list, a page at a time, the way Kubernetes
// pages it: from one snapshot, which it keeps for a few minutes. A page that
// fails, an expired snapshot say, fails the list: the pages read before it
// are never taken for all the items there are.
func listAll[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("limit", strconv.Itoa(listPage))
	var all []T
	for {
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []T `json:"items"`
		}
		if err := c.get(ctx, path, q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Metadata.Continue == "" {
			return all, nil
		}
		q.Set("continue", page.Metadata.Continue)
	}
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// do sends one request. Authentication, permission, throttling and server
// errors all mean GameFleet cannot use the cluster right now, so they map to
// ErrUnavailable; other client errors are bugs and are returned as they are.
// An out of type *[]byte receives the raw body, which is how logs are read.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.send(ctx, method, path, "application/json", in, out)
}

// send is do with the request body's content type, which PATCH needs.
func (c *Client) send(ctx context.Context, method, path, contentType string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	raw, wantRaw := out.(*[]byte)
	if wantRaw {
		req.Header.Set("Accept", "*/*")
	}
	if in != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: cluster %s: %v", ErrUnavailable, c.name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("%w: cluster %s: read response: %v", ErrUnavailable, c.name, err)
	}
	if len(data) > maxResponse {
		return fmt.Errorf("cluster %s: %s %s: the response is larger than %d MiB", c.name, method, path, maxResponse>>20)
	}

	switch code := resp.StatusCode; {
	case code == http.StatusNotFound:
		return errNotFound
	case code == http.StatusUnauthorized, code == http.StatusForbidden,
		code == http.StatusTooManyRequests, code >= 500:
		return fmt.Errorf("%w: cluster %s: %s %s returned %d", ErrUnavailable, c.name, method, path, code)
	case code == http.StatusConflict && method == http.MethodPost:
		return errConflict
	case code >= 300:
		var status struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &status)
		return fmt.Errorf("cluster %s: %s %s returned %d: %s", c.name, method, path, code, status.Message)
	}
	if out == nil {
		return nil
	}
	if wantRaw {
		*raw = data
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("cluster %s: decode %s %s: %w", c.name, method, path, err)
	}
	return nil
}
