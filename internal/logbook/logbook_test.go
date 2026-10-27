package logbook

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
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
