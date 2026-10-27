package logbook

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBookKeepsTheNewestLines(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	b := New(3, func() time.Time { return now })
	for i := range 5 {
		fmt.Fprintf(b, "line %d\n", i)
		now = now.Add(time.Minute)
	}
	if got := b.Read(time.Time{}, 10, false); !slices.Equal(got, []string{"line 2", "line 3", "line 4"}) {
		t.Fatalf("all = %q", got)
	}
	if got := b.Read(time.Time{}, 2, false); !slices.Equal(got, []string{"line 3", "line 4"}) {
		t.Fatalf("newest two = %q", got)
	}
	since := time.Date(2026, 10, 27, 12, 3, 0, 0, time.UTC)
	if got := b.Read(since, 10, false); !slices.Equal(got, []string{"line 3", "line 4"}) {
		t.Fatalf("since 12:03 = %q", got)
	}
	if got := New(3, time.Now).Read(time.Time{}, 10, false); len(got) != 0 {
		t.Fatalf("empty book = %q", got)
	}
}

func TestBookPicksWarnings(t *testing.T) {
	b := New(10, time.Now)
	log := slog.New(slog.NewJSONHandler(b, nil))
	log.Info("fine")
	log.Warn("allocation failed", "error", "no ready game server")
	log.Error("cluster unreachable")
	got := b.Read(time.Time{}, 10, true)
	if len(got) != 2 || !strings.Contains(got[0], "allocation failed") || !strings.Contains(got[1], "cluster unreachable") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestBookKeepsWithinItsBytes(t *testing.T) {
	b := New(100, time.Now)
	b.budget = 30
	for i := range 4 {
		fmt.Fprintf(b, "line %04d\n", i) // 9 bytes
	}
	if got := b.Read(time.Time{}, 10, false); !slices.Equal(got, []string{"line 0001", "line 0002", "line 0003"}) {
		t.Fatalf("got %q", got)
	}

	b = New(100, time.Now)
	fmt.Fprintln(b, strings.Repeat("é", 3000))
	got := b.Read(time.Time{}, 10, false)
	if len(got) != 1 || len(got[0]) > maxLine || !utf8.ValidString(got[0]) || !strings.HasPrefix(got[0], "éé") {
		t.Fatalf("a long line kept as %d bytes, valid %v", len(got[0]), utf8.ValidString(got[0]))
	}
}
