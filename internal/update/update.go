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

// Check is what asking GitHub for the latest release found.
type Check struct {
	// Latest is the latest release found so far, nil until GitHub answered.
	Latest *Release
	// At is when GitHub was last asked, zero if it never was.
	At time.Time
	// Err is why the last time failed, nil if it worked.
	Err error
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

	mu       sync.Mutex
	last     Check
	checking bool // a CheckSoon is running
}

// New makes the service. GameFleet asks GitHub only when someone uses the
// console (see Check and CheckSoon), never on its own.
func New(o Options) *Service {
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
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

// Available is the latest release when it is newer than the running
// version, as of the last check; it asks nothing.
func (s *Service) Available() *Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last.Latest == nil || !Newer(s.last.Latest.Version, s.o.Current) {
		return nil
	}
	return s.last.Latest
}

// Checked is the last check; it asks nothing.
func (s *Service) Checked() Check {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Service) checks() bool { return s.o.Repository != "" && s.Released() }

// Check asks GitHub for the latest release, unless it was asked less than
// maxAge ago. A build from source never asks.
func (s *Service) Check(ctx context.Context, maxAge time.Duration) Check {
	if !s.checks() {
		return Check{}
	}
	s.mu.Lock()
	c := s.last
	s.mu.Unlock()
	if !c.At.IsZero() && s.now().Sub(c.At) < maxAge {
		return c
	}
	r, err := s.fetch(ctx)
	if err != nil {
		s.o.Log.Warn("update check failed", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last.At, s.last.Err = s.now(), err
	if err == nil {
		s.last.Latest = r
	}
	return s.last
}

// CheckSoon checks in the background when the last check is more than
// maxAge old, so that a page can show a new release without waiting for
// GitHub.
func (s *Service) CheckSoon(maxAge time.Duration) {
	if !s.checks() {
		return
	}
	s.mu.Lock()
	due := !s.checking && (s.last.At.IsZero() || s.now().Sub(s.last.At) >= maxAge)
	s.checking = s.checking || due
	s.mu.Unlock()
	if !due {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.Check(ctx, maxAge)
		s.mu.Lock()
		s.checking = false
		s.mu.Unlock()
	}()
}

// checkError is why a check failed, in words for the console; err is the
// detail for the log.
type checkError struct {
	text string
	err  error
}

func (e *checkError) Error() string {
	if e.err == nil {
		return e.text
	}
	return e.text + ": " + e.err.Error()
}

// Reason is the part of a failed check to show people.
func Reason(err error) string {
	var ce *checkError
	if errors.As(err, &ce) {
		return ce.text
	}
	return err.Error()
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
		return nil, &checkError{"连不上 GitHub", err}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, &checkError{"GitHub 暂时不回答这台机器（每小时最多 60 次），过一会儿再试", nil}
	case http.StatusNotFound:
		return nil, &checkError{"GitHub 上还没有发布过版本", nil}
	default:
		return nil, &checkError{fmt.Sprintf("GitHub 返回了 %d", resp.StatusCode), nil}
	}
	var body struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, &checkError{"读不懂 GitHub 的回答", err}
	}
	if !version.MatchString(body.TagName) {
		return nil, &checkError{fmt.Sprintf("GitHub 上最新的发布 %q 不是版本号", body.TagName), nil}
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
	// A request the updater has not taken yet, which it does within two
	// seconds, counts as started: the console shows it, and takes no other.
	if data, err := os.ReadFile(filepath.Join(s.o.Dir, "request.json")); err == nil {
		var req struct {
			Action, Version string
			At              time.Time
		}
		if json.Unmarshal(data, &req) == nil {
			st = Status{Action: req.Action, Version: req.Version, State: Downloading, At: req.At}
			if req.Action == "apply" {
				st.State = Applying
			}
			return st, nil
		}
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
