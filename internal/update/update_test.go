package update

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.1.0", "2.0.9", true}, {"v2.0.10", "2.0.9", true}, {"2.0.0", "2.0.0", false},
		{"1.9.9", "2.0.0", false}, {"2.0.0", "dev", false}, {"dev", "2.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func fakeGitHub(t *testing.T, tag string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.URL.Path != "/repos/xuhuanhello/gamefleet/releases/latest" || r.Header.Get("User-Agent") == "" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "name": "GameFleet " + tag, "body": "- 新功能",
			"html_url": "https://github.com/xuhuanhello/gamefleet/releases/tag/" + tag, "published_at": "2026-10-23T00:00:00Z"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newService(t *testing.T, current, api string) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	s := New(Options{Current: current, Repository: "xuhuanhello/gamefleet", Dir: filepath.Join(dir, "update"),
		BackupDir: filepath.Join(dir, "backups"), API: api, Log: slog.New(slog.DiscardHandler),
		Backup: func(_ context.Context, path string) error { return os.WriteFile(path, []byte("db"), 0o600) }})
	if err := os.MkdirAll(filepath.Join(dir, "update"), 0o750); err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(dir, "update")
}

func TestLatestAndAvailable(t *testing.T) {
	calls := 0
	gh := fakeGitHub(t, "v2.1.0", &calls)
	s, _ := newService(t, "2.0.0", gh.URL)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if s.Available() != nil {
		t.Fatal("available before any check")
	}
	r, err := s.Latest(context.Background(), false)
	if err != nil || r.Version != "2.1.0" || !strings.HasSuffix(r.URL, "/v2.1.0") {
		t.Fatalf("latest = %+v, %v", r, err)
	}
	if a := s.Available(); a == nil || a.Version != "2.1.0" {
		t.Fatalf("available = %+v", a)
	}
	// Asking again within a minute does not reach GitHub.
	s.Latest(context.Background(), true)
	if calls != 1 {
		t.Fatalf("GitHub asked %d times", calls)
	}
	now = now.Add(time.Minute)
	s.Latest(context.Background(), true)
	if calls != 2 {
		t.Fatalf("GitHub asked %d times after a minute", calls)
	}

	current, _ := newService(t, "2.1.0", gh.URL)
	current.Latest(context.Background(), false)
	if current.Available() != nil {
		t.Fatal("the running version offered as an update")
	}
}

func TestDownloadAndApplyWriteRequests(t *testing.T) {
	s, dir := newService(t, "2.0.0", "")
	if err := s.Download("2.1.0"); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, "request.json"))
	if json.Unmarshal(data, &req) != nil || req["action"] != "download" || req["version"] != "2.1.0" {
		t.Fatalf("request = %s", data)
	}
	if err := s.Download("latest; rm -rf /"); err == nil {
		t.Fatal("a request that is not a version was written")
	}

	// While the updater pulls, nothing else is asked, and progress adds up
	// each layer: one done, one half way.
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"action":"download","version":"2.1.0","state":"downloading"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "pull.jsonl"), []byte(`{"status":"Pulling from xuhuanhello/gamefleet","id":"2.1.0"}
{"status":"Downloading","progressDetail":{"current":50,"total":100},"id":"a"}
{"status":"Downloading","progressDetail":{"current":100,"total":300},"id":"b"}
{"status":"Download complete","id":"a"}
{"status":"Downloading","progressDetail":{"current":150,"total":300},"id":"b"}
`), 0o644)
	st, err := s.Status()
	if err != nil || !st.Busy() || st.Done != 250 || st.Total != 400 || st.Percent() != 62 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if err := s.Apply(context.Background(), "2.1.0"); !errors.Is(err, ErrBusy) {
		t.Fatalf("apply while downloading: %v", err)
	}

	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"action":"download","version":"2.1.0","state":"downloaded"}`), 0o644)
	if err := s.Apply(context.Background(), "2.1.0"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "request.json"))
	if !strings.Contains(string(data), `"action":"apply"`) {
		t.Fatalf("request = %s", data)
	}
	backups, _ := os.ReadDir(s.o.BackupDir)
	if len(backups) != 1 || !strings.HasPrefix(backups[0].Name(), "gamefleet-2.0.0-") {
		t.Fatalf("backups = %v", backups)
	}
}

func TestOnlyFiveBackupsStay(t *testing.T) {
	s, dir := newService(t, "2.0.0", "")
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"state":"downloaded"}`), 0o644)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for i := range 7 {
		s.now = func() time.Time { return now.Add(time.Duration(i) * time.Hour) }
		if err := s.Apply(context.Background(), "2.1.0"); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(filepath.Join(s.o.BackupDir, "gamefleet-2.0.0-"+s.now().UTC().Format("20060102-150405")+".db"), s.now(), s.now())
	}
	backups, _ := os.ReadDir(s.o.BackupDir)
	if len(backups) != 5 {
		t.Fatalf("%d backups kept", len(backups))
	}
}

func TestWithoutAnUpdater(t *testing.T) {
	s := New(Options{Current: "2.0.0", Log: slog.New(slog.DiscardHandler)})
	if s.HasUpdater() || !errors.Is(s.Download("2.1.0"), ErrNoUpdater) {
		t.Fatal("downloads without an updater")
	}
	if st, err := s.Status(); err != nil || st.State != "" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if New(Options{Current: "dev"}).Released() {
		t.Fatal("a build from source counts as a release")
	}
}
