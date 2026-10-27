// Package health keeps what GameFleet knows of its own health: when its
// loops last got through, when it last reached each cluster, how allocation
// went lately, and how old the newest daily backup is. An outside monitor
// reads it at /status, which answers 503 when something needs a person.
//
// /healthz stays what it was: the process runs, and its version. The updater
// waits on it, and a cluster being down must not fail an update.
package health

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// Thresholds, for the docs and the operator.
const (
	// StaleLoop: a loop runs every 15 seconds; one that has not got through
	// for this long is stuck.
	StaleLoop = 2 * time.Minute
	// StaleCluster: a cluster GameFleet has not reached for this long is
	// down for it.
	StaleCluster = 2 * time.Minute
	// AllocationWindow is how far back allocation counts. When at least
	// MinAllocations ran in it and FailedShare of them failed, or one took
	// longer than SlowAllocation, allocation is unwell.
	AllocationWindow = 5 * time.Minute
	MinAllocations   = 5
	FailedShare      = 0.5
	SlowAllocation   = 10 * time.Second
	// StaleBackup: the daily backup runs once a day, at a random time in an
	// hour; one older than this has failed or stopped.
	StaleBackup = 26 * time.Hour
)

// Monitor collects the facts. Its methods do nothing on a nil Monitor, so
// the parts that report to it run without one in tests.
type Monitor struct {
	now func() time.Time
	// newestBackup returns when the newest daily backup was written, zero
	// when there is none.
	newestBackup func() (time.Time, error)

	// What Status watches: the loops by name, and the clusters there are.
	loops    []string
	clusters func() []string

	mu       sync.Mutex
	started  time.Time
	passes   map[string]pass    // by loop
	contacts map[string]contact // cluster: when GameFleet last reached it
	allocs   []allocation
}

// pass is when a loop last got through a pass, and last got through one
// without an error.
type pass struct {
	done, ok time.Time
}

type contact struct {
	ok      time.Time // last success
	lastErr string
}

type allocation struct {
	at     time.Time
	took   time.Duration
	failed bool
}

// New returns a monitor. newestBackup may be nil when there are no daily
// backups to watch.
func New(now func() time.Time, newestBackup func() (time.Time, error)) *Monitor {
	return &Monitor{now: now, newestBackup: newestBackup, started: now(), passes: map[string]pass{}, contacts: map[string]contact{}}
}

// Pass records that a loop got through a pass, and whether the pass had an
// error: a loop that gets through every pass and fails in each needs a
// person as much as one that is stuck.
func (m *Monitor) Pass(loop string, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.passes[loop]
	p.done = m.now()
	if err == nil {
		p.ok = p.done
	}
	m.passes[loop] = p
}

// Contact records whether GameFleet reached the cluster.
func (m *Monitor) Contact(cluster string, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.contacts[cluster]
	if err == nil {
		c.ok, c.lastErr = m.now(), ""
	} else {
		c.lastErr = err.Error()
	}
	m.contacts[cluster] = c
}

// Allocation records one allocation: how long it took and whether it failed
// on GameFleet's or a cluster's side. Callers leave out requests that were
// wrong, or for a route there is not: those are the caller's, and counted
// they would dilute real failures.
func (m *Monitor) Allocation(took time.Duration, failed bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.allocs = append(m.allocs, allocation{at: now, took: took, failed: failed})
	// Keep the window only.
	i := 0
	for i < len(m.allocs) && now.Sub(m.allocs[i].at) > AllocationWindow {
		i++
	}
	m.allocs = m.allocs[i:]
}

// Check is one thing the monitor looks at.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Status is all the checks. OK when every one is.
type Status struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Watch names the loops to check and where the clusters come from.
func (m *Monitor) Watch(loops []string, clusters func() []string) {
	m.loops, m.clusters = loops, clusters
}

// Status checks what Watch named, allocation and the daily backup.
func (m *Monitor) Status() Status {
	var clusters []string
	if m.clusters != nil {
		clusters = m.clusters()
	}
	return m.Check(m.loops, clusters)
}

// Check checks the loops named, the clusters named, allocation and the daily
// backup.
func (m *Monitor) Check(loops, clusters []string) Status {
	m.mu.Lock()
	now := m.now()
	var checks []Check
	for _, loop := range loops {
		p, ran := m.passes[loop]
		c := Check{Name: "loop " + loop}
		switch {
		case !ran:
			c.Detail = fmt.Sprintf("启动 %s 了，还没跑完一轮", age(now.Sub(m.started)))
			c.OK = now.Sub(m.started) <= StaleLoop
		case now.Sub(p.done) > StaleLoop:
			c.Detail = fmt.Sprintf("上一轮 %s 前跑完，之后卡住了", age(now.Sub(p.done)))
		case p.ok.IsZero() && now.Sub(m.started) > StaleLoop:
			c.Detail = "每轮都跑完了，但启动以来每轮都出错，详情看 GameFleet 的日志"
		case !p.ok.IsZero() && now.Sub(p.ok) > StaleLoop:
			c.Detail = fmt.Sprintf("每轮都跑完了，但 %s 来每轮都出错，详情看 GameFleet 的日志", age(now.Sub(p.ok)))
		default:
			c.OK = true
			c.Detail = fmt.Sprintf("上一轮 %s 前跑完", age(now.Sub(p.done)))
		}
		checks = append(checks, c)
	}
	for _, name := range clusters {
		ct := m.contacts[name]
		c := Check{Name: "cluster " + name}
		switch {
		case !ct.ok.IsZero() && now.Sub(ct.ok) <= StaleCluster:
			c.OK, c.Detail = true, fmt.Sprintf("%s 前连上", age(now.Sub(ct.ok)))
		case ct.ok.IsZero() && now.Sub(m.started) <= StaleCluster:
			c.OK, c.Detail = true, "刚启动，还没连过"
		case ct.ok.IsZero():
			c.Detail = "启动以来一直连不上"
		default:
			c.Detail = fmt.Sprintf("%s 前最后一次连上", age(now.Sub(ct.ok)))
		}
		checks = append(checks, c)
	}
	var count, failed int
	var slowest time.Duration
	for _, a := range m.allocs {
		if now.Sub(a.at) > AllocationWindow {
			continue
		}
		count++
		if a.failed {
			failed++
		}
		slowest = max(slowest, a.took)
	}
	m.mu.Unlock()

	alloc := Check{Name: "allocation", OK: true, Detail: fmt.Sprintf("最近 %s %d 次，失败 %d 次，最慢 %s",
		age(AllocationWindow), count, failed, slowest.Round(time.Millisecond))}
	if count >= MinAllocations && float64(failed) >= FailedShare*float64(count) || slowest > SlowAllocation {
		alloc.OK = false
	}
	checks = append(checks, alloc)

	if m.newestBackup != nil {
		c := Check{Name: "backup"}
		at, err := m.newestBackup()
		switch {
		case err != nil:
			c.Detail = "读不到备份目录，详情看 GameFleet 的日志"
		case at.IsZero():
			c.Detail = "还没有每日备份"
		default:
			c.OK = now.Sub(at) <= StaleBackup
			c.Detail = fmt.Sprintf("最新一份是 %s 前的", age(now.Sub(at)))
		}
		checks = append(checks, c)
	}
	return Status{OK: !slices.ContainsFunc(checks, func(c Check) bool { return !c.OK }), Checks: checks}
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1f 小时", d.Hours())
	}
}
