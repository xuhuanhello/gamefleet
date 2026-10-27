package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/clusters"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Releases is what the console changes through release.Service.
type Releases interface {
	Deploy(ctx context.Context, req release.DeployRequest, actor string) (store.Deployment, error)
	Undeploy(ctx context.Context, id int64, actor string) error
	SetScale(ctx context.Context, id int64, rooms, buffer, maxReplicas int, actor string) error
	DefaultsFor(ctx context.Context, game, cluster string) (store.Deployment, error)
	SetRoute(ctx context.Context, channel string, buildID int64, actor string, force bool) error
	Promote(ctx context.Context, game, compat, actor string, force bool) error
	Rollback(ctx context.Context, game, channel, compat, actor string) error
	DeleteRoute(ctx context.Context, game, channel, compat, actor string) error
	Routes(ctx context.Context, game string) ([]release.RouteView, error)
	Retention() time.Duration
}

// Keys is what the console manages through auth.Keys.
type Keys interface {
	List(ctx context.Context) ([]store.Key, error)
	Create(ctx context.Context, name string, scopes []string, expires time.Time, actor string) (string, error)
	Revoke(ctx context.Context, name, actor string) error
}

// state of the release records, read in one transaction.
type releaseState struct {
	builds      map[int64]store.Build
	deployments []store.Deployment // live ones
	routes      []store.Route
}

func (c *Console) readReleases(ctx context.Context) (releaseState, error) {
	st := releaseState{builds: map[int64]store.Build{}}
	err := c.store.View(ctx, func(tx *store.Tx) error {
		var err error
		if st.deployments, err = tx.Deployments(); err != nil {
			return err
		}
		if st.routes, err = tx.Routes(""); err != nil {
			return err
		}
		builds, err := tx.Builds("", 1000)
		if err != nil {
			return err
		}
		for _, b := range builds {
			st.builds[b.ID] = b
		}
		return nil
	})
	return st, err
}

// channels lists the channels that route to the build, stable first.
func (st releaseState) channels(buildID int64) []string {
	var out []string
	for _, r := range st.routes {
		if r.BuildID == buildID && !slices.Contains(out, r.Channel) {
			out = append(out, r.Channel)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(channelOrder(a), channelOrder(b)) })
	return out
}

func channelOrder(channel string) int {
	if channel == release.Stable {
		return 0
	}
	return 1
}

// fleetOf finds the live deployment that owns a Fleet in a cluster.
func (st releaseState) fleetOf(clusterName, fleet string) (store.Deployment, bool) {
	for _, d := range st.deployments {
		if d.Cluster == clusterName && d.Fleet == fleet {
			return d, true
		}
	}
	return store.Deployment{}, false
}

// fleetRow is a Fleet with what GameFleet knows about it.
type fleetRow struct {
	cluster.Fleet
	Deployment *store.Deployment
	Version    string
	// Compat is the compat version of the build: only clients of that
	// version get its game servers.
	Compat   string
	Channels []string
	// Matches running on its game servers, and how many more could start
	// right away.
	Playing, Free int
}

func (st releaseState) fleetRows(clusterName string, fleets []cluster.Fleet, rooms []cluster.Room) []fleetRow {
	rows := make([]fleetRow, 0, len(fleets))
	for _, f := range fleets {
		row := fleetRow{Fleet: f}
		for _, room := range rooms {
			if room.Fleet == f.Name {
				st.tally(clusterName, room, &row.Playing, &row.Free)
			}
		}
		if d, ok := st.fleetOf(clusterName, f.Name); ok {
			row.Deployment = &d
			row.Version = st.builds[d.BuildID].Version()
			row.Compat = st.builds[d.BuildID].Compat
			row.Channels = st.channels(d.BuildID)
		}
		rows = append(rows, row)
	}
	return rows
}

// --- Builds

type chip struct {
	Cluster string
	State   string
}

type buildRow struct {
	store.Build
	Deployed []chip
	Channels []string
}

type buildsPage struct {
	Rows  []buildRow
	Total int
}

// matches reports whether the query appears in the build's game, number,
// version, compat, digest, note or any label.
func (b buildRow) matches(needle string) bool {
	if needle == "" {
		return true
	}
	fields := []string{b.Game, "#" + strconv.FormatInt(b.ID, 10), b.Version(), b.Compat, b.Digest, b.Note}
	for k, v := range b.Labels {
		fields = append(fields, k+"="+v)
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	return false
}

func (c *Console) builds(w http.ResponseWriter, r *http.Request, p page) {
	st, err := c.readReleases(r.Context())
	if err != nil {
		c.fail(w, p, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	needle := strings.ToLower(query)
	var data buildsPage
	ids := make([]int64, 0, len(st.builds))
	for id := range st.builds {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	slices.Reverse(ids)
	for _, id := range ids {
		row := buildRow{Build: st.builds[id], Channels: st.channels(id)}
		for _, d := range st.deployments {
			if d.BuildID == id {
				row.Deployed = append(row.Deployed, chip{d.Cluster, d.State})
			}
		}
		data.Total++
		if row.matches(needle) {
			data.Rows = append(data.Rows, row)
		}
	}
	p.Title = "构建"
	p.Nav = "builds"
	p.Query = query
	p.Data = data
	c.render(w, http.StatusOK, "builds", p)
}

type deploymentRow struct {
	store.Deployment
	Live      *cluster.ManagedFleet // nil when the cluster could not be read
	ReclaimAt time.Time
	// Matches running on its game servers, and how many more could start
	// right away.
	Playing, Free int
}

type label struct{ Key, Value string }

type buildPage struct {
	Build       store.Build
	Finding     finding
	Channels    []string
	Labels      []label
	Deployments []deploymentRow
	History     []deploymentRow // deleted deployments
	Errs        []string
	// The deploy form: clusters without a live deployment of this build,
	// and the settings it starts from.
	Clusters []string
	Form     store.Deployment
	Routable bool
}

func (c *Console) build(w http.ResponseWriter, r *http.Request, p page) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		c.message(w, http.StatusNotFound, p, "找不到", "地址里的构建号不对。")
		return
	}
	ctx := r.Context()
	data := buildPage{}
	var all []store.Deployment
	err = c.store.View(ctx, func(tx *store.Tx) error {
		var err error
		if data.Build, err = tx.Build(id); err != nil {
			return err
		}
		all, err = tx.DeploymentsOf(id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, p, "找不到", "没有这个构建。")
		return
	}
	if err != nil {
		c.fail(w, p, err)
		return
	}
	st, err := c.readReleases(ctx)
	if err != nil {
		c.fail(w, p, err)
		return
	}
	b := data.Build
	data.Channels = st.channels(b.ID)
	for k, v := range b.Labels {
		data.Labels = append(data.Labels, label{k, v})
	}
	slices.SortFunc(data.Labels, func(x, y label) int { return cmp.Compare(x.Key, y.Key) })

	// Live counts of the Fleets and their matches, one read of each cluster.
	fleets := map[string][]cluster.ManagedFleet{}
	rooms := map[string][]cluster.Room{}
	for _, d := range all {
		if _, read := fleets[d.Cluster]; read || d.State == store.StateDeleted {
			continue
		}
		site, ok := c.siteNamed(d.Cluster)
		if !ok {
			continue
		}
		list, err := site.Cluster.ManagedFleets(ctx)
		if err == nil {
			rooms[d.Cluster], err = site.Cluster.Rooms(ctx)
		}
		if err != nil {
			data.Errs = append(data.Errs, d.Cluster+": "+err.Error())
		}
		fleets[d.Cluster] = list
	}
	deployed := map[string]bool{}
	for _, d := range all {
		row := deploymentRow{Deployment: d}
		if d.State == store.StateDeleted {
			data.History = append(data.History, row)
			continue
		}
		deployed[d.Cluster] = true
		for _, f := range fleets[d.Cluster] {
			if f.Name == d.Fleet && f.Deployment == d.ID {
				row.Live = &f
			}
		}
		for _, room := range rooms[d.Cluster] {
			if room.Fleet == d.Fleet {
				st.tally(d.Cluster, room, &row.Playing, &row.Free)
			}
		}
		if d.State == store.StateDraining {
			row.ReclaimAt = d.Since.Add(c.releases.Retention())
		}
		data.Deployments = append(data.Deployments, row)
	}
	for _, s := range c.sites.All() {
		if !deployed[s.Name] {
			data.Clusters = append(data.Clusters, s.Name)
		}
	}
	if len(data.Clusters) > 0 {
		data.Form, err = c.releases.DefaultsFor(ctx, b.Game, data.Clusters[0])
		if err != nil {
			data.Errs = append(data.Errs, err.Error())
			data.Form = release.Defaults
		}
	}
	data.Routable = len(data.Deployments) > 0
	data.Finding = buildFinding(data)

	p.Title = fmt.Sprintf("%s #%d", b.Game, b.ID)
	p.Nav = "builds"
	p.Refresh = len(data.Deployments) > 0
	p.Data = data
	c.render(w, http.StatusOK, "build", p)
}

func buildFinding(data buildPage) finding {
	has := func(ch string) bool { return slices.Contains(data.Channels, ch) }
	switch {
	case has(release.Stable):
		return finding{"ok", "正式通道在用这个构建：新对局都进它，只有灰度中的测试账号除外。"}
	case has(release.Canary):
		return finding{"warn", "灰度通道在用这个构建：只有测试账号的新对局进它。"}
	}
	for _, d := range data.Deployments {
		if d.State == store.StateDraining {
			return finding{"warn", "没有路由再用它，正在排空：打完现有的对局后自动回收。"}
		}
	}
	if len(data.Deployments) > 0 {
		return finding{"warn", "已经部署，但还没有路由指向它，所以不会有对局进来。"}
	}
	return finding{"plain", "还没有部署。部署后才能设为灰度或正式。"}
}

// Deploy, delete and routes. Each action redirects back to the page it came
// from, or explains why it did not happen.

func (c *Console) deploy(w http.ResponseWriter, r *http.Request, p page) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c.act(w, r, p, fmt.Sprintf("/builds/%d", id), func(ctx context.Context, f formValues) error {
		_, err := c.releases.Deploy(ctx, release.DeployRequest{
			BuildID: id, Cluster: f.text("cluster"), Buffer: f.number("buffer"), MaxReplicas: f.number("max"), Rooms: f.number("rooms"),
			Resources: store.Resources{
				CPURequest: f.number("cpuRequest"), CPULimit: f.number("cpuLimit"),
				MemoryRequest: f.number("memoryRequest"), MemoryLimit: f.number("memoryLimit"),
			},
		}, p.User)
		return err
	})
}

func (c *Console) undeploy(w http.ResponseWriter, r *http.Request, p page) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c.act(w, r, p, "/builds", func(ctx context.Context, _ formValues) error {
		return c.releases.Undeploy(ctx, id, p.User)
	})
}

func (c *Console) setScale(w http.ResponseWriter, r *http.Request, p page) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c.act(w, r, p, "/builds", func(ctx context.Context, f formValues) error {
		return c.releases.SetScale(ctx, id, f.number("rooms"), f.number("buffer"), f.number("max"), p.User)
	})
}

func (c *Console) routeAction(w http.ResponseWriter, r *http.Request, p page) {
	c.act(w, r, p, "/routes", func(ctx context.Context, f formValues) error {
		game, channel, compat := f.text("game"), f.text("channel"), f.text("compat")
		force := f.text("force") == "1"
		switch r.PathValue("action") {
		case "set":
			build, _ := strconv.ParseInt(f.text("build"), 10, 64)
			return c.releases.SetRoute(ctx, channel, build, p.User, force)
		case "promote":
			return c.releases.Promote(ctx, game, compat, p.User, force)
		case "rollback":
			return c.releases.Rollback(ctx, game, channel, compat, p.User)
		case "delete":
			return c.releases.DeleteRoute(ctx, game, channel, compat, p.User)
		}
		return &release.Error{Kind: release.ErrNotFound, Text: "没有这个操作。"}
	})
}

// --- Routes

type routeCell struct {
	store.Route
	Build    store.Build
	Previous *store.Build
	Regions  []string
}

type routeRow struct {
	Game, Compat   string
	Stable, Canary *routeCell
}

type routesPage struct {
	Rows    []routeRow
	Finding finding
}

func (c *Console) routes(w http.ResponseWriter, r *http.Request, p page) {
	ctx := r.Context()
	st, err := c.readReleases(ctx)
	if err != nil {
		c.fail(w, p, err)
		return
	}
	views, err := c.releases.Routes(ctx, "")
	if err != nil {
		c.fail(w, p, err)
		return
	}
	var data routesPage
	for _, route := range st.routes {
		cell := &routeCell{Route: route, Build: st.builds[route.BuildID]}
		if prev, ok := st.builds[route.PreviousBuildID]; ok {
			cell.Previous = &prev
		}
		for _, v := range views {
			if v.Game == route.Game && v.Channel == route.Channel && v.Compat == route.Compat {
				cell.Regions = v.Regions
			}
		}
		i := slices.IndexFunc(data.Rows, func(row routeRow) bool { return row.Game == route.Game && row.Compat == route.Compat })
		if i < 0 {
			data.Rows = append(data.Rows, routeRow{Game: route.Game, Compat: route.Compat})
			i = len(data.Rows) - 1
		}
		if route.Channel == release.Stable {
			data.Rows[i].Stable = cell
		} else {
			data.Rows[i].Canary = cell
		}
	}
	data.Finding = routesFinding(data.Rows)
	p.Title = "路由"
	p.Nav = "routes"
	p.Data = data
	c.render(w, http.StatusOK, "routes", p)
}

func routesFinding(rows []routeRow) finding {
	if len(rows) == 0 {
		return finding{"bad", "还没有路由，玩家开不了局。到构建页把一个已部署的构建设为正式。"}
	}
	for _, row := range rows {
		if row.Stable == nil {
			return finding{"warn", fmt.Sprintf("联机版本 %s 只有灰度、没有正式路由：普通玩家用这个版本的客户端开不了局。", row.Compat)}
		}
	}
	for _, row := range rows {
		if row.Canary != nil && row.Canary.BuildID != row.Stable.BuildID {
			which := ""
			if len(rows) > 1 {
				which = "（联机版本 " + row.Compat + "）"
			}
			return finding{"warn", fmt.Sprintf("正在灰度%s：测试账号进 #%d，其他玩家进 #%d。", which, row.Canary.BuildID, row.Stable.BuildID)}
		}
	}
	if len(rows) > 1 {
		return finding{"ok", fmt.Sprintf("没有进行中的灰度。%d 个联机版本在服务，各自的客户端进各自的构建。", len(rows))}
	}
	return finding{"ok", "没有进行中的灰度，所有玩家用同一个构建。"}
}

// --- Settings

type keysPage struct {
	Now    time.Time
	Keys   []store.Key
	Scopes []string
	// PullKeys names the cluster each cluster's own pull key belongs to.
	PullKeys map[string]string
	// NewName and NewKey show a key that was just made, once.
	NewName, NewKey string
	Err             string
}

func (c *Console) keys(w http.ResponseWriter, r *http.Request, p page) {
	c.renderKeys(w, r, p, http.StatusOK, keysPage{})
}

func (c *Console) renderKeys(w http.ResponseWriter, r *http.Request, p page, status int, data keysPage) {
	keys, err := c.apiKeys.List(r.Context())
	if err != nil {
		c.fail(w, p, err)
		return
	}
	data.Now = c.now()
	data.Keys = keys
	data.Scopes = auth.Scopes
	data.PullKeys = map[string]string{}
	var names []string
	for _, s := range c.sites.All() {
		names = append(names, s.Name)
	}
	for _, k := range keys {
		if cluster, ok := clusters.PullKeyCluster(k.Name, names); ok {
			data.PullKeys[k.Name] = cluster
		}
	}
	p.Title = "API key"
	p.Nav = "settings"
	p.Data = data
	c.render(w, status, "keys", p)
}

func (c *Console) createKey(w http.ResponseWriter, r *http.Request, p page) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	var expires time.Time
	switch days := r.PostForm.Get("days"); days {
	case "", "0":
	case "1", "7", "30":
		n, _ := strconv.Atoi(days)
		// Days of 24 hours: AddDate counts in the local time zone, where a
		// day around a clock change is 23 or 25 hours.
		expires = c.now().Add(time.Duration(n) * 24 * time.Hour)
	default:
		c.renderKeys(w, r, p, http.StatusBadRequest, keysPage{Err: "有效期只能选 1、7、30 天或不过期"})
		return
	}
	value, err := c.apiKeys.Create(r.Context(), name, r.PostForm["scope"], expires, p.User)
	if errors.Is(err, auth.ErrInvalidKey) {
		c.renderKeys(w, r, p, http.StatusBadRequest, keysPage{Err: err.Error()})
		return
	}
	if err != nil {
		c.fail(w, p, err)
		return
	}
	c.log.Info("api key created", "user", p.User, "key", name)
	c.renderKeys(w, r, p, http.StatusOK, keysPage{NewName: name, NewKey: value})
}

func (c *Console) revokeKey(w http.ResponseWriter, r *http.Request, p page) {
	name := r.PathValue("name")
	c.act(w, r, p, "/settings/keys", func(ctx context.Context, _ formValues) error {
		err := c.apiKeys.Revoke(ctx, name, p.User)
		if errors.Is(err, store.ErrNotFound) {
			return &release.Error{Kind: release.ErrNotFound, Text: "没有这个 key，可能已经吊销了。"}
		}
		if err == nil {
			c.log.Info("api key revoked", "user", p.User, "key", name)
		}
		return err
	})
}

type auditPage struct {
	Entries []store.Entry
	// Older is the id to page from, 0 when there is nothing older.
	Older int64
}

const auditPageSize = 100

func (c *Console) audit(w http.ResponseWriter, r *http.Request, p page) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	var data auditPage
	err := c.store.View(r.Context(), func(tx *store.Tx) error {
		var err error
		data.Entries, err = tx.AuditLog(max(before, 0), auditPageSize+1)
		return err
	})
	if err != nil {
		c.fail(w, p, err)
		return
	}
	if len(data.Entries) > auditPageSize {
		data.Entries = data.Entries[:auditPageSize]
		data.Older = data.Entries[auditPageSize-1].ID
	}
	p.Title = "审计日志"
	p.Nav = "settings"
	p.Data = data
	c.render(w, http.StatusOK, "audit", p)
}

// --- Actions

type formValues struct{ r *http.Request }

func (f formValues) text(name string) string { return strings.TrimSpace(f.r.PostForm.Get(name)) }

func (f formValues) number(name string) int {
	n, err := strconv.Atoi(f.text(name))
	if err != nil {
		return -1
	}
	return n
}

// act runs a change from a form. It goes back to the page named by the
// form's "back" field, or fallback, or explains why the change was refused.
func (c *Console) act(w http.ResponseWriter, r *http.Request, p page, fallback string, change func(context.Context, formValues) error) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	back := fallback
	if b := r.PostForm.Get("back"); b != "" {
		back = localPath(b)
	}
	err := change(r.Context(), formValues{r})
	var refusal *release.Error
	var clusterRefusal *clusters.Error
	switch {
	case err == nil:
		http.Redirect(w, r, back, http.StatusSeeOther)
	case errors.As(err, &clusterRefusal):
		c.refused(w, p, http.StatusConflict, clusterRefusal.Text, back)
	case errors.Is(err, release.ErrNotReady) && errors.As(err, &refusal):
		values := url.Values{}
		for k, v := range r.PostForm {
			if k != "force" {
				values[k] = v
			}
		}
		p.Title = "没有改动"
		p.Data = refusalPage{Text: refusal.Text, Back: back, Force: true, Action: r.URL.Path, Values: values}
		c.render(w, http.StatusConflict, "refused", p)
	case errors.As(err, &refusal):
		status := http.StatusConflict
		switch {
		case errors.Is(err, release.ErrInvalid):
			status = http.StatusBadRequest
		case errors.Is(err, release.ErrNotFound):
			status = http.StatusNotFound
		}
		c.refused(w, p, status, refusal.Text, back)
	case errors.Is(err, cluster.ErrUnavailable):
		c.refused(w, p, http.StatusBadGateway, "读写集群失败："+err.Error(), back)
	default:
		c.fail(w, p, err)
	}
}

type refusalPage struct {
	Text, Back string
	// Force, when set, offers to do it anyway: the same form again, with
	// force=1, to Action.
	Force  bool
	Action string
	Values url.Values
}

func (c *Console) refused(w http.ResponseWriter, p page, status int, text, back string) {
	p.Title = "没有改动"
	p.Data = refusalPage{Text: text, Back: back}
	c.render(w, status, "refused", p)
}

func (c *Console) fail(w http.ResponseWriter, p page, err error) {
	c.log.Error("console", "error", err)
	c.message(w, http.StatusInternalServerError, p, "出错了", err.Error())
}
