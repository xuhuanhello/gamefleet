// Package logbook keeps GameFleet's own newest log lines in memory, for the
// logs API. GameFleet writes its log to the book as well as to stdout; the
// book forgets the oldest lines first, and everything when GameFleet stops.
package logbook

import (
	"bytes"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxBytes is the most text the book keeps, however many lines that is:
	// a line can be as long as what a caller asked for.
	MaxBytes = 8 << 20
	// maxLine is how much of a line the book keeps. The logs API shows less.
	maxLine = 4 << 10
)

// Book is the newest log lines. It is an io.Writer: each write is one or more
// whole lines, as log/slog's handlers write them.
type Book struct {
	mu     sync.Mutex
	lines  []line // oldest first, from lines[first]
	first  int
	bytes  int // of the kept lines' text
	size   int
	budget int
	now    func() time.Time
}

type line struct {
	at   time.Time
	text string
}

// New returns a book that keeps the newest size lines, and at most MaxBytes.
func New(size int, now func() time.Time) *Book {
	return &Book{size: size, budget: MaxBytes, now: now}
}

func (b *Book) Write(p []byte) (int, error) {
	at := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, text := range bytes.Split(bytes.TrimRight(p, "\n"), []byte("\n")) {
		if len(text) == 0 {
			continue
		}
		if len(text) > maxLine {
			cut := maxLine
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut]
		}
		b.lines = append(b.lines, line{at: at, text: string(text)})
		b.bytes += len(text)
		for len(b.lines)-b.first > b.size || b.bytes > b.budget {
			b.bytes -= len(b.lines[b.first].text)
			b.lines[b.first] = line{}
			b.first++
		}
		// Move the kept lines to the front once more than half are gone.
		if b.first > len(b.lines)/2 {
			b.lines = append(b.lines[:0], b.lines[b.first:]...)
			b.first = 0
		}
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
	for i := len(b.lines) - 1; i >= b.first; i-- {
		l := b.lines[i]
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
