package api

import (
	"context"
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
	"github.com/xuhuanhello/gamefleet/internal/logbook"
	"github.com/xuhuanhello/gamefleet/internal/logs"
	"github.com/xuhuanhello/gamefleet/internal/redact"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

const (
	logsKey = "logs-test-key-0123456789abcdef0123456789abcdef"
	// ticketKey is a runtime setting of the game; no log may show it.
	ticketKey = "ticket-signing-key-do-not-show"
)

// newLogsServer runs the API over a cluster with one game server running
// match m-1, whose log names a player's address and the ticket key.
func newLogsServer(t *testing.T) (*httptest.Server, *clustertest.Fake, *logbook.Book) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "gamefleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keys := auth.NewKeys(db)
	for value, k := range map[string][]string{logsKey: {"agent", auth.ScopeLogs}, nakamaKey: {"nakama", auth.ScopeAllocate}} {
		if err := keys.Add(ctx, k[0], k[1:], value, "test"); err != nil {
			t.Fatal(err)
		}
	}
	err = db.Update(ctx, func(tx *store.Tx) error {
		return tx.PutVar(store.Var{Game: "billiards", Name: "TICKET_KEY", Value: ticketKey, UpdatedBy: "admin"})
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	fake := clustertest.New("us-1", 0)
	fake.AddRoom(cluster.Room{ID: "billiards-1-gs", State: "Allocated", Fleet: "billiards-1", Capacity: 50,
		Matches: []cluster.Match{{ID: "m-1"}, {ID: "m-2"}}})
	fake.SetView(clustertest.View{
		Pods: []cluster.Pod{
			{Namespace: clustertest.Namespace, Name: "billiards-1-gs", Owner: "GameServer", Status: "Running",
				Containers: []string{"billiards", "agones-gameserver-sidecar"}},
			{Namespace: "agones-system", Name: "agones-controller-abc", Owner: "ReplicaSet", Status: "Running", Containers: []string{"agones-controller"}},
		},
		Events: []cluster.Event{
			{Namespace: clustertest.Namespace, Kind: "Pod", Name: "billiards-1-gs6", Type: "Warning", Reason: "FailedScheduling",
				Message: "0/2 nodes are available: 1 Insufficient memory.", Count: 12, Last: now.Add(-10 * time.Minute)},
			{Namespace: clustertest.Namespace, Kind: "Pod", Name: "old", Type: "Normal", Reason: "Pulled", Last: now.Add(-3 * time.Hour)},
		},
		Logs: map[string]string{
			clustertest.Namespace + "/billiards-1-gs/billiards": "DM_SERVER_READY\n" +
				"DM_ALLOCATED match=m-1 players=2\n" +
				"DM_JOIN match=m-1 client=198.51.100.7:50123 key=" + ticketKey + "\n" +
				"DM_ALLOCATED match=m-2 players=2\n" +
				"DM_RESULT match=m-1 winner=u-1\n",
			clustertest.Namespace + "/billiards-1-gs/billiards#previous": "DM_CRASH before restart\n",
		},
	})
	book := logbook.New(100, time.Now)
	log := slog.New(slog.NewJSONHandler(book, nil))
	log.Info("allocation", "caller", "nakama", "matchId", "m-1")
	log.Warn("allocation failed", "caller", "nakama", "error", "no ready game server")

	dir := sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: fake})
	reader := logs.New(dir, book, db, redact.New(), time.Now)
	server := httptest.NewServer(Handler(allocate.New(nil, dir), nil, keys, nil, reader, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(server.Close)
	return server, fake, book
}

func read(t *testing.T, server *httptest.Server, path, key string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

func TestLogsKeyReadsRedactedLogs(t *testing.T) {
	server, fake, _ := newLogsServer(t)

	status, _, text := read(t, server, "/api/v1/logs/pods/us-1/games/billiards-1-gs?since=30m&lines=3", logsKey)
	if status != http.StatusOK || strings.Count(text, "\n") != 3 || !strings.HasPrefix(text, "DM_JOIN match=m-1") {
		t.Fatalf("game server log: %d\n%s", status, text)
	}
	if strings.Contains(text, ticketKey) || strings.Contains(text, "198.51.100.7") ||
		!strings.Contains(text, "[runtime:billiards:TICKET_KEY]") || !strings.Contains(text, "client=ip#") {
		t.Fatalf("not redacted:\n%s", text)
	}
	if q := fake.LogQuery(); q.Container != "billiards" || q.Since != 30*time.Minute || q.Lines != 3 {
		t.Errorf("log query = %+v", q)
	}
	if _, _, text := read(t, server, "/api/v1/logs/pods/us-1/games/billiards-1-gs?previous=true", logsKey); text != "DM_CRASH before restart\n" {
		t.Errorf("previous run: %q", text)
	}

	status, header, text := read(t, server, "/api/v1/logs/matches/m-1", logsKey)
	if status != http.StatusOK || header.Get("GameFleet-Room") != "us-1/billiards-1-gs" ||
		strings.Count(text, "\n") != 3 || strings.Contains(text, "m-2") || strings.Contains(text, ticketKey) {
		t.Fatalf("match log: %d %s\n%s", status, header.Get("GameFleet-Room"), text)
	}

	_, _, text = read(t, server, "/api/v1/logs/gamefleet?level=warn", logsKey)
	if strings.Count(text, "\n") != 1 || !strings.Contains(text, "allocation failed") {
		t.Fatalf("GameFleet's warnings:\n%s", text)
	}

	status, _, text = read(t, server, "/api/v1/events?since=1h", logsKey)
	if status != http.StatusOK || !strings.Contains(text, "Insufficient memory") || strings.Contains(text, `"Pulled"`) {
		t.Fatalf("events: %d\n%s", status, text)
	}
	status, _, text = read(t, server, "/api/v1/pods?cluster=us-1", logsKey)
	if status != http.StatusOK || !strings.Contains(text, `"agones-controller-abc"`) || !strings.Contains(text, `"owner":"GameServer"`) {
		t.Fatalf("pods: %d\n%s", status, text)
	}
}

func TestLogsReadsAreBounded(t *testing.T) {
	server, _, _ := newLogsServer(t)
	for path, want := range map[string]int{
		"/api/v1/logs/pods/us-1/games/billiards-1-gs?since=48h":               http.StatusBadRequest,
		"/api/v1/logs/pods/us-1/games/billiards-1-gs?lines=5000":              http.StatusBadRequest,
		"/api/v1/logs/pods/us-1/games/billiards-1-gs?since=soon":              http.StatusBadRequest,
		"/api/v1/logs/pods/us-1/games/billiards-1-gs?container=other":         http.StatusNotFound,
		"/api/v1/logs/pods/us-1/games/nobody":                                 http.StatusNotFound,
		"/api/v1/logs/pods/sg-1/games/billiards-1-gs":                         http.StatusNotFound,
		"/api/v1/logs/matches/m-9":                                            http.StatusNotFound,
		"/api/v1/logs/pods/us-1/agones-system/agones-controller-abc?lines=10": http.StatusOK,
	} {
		if status, _, body := read(t, server, path, logsKey); status != want {
			t.Errorf("%s: %d, want %d\n%s", path, status, want, body)
		}
	}
}

// A logs key reads and nothing else, and other keys do not read logs.
func TestLogsKeyReadsOnly(t *testing.T) {
	server, _, _ := newLogsServer(t)
	if status, _, _ := read(t, server, "/api/v1/logs/gamefleet", nakamaKey); status != http.StatusForbidden {
		t.Errorf("an allocate key read logs: %d", status)
	}
	if status, _ := call(t, server, http.MethodPost, "/api/v1/allocations", logsKey, match); status != http.StatusForbidden {
		t.Errorf("a logs key allocated: %d", status)
	}
	if status, _ := call(t, server, http.MethodDelete, "/api/v1/rooms/billiards-1-gs", logsKey, ""); status != http.StatusForbidden {
		t.Errorf("a logs key deleted a game server: %d", status)
	}
}

// One key reads at most readsPerMinute times a minute.
func TestLogReadsPerMinute(t *testing.T) {
	l := &limiter{}
	at := time.Date(2026, 10, 27, 12, 0, 30, 0, time.UTC)
	for range readsPerMinute {
		if !l.allow("agent", at) {
			t.Fatal("limited too soon")
		}
	}
	if l.allow("agent", at) {
		t.Fatal("one read too many went through")
	}
	if !l.allow("other", at) {
		t.Fatal("another key was limited")
	}
	if !l.allow("agent", at.Add(30*time.Second)) {
		t.Fatal("still limited the next minute")
	}
}
