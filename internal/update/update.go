// Package update tells whether a newer GameFleet release is out, and asks
// the updater on the management machine to download it and put it in place.
//
// GameFleet itself has no hold on Docker. It shares a directory with the
// updater, a systemd service that install.sh sets up (deploy/updater.sh):
//
//	request.json  written here: an action, download or apply, and a version
//	status.json   written by the updater: how the last request went
//	pull.jsonl    written by the updater: Docker's progress while it pulls
//
// A request names a version, never an image or a command: the updater builds
// the image name itself, so it only ever installs GameFleet releases.
package update

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// CheckInterval is how often GameFleet asks GitHub for the latest release.
	CheckInterval = 6 * time.Hour
	// refreshAfter limits checks that people ask for, such as opening the
	// update page, to one a minute: GitHub allows 60 an hour.
	refreshAfter = time.Minute
)

// States of the updater, as status.json reports them.
const (
	Downloading = "downloading"
	Downloaded  = "downloaded"
	Applying    = "applying"
	Applied     = "applied"
	Failed      = "failed"
)

var (
	// ErrBusy means the updater is still working on the last request.
	ErrBusy = errors.New("the updater is busy")
	// ErrNoUpdater means this GameFleet was not installed with install.sh.
	ErrNoUpdater = errors.New("no updater")

	version = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)
)

// Release is a published GameFleet version.
type Release struct {
	Version   string // without the leading v
	Name      string
	Notes     string
	URL       string // its page on GitHub
	Published time.Time
}

// Status is how the updater's last request went.
type Status struct {
	Action  string    `json:"action"`
	Version string    `json:"version"`
	State   string    `json:"state"`
	Error   string    `json:"error"`
	At      time.Time `json:"at"`
	// Done and Total are the bytes of a download so far, while it runs.
	Done, Total int64 `json:"-"`
}

// Busy reports whether the updater is still working.
func (s Status) Busy() bool { return s.State == Downloading || s.State == Applying }

// Percent is how far a download is, 0 to 100.
func (s Status) Percent() int {
	if s.Total <= 0 {
		return 0
	}
	return int(s.Done * 100 / s.Total)
}

// Options configures the service.
type Options struct {
	// Current is the version running, "dev" for a build from source.
	Current string
	// Repository is the GitHub repository releases come from, owner/name.
	// Empty turns the checks off.
	Repository string
	// Dir is the directory shared with the updater; empty when there is none.
	Dir string
	// Backup copies the database to a file before an update is applied.
	Backup func(ctx context.Context, path string) error
	// BackupDir keeps the copies; the newest five stay.
	BackupDir string
	API       string // GitHub's API; https://api.github.com when empty
	Client    *http.Client
	Log       *slog.Logger
}

// Service checks for releases and talks to the updater.
type Service struct {
	o   Options
	now func() time.Time

	mu      sync.Mutex
	latest  *Release
	err     error
	checked time.Time
}

// New makes the service. It checks nothing until Run or Latest is called.
func New(o Options) *Service {
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Service{o: o, now: time.Now}
}

// Current is the version running.
func (s *Service) Current() string { return s.o.Current }

// Released reports whether the running version is a release, which is the
// only kind that can be updated.
func (s *Service) Released() bool { return version.MatchString(s.o.Current) }

// HasUpdater reports whether the updater can download and apply releases.
func (s *Service) HasUpdater() bool { return s.o.Dir != "" }

// Run checks for a new release now and every CheckInterval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	if s.o.Repository == "" || !s.Released() {
		return
	}
	ticker := time.NewTicker(CheckInterval)
	defer ticker.Stop()
	for {
		if _, err := s.Latest(ctx, true); err != nil {
			s.o.Log.Warn("update check failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Available is the latest release when it is newer than the running
// version, as of the last check; it asks nothing.
func (s *Service) Available() *Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil || !Newer(s.latest.Version, s.o.Current) {
		return nil
	}
	return s.latest
}

// Latest returns the latest release. It asks GitHub again when refresh is
// set and the last check is more than a minute old.
func (s *Service) Latest(ctx context.Context, refresh bool) (*Release, error) {
	s.mu.Lock()
	stale := s.checked.IsZero() || refresh && s.now().Sub(s.checked) >= refreshAfter
	s.mu.Unlock()
	if !stale || s.o.Repository == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.latest, s.err
	}
	r, err := s.fetch(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = s.now()
	s.err = err
	if err == nil {
		s.latest = r
	}
	return s.latest, s.err
}

func (s *Service) fetch(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.o.API+"/repos/"+s.o.Repository+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "gamefleet/"+s.o.Current)
	resp, err := s.o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	var body struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	m := version.FindStringSubmatch(body.TagName)
	if m == nil {
		return nil, fmt.Errorf("the latest release %q has no version", body.TagName)
	}
	return &Release{Version: strings.TrimPrefix(body.TagName, "v"), Name: body.Name, Notes: body.Body,
		URL: body.HTMLURL, Published: body.PublishedAt}, nil
}

// Newer reports whether version a is newer than b. Anything that is not a
// version, such as "dev", is never newer and never older.
func Newer(a, b string) bool {
	pa, pb := version.FindStringSubmatch(a), version.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// Status reads how the updater's last request went. With no request yet,
// the state is empty.
func (s *Service) Status() (Status, error) {
	var st Status
	if !s.HasUpdater() {
		return st, nil
	}
	data, err := os.ReadFile(filepath.Join(s.o.Dir, "status.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("status.json: %w", err)
	}
	if st.State == Downloading {
		st.Done, st.Total = s.progress()
	}
	return st, nil
}

// progress adds up Docker's pull progress: each layer reports its own bytes,
// and a layer that is done or was there already counts as complete.
func (s *Service) progress() (done, total int64) {
	f, err := os.Open(filepath.Join(s.o.Dir, "pull.jsonl"))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	type layer struct{ done, total int64 }
	layers := map[string]*layer{}
	dec := json.NewDecoder(bufio.NewReader(io.LimitReader(f, 16<<20)))
	for {
		var line struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Detail struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
		}
		if dec.Decode(&line) != nil {
			break
		}
		if line.ID == "" {
			continue
		}
		l := layers[line.ID]
		if l == nil {
			l = &layer{}
			layers[line.ID] = l
		}
		switch {
		case line.Status == "Downloading" && line.Detail.Total > 0:
			l.done, l.total = line.Detail.Current, line.Detail.Total
		case line.Status == "Download complete" || line.Status == "Pull complete" || line.Status == "Already exists":
			l.done = l.total
		}
	}
	for _, l := range layers {
		done += l.done
		total += l.total
	}
	return done, total
}

// Download asks the updater to pull the release's image.
func (s *Service) Download(v string) error {
	return s.ask("download", v)
}

// Apply backs the database up and asks the updater to put the downloaded
// release in place, which restarts GameFleet.
func (s *Service) Apply(ctx context.Context, v string) error {
	if err := s.check(v); err != nil {
		return err
	}
	if err := os.MkdirAll(s.o.BackupDir, 0o750); err != nil {
		return err
	}
	name := fmt.Sprintf("gamefleet-%s-%s.db", s.o.Current, s.now().UTC().Format("20060102-150405"))
	if err := s.o.Backup(ctx, filepath.Join(s.o.BackupDir, name)); err != nil {
		return fmt.Errorf("backing the database up: %w", err)
	}
	s.prune()
	return s.ask("apply", v)
}

// prune keeps the newest five backups.
func (s *Service) prune() {
	entries, err := os.ReadDir(s.o.BackupDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gamefleet-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	type dated struct {
		name string
		at   time.Time
	}
	var all []dated
	for _, n := range names {
		if info, err := os.Stat(filepath.Join(s.o.BackupDir, n)); err == nil {
			all = append(all, dated{n, info.ModTime()})
		}
	}
	for len(all) > 5 {
		oldest := 0
		for i := range all {
			if all[i].at.Before(all[oldest].at) {
				oldest = i
			}
		}
		os.Remove(filepath.Join(s.o.BackupDir, all[oldest].name))
		all = append(all[:oldest], all[oldest+1:]...)
	}
}

func (s *Service) check(v string) error {
	if !s.HasUpdater() {
		return ErrNoUpdater
	}
	if !version.MatchString(v) {
		return fmt.Errorf("%q is not a version", v)
	}
	st, err := s.Status()
	if err != nil {
		return err
	}
	if st.Busy() {
		return ErrBusy
	}
	return nil
}

func (s *Service) ask(action, v string) error {
	if err := s.check(v); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"action": action, "version": strings.TrimPrefix(v, "v"), "at": s.now().UTC()})
	if err != nil {
		return err
	}
	// Written whole, then renamed, so the updater never reads half of it.
	tmp := filepath.Join(s.o.Dir, ".request.json")
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.o.Dir, "request.json"))
}
