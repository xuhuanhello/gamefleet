package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/update"
)

// fakeUpdates stands in for the update service and the updater behind it.
type fakeUpdates struct {
	current            string
	updater            bool
	latest             *update.Release
	status             update.Status
	downloads, applies []string
}

func (f *fakeUpdates) Current() string                { return f.current }
func (f *fakeUpdates) Released() bool                 { return f.current != "dev" }
func (f *fakeUpdates) HasUpdater() bool               { return f.updater }
func (f *fakeUpdates) Status() (update.Status, error) { return f.status, nil }
func (f *fakeUpdates) Latest(context.Context, bool) (*update.Release, error) {
	return f.latest, nil
}
func (f *fakeUpdates) Available() *update.Release {
	if f.latest != nil && update.Newer(f.latest.Version, f.current) {
		return f.latest
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

func TestUpdatingGameFleetFromTheConsole(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	u := e.updates

	// Up to date: the menu names the version and offers nothing.
	_, body := e.get(t, "/")
	if page := visible(body); !strings.Contains(page, "GameFleet v2.0.0") || strings.Contains(page, "新版本") {
		t.Errorf("menu:\n%s", page)
	}

	u.latest = &update.Release{Version: "2.1.0", Name: "GameFleet v2.1.0", Notes: "- 战斗服页显示联机版本\n- <b>修复</b>",
		URL: "https://github.com/xuhuanhello/gamefleet/releases/tag/v2.1.0", Published: time.Now().Add(-2 * time.Hour)}
	_, body = e.get(t, "/")
	if page := visible(body); !strings.Contains(page, "新版本 v2.1.0 查看并更新") {
		t.Errorf("menu with a release out:\n%s", page)
	}
	_, body = e.get(t, "/settings/update")
	page := visible(body)
	for _, want := range []string{"GameFleet v2.0.0", "v2.1.0 2h0m前", "- 战斗服页显示联机版本", "在 GitHub 上看完整的更新说明", "下载 v2.1.0"} {
		if !strings.Contains(page, want) {
			t.Errorf("update page lacks %q\n%s", want, page)
		}
	}
	if strings.Contains(body, "<b>修复</b>") {
		t.Error("release notes are not escaped")
	}

	// Download, with progress, then restart.
	e.expectRedirect(t, "/settings/update/download", url.Values{"version": {"2.1.0"}}, "/settings/update")
	if len(u.downloads) != 1 || u.downloads[0] != "2.1.0" {
		t.Fatalf("downloads = %v", u.downloads)
	}
	u.status.Done, u.status.Total = 31, 50
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "正在下载 v2.1.0：62%") || !strings.Contains(body, `data-refresh="2"`) {
		t.Errorf("downloading:\n%s", page)
	}
	resp, body := e.post(t, "/settings/update/download", url.Values{"version": {"2.1.0"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(visible(body), "更新器还在处理上一个请求") {
		t.Errorf("download while downloading: %d", resp.StatusCode)
	}
	u.status = update.Status{Action: "download", Version: "2.1.0", State: update.Downloaded}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "已经下载好了") || !strings.Contains(page, "重启并更新") || !strings.Contains(body, "data-confirm=") {
		t.Errorf("downloaded:\n%s", page)
	}
	e.expectRedirect(t, "/settings/update/apply", url.Values{"version": {"2.1.0"}}, "/settings/update")
	if len(u.applies) != 1 {
		t.Fatalf("applies = %v", u.applies)
	}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "正在更新到 v2.1.0") {
		t.Errorf("applying:\n%s", page)
	}

	// The new version did not start: the reason shows, and it can be tried again.
	u.status = update.Status{Action: "apply", Version: "2.1.0", State: update.Failed, Error: "新版本没有启动，已经回到原来的版本"}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "更新到 v2.1.0 没有成功：新版本没有启动") || !strings.Contains(page, "重启并更新") {
		t.Errorf("failed:\n%s", page)
	}

	// After the restart the new version says so.
	u.current = "2.1.0"
	u.status = update.Status{Action: "apply", Version: "2.1.0", State: update.Applied}
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "已经更新到 v2.1.0") || !strings.Contains(page, "已经是最新版本") {
		t.Errorf("applied:\n%s", page)
	}

	_, body = e.get(t, "/settings/audit")
	if page := visible(body); !strings.Contains(page, "admin 下载新版本 v2.1.0 从 v2.0.0") || !strings.Contains(page, "admin 更新 GameFleet v2.1.0 从 v2.0.0") {
		t.Errorf("audit:\n%s", page)
	}
}

func TestUpdatesWithoutAnUpdaterOrARelease(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.updates.updater = false
	e.updates.latest = &update.Release{Version: "2.1.0", URL: "https://github.com/xuhuanhello/gamefleet/releases/tag/v2.1.0"}
	_, body := e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "没有装更新器") || strings.Contains(page, "下载 v2.1.0") {
		t.Errorf("no updater:\n%s", page)
	}
	e.updates.current = "dev"
	_, body = e.get(t, "/settings/update")
	if page := visible(body); !strings.Contains(page, "从源码编译") || !strings.Contains(page, "GameFleet dev") {
		t.Errorf("dev build:\n%s", page)
	}
}
