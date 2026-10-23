package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/update"
)

// fakeUpdates stands in for the update service and the updater behind it.
type fakeUpdates struct {
	current string
	updater bool
	check   update.Check
	status  update.Status
	// checks and soon record the maxAge of each Check and CheckSoon.
	checks, soon       []time.Duration
	downloads, applies []string
}

func (f *fakeUpdates) Current() string                { return f.current }
func (f *fakeUpdates) Released() bool                 { return f.current != "dev" }
func (f *fakeUpdates) HasUpdater() bool               { return f.updater }
func (f *fakeUpdates) Status() (update.Status, error) { return f.status, nil }
func (f *fakeUpdates) Checked() update.Check          { return f.check }
func (f *fakeUpdates) Check(_ context.Context, maxAge time.Duration) update.Check {
	f.checks = append(f.checks, maxAge)
	return f.check
}
func (f *fakeUpdates) CheckSoon(maxAge time.Duration) { f.soon = append(f.soon, maxAge) }
func (f *fakeUpdates) Available() *update.Release {
	if f.check.Latest != nil && update.Newer(f.check.Latest.Version, f.current) {
		return f.check.Latest
	}
	return nil
}
func (f *fakeUpdates) Download(v string) error {
	if f.status.Busy() {
		return update.ErrBusy
	}
	f.downloads = append(f.downloads, v)
	f.status = update.Status{Action: "download", Version: v, State: update.Downloading}
	return nil
}
func (f *fakeUpdates) Apply(_ context.Context, v string) error {
	if f.status.Busy() {
		return update.ErrBusy
	}
	f.applies = append(f.applies, v)
	f.status = update.Status{Action: "apply", Version: v, State: update.Applying}
	return nil
}

// quietGet fetches a page the way console.js refreshes it.
func (e *env) quietGet(t *testing.T, path string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.server.URL+path, nil)
	req.Header.Set("X-GameFleet-Refresh", "1")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

var release210 = &update.Release{Version: "2.1.0", Name: "GameFleet v2.1.0",
	Notes: "## 新功能\n- 战斗服页显示联机版本\n- <b>修复</b> 晋升后的灰度",
	URL:   "https://github.com/xuhuanhello/gamefleet/releases/tag/v2.1.0", Published: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)}

func TestUpdatingGameFleetFromTheConsole(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	u := e.updates

	// Up to date: the menu names the version and offers nothing. Opening a
	// page checks in the background, now and then.
	u.soon = nil
	_, body := e.get(t, "/")
	if page := visible(body); !strings.Contains(page, "GameFleet v2.0.0") || strings.Contains(page, "新版本") {
		t.Errorf("menu:\n%s", page)
	}
	if len(u.soon) != 1 || u.soon[0] != pageCheckAge {
		t.Errorf("background checks = %v", u.soon)
	}
	e.quietGet(t, "/")
	if len(u.soon) != 1 {
		t.Errorf("a quiet refresh checked GitHub: %v", u.soon)
	}

	u.check = update.Check{Latest: release210, At: time.Now()}
	_, body = e.get(t, "/")
	if page := visible(body); !strings.Contains(page, "新版本 v2.1.0 查看并更新") {
		t.Errorf("menu with a release out:\n%s", page)
	}

	// Opening the update page asks GitHub right away; refreshing it quietly does not.
	u.soon, u.checks = nil, nil
	_, body = e.get(t, "/settings/update")
	if len(u.checks) != 1 || u.checks[0] != updateCheckAge || len(u.soon) != 0 {
		t.Errorf("checks = %v, background = %v", u.checks, u.soon)
	}
	page := visible(body)
	for _, want := range []string{"有新版本 v2.1.0。", "正在运行 v2.0.0", "最新发布 v2.1.0 2026-10-30 发布", "检查更新",
		"v2.1.0 的更新说明", "在 GitHub 上看", "下载新版本", "重启并更新", "下载 v2.1.0"} {
		if !strings.Contains(page, want) {
			t.Errorf("update page lacks %q\n%s", want, page)
		}
	}
	if !strings.Contains(body, "<li>战斗服页显示联机版本</li>") || !strings.Contains(body, "<p>新功能</p>") ||
		strings.Contains(body, "<b>修复</b>") || !strings.Contains(body, "&lt;b&gt;修复&lt;/b&gt;") {
		t.Errorf("release notes are not a list of escaped text:\n%s", body)
	}
	if !strings.Contains(body, `action="/settings/update" method="get"`) {
		t.Error("检查更新 does not reload the page")
	}
	e.quietGet(t, "/settings/update")
	if len(u.checks) != 1 {
		t.Errorf("a quiet refresh asked GitHub: %v", u.checks)
	}

	// Download, with progress, then restart.
	e.expectRedirect(t, "/settings/update/download", url.Values{"version": {"2.1.0"}}, "/settings/update")
	if len(u.downloads) != 1 || u.downloads[0] != "2.1.0" {
		t.Fatalf("downloads = %v", u.downloads)
	}
	u.status.Done, u.status.Total = 31<<20, 50<<20
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "正在下载 v2.1.0。") || !strings.Contains(page, "31 MiB / 50 MiB · 62%") ||
		!strings.Contains(body, `data-refresh="2"`) || strings.Contains(body, `action="/settings/update/download"`) {
		t.Errorf("downloading:\n%s", page)
	}
	resp, body := e.post(t, "/settings/update/download", url.Values{"version": {"2.1.0"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(visible(body), "更新器还在处理上一个请求") {
		t.Errorf("download while downloading: %d", resp.StatusCode)
	}
	u.status = update.Status{Action: "download", Version: "2.1.0", State: update.Downloaded}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "v2.1.0 已经下载好了，可以重启并更新。") || !strings.Contains(body, "data-confirm=") {
		t.Errorf("downloaded:\n%s", page)
	}
	e.expectRedirect(t, "/settings/update/apply", url.Values{"version": {"2.1.0"}}, "/settings/update")
	if len(u.applies) != 1 {
		t.Fatalf("applies = %v", u.applies)
	}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "正在更新到 v2.1.0，GameFleet 马上重启。") {
		t.Errorf("applying:\n%s", page)
	}
	// The restart ends the session, so the page waits on /healthz rather than
	// refreshing itself onto the sign-in page.
	if !strings.Contains(body, `data-await-version="2.1.0" data-from="2.0.0"`) || strings.Contains(body, "data-refresh") ||
		!strings.Contains(body, "data-reload hidden") {
		t.Errorf("applying page does not wait for the new version:\n%s", body)
	}

	// The new version did not start: the line says so, the reason is in the
	// steps, and it can be tried again.
	u.status = update.Status{Action: "apply", Version: "2.1.0", State: update.Failed, Error: "新版本没有启动起来，已经回到原来的版本"}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "更新到 v2.1.0 没有成功，现在跑的还是 v2.0.0。") ||
		!strings.Contains(page, "新版本没有启动起来，已经回到原来的版本。") || !strings.Contains(page, "再试一次") {
		t.Errorf("failed:\n%s", page)
	}

	// After the restart the new version says so, once.
	u.current = "2.1.0"
	u.status = update.Status{Action: "apply", Version: "2.1.0", State: update.Applied, At: time.Now()}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "已经更新到 v2.1.0，这是最新版本。") || strings.Contains(page, "已经是最新版本") ||
		strings.Contains(page, "更新说明") {
		t.Errorf("applied:\n%s", page)
	}
	u.status.At = time.Now().Add(-25 * time.Hour)
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "已经是最新版本 v2.1.0。") {
		t.Errorf("a day after the update:\n%s", page)
	}

	_, body = e.get(t, "/settings/audit")
	if page := visible(body); !strings.Contains(page, "admin 下载新版本 v2.1.0 从 v2.0.0") || !strings.Contains(page, "admin 更新 GameFleet v2.1.0 从 v2.0.0") {
		t.Errorf("audit:\n%s", page)
	}
}

func TestFailedUpdateChecks(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.updates.check = update.Check{At: time.Now(), Err: errors.New("连不上 GitHub")}
	_, body := e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "检查更新没有成功：连不上 GitHub。") || !strings.Contains(page, "上次检查 0s前 没有成功") {
		t.Errorf("check failed:\n%s", page)
	}
	// A failed download says so on top, and why in the steps.
	e.updates.check = update.Check{Latest: release210, At: time.Now()}
	e.updates.status = update.Status{Action: "download", Version: "2.1.0", State: update.Failed, Error: "镜像没下载下来"}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "下载 v2.1.0 没有成功。") || !strings.Contains(page, "镜像没下载下来。 重新下载") {
		t.Errorf("download failed:\n%s", page)
	}
}

func TestUpdatesWithoutAnUpdaterOrARelease(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.updates.updater = false
	e.updates.check = update.Check{Latest: &update.Release{Version: "2.1.0", URL: "https://github.com/xuhuanhello/gamefleet/releases/tag/v2.1.0"}, At: time.Now()}
	_, body := e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "没有装更新器") || strings.Contains(page, "下载 v2.1.0") || !strings.Contains(page, "这个版本没有写更新说明") {
		t.Errorf("no updater:\n%s", page)
	}
	e.updates.current = "dev"
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "从源码编译的 GameFleet 没有版本号，不检查更新。") ||
		!strings.Contains(page, "GameFleet dev") || strings.Contains(body, `action="/settings/update" method="get"`) {
		t.Errorf("dev build:\n%s", page)
	}
}

func TestReleaseNotes(t *testing.T) {
	got := string(releaseNotes("## v2.1.0\n\n- one **bold**\n* two `code`\n\nafter <script>\n- three"))
	want := "<p>v2.1.0</p><ul><li>one bold</li><li>two code</li></ul><p>after &lt;script&gt;</p><ul><li>three</li></ul>"
	if got != want {
		t.Errorf("releaseNotes =\n%s\nwant\n%s", got, want)
	}
}

// Revoking a cluster's own pull key breaks that cluster, so the question says so.
func TestRevokingAPullKeyNamesItsCluster(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	for _, name := range []string{"pull-us-1", "ci-2"} {
		e.post(t, "/settings/keys", url.Values{"name": {name}, "scope": {"pull"}})
	}
	_, body := e.get(t, "/settings/keys")
	if !strings.Contains(body, "吊销 pull-us-1？这是集群 us-1 拉镜像用的 key") || !strings.Contains(body, "吊销 ci-2？用它的调用方会马上被拒绝。") {
		t.Errorf("revoke questions:\n%s", body)
	}
}
