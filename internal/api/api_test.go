package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/allocate"
	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/registry"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

const (
	nakamaKey = "nakama-test-key-0123456789abcdef0123456789"
	gmKey     = "gm-test-key-0123456789abcdef0123456789abcdef"
	ciKey     = "ci-test-key-0123456789abcdef0123456789abcdef"
	match     = `{"matchId":"m-1","game":"billiards","region":"us","channel":"stable","compat":"dm-v7",
		"players":[{"userId":"u-1","seat":0},{"userId":"u-2","seat":1}]}`
)

// images is a registry holding images by tag.
type images map[string]registry.Image

func (i images) Inspect(_ context.Context, _, ref string) (registry.Image, error) {
	for tag, img := range i {
		if ref == tag || ref == img.Digest {
			return img, nil
		}
	}
	return registry.Image{}, registry.ErrNotFound
}

func image(digest, version string, labels ...string) registry.Image {
	img := registry.Image{Digest: digest, Port: 7770, Protocol: "UDP", Labels: map[string]string{}}
	for i := 0; i+1 < len(labels); i += 2 {
		img.Labels[labels[i]] = labels[i+1]
	}
	return img
}

// newServer runs the API with build 1 deployed in us-1 and routed on stable.
func newServer(t *testing.T, ready int) (*httptest.Server, *clustertest.Fake) {
	t.Helper()
	return newServerWith(t, ready, func(r Releases) Releases { return r })
}

// newServerWith is newServer with the releases the API sees passed through
// wrap.
func newServerWith(t *testing.T, ready int, wrap func(Releases) Releases) (*httptest.Server, *clustertest.Fake) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "gamefleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keys := auth.NewKeys(db)
	for name, k := range map[string][]string{nakamaKey: {"nakama", auth.ScopeAllocate}, gmKey: {"gm", auth.ScopeGM}, ciKey: {"ci", auth.ScopePush}} {
		if err := keys.Add(ctx, k[0], k[1:], name, "test"); err != nil {
			t.Fatal(err)
		}
	}
	full := []string{release.LabelCompat, "dm-v7", release.LabelVersion, "142", release.LabelRevision, "abc"}
	reg := images{
		"142":      image("sha256:142", "142", full...),
		"143":      image("sha256:143", "143", release.LabelCompat, "dm-v7", release.LabelVersion, "143", release.LabelRevision, "def"),
		"unlabled": image("sha256:000", "0"),
	}
	fake := clustertest.New("us-1", ready)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: fake})
	releases := release.New(db, reg, "fleet.example.com", dir, 24*time.Hour, log)
	b, _, err := releases.Register(ctx, release.Registration{Repository: "billiards", Tag: "142"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releases.Deploy(ctx, release.DeployRequest{BuildID: b.ID, Cluster: "us-1", Buffer: 2, MaxReplicas: 4, Rooms: 1, Resources: release.Defaults.Resources}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := releases.SetRoute(ctx, release.Stable, b.ID, "test"); err != nil {
		t.Fatal(err)
	}
	svc := allocate.New(releases, dir)
	server := httptest.NewServer(Handler(svc, wrap(releases), keys, log))
	t.Cleanup(server.Close)
	return server, fake
}

func call(t *testing.T, server *httptest.Server, method, path, key, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode response: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func TestHealthzNeedsNoKey(t *testing.T) {
	server, _ := newServer(t, 1)
	// The version tells an update apart from the GameFleet it replaces.
	if status, out := call(t, server, http.MethodGet, "/healthz", "", ""); status != http.StatusOK || out["version"] != "dev" {
		t.Fatalf("healthz = %d %v", status, out)
	}
}

func TestKeysAndScopes(t *testing.T) {
	server, _ := newServer(t, 1)
	cases := []struct {
		name, method, path, key string
		want                    int
	}{
		{"no key", http.MethodGet, "/api/v1/routes", "", http.StatusUnauthorized},
		{"unknown key", http.MethodGet, "/api/v1/routes", "guess", http.StatusUnauthorized},
		{"gm cannot allocate", http.MethodPost, "/api/v1/allocations", gmKey, http.StatusForbidden},
		{"gm cannot read routes", http.MethodGet, "/api/v1/routes", gmKey, http.StatusForbidden},
		{"nakama cannot end rooms", http.MethodDelete, "/api/v1/rooms/any-room", nakamaKey, http.StatusForbidden},
		{"gm can look rooms up", http.MethodGet, "/api/v1/rooms/any-room", gmKey, http.StatusNotFound},
		{"nakama cannot register builds", http.MethodPost, "/api/v1/builds", nakamaKey, http.StatusForbidden},
		{"ci cannot allocate", http.MethodPost, "/api/v1/allocations", ciKey, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if status, _ := call(t, server, tc.method, tc.path, tc.key, match); status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
		})
	}
}

func TestAllocateThenRetry(t *testing.T) {
	server, fake := newServer(t, 2)

	status, first := call(t, server, http.MethodPost, "/api/v1/allocations", nakamaKey, match)
	if status != http.StatusCreated || first["roomId"] == "" || first["address"] != "203.0.113.10" || first["port"] == nil || first["build"] != "1" {
		t.Fatalf("first: %d %v", status, first)
	}
	status, second := call(t, server, http.MethodPost, "/api/v1/allocations", nakamaKey, match)
	if status != http.StatusOK || second["roomId"] != first["roomId"] {
		t.Fatalf("retry: %d %v", status, second)
	}
	if fake.Allocations() != 1 {
		t.Fatalf("allocations = %d", fake.Allocations())
	}
}

func TestAllocationErrors(t *testing.T) {
	cases := []struct {
		name  string
		ready int
		body  string
		want  int
		code  string
	}{
		{"malformed json", 1, `{"matchId":`, http.StatusBadRequest, "invalid_request"},
		{"unknown field", 1, `{"matchId":"m-1","team":"red"}`, http.StatusBadRequest, "invalid_request"},
		{"no route", 1, strings.Replace(match, "dm-v7", "dm-v8", 1), http.StatusNotFound, "route_not_found"},
		{"no ready server", 0, match, http.StatusConflict, "no_capacity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newServer(t, tc.ready)
			status, out := call(t, server, http.MethodPost, "/api/v1/allocations", nakamaKey, tc.body)
			if status != tc.want || out["error"] != tc.code {
				t.Fatalf("got %d %v, want %d %s", status, out, tc.want, tc.code)
			}
		})
	}
}

func TestClusterDownIsServiceUnavailable(t *testing.T) {
	server, fake := newServer(t, 1)
	fake.Fail(cluster.ErrUnavailable)
	status, out := call(t, server, http.MethodPost, "/api/v1/allocations", nakamaKey, match)
	if status != http.StatusServiceUnavailable || out["error"] != "cluster_unavailable" {
		t.Fatalf("got %d %v", status, out)
	}
}

func TestRoomLookupAndShutdown(t *testing.T) {
	server, _ := newServer(t, 1)
	_, allocated := call(t, server, http.MethodPost, "/api/v1/allocations", nakamaKey, match)
	id := allocated["roomId"].(string)

	// A room is a game server, which runs several matches: by name it names none.
	if status, room := call(t, server, http.MethodGet, "/api/v1/rooms/"+id, nakamaKey, ""); status != http.StatusOK || room["roomId"] != id || room["matchId"] != nil {
		t.Fatalf("lookup: %d %v", status, room)
	}
	status, list := call(t, server, http.MethodGet, "/api/v1/rooms?matchId=m-1", gmKey, "")
	rooms, _ := list["rooms"].([]any)
	if status != http.StatusOK || len(rooms) != 1 || rooms[0].(map[string]any)["matchId"] != "m-1" || rooms[0].(map[string]any)["players"] == nil {
		t.Fatalf("by match: %d %v", status, list)
	}
	if status, _ := call(t, server, http.MethodDelete, "/api/v1/rooms/"+id, gmKey, ""); status != http.StatusAccepted {
		t.Fatalf("shutdown: %d", status)
	}
	if status, out := call(t, server, http.MethodGet, "/api/v1/rooms/"+id, nakamaKey, ""); status != http.StatusNotFound || out["error"] != "room_not_found" {
		t.Fatalf("after shutdown: %d %v", status, out)
	}
	if status, _ := call(t, server, http.MethodDelete, "/api/v1/rooms/"+id, gmKey, ""); status != http.StatusNotFound {
		t.Fatalf("second shutdown: %d", status)
	}
}

func TestEndOneMatch(t *testing.T) {
	server, fake := newServer(t, 1)
	_, allocated := call(t, server, http.MethodPost, "/api/v1/allocations", nakamaKey, match)
	id := allocated["roomId"].(string)

	if status, _ := call(t, server, http.MethodPost, "/api/v1/matches/m-1/end", nakamaKey, ""); status != http.StatusForbidden {
		t.Fatalf("allocate key ended a match: %d", status)
	}
	if status, out := call(t, server, http.MethodPost, "/api/v1/matches/m-1/end", gmKey, ""); status != http.StatusAccepted || out["roomId"] != id {
		t.Fatalf("end: %d %v", status, out)
	}
	if ended := fake.Ended(id); len(ended) != 1 || ended[0] != "m-1" {
		t.Fatalf("ended = %v", ended)
	}
	if status, out := call(t, server, http.MethodPost, "/api/v1/matches/m-9/end", gmKey, ""); status != http.StatusNotFound || out["error"] != "room_not_found" {
		t.Fatalf("unknown match: %d %v", status, out)
	}
}

// unaudited is releases with an audit log that cannot be written.
type unaudited struct{ Releases }

func (unaudited) Record(context.Context, string, string, string, string) error {
	return errors.New("disk full")
}

// A GM action that happened is answered as done, saying whether the audit
// log has it.
func TestGMActionsSayWhetherTheyWereAudited(t *testing.T) {
	for _, audit := range []bool{true, false} {
		wrap := func(r Releases) Releases { return r }
		if !audit {
			wrap = func(r Releases) Releases { return unaudited{r} }
		}
		server, _ := newServerWith(t, 2, wrap)
		_, first := call(t, server, http.MethodPost, "/api/v1/allocations", nakamaKey, match)
		if status, out := call(t, server, http.MethodPost, "/api/v1/matches/m-1/end", gmKey, ""); status != http.StatusAccepted || out["audited"] != audit {
			t.Fatalf("audit log works %v: end = %d %v", audit, status, out)
		}
		if status, out := call(t, server, http.MethodDelete, "/api/v1/rooms/"+first["roomId"].(string), gmKey, ""); status != http.StatusAccepted || out["audited"] != audit {
			t.Fatalf("audit log works %v: shutdown = %d %v", audit, status, out)
		}
	}
}

func TestRoutesForOneGame(t *testing.T) {
	server, _ := newServer(t, 0)
	status, out := call(t, server, http.MethodGet, "/api/v1/routes?game=billiards", nakamaKey, "")
	routes, _ := out["routes"].([]any)
	if status != http.StatusOK || len(routes) != 1 {
		t.Fatalf("got %d %v", status, out)
	}
	route := routes[0].(map[string]any)
	if route["channel"] != "stable" || route["compat"] != "dm-v7" || route["fleet"] != nil {
		t.Fatalf("route = %v (the fleet name stays internal)", route)
	}
	if regions, _ := route["regions"].([]any); len(regions) != 1 || regions[0] != "us" {
		t.Fatalf("regions = %v", route["regions"])
	}
}

func TestRegisterBuilds(t *testing.T) {
	server, _ := newServer(t, 0)
	status, out := call(t, server, http.MethodPost, "/api/v1/builds", ciKey, `{"repository":"billiards","tag":"143","note":"ci run 143"}`)
	if status != http.StatusCreated || out["build"] != float64(2) || out["version"] != "143" || out["compat"] != "dm-v7" || out["digest"] != "sha256:143" {
		t.Fatalf("register: %d %v", status, out)
	}
	if status, out := call(t, server, http.MethodPost, "/api/v1/builds", ciKey, `{"repository":"billiards","digest":"sha256:143"}`); status != http.StatusOK || out["build"] != float64(2) {
		t.Fatalf("register again: %d %v", status, out)
	}
	cases := []struct {
		body string
		want int
		code string
	}{
		{`{"repository":"billiards","tag":"999"}`, http.StatusNotFound, "image_not_found"},
		{`{"repository":"billiards","tag":"unlabled"}`, http.StatusUnprocessableEntity, "image_unusable"},
		{`{"repository":"billiards"}`, http.StatusBadRequest, "invalid_request"},
		{`{"repo":"billiards"}`, http.StatusBadRequest, "invalid_request"},
	}
	for _, c := range cases {
		status, out := call(t, server, http.MethodPost, "/api/v1/builds", ciKey, c.body)
		if status != c.want || out["error"] != c.code || out["message"] == "" {
			t.Errorf("%s: %d %v, want %d %s with a message", c.body, status, out, c.want, c.code)
		}
	}
}
