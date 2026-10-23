package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/store"
	"github.com/xuhuanhello/gamefleet/internal/update"
)

// Updates is what the console does with GameFleet's own updates;
// *update.Service is the real one.
type Updates interface {
	Current() string
	Released() bool
	HasUpdater() bool
	Available() *update.Release
	Checked() update.Check
	Check(ctx context.Context, maxAge time.Duration) update.Check
	CheckSoon(maxAge time.Duration)
	Status() (update.Status, error)
	Download(version string) error
	Apply(ctx context.Context, version string) error
}

// GameFleet asks GitHub for new releases only when someone uses the console.
const (
	// pageCheckAge: opening any page checks in the background when the last
	// check is older than this, so that the menu can point at a new release.
	pageCheckAge = time.Hour
	// updateCheckAge: opening the update page or pressing 检查更新 asks again
	// unless the last check is only moments old.
	updateCheckAge = 10 * time.Second
)

// quietRefresh reports whether console.js is refreshing the page in the
// background, as opposed to someone opening it.
func quietRefresh(r *http.Request) bool { return r.Header.Get("X-GameFleet-Refresh") != "" }

type updatePage struct {
	Current    string
	Released   bool
	HasUpdater bool
	Check      update.Check
	// Newer is set when the latest release is newer than the running version.
	Newer   bool
	Status  update.Status
	Finding finding
	// Steps is the way to the latest release, when the console can take it.
	Steps *updateSteps
	Errs  []string
}

// updateSteps are the two steps of an update, download and restart, with
// what happens now and the button for the next one.
type updateSteps struct {
	Steps []joinStep
	// Downloading shows the bar at Percent.
	Downloading bool
	Percent     int
	Detail      string
	Figure      string
	// Action is the button: download or apply, or none while the updater works.
	Action, Label string
	// Restarting is set while the updater puts the new version in place; the
	// page then waits for it on /healthz.
	Restarting bool
}

// updatePage shows the running and latest versions and, when there is a
// newer one, its notes and the way to it.
func (c *Console) updatePage(w http.ResponseWriter, r *http.Request, p page) {
	d := updatePage{Current: c.updates.Current(), Released: c.updates.Released(), HasUpdater: c.updates.HasUpdater()}
	if quietRefresh(r) {
		d.Check = c.updates.Checked()
	} else {
		d.Check = c.updates.Check(r.Context(), updateCheckAge)
	}
	p.Update = c.updates.Available()
	d.Newer = d.Check.Latest != nil && update.Newer(d.Check.Latest.Version, d.Current)
	st, err := c.updates.Status()
	if err != nil {
		d.Errs = append(d.Errs, "读不出更新器的状态："+err.Error())
	}
	d.Status = st
	d.Finding = c.updateFinding(d)
	if d.Newer && d.HasUpdater {
		d.Steps = updateStepsTo(d.Check.Latest.Version, st)
	}
	p.Title = "更新"
	p.Nav = "settings"
	// While GameFleet restarts, console.js watches /healthz instead: the
	// restart ends every session, so a refresh would land on the sign-in
	// page before the page could say how the update went.
	if st.Busy() && st.State != update.Applying {
		p.Refresh, p.RefreshSeconds = true, 2
	}
	p.Data = d
	c.render(w, http.StatusOK, "update", p)
}

// updateFinding is the one line at the top of the update page.
func (c *Console) updateFinding(d updatePage) finding {
	st := d.Status
	pending := update.Newer(st.Version, d.Current) // st is about a version not running yet
	switch {
	case !d.Released:
		return finding{"plain", "从源码编译的 GameFleet 没有版本号，不检查更新。"}
	case pending && st.State == update.Failed && st.Action == "apply":
		return finding{"bad", fmt.Sprintf("更新到 v%s 没有成功，现在跑的还是 v%s。", st.Version, d.Current)}
	case pending && st.State == update.Failed:
		return finding{"bad", fmt.Sprintf("下载 v%s 没有成功。", st.Version)}
	case pending && st.State == update.Downloading:
		return finding{"busy", fmt.Sprintf("正在下载 v%s。", st.Version)}
	case pending && st.State == update.Applying:
		return finding{"busy", fmt.Sprintf("正在更新到 v%s，GameFleet 马上重启。", st.Version)}
	}
	if d.Newer {
		v := d.Check.Latest.Version
		switch {
		case st.Version == v && st.State == update.Downloaded:
			return finding{"busy", fmt.Sprintf("v%s 已经下载好了，可以重启并更新。", v)}
		case !d.HasUpdater:
			return finding{"busy", fmt.Sprintf("有新版本 v%s。这台 GameFleet 没有装更新器，要用安装脚本升级。", v)}
		}
		return finding{"busy", fmt.Sprintf("有新版本 v%s。", v)}
	}
	switch {
	case d.Check.Err != nil:
		return finding{"warn", "检查更新没有成功：" + update.Reason(d.Check.Err) + "。"}
	case d.Check.Latest == nil:
		return finding{"plain", "还没有检查过更新。"}
	case st.State == update.Applied && st.Version == d.Current && c.now().Sub(st.At) < 24*time.Hour:
		return finding{"ok", fmt.Sprintf("已经更新到 v%s，这是最新版本。", d.Current)}
	}
	return finding{"ok", fmt.Sprintf("已经是最新版本 v%s。", d.Current)}
}

// updateStepsTo is where an update to version v stands, given the updater's
// status.
func updateStepsTo(v string, st update.Status) *updateSteps {
	s := &updateSteps{Steps: []joinStep{{Label: "下载新版本"}, {Label: "重启并更新"}}}
	download, restart := &s.Steps[0], &s.Steps[1]
	mine := st.Version == v
	switch {
	case mine && st.State == update.Downloading:
		download.State = "now"
		s.Downloading, s.Percent = true, st.Percent()
		s.Detail = "正在把新版本下载到管理机上，不影响正在运行的 GameFleet。"
		if st.Total > 0 {
			s.Figure = fmt.Sprintf("%s / %s · %d%%", formatMemory(st.Done), formatMemory(st.Total), s.Percent)
		}
	case mine && st.State == update.Failed && st.Action == "download":
		download.State = "failed"
		s.Detail = sentence(st.Error)
		s.Action, s.Label = "download", "重新下载"
	case mine && st.State == update.Downloaded:
		download.State = "done"
		s.Detail = "重启时 GameFleet 停十几秒：这期间新的对局分配不到战斗服，正在打的对局不受影响；所有人都要重新登录控制台。"
		s.Action, s.Label = "apply", "重启并更新"
	case mine && st.State == update.Applying:
		download.State, restart.State = "done", "now"
		s.Restarting = true
		s.Detail = "GameFleet 正在重启，大约十几秒。好了以后这个页面会告诉你结果，再自动刷新。"
	case mine && st.State == update.Failed:
		download.State, restart.State = "done", "failed"
		s.Detail = sentence(st.Error)
		s.Action, s.Label = "apply", "再试一次"
	default:
		s.Detail = "先把新版本下载到管理机上，这不影响正在运行的 GameFleet；下载完再决定什么时候重启。"
		s.Action, s.Label = "download", "下载 v"+v
	}
	return s
}

func (c *Console) updateAction(w http.ResponseWriter, r *http.Request, p page) {
	c.act(w, r, p, "/settings/update", func(ctx context.Context, f formValues) error {
		version := f.text("version")
		var err error
		action := r.PathValue("action")
		switch action {
		case "download":
			err = c.updates.Download(version)
		case "apply":
			err = c.updates.Apply(ctx, version)
		default:
			return refusal("没有这个操作。")
		}
		switch {
		case errors.Is(err, update.ErrBusy):
			return refusal("更新器还在处理上一个请求，等它完成。")
		case errors.Is(err, update.ErrNoUpdater):
			return refusal("这台 GameFleet 没有装更新器，不能在控制台里更新。按文档用安装脚本升级。")
		case err != nil:
			return err
		}
		return c.store.Update(ctx, func(tx *store.Tx) error {
			return tx.Audit(p.User, "gamefleet."+action, "v"+version, "从 v"+c.updates.Current())
		})
	})
}

// sentence ends text with a full stop.
func sentence(text string) string {
	if text == "" || strings.HasSuffix(text, "。") {
		return text
	}
	return text + "。"
}

// releaseNotes shows release notes written in Markdown, as GitHub keeps
// them: list items as a list, other lines as paragraphs. Nothing else is
// interpreted, and everything is escaped.
func releaseNotes(text string) template.HTML {
	var b strings.Builder
	list := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.NewReplacer("**", "", "`", "").Replace(strings.TrimSpace(line))
		item, isItem := strings.CutPrefix(line, "- ")
		if !isItem {
			item, isItem = strings.CutPrefix(line, "* ")
		}
		if isItem != list {
			b.WriteString(map[bool]string{true: "<ul>", false: "</ul>"}[isItem])
			list = isItem
		}
		switch {
		case isItem:
			b.WriteString("<li>" + template.HTMLEscapeString(item) + "</li>")
		case line != "":
			b.WriteString("<p>" + template.HTMLEscapeString(strings.TrimLeft(line, "# ")) + "</p>")
		}
	}
	if list {
		b.WriteString("</ul>")
	}
	return template.HTML(b.String())
}

// refusal is a change the console turns down, with the reason to show.
func refusal(text string) error { return &release.Error{Kind: release.ErrConflict, Text: text} }
