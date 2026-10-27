// Package logbook keeps GameFleet's own newest log lines in memory, for the
// logs API. GameFleet writes its log to the book as well as to stdout; the
// book forgets the oldest lines first, and everything when GameFleet stops.
package logbook

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// Book is a ring of log lines. It is an io.Writer: each write is one or more
// whole lines, as log/slog's handlers write them.
type Book struct {
	mu    sync.Mutex
	lines []line
	next  int // where the next line goes once the ring is full
	now   func() time.Time
}

type line struct {
	at   time.Time
	text string
}

// New returns a book that keeps the newest size lines.
func New(size int, now func() time.Time) *Book {
	return &Book{lines: make([]line, 0, size), now: now}
}

func (b *Book) Write(p []byte) (int, error) {
	at := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, text := range bytes.Split(bytes.TrimRight(p, "\n"), []byte("\n")) {
		if len(text) == 0 {
			continue
		}
		l := line{at: at, text: string(text)}
		if len(b.lines) < cap(b.lines) {
			b.lines = append(b.lines, l)
			continue
		}
		b.lines[b.next] = l
		b.next = (b.next + 1) % len(b.lines)
	}
	return len(p), nil
}

// Read returns the newest lines written since then, at most n, oldest
// first. With warnings, only lines of level WARN or ERROR: it reads the
// level as log/slog's JSON handler writes it.
func (b *Book) Read(since time.Time, n int, warnings bool) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for i := range b.lines {
		l := b.lines[(b.next+len(b.lines)-1-i)%len(b.lines)]
		if len(out) == n || l.at.Before(since) {
			break
		}
		if warnings && !strings.Contains(l.text, `"level":"WARN"`) && !strings.Contains(l.text, `"level":"ERROR"`) {
			continue
		}
		out = append(out, l.text)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
