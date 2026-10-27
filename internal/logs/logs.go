// Package logs serves what a key with the logs scope may read: the logs of
// the clusters' pods, the clusters' events and GameFleet's own log, all
// redacted, a bounded amount at a time. It reads and never changes anything.
package logs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/logbook"
	"github.com/xuhuanhello/gamefleet/internal/redact"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Bounds of one read.
const (
	DefaultSince = time.Hour
	MaxSince     = 24 * time.Hour
	DefaultLines = 500
	MaxLines     = 2000
	// MaxBytes is the most text one read returns; the oldest lines go first.
	MaxBytes = 1 << 20
	// matchScan is how many of a game server's newest lines a read of one
	// match's lines looks through.
	matchScan = 20000
)

var (
	// ErrNotFound means no such cluster, pod, container or running match.
	ErrNotFound = errors.New("not found")
	// ErrInvalid means a query out of bounds.
	ErrInvalid = errors.New("invalid")
)

// Query picks lines. Zero values take the defaults.
type Query struct {
	Since time.Duration
	Lines int
	// Container of a pod; its first one if empty.
	Container string
	// Previous reads the container's run before it last restarted.
	Previous bool
	// Warnings keeps only GameFleet's lines of level WARN and ERROR.
	Warnings bool
}

func (q Query) bounded() (Query, error) {
	switch {
	case q.Since < 0 || q.Since > MaxSince:
		return q, fmt.Errorf("%w: since is at most %s", ErrInvalid, MaxSince)
	case q.Lines < 0 || q.Lines > MaxLines:
		return q, fmt.Errorf("%w: lines is at most %d", ErrInvalid, MaxLines)
	}
	if q.Since == 0 {
		q.Since = DefaultSince
	}
	if q.Lines == 0 {
		q.Lines = DefaultLines
	}
	return q, nil
}

// Service reads logs.
type Service struct {
	sites    *sites.Directory
	book     *logbook.Book
	store    *store.Store
	redactor *redact.Redactor
	now      func() time.Time
}

// New returns the service. book holds GameFleet's own log.
func New(dir *sites.Directory, book *logbook.Book, st *store.Store, r *redact.Redactor, now func() time.Time) *Service {
	return &Service{sites: dir, book: book, store: st, redactor: r, now: now}
}

// Pod is a pod of a cluster, for finding the log to read.
type Pod struct {
	Cluster    string    `json:"cluster"`
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	Node       string    `json:"node"`
	Owner      string    `json:"owner"` // GameServer for game servers
	Status     string    `json:"status"`
	Containers []string  `json:"containers"`
	Restarts   int       `json:"restarts"`
	Created    time.Time `json:"created"`
}

// Event is a cluster event, its message redacted.
type Event struct {
	Cluster   string    `json:"cluster"`
	Namespace string    `json:"namespace"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int       `json:"count"`
	Last      time.Time `json:"last"`
}

// Pods lists the pods of the cluster, or of every cluster if it is empty.
func (s *Service) Pods(ctx context.Context, clusterName string) ([]Pod, error) {
	list, err := s.clusters(clusterName)
	if err != nil {
		return nil, err
	}
	pods := []Pod{}
	for _, site := range list {
		found, err := site.Cluster.Pods(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", site.Name, err)
		}
		for _, p := range found {
			pods = append(pods, Pod{Cluster: site.Name, Namespace: p.Namespace, Name: p.Name, Node: p.Node, Owner: p.Owner,
				Status: p.Status, Containers: p.Containers, Restarts: p.Restarts, Created: p.Created})
		}
	}
	return pods, nil
}

// Events lists the cluster's events seen within since, or every cluster's.
func (s *Service) Events(ctx context.Context, clusterName string, since time.Duration) ([]Event, error) {
	q, err := Query{Since: since}.bounded()
	if err != nil {
		return nil, err
	}
	list, err := s.clusters(clusterName)
	if err != nil {
		return nil, err
	}
	pass, err := s.pass(ctx)
	if err != nil {
		return nil, err
	}
	from := s.now().Add(-q.Since)
	events := []Event{}
	for _, site := range list {
		found, err := site.Cluster.Events(ctx, "", "")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", site.Name, err)
		}
		for _, e := range found {
			if e.Last.Before(from) {
				continue
			}
			events = append(events, Event{Cluster: site.Name, Namespace: e.Namespace, Kind: e.Kind, Name: e.Name,
				Type: e.Type, Reason: e.Reason, Message: pass.Line(e.Message), Count: e.Count, Last: e.Last})
		}
	}
	slices.SortStableFunc(events, func(a, b Event) int { return a.Last.Compare(b.Last) })
	return events, nil
}

// PodLog reads lines of one container of a pod.
func (s *Service) PodLog(ctx context.Context, clusterName, namespace, name string, q Query) (string, error) {
	q, err := q.bounded()
	if err != nil {
		return "", err
	}
	site, ok := s.sites.Get(clusterName)
	if !ok {
		return "", fmt.Errorf("%w: cluster %s", ErrNotFound, clusterName)
	}
	pod, err := site.Cluster.Pod(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	if pod == nil || len(pod.Containers) == 0 {
		return "", fmt.Errorf("%w: pod %s/%s in %s", ErrNotFound, namespace, name, clusterName)
	}
	if q.Container == "" {
		q.Container = pod.Containers[0]
	}
	if !slices.Contains(pod.Containers, q.Container) {
		return "", fmt.Errorf("%w: container %s; the pod has %s", ErrNotFound, q.Container, strings.Join(pod.Containers, ", "))
	}
	text, err := site.Cluster.Log(ctx, namespace, name, cluster.LogQuery{Container: q.Container, Lines: q.Lines, Since: q.Since, Previous: q.Previous})
	if err != nil {
		return "", err
	}
	pass, err := s.pass(ctx)
	if err != nil {
		return "", err
	}
	return capped(pass.Text(text)), nil
}

// MatchLog reads the lines that name a running match from the log of the
// game server that runs it, and returns that game server.
func (s *Service) MatchLog(ctx context.Context, matchID string, q Query) (cluster.Room, string, error) {
	q, err := q.bounded()
	if err != nil {
		return cluster.Room{}, "", err
	}
	for _, site := range s.sites.All() {
		room, err := site.Cluster.FindByMatch(ctx, matchID)
		if err != nil {
			return cluster.Room{}, "", fmt.Errorf("%s: %w", site.Name, err)
		}
		if room == nil {
			continue
		}
		pod, err := site.Cluster.Pod(ctx, site.Cluster.Namespace(), room.ID)
		if err != nil {
			return cluster.Room{}, "", err
		}
		if pod == nil || len(pod.Containers) == 0 {
			break
		}
		text, err := site.Cluster.Log(ctx, pod.Namespace, pod.Name, cluster.LogQuery{Container: pod.Containers[0], Lines: matchScan, Since: q.Since})
		if err != nil {
			return cluster.Room{}, "", err
		}
		var lines []string
		for _, line := range strings.SplitAfter(text, "\n") {
			if strings.Contains(line, matchID) {
				lines = append(lines, line)
			}
		}
		lines = lines[max(0, len(lines)-q.Lines):]
		pass, err := s.pass(ctx)
		if err != nil {
			return cluster.Room{}, "", err
		}
		return *room, capped(pass.Text(strings.Join(lines, ""))), nil
	}
	return cluster.Room{}, "", fmt.Errorf("%w: no game server runs match %s", ErrNotFound, matchID)
}

// GameFleetLog reads GameFleet's own lines.
func (s *Service) GameFleetLog(ctx context.Context, q Query) (string, error) {
	q, err := q.bounded()
	if err != nil {
		return "", err
	}
	pass, err := s.pass(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, line := range s.book.Read(s.now().Add(-q.Since), q.Lines, q.Warnings) {
		b.WriteString(pass.Line(line))
		b.WriteByte('\n')
	}
	return capped(b.String()), nil
}

// pass is a redaction pass with the secrets GameFleet holds now: the games'
// runtime settings and the clusters' tokens.
func (s *Service) pass(ctx context.Context) (*redact.Pass, error) {
	var secrets []redact.Secret
	err := s.store.View(ctx, func(tx *store.Tx) error {
		vars, err := tx.Vars("")
		if err != nil {
			return err
		}
		for _, v := range vars {
			secrets = append(secrets, redact.Secret{Name: "runtime:" + v.Game + ":" + v.Name, Value: v.Value})
		}
		clusters, err := tx.Clusters()
		if err != nil {
			return err
		}
		for _, c := range clusters {
			secrets = append(secrets, redact.Secret{Name: "cluster:" + c.Name + ":token", Value: c.Token},
				redact.Secret{Name: "cluster:" + c.Name + ":join-token", Value: c.JoinToken})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.redactor.With(secrets), nil
}

func (s *Service) clusters(name string) ([]sites.Site, error) {
	if name == "" {
		return s.sites.All(), nil
	}
	site, ok := s.sites.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: cluster %s", ErrNotFound, name)
	}
	return []sites.Site{site}, nil
}

// capped keeps the newest whole lines that fit in MaxBytes.
func capped(text string) string {
	if len(text) <= MaxBytes {
		return text
	}
	text = text[len(text)-MaxBytes:]
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return text
}
