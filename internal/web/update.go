package web

import (
	"context"
	"errors"
	"net/http"

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
	Latest(ctx context.Context, refresh bool) (*update.Release, error)
	Status() (update.Status, error)
	Download(version string) error
	Apply(ctx context.Context, version string) error
}

type updatePage struct {
	Current    string
	Released   bool
	HasUpdater bool
	// Latest is the latest release, nil when GitHub could not be asked.
	Latest   *update.Release
	CheckErr string
	// Newer is set when Latest is newer than the running version.
	Newer  bool
	Status update.Status
	// Ready means the updater has downloaded Latest and can put it in place.
	Ready bool
	Errs  []string
}

// updatePage shows the running and latest versions, the release notes, and
// the next step: download, restart, or nothing.
func (c *Console) updatePage(w http.ResponseWriter, r *http.Request, p page) {
	data := updatePage{Current: c.updates.Current(), Released: c.updates.Released(), HasUpdater: c.updates.HasUpdater()}
	if data.Released {
		latest, err := c.updates.Latest(r.Context(), true)
		if err != nil {
			data.CheckErr = err.Error()
		}
		data.Latest = latest
		data.Newer = latest != nil && update.Newer(latest.Version, data.Current)
	}
	st, err := c.updates.Status()
	if err != nil {
		data.Errs = append(data.Errs, "读不出更新器的状态："+err.Error())
	}
	data.Status = st
	data.Ready = data.Latest != nil && st.Version == data.Latest.Version &&
		(st.State == update.Downloaded || st.State == update.Failed && st.Action == "apply")
	p.Title = "更新"
	p.Nav = "settings"
	if st.Busy() {
		p.Refresh, p.RefreshSeconds = true, 2
	}
	p.Data = data
	c.render(w, http.StatusOK, "update", p)
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

// refusal is a change the console turns down, with the reason to show.
func refusal(text string) error { return &release.Error{Kind: release.ErrConflict, Text: text} }
