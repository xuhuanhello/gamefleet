package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const gameServers = "/apis/agones.dev/v1/namespaces/games/gameservers"

// newTestClient starts a fake Kubernetes API over TLS and returns a client
// that trusts it the same way production trusts the cluster CA.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := Connect("us-1", server.URL, ca, "test-token\n", "games")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAllocateAddsTheMatchToAGameServerWithRoom(t *testing.T) {
	var sent map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/allocation.agones.dev/v1/namespaces/games/gameserverallocations" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"status":{"state":"Allocated","gameServerName":"billiards-142-x7k2p","address":"203.0.113.10","ports":[{"name":"default","port":7342}]}}`)
	})

	players := json.RawMessage(`[{"userId":"u-1","seat":0},{"userId":"u-2","seat":1}]`)
	room, err := client.Allocate(context.Background(), Allocation{
		Fleet: "billiards-142", MatchID: "m-1", Game: "billiards", Channel: "stable", Compat: "dm-v7", Players: players,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Room{
		ID: "billiards-142-x7k2p", Cluster: "us-1", State: "Allocated", Address: "203.0.113.10", Port: 7342,
		Fleet: "billiards-142", MatchID: "m-1", Players: players,
	}
	if !reflect.DeepEqual(room, want) {
		t.Fatalf("room = %+v, want %+v", room, want)
	}

	spec := sent["spec"].(map[string]any)
	selectors := spec["selectors"].([]any)
	if len(selectors) != 2 {
		t.Fatalf("selectors = %v", selectors)
	}
	// Running game servers with room first, then a Ready one; never one on a retiring machine.
	for i, state := range []string{"Allocated", "Ready"} {
		sel := selectors[i].(map[string]any)
		rooms := sel["lists"].(map[string]any)[ListRooms].(map[string]any)
		expr := sel["matchExpressions"].([]any)[0].(map[string]any)
		if sel["gameServerState"] != state || sel["matchLabels"].(map[string]any)["agones.dev/fleet"] != "billiards-142" ||
			rooms["minAvailable"] != float64(1) || expr["key"] != LabelRetiring || expr["operator"] != "DoesNotExist" {
			t.Errorf("selector %d = %v", i, sel)
		}
	}
	priority := spec["priorities"].([]any)[0].(map[string]any)
	if priority["type"] != "List" || priority["key"] != ListRooms || priority["order"] != "Ascending" {
		t.Errorf("priorities = %v", spec["priorities"])
	}
	added := spec["lists"].(map[string]any)[ListRooms].(map[string]any)["addValues"].([]any)
	if len(added) != 1 || added[0] != "m-1" {
		t.Errorf("lists = %v", spec["lists"])
	}
	metadata := spec["metadata"].(map[string]any)
	if labels := metadata["labels"].(map[string]any); labels[LabelGame] != "billiards" || len(labels) != 1 {
		t.Errorf("labels = %v", labels)
	}
	var info MatchInfo
	note := metadata["annotations"].(map[string]any)[AnnotationMatchPrefix+"m-1"].(string)
	if err := json.Unmarshal([]byte(note), &info); err != nil {
		t.Fatal(err)
	}
	if string(info.Players) != string(players) || info.Channel != "stable" || info.Compat != "dm-v7" || info.Allocated.IsZero() {
		t.Errorf("match note = %+v", info)
	}
}

func TestAllocateReportsWhyItGotNoRoom(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   error
	}{
		"no ready server":   {http.StatusCreated, `{"status":{"state":"UnAllocated"}}`, ErrNoCapacity},
		"contention":        {http.StatusCreated, `{"status":{"state":"Contention"}}`, ErrContention},
		"token rejected":    {http.StatusForbidden, `{}`, ErrUnavailable},
		"api server broken": {http.StatusInternalServerError, `{}`, ErrUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			_, err := client.Allocate(context.Background(), Allocation{Fleet: "f", MatchID: "m"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFindByMatchLooksInTheRoomsLists(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != gameServers {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		io.WriteString(w, `{"items":[
			{"metadata":{"name":"late","creationTimestamp":"2026-10-14T10:00:05Z"},
			 "status":{"state":"Allocated","lists":{"rooms":{"capacity":50,"values":["m-1"]}}}},
			{"metadata":{"name":"other","creationTimestamp":"2026-10-14T09:00:00Z"},
			 "status":{"state":"Allocated","lists":{"rooms":{"capacity":50,"values":["m-2"]}}}},
			{"metadata":{"name":"early","creationTimestamp":"2026-10-14T10:00:00Z","labels":{"agones.dev/fleet":"billiards-142"},
			 "annotations":{"gamefleet.io/match.m-1":"{\"players\":[{\"userId\":\"u-1\",\"seat\":0}],\"channel\":\"stable\"}"}},
			 "status":{"state":"Allocated","address":"203.0.113.10","ports":[{"name":"default","port":7001}],
			  "lists":{"rooms":{"capacity":50,"values":["m-0","m-1"]}}}}
		]}`)
	})
	room, err := client.FindByMatch(context.Background(), "m-1")
	if err != nil {
		t.Fatal(err)
	}
	if room == nil || room.ID != "early" || room.Fleet != "billiards-142" || room.Port != 7001 || room.MatchID != "m-1" ||
		string(room.Players) != `[{"userId":"u-1","seat":0}]` || room.Capacity != 50 || len(room.Matches) != 2 || room.Free() != 48 {
		t.Fatalf("room = %+v", room)
	}
}

func TestRoomsListTheirMatchesAndForgottenNotes(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[{"metadata":{"name":"gs","annotations":{
			"gamefleet.io/match.m-1":"{\"channel\":\"canary\",\"allocated\":\"2026-10-21T10:00:00Z\"}",
			"gamefleet.io/match.m-old":"{}","agones.dev/sdk-x":"y",
			"gamefleet.io/end.m-old":"2026-10-21T10:05:00Z","gamefleet.io/end.m-gone":"2026-10-21T10:05:00Z",
			"gamefleet.io/end.m-2":"2026-10-21T10:05:00Z",
			"agones.dev/last-allocated":"2026-10-21T10:00:00.123456789Z"}},
			"status":{"state":"Allocated","lists":{"rooms":{"capacity":3,"values":["m-1","m-2"]}}}}]}`)
	})
	rooms, err := client.Rooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := rooms[0]
	if len(r.Matches) != 2 || r.Matches[1].ID != "m-1" || r.Matches[1].Channel != "canary" || r.Matches[0].ID != "m-2" ||
		!reflect.DeepEqual(r.Stale, []string{"m-gone", "m-old"}) || r.Free() != 1 ||
		!r.LastAllocated.Equal(time.Date(2026, 10, 21, 10, 0, 0, 123456789, time.UTC)) {
		t.Fatalf("room = %+v", r)
	}
}

func TestFindByMatchWithoutRoom(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[]}`)
	})
	room, err := client.FindByMatch(context.Background(), "m-1")
	if err != nil || room != nil {
		t.Fatalf("room = %+v, err = %v", room, err)
	}
}

func TestGetAndShutdownTreatMissingGameServersAsGone(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == gameServers+"/live":
			io.WriteString(w, `{"metadata":{"name":"live"},"status":{"state":"Allocated","lists":{"rooms":{"capacity":2,"values":["m-1"]}}}}`)
		case r.Method == http.MethodDelete && r.URL.Path == gameServers+"/live":
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()

	if room, err := client.Get(ctx, "live"); err != nil || room == nil || len(room.Matches) != 1 || room.State != "Allocated" {
		t.Fatalf("Get(live) = %+v, %v", room, err)
	}
	if room, err := client.Get(ctx, "gone"); err != nil || room != nil {
		t.Fatalf("Get(gone) = %+v, %v", room, err)
	}
	if err := client.Shutdown(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if err := client.Shutdown(ctx, "gone"); err != nil {
		t.Fatalf("Shutdown(gone) = %v", err)
	}
}

func TestConnectRejectsACAWithoutCertificates(t *testing.T) {
	if _, err := Connect("us-1", "https://10.0.0.1:6443", []byte("not a certificate"), "token", "games"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestEndAndForgetMatchesPatchTheirAnnotations(t *testing.T) {
	var patches []map[string]map[string]map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != gameServers+"/gs" ||
			r.Header.Get("Content-Type") != "application/merge-patch+json" {
			http.NotFound(w, r)
			return
		}
		var patch map[string]map[string]map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Error(err)
		}
		patches = append(patches, patch)
		io.WriteString(w, `{}`)
	})
	ctx := context.Background()
	if err := client.EndMatch(ctx, "gs", "m-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.ForgetMatches(ctx, "gs", []string{"m-1"}); err != nil {
		t.Fatal(err)
	}
	if err := client.EndMatch(ctx, "gone", "m-1"); err != nil {
		t.Fatalf("ending a match on a game server that is gone: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("patches = %v", patches)
	}
	if at, _ := patches[0]["metadata"]["annotations"]["gamefleet.io/end.m-1"].(string); at == "" {
		t.Fatalf("end = %v", patches[0])
	}
	forget := patches[1]["metadata"]["annotations"]
	for _, key := range []string{"gamefleet.io/match.m-1", "gamefleet.io/end.m-1"} {
		if v, ok := forget[key]; !ok || v != nil {
			t.Fatalf("forget = %v", forget)
		}
	}
}

// pagedRooms serves the game servers a page at a time: page n holds the
// game server gs-n, and points at the next page until the last.
func pagedRooms(t *testing.T, pages int, page func(n int, w http.ResponseWriter) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != gameServers {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") == "" {
			t.Error("a list without limit")
		}
		n := 1
		if c := r.URL.Query().Get("continue"); c != "" {
			fmt.Sscanf(c, "page-%d", &n)
		}
		if page != nil && !page(n, w) {
			return
		}
		next := ""
		if n < pages {
			next = fmt.Sprintf("page-%d", n+1)
		}
		fmt.Fprintf(w, `{"metadata":{"continue":%q},"items":[{"metadata":{"name":"gs-%d"},"status":{"state":"Ready"}}]}`, next, n)
	}
}

// A cluster lists its game servers a page at a time; GameFleet reads every
// page.
func TestRoomsReadsEveryPage(t *testing.T) {
	client := newTestClient(t, pagedRooms(t, 3, nil))
	rooms, err := client.Rooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 3 {
		t.Fatalf("%d rooms, want one from each of 3 pages", len(rooms))
	}
}

// A page that fails fails the list: the pages read before it are never
// taken for all the game servers there are.
func TestRoomsFailWhenAPageFails(t *testing.T) {
	client := newTestClient(t, pagedRooms(t, 3, func(n int, w http.ResponseWriter) bool {
		if n == 2 {
			// The continue token expired, as Kubernetes says when it compacted.
			w.WriteHeader(http.StatusGone)
			io.WriteString(w, `{"message":"The provided continue parameter is too old"}`)
			return false
		}
		return true
	}))
	if rooms, err := client.Rooms(context.Background()); err == nil {
		t.Fatalf("rooms = %d, want an error", len(rooms))
	}
	if _, err := client.FindByMatch(context.Background(), "m-1"); err == nil {
		t.Fatal("FindByMatch said no room when it could not read every page")
	}
}

// A response larger than GameFleet reads is an error, not a list cut short.
func TestTooLargeAResponseIsAnError(t *testing.T) {
	client := newTestClient(t, pagedRooms(t, 1, func(n int, w http.ResponseWriter) bool {
		w.Write([]byte(`{"metadata":{},"items":[{"metadata":{"name":"gs-1","annotations":{"x":"`))
		w.Write(bytes.Repeat([]byte("a"), maxResponse))
		w.Write([]byte(`"}}}]}`))
		return false
	}))
	_, err := client.Rooms(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want one that says the response is too large", err)
	}
}

// The notes of a full game server fit in its annotations with room to
// spare: MaxRooms matches at MaxMatchNote each, a quarter as many again of
// matches that are over and not yet forgotten, a request to end each, and
// 16 KiB for Agones' and Kubernetes' own annotations.
func TestNotesFitInAGameServer(t *testing.T) {
	endNote := len(AnnotationEndPrefix) + MaxMatchIDLength + len(time.RFC3339Nano)
	notes := MaxRooms * (MaxMatchNote + endNote) * 5 / 4
	if limit := 256<<10 - 16<<10; notes > limit {
		t.Fatalf("a full game server's notes take %d bytes, more than the %d there is room for", notes, limit)
	}
}

// The note of a match with two players is a small part of what a match may
// take.
func TestNoteSizeOfAMatch(t *testing.T) {
	players := json.RawMessage(`[{"userId":"u-1","seat":0},{"userId":"u-2","seat":1}]`)
	n := NoteSize(Allocation{MatchID: "8c1f0c7e-5d1e-4f4c-9a59-3c3b8e0f2a11", Channel: "stable", Compat: "dm-v7", Players: players})
	if n < 150 || n > 300 {
		t.Fatalf("note size = %d", n)
	}
}
