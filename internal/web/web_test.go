package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/clusters"
	"github.com/xuhuanhello/gamefleet/internal/config"
	"github.com/xuhuanhello/gamefleet/internal/health"
	"github.com/xuhuanhello/gamefleet/internal/registry"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/remote"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

const (
	password = "correct horse battery staple"
	matchID  = "3aca62b8-69c3-4fc0-b500-1ec9f332115e"
	worker   = "us-k3sagent-unity"
	manager  = "us-k3smgr"
)

// HashPassword is slow on purpose, so the tests share one hash.
var hashOnce = sync.OnceValue(func() string {
	hash, err := auth.HashPassword(password)
	if err != nil {
		panic(err)
	}
	return hash
})

type env struct {
	server   *httptest.Server
	client   *http.Client
	fake     *clustertest.Fake
	secret   string
	store    *store.Store
	releases *release.Service
	clusters *clusters.Service
	dir      *sites.Directory
	updates  *fakeUpdates
	// ahead moves the store's clock forward from now.
	ahead   *atomic.Int64
	monitor *health.Monitor
}

// later moves the store's clock forward by d.
func (e *env) later(d time.Duration) { e.ahead.Add(int64(d)) }

// images is a registry with two builds of billiards, tagged 141 and 142.
type images map[string]registry.Image

func (i images) Inspect(_ context.Context, _, ref string) (registry.Image, error) {
	for tag, img := range i {
		if ref == tag || ref == img.Digest {
			return img, nil
		}
	}
	return registry.Image{}, registry.ErrNotFound
}

var testImages = images{
	"141": {Digest: "sha256:" + strings.Repeat("a", 64), Port: 7770, Protocol: "UDP", Labels: map[string]string{
		release.LabelCompat: "dm-v7", release.LabelVersion: "141", release.LabelRevision: "1111111111"}},
	"142": {Digest: "sha256:" + strings.Repeat("b", 64), Port: 7770, Protocol: "UDP", Labels: map[string]string{
		release.LabelCompat: "dm-v7", release.LabelVersion: "142", release.LabelRevision: "2222222222"}},
	// A newer client: it needs game servers of its own compat version.
	"200": {Digest: "sha256:" + strings.Repeat("c", 64), Port: 7770, Protocol: "UDP", Labels: map[string]string{
		release.LabelCompat: "dm-v8", release.LabelVersion: "200", release.LabelRevision: "3333333333"}},
}

func newEnv(t *testing.T) *env {
	t.Helper()
	secret := auth.NewTOTPSecret()
	a, err := auth.New([]config.User{{Name: "admin", PasswordHash: hashOnce(), TOTPSecret: secret}})
	if err != nil {
		t.Fatal(err)
	}
	fake := clustertest.New("us-1", 0)
	now := time.Now()
	fake.SetView(clustertest.View{
		Nodes: []cluster.Node{
			{Name: manager, Ready: true, Roles: []string{"control-plane"}, InternalIP: "10.4.0.4",
				Allocatable: cluster.Resources{MilliCPU: 2000, Memory: 4 << 30}, Taints: []string{"CriticalAddonsOnly=true:NoExecute"}},
			{Name: worker, Ready: true, InternalIP: "10.4.0.3", ExternalIP: "203.0.113.20",
				Allocatable: cluster.Resources{MilliCPU: 2000, Memory: 2 << 30}, Created: now.Add(-6 * 24 * time.Hour)},
		},
		Pods: []cluster.Pod{
			{Namespace: "kube-system", Name: "coredns-c5fdd76cf-m5ph7", Node: manager, Owner: "ReplicaSet", Status: "Running",
				Ready: 1, Containers: []string{"coredns"}, Requests: cluster.Resources{MilliCPU: 100, Memory: 70 << 20}},
			{Namespace: "games", Name: "billiards-a", Node: worker, Owner: "GameServer", Status: "Running",
				Ready: 2, Containers: []string{"billiards", "agones-gameserver-sidecar"}, Requests: cluster.Resources{MilliCPU: 280, Memory: 256 << 20}},
			{Namespace: "games", Name: "billiards-b", Node: worker, Owner: "GameServer", Status: "Running",
				Ready: 2, Containers: []string{"billiards", "agones-gameserver-sidecar"}, Requests: cluster.Resources{MilliCPU: 280, Memory: 256 << 20}},
		},
		Fleets: []cluster.Fleet{{Name: "billiards", Image: "fleet.example.com/billiards:3", Replicas: 2, Ready: 1, Allocated: 1}},
		Events: []cluster.Event{
			{Namespace: "games", Kind: "Pod", Name: "billiards-a", Type: "Warning", Reason: "Unhealthy",
				Message: "Liveness probe failed", Count: 2, Last: now.Add(-time.Minute)},
			{Namespace: "games", Kind: "GameServer", Name: "billiards-a", Type: "Normal", Reason: "Allocated", Count: 1, Last: now},
		},
		Logs: map[string]string{
			"games/billiards-a/billiards":                 "DM_SERVER_READY\nDM_ALLOCATED match=" + matchID + " <script>\n",
			"games/billiards-a/agones-gameserver-sidecar": "sdk server started\n",
		},
		Usage: cluster.Usage{
			Nodes: map[string]cluster.Resources{worker: {MilliCPU: 85, Memory: 938 << 20}, manager: {MilliCPU: 70, Memory: 1942 << 20}},
			Pods:  map[string]cluster.Resources{"games/billiards-a": {MilliCPU: 21, Memory: 170 << 20}},
		},
	})
	fake.AddRoom(cluster.Room{
		ID: "billiards-a", State: "Allocated", Address: "203.0.113.20", Port: 20824, Fleet: "billiards-1", Capacity: 50,
		Matches: []cluster.Match{{ID: matchID, MatchInfo: cluster.MatchInfo{
			Players: json.RawMessage(`[{"userId":"u-bob","seat":1},{"userId":"u-alice","seat":0}]`),
			Channel: "stable", Compat: "dm-v7", Allocated: now.Add(-5 * time.Minute)}}},
		Node: worker, Image: "fleet.example.com/billiards:3", Game: "billiards", Created: now.Add(-time.Hour),
	})
	fake.AddRoom(cluster.Room{ID: "billiards-b", State: "Ready", Address: "203.0.113.20", Port: 20175, Fleet: "billiards-1", Node: worker, Capacity: 50})

	db, err := store.Open(filepath.Join(t.TempDir(), "gamefleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ahead := &atomic.Int64{}
	db.SetClock(func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := sites.New(sites.Site{Name: "us-1", Region: "us", Cluster: fake})
	keys := auth.NewKeys(db)
	joined := map[string]*clustertest.Fake{}
	connect := func(c store.Cluster) (cluster.API, error) {
		if joined[c.Name] == nil {
			joined[c.Name] = clustertest.New(c.Name, 0)
		}
		return joined[c.Name], nil
	}
	fleets := clusters.New(db, dir, keys, connect, "fleet.example.com", log)
	fleets.SetHostKeyReader(func(context.Context, string, int) (remote.HostKey, error) {
		return remote.HostKey{Fingerprint: "SHA256:abcdef", File: "/etc/ssh/ssh_host_ed25519_key.pub"}, nil
	})
	releases := release.New(db, testImages, "fleet.example.com", dir, 24*time.Hour, log)
	updates := &fakeUpdates{current: "2.0.0", updater: true}
	monitor := health.New(time.Now, nil)
	monitor.Watch(nil, nil)
	console, err := New(Options{
		Host: "fleet.example.com", Sites: dir, Clusters: fleets, Auth: a, Keys: keys,
		Store: db, Releases: releases, Updates: updates, Monitor: monitor, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(console.Handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &env{server: server, client: client, fake: fake, secret: secret, store: db, releases: releases, clusters: fleets, dir: dir, updates: updates, ahead: ahead, monitor: monitor}
}

func (e *env) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := e.client.Get(e.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (e *env) post(t *testing.T, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := e.client.PostForm(e.server.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (e *env) loginForm(t *testing.T, next string) url.Values {
	t.Helper()
	code, err := auth.Code(e.secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{"name": {"admin"}, "password": {password}, "code": {code}, "next": {next}}
}

// signIn starts a session. A TOTP code works once, so each env signs in once.
func (e *env) signIn(t *testing.T) {
	t.Helper()
	if resp, body := e.post(t, "/login", e.loginForm(t, "/")); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: %d %s", resp.StatusCode, body)
	}
}

func contains(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

func TestPagesNeedASession(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/servers?q=3aca", "/clusters/us-1/nodes/" + worker} {
		resp, _ := e.get(t, path)
		if want := "/login?next=" + url.QueryEscape(path); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
			t.Errorf("GET %s = %d to %q, want a redirect to %q", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
	}
}

func TestSignInTakesPasswordAndCode(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get(t, "/login?next=%2Fservers%3Fq%3D3aca")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("login page: %d, CSP %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	contains(t, body, `name="code"`, `value="/servers?q=3aca"`)

	wrong := e.loginForm(t, "/")
	wrong.Set("code", "000000")
	if resp, body := e.post(t, "/login", wrong); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "验证码不对") {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}

	resp, _ = e.post(t, "/login", e.loginForm(t, "/servers?q=3aca"))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/servers?q=3aca" {
		t.Fatalf("sign-in: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookie := resp.Cookies()[0]
	if cookie.Name != SessionCookie || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v", cookie)
	}
	if resp, _ := e.get(t, "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("overview after sign-in: %d", resp.StatusCode)
	}

	if resp, _ := e.post(t, "/logout", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("overview after logout: %d", resp.StatusCode)
	}
}

func TestSignInStaysOnThisSite(t *testing.T) {
	for _, next := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "rooms"} {
		if got := localPath(next); got != "/" {
			t.Errorf("localPath(%q) = %q", next, got)
		}
	}
	if got := localPath("/clusters/us-1?x=1"); got != "/clusters/us-1?x=1" {
		t.Errorf("a local path was changed to %q", got)
	}
}

func TestCrossSiteFormsAreRejected(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/login", strings.NewReader(e.loginForm(t, "/").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site sign-in = %d", resp.StatusCode)
	}
}

func TestOverview(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.serve(t)
	_, body := e.get(t, "/")
	contains(t, visible(body),
		"us-1 地区 us",
		"能开局，但最近一小时有 1 条警告。",
		"进行中的对局 1 还能再开 99 局",
		"机器 2 / 2 正常",
		"CPU（核） 0.16 / 4",
		"内存 2.8 GiB / 6.0 GiB",
		"最近一小时的警告 1",
		matchID+" billiards-a "+worker+" 5m前 2",
	)
	contains(t, body, `data-refresh="10"`, `data-copy="`+matchID+`"`)
}

// The overview's first line says what needs attention.
func TestOverviewFinding(t *testing.T) {
	healthy := cluster.Node{Name: worker, Ready: true, Allocatable: cluster.Resources{MilliCPU: 2000, Memory: 2 << 30}}
	unreadable := errors.New("connection reset")
	cases := map[string]struct {
		view  clustertest.View
		ready bool
		want  string
	}{
		"all good":     {clustertest.View{Nodes: []cluster.Node{healthy}}, true, "一切正常，现在还能立刻开 99 局。"},
		"no idle room": {clustertest.View{Nodes: []cluster.Node{healthy}}, false, "战斗服都满了，新的对局会分配失败。"},
		"node down":    {clustertest.View{Nodes: []cluster.Node{healthy, {Name: "us-2", Ready: false}}}, true, "1 台机器失联：us-2。"},
		// What could not be read is not taken as fine.
		"events unreadable": {clustertest.View{Nodes: []cluster.Node{healthy}, EventsErr: unreadable}, true,
			"能开局，但读不到警告（connection reset），没法确认一切正常。"},
		"pods unreadable": {clustertest.View{Nodes: []cluster.Node{healthy}, PodsErr: unreadable, EventsErr: unreadable}, true,
			"能开局，但读不到系统组件和警告（connection reset），没法确认一切正常。"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.signIn(t)
			e.serve(t)
			e.fake.SetView(tc.view)
			if !tc.ready {
				for _, id := range []string{"billiards-a", "billiards-b"} {
					e.fake.AddRoom(cluster.Room{ID: id, State: "Allocated", Node: worker, Capacity: 1, Matches: []cluster.Match{{ID: "m-" + id}}})
				}
			}
			_, body := e.get(t, "/")
			contains(t, visible(body), tc.want)
		})
	}
}

var tags = regexp.MustCompile(`<[^>]*>`)

// visible is the text a reader sees, with runs of whitespace collapsed.
func visible(html string) string {
	// A word break opportunity is no gap in the text.
	html = strings.ReplaceAll(html, "<wbr>", "")
	return strings.Join(strings.Fields(tags.ReplaceAllString(html, " ")), " ")
}

// The console must answer "which machine runs this match".
func TestFindTheMachineOfAMatch(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/servers?q=3ACA62B8")
	contains(t, body, matchID, "/clusters/us-1/servers/billiards-a", "/clusters/us-1/nodes/"+worker, "203.0.113.20:20824", "5m前")
	if strings.Contains(body, "billiards-b") {
		t.Error("the search kept a room of another match")
	}

	_, body = e.get(t, "/servers?q=no-such-match")
	contains(t, body, "已结束的对局请到你的游戏后端查结果")

	_, body = e.get(t, "/servers")
	if strings.Index(body, "billiards-a") > strings.Index(body, "billiards-b") {
		t.Error("servers in a match should come first")
	}
}

// The console must answer "what runs on this machine".
func TestNodeShowsItsPodsAndMatches(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.serve(t)
	resp, body := e.get(t, "/clusters/us-1/nodes/"+worker)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node page: %d", resp.StatusCode)
	}
	contains(t, body, "billiards-a", "billiards-b", "203.0.113.20", "170 MiB", "/clusters/us-1/pods/games/billiards-a/log")
	contains(t, visible(body), "CPU 使用（核） 0.09 / 2", "进行中的对局 1 还能再开 99 局", "对局中", "空闲", "1 / 50 局", "0 / 50 局")
	if strings.Contains(body, "coredns") {
		t.Error("the worker lists a pod of another node")
	}
}

func TestClusterPage(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/clusters/us-1")
	contains(t, body, manager, worker, "control-plane", "0.56 / 2", "Unhealthy", "Liveness probe failed")
	if strings.Contains(body, ">Allocated<") {
		t.Error("the warnings list shows a normal event")
	}
}

func TestRoomShowsPlayersLogAndEvents(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/clusters/us-1/servers/billiards-a")
	contains(t, body, "u-alice", "u-bob", "dm-v7", "DM_SERVER_READY", "&lt;script&gt;", "Unhealthy", "0.02 核 / 170 MiB")
	if strings.Index(body, "u-alice") > strings.Index(body, "u-bob") {
		t.Error("players are not in seat order")
	}

	resp, body := e.get(t, "/clusters/us-1/servers/billiards-gone")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "对局结束后战斗服会被删除") {
		t.Errorf("missing room: %d", resp.StatusCode)
	}
}

func TestLogPage(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/clusters/us-1/pods/games/billiards-a/log")
	contains(t, body, "DM_ALLOCATED", "?container=agones-gameserver-sidecar&amp;lines=500")

	_, body = e.get(t, "/clusters/us-1/pods/games/billiards-a/log?container=agones-gameserver-sidecar&lines=2000")
	contains(t, body, "sdk server started", "<strong>2000</strong>")

	for _, path := range []string{
		"/clusters/us-1/pods/games/billiards-a/log?container=etc",
		"/clusters/us-1/pods/games/nope/log",
		"/clusters/eu-1/pods/games/billiards-a/log",
		"/clusters/us-1/pods/Games/billiards-a/log",
	} {
		if resp, _ := e.get(t, path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestEventsPage(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/clusters/us-1/events")
	contains(t, body, "Unhealthy", "Allocated")
	_, body = e.get(t, "/clusters/us-1/events?type=Warning")
	contains(t, body, "Unhealthy")
	if strings.Contains(body, ">Allocated<") {
		t.Error("the warnings filter kept a normal event")
	}
}

func TestPagesShowClusterErrors(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.fake.Fail(fmt.Errorf("%w: cluster us-1: GET /api/v1/nodes returned 403", cluster.ErrUnavailable))
	for _, path := range []string{"/", "/clusters/us-1", "/servers"} {
		resp, body := e.get(t, path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "returned 403") {
			t.Errorf("GET %s = %d without the error", path, resp.StatusCode)
		}
	}
}

func TestMissingMetricsShowADash(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	view := clustertest.View{Nodes: []cluster.Node{{Name: worker, Ready: true, Allocatable: cluster.Resources{MilliCPU: 2000, Memory: 2 << 30}}},
		UsageErr: fmt.Errorf("%w: metrics server is down", cluster.ErrUnavailable)}
	e.fake.SetView(view)
	resp, body := e.get(t, "/clusters/us-1")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<span class="muted">–</span>`) {
		t.Fatalf("cluster page without metrics: %d", resp.StatusCode)
	}
}

// The menu lists the machines of the cluster a page is in, and marks the
// machine a page is about.
func TestMenuListsTheMachinesOfTheCluster(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/")
	if strings.Contains(body, `class="leaf`) {
		t.Error("the overview lists machines in the menu")
	}
	_, body = e.get(t, "/clusters/us-1/nodes/"+worker)
	contains(t, body, `href="/clusters/us-1/nodes/`+manager+`" class="leaf"`, `href="/clusters/us-1/nodes/`+worker+`" class="leaf active"`)
	_, body = e.get(t, "/clusters/us-1")
	contains(t, body, `href="/clusters/us-1" class="active"`, `href="/clusters/us-1/nodes/`+worker+`" class="leaf"`)
	// A game server belongs with the game servers.
	_, body = e.get(t, "/clusters/us-1/servers/billiards-a")
	contains(t, body, `href="/servers" class="active"`)
	contains(t, visible(body), "战斗服 / us-1 / "+worker)
}

func TestServersFilterByStateAndCluster(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/servers?state=ready")
	if !strings.Contains(body, `servers/billiards-b"`) || strings.Contains(body, `servers/billiards-a"`) {
		t.Errorf("idle servers:\n%s", visible(body))
	}
	contains(t, visible(body), "全部 2 对局中 1 空闲 1 启动中或出错 0", "全部集群 us-1")
	_, body = e.get(t, "/servers?state=allocated&cluster=us-1")
	contains(t, body, `servers/billiards-a"`, `href="/servers?cluster=us-1&amp;state=ready"`)
	_, body = e.get(t, "/servers?cluster=eu-9")
	contains(t, visible(body), "没有符合条件的战斗服")
}

// A system component that stays unready shows on the cluster page and in
// the overview's first line; one that just started does not, yet.
func TestStuckSystemComponents(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	view := e.fake.View()
	view.Pods = append(view.Pods,
		cluster.Pod{Namespace: "kube-system", Name: "coredns-c5fdd76cf-x9z8y", Node: manager, Owner: "ReplicaSet",
			Status: "Running", Containers: []string{"coredns"}, Created: time.Now().Add(-10 * time.Minute)},
		cluster.Pod{Namespace: "kube-system", Name: "metrics-server-7f4b6d9bd7-zdkwx", Node: manager, Owner: "ReplicaSet",
			Status: "Running", Containers: []string{"metrics-server"}, Created: time.Now().Add(-30 * time.Second)},
		cluster.Pod{Namespace: "kube-system", Name: "helm-install-agones-cr8tj", Node: manager, Owner: "Job",
			Status: "Succeeded", Containers: []string{"helm"}})
	e.fake.SetView(view)
	_, body := e.get(t, "/clusters/us-1")
	page := visible(body)
	contains(t, page, "系统组件没有就绪：coredns。", "coredns kube-system · coredns-c5fdd76cf-x9z8y 未就绪", "metrics-server")
	if strings.Contains(page, "helm-install-agones") || strings.Contains(page, "coredns、metrics-server") {
		t.Errorf("cluster page:\n%s", page)
	}
	_, body = e.get(t, "/")
	contains(t, visible(body), "系统组件没有就绪：coredns。")
}

// Usage counts K3s and the system too, so it is measured against the whole
// machine; requests against what is left for pods.
func TestUsageCountsAgainstTheWholeMachine(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	view := e.fake.View()
	view.Nodes[1].Capacity = cluster.Resources{MilliCPU: 2000, Memory: 4 << 30}
	e.fake.SetView(view)
	_, body := e.get(t, "/clusters/us-1/nodes/"+worker)
	contains(t, visible(body), "938 MiB / 4.0 GiB", "总量 2 核 · 4.0 GiB", "留给 Pod 2 核 · 2.0 GiB", "Pod 的请求 512 MiB / 2.0 GiB")
}

// A machine can get a name in the console; everything that names the
// machine uses it, and the node's own name stays visible on its pages.
func TestRenameAMachine(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	resp, _ := e.post(t, "/clusters/us-1/nodes/"+worker+"/name", url.Values{"title": {"美西 1 号机"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rename: %d", resp.StatusCode)
	}
	_, body := e.get(t, "/clusters/us-1/nodes/"+worker)
	page := visible(body)
	contains(t, page, "美西 1 号机 正常", "节点名 "+worker, `只在控制台里显示`)
	contains(t, body, `class="leaf active" title="`+worker+`"><span>美西 1 号机</span>`, `value="美西 1 号机"`)
	_, body = e.get(t, "/clusters/us-1")
	contains(t, visible(body), "美西 1 号机 "+worker+" · 10.4.0.3")
	_, body = e.get(t, "/servers")
	contains(t, body, `href="/clusters/us-1/nodes/`+worker+`">美西 1 号机</a>`)
	_, body = e.get(t, "/settings/audit")
	contains(t, visible(body), "机器改名", "改叫 美西 1 号机")

	resp, body = e.post(t, "/clusters/us-1/nodes/"+worker+"/name", url.Values{"title": {strings.Repeat("长", 41)}})
	if resp.StatusCode == http.StatusSeeOther || !strings.Contains(visible(body), "名称最多 40 个字") {
		t.Errorf("long name: %d\n%s", resp.StatusCode, visible(body))
	}
}

// What needs a person about GameFleet itself heads the overview.
func TestOverviewSaysWhatNeedsAPerson(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/")
	if strings.Contains(visible(body), "GameFleet 自己") {
		t.Fatal("a problem shown with none")
	}
	for range health.MinAllocations {
		e.monitor.Allocation(time.Second, true)
	}
	_, body = e.get(t, "/")
	contains(t, visible(body), "GameFleet 自己：allocation，最近 5 分钟 5 次，失败 5 次")
}
