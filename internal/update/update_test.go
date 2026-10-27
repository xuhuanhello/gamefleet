package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func (s *Service) busyChecking() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checking
}

func TestChecks(t *testing.T) {
	calls := 0
	gh := fakeGitHub(t, "v2.1.0", &calls)
	s, _ := newService(t, "2.0.0", gh.URL)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if s.Available() != nil || !s.Checked().At.IsZero() || calls != 0 {
		t.Fatal("GitHub asked before anyone used the console")
	}
	c := s.Check(context.Background(), time.Minute)
	if c.Err != nil || c.Latest.Version != "2.1.0" || !strings.HasSuffix(c.Latest.URL, "/v2.1.0") || !c.At.Equal(now) {
		t.Fatalf("check = %+v", c)
	}
	if a := s.Available(); a == nil || a.Version != "2.1.0" {
		t.Fatalf("available = %+v", a)
	}
	// A check younger than maxAge is reused; zero always asks.
	s.Check(context.Background(), time.Minute)
	if calls != 1 {
		t.Fatalf("GitHub asked %d times", calls)
	}
	s.Check(context.Background(), 0)
	if calls != 2 {
		t.Fatalf("GitHub asked %d times with maxAge 0", calls)
	}

	// In the background, one check at a time.
	now = now.Add(2 * time.Hour)
	s.CheckSoon(time.Hour)
	s.CheckSoon(time.Hour)
	for deadline := time.Now().Add(5 * time.Second); s.Checked().At.Equal(now.Add(-2*time.Hour)) || s.busyChecking(); {
		if time.Now().After(deadline) {
			t.Fatal("the background check never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls != 3 {
		t.Fatalf("GitHub asked %d times after two CheckSoon", calls)
	}

	current, _ := newService(t, "2.1.0", gh.URL)
	current.Check(context.Background(), 0)
	if current.Available() != nil {
		t.Fatal("the running version offered as an update")
	}
	dev, _ := newService(t, "dev", gh.URL)
	dev.Check(context.Background(), 0)
	dev.CheckSoon(0)
	if calls != 4 {
		t.Fatalf("a build from source asked GitHub (%d calls)", calls)
	}
}

func TestFailedChecksSayWhy(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(limited.Close)
	s, _ := newService(t, "2.0.0", limited.URL)
	c := s.Check(context.Background(), 0)
	if c.Err == nil || c.Latest != nil || !strings.Contains(Reason(c.Err), "每小时最多 60 次") {
		t.Fatalf("rate limited: %+v", c)
	}
	limited.Close()
	c = s.Check(context.Background(), 0)
	if Reason(c.Err) != "连不上 GitHub" || !strings.Contains(c.Err.Error(), "连不上 GitHub: ") {
		t.Fatalf("unreachable: %v", c.Err)
	}

	// A failure keeps what an earlier check found.
	calls := 0
	gh := fakeGitHub(t, "v2.1.0", &calls)
	s, _ = newService(t, "2.0.0", gh.URL)
	s.Check(context.Background(), 0)
	gh.Close()
	if c := s.Check(context.Background(), 0); c.Err == nil || c.Latest == nil || c.Latest.Version != "2.1.0" {
		t.Fatalf("after a failure: %+v", c)
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

	// Until the updater takes the request, it counts as started, with no
	// progress yet, and nothing else is asked.
	if st, err := s.Status(); err != nil || st.State != Downloading || st.Version != "2.1.0" || st.Total != 0 {
		t.Fatalf("status before the updater took the request = %+v, %v", st, err)
	}
	if err := s.Download("2.1.0"); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second request while the first waits: %v", err)
	}
	os.Remove(filepath.Join(dir, "request.json")) // the updater takes it

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
	if st, _ := s.Status(); st.State != Applying || st.Version != "2.1.0" {
		t.Fatalf("status before the updater took the apply = %+v", st)
	}
	backups, _ := os.ReadDir(s.o.BackupDir)
	if len(backups) != 1 || !strings.HasPrefix(backups[0].Name(), "gamefleet-2.0.0-") {
		t.Fatalf("backups = %v", backups)
	}
	// The request names the backup, for the updater to point at if the
	// update fails.
	var apply map[string]any
	if json.Unmarshal(data, &apply) != nil || apply["backup"] != backups[0].Name() {
		t.Fatalf("apply request = %s, backup %s", data, backups[0].Name())
	}
}

// Requests sent at the same time, from two tabs say, get one through.
func TestOneRequestAtATime(t *testing.T) {
	s, _ := newService(t, "2.0.0", "")
	// A request is stamped with the time once it passed the check that the
	// updater is idle, before it is written. The test hears of each stamp,
	// and holds the first until it lets it go.
	var mu sync.Mutex
	n := 0
	stamps := make(chan int, 2)
	release := make(chan struct{})
	now := s.now
	s.now = func() time.Time {
		mu.Lock()
		n++
		i := n
		mu.Unlock()
		stamps <- i
		if i == 1 {
			<-release
		}
		return now()
	}
	var errs [2]error
	var wg sync.WaitGroup
	send := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Download("2.1.0")
		}()
	}
	send(0)
	<-stamps
	// The second one would pass the check now too, as nothing is written yet,
	// but has to wait for the first. Give it the time to get there either way.
	send(1)
	select {
	case <-stamps:
		t.Error("the second request passed the check while the first was being written")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if errs[0] != nil || !errors.Is(errs[1], ErrBusy) {
		t.Fatalf("errors = %v; want the first accepted and the second busy", errs)
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
		os.Remove(filepath.Join(dir, "request.json"))
		os.Chtimes(filepath.Join(s.o.BackupDir, "gamefleet-2.0.0-"+s.now().UTC().Format("20060102-150405")+".db"), s.now(), s.now())
	}
	backups, _ := os.ReadDir(s.o.BackupDir)
	if len(backups) != 5 {
		t.Fatalf("%d backups kept", len(backups))
	}
}

// The daily backups in the same directory are not the update's to prune.
func TestUpdatesLeaveDailyBackups(t *testing.T) {
	s, dir := newService(t, "2.0.0", "")
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"state":"downloaded"}`), 0o644)
	os.MkdirAll(s.o.BackupDir, 0o750)
	for i := range 14 {
		os.WriteFile(filepath.Join(s.o.BackupDir, fmt.Sprintf("gamefleet-daily-202610%02d-030000.db", i+1)), []byte("x"), 0o600)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for i := range 7 {
		s.now = func() time.Time { return now.Add(time.Duration(i) * time.Hour) }
		if err := s.Apply(context.Background(), "2.1.0"); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, "request.json"))
		os.Chtimes(filepath.Join(s.o.BackupDir, "gamefleet-2.0.0-"+s.now().UTC().Format("20060102-150405")+".db"), s.now(), s.now())
	}
	entries, _ := os.ReadDir(s.o.BackupDir)
	daily := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gamefleet-daily-") {
			daily++
		}
	}
	if daily != 14 || len(entries) != 14+5 {
		t.Fatalf("%d daily backups of 14, %d files in all", daily, len(entries))
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
