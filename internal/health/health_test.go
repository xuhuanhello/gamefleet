package health

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func check(t *testing.T, s Status, name string) Check {
	t.Helper()
	for _, c := range s.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %s in %+v", name, s.Checks)
	return Check{}
}

func TestLoopsAndClusters(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := New(c.now, nil)
	loops, clusters := []string{"release"}, []string{"us-1", "sg-1"}
	// Just started, nothing has run yet: no alarm.
	if s := m.Check(loops, clusters); !s.OK {
		t.Fatalf("just started: %+v", s)
	}
	c.t = c.t.Add(StaleLoop + time.Second)
	if s := m.Check(loops, clusters); s.OK || check(t, s, "loop release").OK || check(t, s, "cluster us-1").OK {
		t.Fatalf("nothing ran for long: %+v", s)
	}
	m.Pass("release", nil)
	m.Contact("us-1", nil)
	m.Contact("sg-1", errors.New("connection refused"))
	s := m.Check(loops, clusters)
	if !check(t, s, "loop release").OK || !check(t, s, "cluster us-1").OK || check(t, s, "cluster sg-1").OK || s.OK {
		t.Fatalf("sg-1 down: %+v", s)
	}
	// A loop stuck in a pass gets through no more.
	c.t = c.t.Add(StaleLoop + time.Second)
	m.Contact("us-1", nil)
	m.Contact("sg-1", nil)
	if s := m.Check(loops, clusters); check(t, s, "loop release").OK || !check(t, s, "cluster sg-1").OK {
		t.Fatalf("loop stuck: %+v", s)
	}
}

func TestAllocation(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := New(c.now, nil)
	alloc := func() Check { t.Helper(); return check(t, m.Check(nil, nil), "allocation") }
	// A few failures out of few attempts are no alarm.
	for range MinAllocations - 1 {
		m.Allocation(time.Second, true)
	}
	if !alloc().OK {
		t.Fatalf("too few to judge: %+v", alloc())
	}
	m.Allocation(time.Second, true)
	if alloc().OK {
		t.Fatalf("all of %d failed: %+v", MinAllocations, alloc())
	}
	// They age out of the window.
	c.t = c.t.Add(AllocationWindow + time.Second)
	for range 10 {
		m.Allocation(100*time.Millisecond, false)
	}
	if a := alloc(); !a.OK || a.Detail != "最近 5 分钟 10 次，失败 0 次，最慢 100ms" {
		t.Fatalf("after the window: %+v", a)
	}
	m.Allocation(SlowAllocation+time.Second, false)
	if alloc().OK {
		t.Fatalf("a slow one: %+v", alloc())
	}
}

func TestBackup(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	var newest time.Time
	m := New(c.now, func() (time.Time, error) { return newest, nil })
	if b := check(t, m.Check(nil, nil), "backup"); b.OK {
		t.Fatalf("no backup yet: %+v", b)
	}
	newest = c.t.Add(-time.Hour)
	if b := check(t, m.Check(nil, nil), "backup"); !b.OK {
		t.Fatalf("an hour old: %+v", b)
	}
	newest = c.t.Add(-StaleBackup - time.Minute)
	if b := check(t, m.Check(nil, nil), "backup"); b.OK {
		t.Fatalf("stale: %+v", b)
	}
}

// The parts that report to a monitor run without one.
func TestNilMonitor(t *testing.T) {
	var m *Monitor
	m.Pass("release", nil)
	m.Contact("us-1", nil)
	m.Allocation(time.Second, false)
}

func TestStatusWatchesWhatItIsTold(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := New(c.now, nil)
	m.Watch([]string{"release"}, func() []string { return []string{"us-1"} })
	s := m.Status()
	check(t, s, "loop release")
	check(t, s, "cluster us-1")
}

// A loop that gets through every pass and fails in each needs a person.
func TestALoopThatKeepsFailing(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := New(c.now, nil)
	m.Pass("release", nil)
	for range 10 {
		c.t = c.t.Add(15 * time.Second)
		m.Pass("release", errors.New("cluster us-1: apply secret: forbidden"))
	}
	l := check(t, m.Check([]string{"release"}, nil), "loop release")
	if l.OK || !strings.Contains(l.Detail, "每轮都出错") || strings.Contains(l.Detail, "forbidden") {
		t.Fatalf("failing loop: %+v", l)
	}
	m.Pass("release", nil)
	if l := check(t, m.Check([]string{"release"}, nil), "loop release"); !l.OK {
		t.Fatalf("after a good pass: %+v", l)
	}
}

func TestOffsite(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	var last time.Time
	m := New(c.now, nil)
	m.WatchOffsite(func() (time.Time, error) { return last, nil })
	if o := check(t, m.Check(nil, nil), "offsite"); o.OK {
		t.Fatalf("never copied: %+v", o)
	}
	last = c.t.Add(-time.Hour)
	if o := check(t, m.Check(nil, nil), "offsite"); !o.OK {
		t.Fatalf("copied an hour ago: %+v", o)
	}
	last = c.t.Add(-StaleBackup - time.Minute)
	if o := check(t, m.Check(nil, nil), "offsite"); o.OK {
		t.Fatalf("stale: %+v", o)
	}
}

func TestStuckGameServers(t *testing.T) {
	c := &clock{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := New(c.now, nil)
	servers := func() Check { t.Helper(); return check(t, m.Check(nil, []string{"us-1"}), "gameservers us-1") }
	if s := servers(); !s.OK || !strings.Contains(s.Detail, "还没看过") {
		t.Fatalf("nothing seen yet: %+v", s)
	}
	m.Starting("us-1", []Starting{{State: "Scheduled", Created: c.t.Add(-time.Minute)}})
	if s := servers(); !s.OK || !strings.Contains(s.Detail, "1 个正在启动") {
		t.Fatalf("one starting for a minute: %+v", s)
	}
	m.Starting("us-1", []Starting{
		{State: "Scheduled", Created: c.t.Add(-time.Minute)},
		{State: "Starting", Created: c.t.Add(-StuckGameServer - 2*time.Minute)},
		{State: "Scheduled", Created: c.t.Add(-StuckGameServer - time.Minute)},
	})
	s := servers()
	if s.OK || !strings.Contains(s.Detail, "2 个战斗服") || !strings.Contains(s.Detail, "停在 Starting") || !strings.Contains(s.Detail, "CPU 或内存") {
		t.Fatalf("two stuck, the oldest unscheduled: %+v", s)
	}
	m.Starting("us-1", []Starting{{State: "Scheduled", Created: c.t.Add(-StuckGameServer - time.Minute)}})
	if s := servers(); s.OK || !strings.Contains(s.Detail, "已经分到节点") {
		t.Fatalf("one stuck on a node: %+v", s)
	}
	m.Starting("us-1", nil)
	if s := servers(); !s.OK || s.Detail != "没有卡住的战斗服" {
		t.Fatalf("all up: %+v", s)
	}
	// GameFleet lost the cluster with a stuck game server in view: the
	// cluster's check fails, this one cannot tell.
	m.Starting("us-1", []Starting{{State: "Starting", Created: c.t.Add(-StuckGameServer - time.Minute)}})
	c.t = c.t.Add(StaleCluster + time.Second)
	if s := servers(); !s.OK || !strings.Contains(s.Detail, "判断不了") {
		t.Fatalf("cluster lost: %+v", s)
	}
}
