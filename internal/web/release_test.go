package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/release"
)

func (e *env) register(t *testing.T, tag string) int64 {
	t.Helper()
	b, _, err := e.releases.Register(context.Background(), release.Registration{Repository: "billiards", Tag: tag}, "key:ci")
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

// serve deploys build 141 to us-1, moves the test game servers into its
// Fleet, and routes stable to it, so their free places count.
func (e *env) serve(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	id := e.register(t, "141")
	d, err := e.releases.Deploy(ctx, release.DeployRequest{BuildID: id, Cluster: "us-1", Buffer: 1, MaxReplicas: 10, Rooms: 50,
		Resources: release.Defaults.Resources}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rooms, err := e.fake.Rooms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range rooms {
		room.Fleet = d.Fleet
		e.fake.AddRoom(room)
	}
	if err := e.releases.SetRoute(ctx, release.Stable, id, "admin"); err != nil {
		t.Fatal(err)
	}
}

// fleetOf returns the name of the Fleet GameFleet created for the deployment.
func (e *env) fleetOf(t *testing.T, deployment int64) string {
	t.Helper()
	fleets, err := e.fake.ManagedFleets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fleets {
		if f.Deployment == deployment {
			return f.Name
		}
	}
	t.Fatalf("no Fleet for deployment %d in %+v", deployment, fleets)
	return ""
}

func (e *env) reconcile(t *testing.T) {
	t.Helper()
	if err := e.releases.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// expectRedirect posts a form and checks it went back to want.
func (e *env) expectRedirect(t *testing.T, path string, form url.Values, want string) {
	t.Helper()
	resp, body := e.post(t, path, form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
		t.Fatalf("POST %s = %d %s\n%s", path, resp.StatusCode, resp.Header.Get("Location"), visible(body))
	}
}

func deployForm(cluster string) url.Values {
	return url.Values{"cluster": {cluster}, "rooms": {"50"}, "buffer": {"2"}, "max": {"10"},
		"cpuRequest": {"250"}, "cpuLimit": {"1500"}, "memoryRequest": {"256"}, "memoryLimit": {"512"}}
}

func TestBuildsPageListsAndFilters(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.register(t, "141")
	e.register(t, "142")

	_, body := e.get(t, "/builds")
	page := visible(body)
	for _, want := range []string{"共 2 个", "#2 版本 142 billiards dm-v7 22222222", "#1 版本 141", "key:ci"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q\n%s", want, page)
		}
	}
	_, body = e.get(t, "/builds?q=11111")
	if page := visible(body); !strings.Contains(page, "#1 版本 141") || strings.Contains(page, "#2 版本") {
		t.Errorf("filter by revision:\n%s", page)
	}
}

// The release of section 8.2 through the console: deploy, canary,
// promotion, rollback and ending the canary, then the audit log.
func TestReleaseThroughTheConsole(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	b141, b142 := e.register(t, "141"), e.register(t, "142")

	// A build that is not deployed cannot be routed.
	resp, body := e.post(t, "/routes/set", url.Values{"channel": {"stable"}, "build": {"1"}, "back": {"/builds/1"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(visible(body), "billiards #1 没有部署在任何集群，先部署") {
		t.Fatalf("routing an undeployed build: %d\n%s", resp.StatusCode, visible(body))
	}
	if !strings.Contains(body, `href="/builds/1"`) {
		t.Error("the refusal has no way back")
	}

	e.expectRedirect(t, "/builds/1/deploy", deployForm("us-1"), "/builds/1")
	e.expectRedirect(t, "/routes/set", url.Values{"channel": {"stable"}, "build": {"1"}, "back": {"/builds/1"}}, "/builds/1")
	e.reconcile(t)
	_, body = e.get(t, "/builds/1")
	page := visible(body)
	for _, want := range []string{"billiards #1 正式", "正式通道在用这个构建", "us-1 " + e.fleetOf(t, 1) + " 服务中", "CPU 250–1500m"} {
		if !strings.Contains(page, want) {
			t.Errorf("build page lacks %q\n%s", want, page)
		}
	}
	// Canary on the stable build would be no canary.
	if strings.Contains(page, "设为正式") || strings.Contains(page, "设为灰度") || strings.Contains(page, "部署到集群") {
		t.Errorf("build 1 is already stable and deployed everywhere:\n%s", page)
	}

	// 142 to testers, then to everyone, then back.
	e.expectRedirect(t, "/builds/2/deploy", deployForm("us-1"), "/builds/2")
	e.expectRedirect(t, "/routes/set", url.Values{"channel": {"canary"}, "build": {"2"}}, "/routes")
	e.reconcile(t)
	_, body = e.get(t, "/routes")
	page = visible(body)
	for _, want := range []string{"正在灰度：测试账号进 #2，其他玩家进 #1。", "billiards dm-v7 #1 版本 141", "#2 版本 142", "晋升为正式", "结束灰度"} {
		if !strings.Contains(page, want) {
			t.Errorf("routes page lacks %q\n%s", want, page)
		}
	}
	if !strings.Contains(body, `data-confirm=`) {
		t.Error("route changes do not ask first")
	}
	e.expectRedirect(t, "/routes/promote", url.Values{"game": {"billiards"}, "compat": {"dm-v7"}}, "/routes")
	_, body = e.get(t, "/builds/1")
	if page := visible(body); !strings.Contains(page, "排空中") || !strings.Contains(page, "没有路由再用它，正在排空") {
		t.Errorf("after promotion build 1 drains:\n%s", page)
	}
	_, body = e.get(t, "/routes")
	if page := visible(body); !strings.Contains(page, "没有进行中的灰度") {
		t.Errorf("promotion ends the canary:\n%s", page)
	}
	// Back to 141 for everyone, 142 for testers again, then the canary ends.
	e.expectRedirect(t, "/routes/rollback", url.Values{"game": {"billiards"}, "channel": {"stable"}, "compat": {"dm-v7"}}, "/routes")
	e.expectRedirect(t, "/routes/set", url.Values{"channel": {"canary"}, "build": {"2"}}, "/routes")
	e.expectRedirect(t, "/routes/delete", url.Values{"game": {"billiards"}, "channel": {"canary"}, "compat": {"dm-v7"}}, "/routes")
	_, body = e.get(t, "/routes")
	if page := visible(body); !strings.Contains(page, "没有进行中的灰度") || !strings.Contains(page, "之前 #2") {
		t.Errorf("after rollback:\n%s", page)
	}

	// 142 drains now; deleting it works a minute later because no match runs
	// on it.
	e.reconcile(t)
	e.later(release.UndeployWait)
	e.expectRedirect(t, "/deployments/2/delete", url.Values{"back": {"/builds/2"}}, "/builds/2")
	_, body = e.get(t, "/builds/2")
	if page := visible(body); !strings.Contains(page, "删除过的部署") || !strings.Contains(page, "部署到集群") {
		t.Errorf("after deleting the deployment:\n%s", page)
	}
	// The serving deployment cannot be deleted.
	resp, body = e.post(t, "/deployments/1/delete", url.Values{"back": {"/builds/1"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(visible(body), "先改路由") {
		t.Errorf("deleting a serving deployment: %d %s", resp.StatusCode, visible(body))
	}

	_, body = e.get(t, "/settings/audit")
	page = visible(body)
	for _, want := range []string{
		"admin 删除部署 billiards-2-", "admin 删除路由 billiards canary dm-v7 原来是 #2",
		"admin 删除路由 billiards canary dm-v7 原来是 #2，和正式一样，灰度结束",
		"admin 回滚 billiards stable dm-v7 #2 → #1", "admin 晋升 billiards stable dm-v7 #1 → #2",
		"admin 部署 " + e.fleetOf(t, 1) + " @ us-1", "key:ci 登记构建 billiards #1",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("audit log lacks %q\n%s", want, page)
		}
	}
	if b141 != 1 || b142 != 2 {
		t.Errorf("builds %d and %d", b141, b142)
	}
}

func TestOverviewNamesTheBuildOfEachFleet(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.register(t, "141")
	e.expectRedirect(t, "/builds/1/deploy", deployForm("us-1"), "/builds/1")
	e.expectRedirect(t, "/routes/set", url.Values{"channel": {"stable"}, "build": {"1"}}, "/routes")
	e.reconcile(t)
	if fleets, _ := e.fake.ManagedFleets(context.Background()); len(fleets) != 1 {
		t.Fatalf("fleets = %+v", fleets)
	}
	// The fake's console view lists the hand-made Fleet "billiards" and, as
	// a real cluster would, the one GameFleet created.
	view := e.fake.View()
	view.Fleets = append(view.Fleets, cluster.Fleet{Name: e.fleetOf(t, 1), Replicas: 2, Ready: 2})
	e.fake.SetView(view)
	for _, path := range []string{"/", "/clusters/us-1"} {
		_, body := e.get(t, path)
		page := visible(body)
		for _, want := range []string{"3 手动部署", "#1 版本 141 · dm-v7 服务中 正式"} {
			if !strings.Contains(page, want) {
				t.Errorf("%s lacks %q\n%s", path, want, page)
			}
		}
	}
}

func TestPromotingEndsTheCanary(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.register(t, "141")
	e.register(t, "142")
	e.expectRedirect(t, "/builds/1/deploy", deployForm("us-1"), "/builds/1")
	e.expectRedirect(t, "/builds/2/deploy", deployForm("us-1"), "/builds/2")
	// Canary where stable already is ends at once: it would only keep
	// testers apart from everyone else.
	for _, channel := range []string{"stable", "canary"} {
		e.expectRedirect(t, "/routes/set", url.Values{"channel": {channel}, "build": {"1"}}, "/routes")
	}
	_, body := e.get(t, "/builds")
	if page := visible(body); !strings.Contains(page, "11111111 us-1 服务中 正式") || strings.Contains(page, "灰度") {
		t.Errorf("canary like stable:\n%s", page)
	}
	_, body = e.get(t, "/builds/1")
	if page := visible(body); strings.Contains(page, "设为灰度") {
		t.Errorf("the stable build offers to become canary:\n%s", page)
	}
	e.expectRedirect(t, "/routes/set", url.Values{"channel": {"canary"}, "build": {"2"}}, "/routes")
	_, body = e.get(t, "/builds")
	if page := visible(body); !strings.Contains(page, "11111111 us-1 服务中 正式") || !strings.Contains(page, "22222222 us-1 服务中 灰度") {
		t.Errorf("canary on #2:\n%s", page)
	}
	e.expectRedirect(t, "/routes/promote", url.Values{"game": {"billiards"}, "compat": {"dm-v7"}}, "/routes")
	_, body = e.get(t, "/routes")
	if page := visible(body); !strings.Contains(page, "没有进行中的灰度") {
		t.Errorf("after promoting:\n%s", page)
	}
	_, body = e.get(t, "/builds")
	if page := visible(body); !strings.Contains(page, "22222222 us-1 服务中 正式") || strings.Contains(page, "灰度") {
		t.Errorf("after promoting, builds:\n%s", page)
	}
}

func TestTwoCompatVersionsServeSideBySide(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.register(t, "141")
	e.register(t, "200")
	for _, id := range []string{"1", "2"} {
		e.expectRedirect(t, "/builds/"+id+"/deploy", deployForm("us-1"), "/builds/"+id)
		e.expectRedirect(t, "/routes/set", url.Values{"channel": {"stable"}, "build": {id}}, "/routes")
	}
	e.reconcile(t)
	view := e.fake.View()
	view.Fleets = append(view.Fleets, cluster.Fleet{Name: e.fleetOf(t, 1), Replicas: 1}, cluster.Fleet{Name: e.fleetOf(t, 2), Replicas: 1})
	e.fake.SetView(view)
	_, body := e.get(t, "/")
	page := visible(body)
	for _, want := range []string{"#1 版本 141 · dm-v7 服务中 正式", "#2 版本 200 · dm-v8 服务中 正式"} {
		if !strings.Contains(page, want) {
			t.Errorf("overview lacks %q\n%s", want, page)
		}
	}
	_, body = e.get(t, "/routes")
	if page := visible(body); !strings.Contains(page, "billiards dm-v7 #1 版本 141 0s前 · admin – us 停止服务") ||
		!strings.Contains(page, "billiards dm-v8 #2 版本 200 0s前 · admin – us 停止服务") || !strings.Contains(page, "2 个联机版本在服务") {
		t.Errorf("routes:\n%s", page)
	}
	// Old clients are told to update from now on; their build drains.
	e.expectRedirect(t, "/routes/delete", url.Values{"game": {"billiards"}, "channel": {"stable"}, "compat": {"dm-v7"}}, "/routes")
	_, body = e.get(t, "/routes")
	if page := visible(body); strings.Contains(page, "dm-v7") || !strings.Contains(page, "billiards dm-v8 #2") {
		t.Errorf("after stopping dm-v7:\n%s", page)
	}
	_, body = e.get(t, "/builds/1")
	if page := visible(body); !strings.Contains(page, "没有路由再用它，正在排空") {
		t.Errorf("build 1 after stopping its compat version:\n%s", page)
	}
}

func TestKeys(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	resp, body := e.post(t, "/settings/keys", url.Values{"name": {"nakama-2"}, "scope": {"allocate"}})
	page := visible(body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "新 key：nakama-2") || !strings.Contains(page, "只显示这一次") {
		t.Fatalf("create: %d\n%s", resp.StatusCode, page)
	}
	_, body = e.get(t, "/settings/keys")
	if page := visible(body); !strings.Contains(page, "nakama-2 allocate") || strings.Contains(page, "新 key") {
		t.Errorf("list:\n%s", page)
	}
	resp, body = e.post(t, "/settings/keys", url.Values{"name": {"Bad Name"}, "scope": {"allocate"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(visible(body), "名字只能用小写字母、数字和横线") {
		t.Errorf("bad name: %d %s", resp.StatusCode, visible(body))
	}
	e.expectRedirect(t, "/settings/keys/nakama-2/revoke", nil, "/settings/keys")
	_, body = e.get(t, "/settings/keys")
	if strings.Contains(visible(body), "nakama-2 allocate") {
		t.Error("revoked key still listed")
	}
	if resp, _ := e.post(t, "/settings/keys/nakama-2/revoke", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoking twice: %d", resp.StatusCode)
	}
}

func TestChangesNeedASessionAndThisSite(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.post(t, "/routes/set", url.Values{"channel": {"stable"}, "build": {"1"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("without a session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	e.signIn(t)
	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/settings/keys", strings.NewReader("name=evil&scope=push"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site form: %d", resp.StatusCode)
	}
}
