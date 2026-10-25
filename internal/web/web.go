// Package web serves the console: server-rendered pages that show what runs
// in each cluster, and the forms that release builds and manage API keys.
// Every page except sign-in needs a session. Changes go through
// release.Service and auth.Keys, which record them in the audit log.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
	"github.com/xuhuanhello/gamefleet/internal/update"
)

// Site is one cluster.
type Site = sites.Site

// Console serves the console pages.
type Console struct {
	host     string
	sites    *sites.Directory
	clusters Clusters
	auth     *auth.Auth
	apiKeys  Keys
	store    *store.Store
	releases Releases
	log      *slog.Logger
	updates  Updates
	now      func() time.Time
	pages    map[string]*template.Template
}

// Options is everything the console uses.
type Options struct {
	// Host is GameFleet's public host name.
	Host     string
	Sites    *sites.Directory
	Clusters Clusters
	Auth     *auth.Auth
	Keys     Keys
	Store    *store.Store
	Releases Releases
	Updates  Updates
	Log      *slog.Logger
}

// SessionCookie holds the session token.
const SessionCookie = "__Host-gamefleet"

var (
	//go:embed templates
	templates embed.FS
	//go:embed console.css
	stylesheet []byte
	//go:embed console.js
	script []byte

	// objectName matches Kubernetes object names, which every name in a console URL is.
	objectName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	logLines   = []int{100, 500, 2000, 5000}
)

// New parses the page templates.
func New(o Options) (*Console, error) {
	if o.Updates == nil {
		// A build from source with no updater: nothing to check or apply.
		o.Updates = update.New(update.Options{Current: "dev"})
	}
	c := &Console{host: o.Host, sites: o.Sites, clusters: o.Clusters, auth: o.Auth, apiKeys: o.Keys, store: o.Store, releases: o.Releases, log: o.Log, updates: o.Updates,
		now: time.Now, pages: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"age":          c.age,
		"breakable":    breakable,
		"releaseNotes": releaseNotes,
		"shown":        shown,
		"newest":       newest,
		"tag":          imageTag,
		"join":         strings.Join,
		"state":        stateClass,
		"stateLabel":   stateLabel,
		"cpu":          formatCPU,
		"memory":       formatMemory,
		"initial":      func(s string) string { return strings.ToUpper(s[:min(len(s), 1)]) },
		"short":        shortDigest,
		"revision":     func(b store.Build) string { return shortRevision(b.Labels["org.opencontainers.image.revision"]) },
		"deployState":  deployState,
		"channel":      channelLabel,
		"action":       actionLabel,
		"scope":        scopeLabel,
		"has":          func(list []string, s string) bool { return slices.Contains(list, s) },
		"until":        c.until,
	}
	for _, name := range []string{"login", "message", "refused", "overview", "cluster", "node", "servers", "server", "log", "events",
		"builds", "build", "routes", "keys", "audit", "join", "runtime", "nodejob", "addnode", "update"} {
		t, err := template.New(name).Funcs(funcs).ParseFS(templates,
			"templates/layout.html", "templates/parts.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		c.pages[name] = t
	}
	return c, nil
}

// Handler routes the console pages.
func (c *Console) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /console.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(stylesheet)
	})
	mux.HandleFunc("GET /console.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(script)
	})
	mux.HandleFunc("GET /login", c.loginForm)
	mux.HandleFunc("POST /login", c.login)
	mux.HandleFunc("POST /logout", c.logout)
	mux.Handle("GET /{$}", c.signedIn(c.overview))
	mux.Handle("GET /servers", c.signedIn(c.servers))
	mux.Handle("GET /clusters/{cluster}", c.signedIn(c.cluster))
	mux.Handle("GET /clusters/{cluster}/events", c.signedIn(c.events))
	mux.Handle("GET /clusters/{cluster}/nodes/{node}", c.signedIn(c.node))
	mux.Handle("GET /clusters/{cluster}/servers/{server}", c.signedIn(c.server))
	mux.Handle("GET /clusters/{cluster}/pods/{namespace}/{pod}/log", c.signedIn(c.podLog))
	mux.Handle("GET /builds", c.signedIn(c.builds))
	mux.Handle("GET /builds/{id}", c.signedIn(c.build))
	mux.Handle("POST /builds/{id}/deploy", c.signedIn(c.deploy))
	mux.Handle("POST /deployments/{id}/delete", c.signedIn(c.undeploy))
	mux.Handle("POST /deployments/{id}/buffer", c.signedIn(c.setBuffer))
	mux.Handle("GET /routes", c.signedIn(c.routes))
	mux.Handle("POST /routes/{action}", c.signedIn(c.routeAction))
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
	})
	mux.Handle("GET /settings/keys", c.signedIn(c.keys))
	mux.Handle("POST /settings/keys", c.signedIn(c.createKey))
	mux.Handle("POST /settings/keys/{name}/revoke", c.signedIn(c.revokeKey))
	mux.Handle("GET /settings/update", c.signedIn(c.updatePage))
	mux.Handle("POST /settings/update/{action}", c.signedIn(c.updateAction))
	mux.Handle("GET /settings/audit", c.signedIn(c.audit))
	mux.Handle("GET /settings/runtime", c.signedIn(c.runtime))
	mux.Handle("POST /settings/runtime", c.signedIn(c.setVar))
	mux.Handle("POST /settings/runtime/delete", c.signedIn(c.deleteVar))
	mux.Handle("GET /clusters/new", c.signedIn(c.newCluster))
	mux.Handle("POST /clusters/new", c.signedIn(c.startJoin))
	mux.Handle("POST /clusters/new/{id}/retry", c.signedIn(c.retryJoin))
	mux.Handle("GET /clusters/{cluster}/nodes/new", c.signedIn(c.nodeJob))
	mux.Handle("POST /clusters/{cluster}/nodes/new", c.signedIn(c.startNodeJoin))
	mux.Handle("POST /clusters/{cluster}/nodes/new/{id}/retry", c.signedIn(c.retryNode))
	mux.Handle("POST /clusters/{cluster}/nodes/{node}/retire", c.signedIn(c.retireNode))
	mux.Handle("POST /clusters/{cluster}/nodes/{node}/keep", c.signedIn(c.keepNode))
	mux.Handle("POST /clusters/{cluster}/nodes/{node}/name", c.signedIn(c.renameNode))
	return secure(http.NewCrossOriginProtection().Handler(mux))
}

// secure adds the headers every console response carries.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; "+
			"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// page is what every template gets.
type page struct {
	Title    string
	User     string
	Clusters []string
	// Nav names the highlighted menu entry: overview, servers, builds,
	// routes, settings, newcluster, cluster:NAME or node:NAME.
	Nav string
	// Cluster is the cluster a page is in, whose machines the menu lists.
	Cluster  string
	Machines []cluster.Node
	Query    string
	// Refresh makes console.js reload the page content every ten seconds,
	// or every RefreshSeconds when that is set.
	Refresh        bool
	RefreshSeconds int
	// Version is the GameFleet version running; Update is a newer release
	// when there is one, which the menu offers.
	Version string
	Update  *update.Release
	Data    any
}

type pageHandler func(w http.ResponseWriter, r *http.Request, p page)

// signedIn sends visitors without a session to the sign-in page.
func (c *Console) signedIn(h pageHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(SessionCookie); err == nil {
			if user, ok := c.auth.User(cookie.Value); ok {
				if r.Method == http.MethodGet && !quietRefresh(r) && r.URL.Path != "/settings/update" {
					c.updates.CheckSoon(pageCheckAge)
				}
				p := page{User: user, Version: c.updates.Current(), Update: c.updates.Available()}
				for _, s := range c.sites.All() {
					p.Clusters = append(p.Clusters, s.Name)
				}
				h(w, r, p)
				return
			}
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	})
}

func (c *Console) render(w http.ResponseWriter, status int, name string, p page) {
	var buf bytes.Buffer
	if err := c.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		c.log.Error("render console page", "page", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (c *Console) message(w http.ResponseWriter, status int, p page, title, text string) {
	p.Title = title
	p.Data = text
	c.render(w, status, "message", p)
}

func (c *Console) siteNamed(name string) (Site, bool) { return c.sites.Get(name) }

// within makes the menu list the machines of cluster s. Pages that read the
// nodes anyway pass them; with nil, within reads them.
func (c *Console) within(ctx context.Context, p *page, s Site, nodes []cluster.Node) {
	if nodes == nil {
		nodes, _ = s.Cluster.Nodes(ctx)
	}
	p.Cluster, p.Machines = s.Name, nodes
}

// titles maps node names to what the console calls those machines.
func titles(nodes []cluster.Node) map[string]string {
	m := map[string]string{}
	for _, n := range nodes {
		m[n.Name] = n.Title()
	}
	return m
}

// size is the whole machine. Usage counts K3s and the system too, so it is
// measured against this rather than against what pods may request.
func size(n cluster.Node) cluster.Resources {
	if n.Capacity.MilliCPU == 0 || n.Capacity.Memory == 0 {
		return n.Allocatable
	}
	return n.Capacity
}

// isServer reports whether a pod is a game server; the other pods are the
// system components K3s and Agones run.
func isServer(pod cluster.Pod, namespace string) bool {
	return pod.Owner == "GameServer" && pod.Namespace == namespace
}

// components lists the system components among pods, the ones in trouble
// first. Finished jobs are left out.
func (c *Console) components(pods []cluster.Pod, namespace string, usage cluster.Usage) []podRow {
	var rows []podRow
	for _, pod := range pods {
		if isServer(pod, namespace) || pod.Status == "Succeeded" {
			continue
		}
		row := podRow{Pod: pod, CPU: "–", Memory: "–", Component: pod.Name,
			Healthy: pod.Status == "Running" && pod.Ready == len(pod.Containers)}
		// Controllers append a hash or two to the names of the pods they make.
		for range map[string]int{"ReplicaSet": 2, "DaemonSet": 1, "Job": 1, "StatefulSet": 1}[pod.Owner] {
			if i := strings.LastIndex(row.Component, "-"); i > 0 {
				row.Component = row.Component[:i]
			}
		}
		if u, ok := usage.Pods[pod.Namespace+"/"+pod.Name]; ok {
			row.CPU, row.Memory = formatCPU(u.MilliCPU), formatMemory(u.Memory)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Healthy != rows[j].Healthy {
			return !rows[i].Healthy
		}
		return rows[i].Component < rows[j].Component
	})
	return rows
}

// componentStartup is how long a system component may take to get ready
// before the console calls it stuck.
const componentStartup = 2 * time.Minute

// stuck names the system components that have not been ready for a while.
func (c *Console) stuck(rows []podRow) []string {
	var names []string
	for _, row := range rows {
		if !row.Healthy && c.now().Sub(row.Created) > componentStartup && !slices.Contains(names, row.Component) {
			names = append(names, row.Component)
		}
	}
	return names
}

// site finds the cluster named in the URL and checks the other names in it.
func (c *Console) site(w http.ResponseWriter, r *http.Request, p page, names ...string) (Site, bool) {
	if s, ok := c.sites.Get(r.PathValue("cluster")); ok {
		p.Nav = "cluster:" + s.Name
	}
	for _, n := range names {
		if !objectName.MatchString(r.PathValue(n)) {
			c.message(w, http.StatusNotFound, p, "找不到", "地址里的名字不对。")
			return Site{}, false
		}
	}
	if s, ok := c.sites.Get(r.PathValue("cluster")); ok {
		return s, true
	}
	c.message(w, http.StatusNotFound, p, "找不到", "没有这个集群。")
	return Site{}, false
}

// Sign-in

type loginPage struct {
	Next   string
	Name   string
	Failed bool
}

func (c *Console) loginForm(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.URL.Query().Get("next"))
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		if _, ok := c.auth.User(cookie.Value); ok {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
	}
	c.render(w, http.StatusOK, "login", page{Title: "登录", Data: loginPage{Next: next}})
}

func (c *Console) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := r.PostForm.Get("name")
	next := localPath(r.PostForm.Get("next"))
	token, err := c.auth.Login(name, r.PostForm.Get("password"), strings.TrimSpace(r.PostForm.Get("code")))
	if len(name) > 64 {
		name = name[:64]
	}
	if err != nil {
		c.log.Warn("console sign-in failed", "user", name, "remote", remoteAddr(r))
		c.render(w, http.StatusUnauthorized, "login", page{Title: "登录", Data: loginPage{Next: next, Name: name, Failed: true}})
		return
	}
	c.log.Info("console sign-in", "user", name, "remote", remoteAddr(r))
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", MaxAge: int(auth.SessionTTL.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (c *Console) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		c.auth.Logout(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// localPath keeps a redirect target on this site.
func localPath(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// remoteAddr is the client address. Caddy, the only way in, puts it first in X-Forwarded-For.
func remoteAddr(r *http.Request) string {
	if forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); forwarded != "" {
		return strings.TrimSpace(forwarded)
	}
	return r.RemoteAddr
}

// Overview

type clusterSummary struct {
	Name, Region      string
	Err               string
	Finding           finding
	Nodes, NodesReady int
	CPU, Memory       gauge
	// Matches running, and how many more could start right away.
	Ready, Allocated int
	Warnings         int
	Matches          []matchRow
	Fleets           []fleetRow
}

// finding is the one sentence the overview says about a cluster. Level is
// ok, warn or bad.
type finding struct {
	Level, Text string
}

const overviewMatches = 8

func (c *Console) overview(w http.ResponseWriter, r *http.Request, p page) {
	st, err := c.readReleases(r.Context())
	if err != nil {
		c.fail(w, p, err)
		return
	}
	var summaries []clusterSummary
	for _, s := range c.sites.All() {
		summaries = append(summaries, c.summarize(r.Context(), s, st))
	}
	p.Title = "总览"
	p.Nav = "overview"
	p.Refresh = true
	p.Data = summaries
	c.render(w, http.StatusOK, "overview", p)
}

func (c *Console) summarize(ctx context.Context, s Site, st releaseState) clusterSummary {
	sum := clusterSummary{Name: s.Name, Region: s.Region}
	nodes, err := s.Cluster.Nodes(ctx)
	if err != nil {
		sum.Err = err.Error()
		return sum
	}
	fleets, err := s.Cluster.Fleets(ctx)
	if err != nil {
		sum.Err = err.Error()
		return sum
	}
	rooms, err := s.Cluster.Rooms(ctx)
	if err != nil {
		sum.Err = err.Error()
		return sum
	}
	usage, usageErr := s.Cluster.Usage(ctx)
	// Events and pods are extra: without them the overview just counts no
	// warnings and no stuck components.
	events, _ := s.Cluster.Events(ctx, "", "")
	pods, _ := s.Cluster.Pods(ctx, "")
	broken := c.stuck(c.components(pods, s.Cluster.Namespace(), cluster.Usage{}))

	var used, total cluster.Resources
	var down []string
	for _, n := range nodes {
		sum.Nodes++
		if n.Ready {
			sum.NodesReady++
		} else {
			down = append(down, n.Name)
		}
		total = total.Add(size(n))
		used = used.Add(usage.Nodes[n.Name])
	}
	sum.CPU = cpuGauge(used.MilliCPU, total.MilliCPU, usageErr == nil)
	sum.Memory = memoryGauge(used.Memory, total.Memory, usageErr == nil)
	for _, room := range rooms {
		st.tally(s.Name, room, &sum.Allocated, &sum.Ready)
		sum.Matches = append(sum.Matches, matchRows(s.Name, room, titles(nodes))...)
	}
	sort.SliceStable(sum.Matches, func(i, j int) bool { return sum.Matches[i].Allocated.After(sum.Matches[j].Allocated) })
	sum.Matches = sum.Matches[:min(len(sum.Matches), overviewMatches)]
	hourAgo := c.now().Add(-time.Hour)
	for _, e := range events {
		if e.Type == "Warning" && e.Last.After(hourAgo) {
			sum.Warnings++
		}
	}
	sum.Fleets = st.fleetRows(s.Name, fleets, rooms)

	switch {
	case len(down) > 0:
		sum.Finding = finding{"bad", fmt.Sprintf("%d 台机器失联：%s。它上面的对局可能已经中断。", len(down), strings.Join(down, "、"))}
	case len(broken) > 0:
		sum.Finding = finding{"bad", fmt.Sprintf("系统组件没有就绪：%s。集群可能开不了局，到集群页看系统组件。", strings.Join(broken, "、"))}
	case sum.Ready == 0:
		sum.Finding = finding{"bad", "战斗服都满了，新的对局会分配失败。"}
	case sum.Warnings > 0:
		sum.Finding = finding{"warn", fmt.Sprintf("能开局，但最近一小时有 %d 条警告。", sum.Warnings)}
	default:
		sum.Finding = finding{"ok", fmt.Sprintf("一切正常，现在还能立刻开 %d 局。", sum.Ready)}
	}
	return sum
}

// Cluster

// gaugePair is what a node uses of a resource and what its pods requested.
type gaugePair struct {
	Used, Requested gauge
}

type nodeRow struct {
	cluster.Node
	CPU, Memory      gaugePair
	Allocated, Ready int
}

type clusterPage struct {
	Name, Region string
	Kubelet      string
	Errs         []string
	Nodes        []nodeRow
	Components   componentTable
	Stuck        []string // components that have not been ready for a while
	Fleets       []fleetRow
	Warnings     []cluster.Event
}

func (c *Console) cluster(w http.ResponseWriter, r *http.Request, p page) {
	s, ok := c.site(w, r, p)
	if !ok {
		return
	}
	ctx := r.Context()
	data := clusterPage{Name: s.Name, Region: s.Region}
	fail := func(err error) {
		if err != nil {
			data.Errs = append(data.Errs, err.Error())
		}
	}
	nodes, err := s.Cluster.Nodes(ctx)
	fail(err)
	pods, err := s.Cluster.Pods(ctx, "")
	fail(err)
	rooms, err := s.Cluster.Rooms(ctx)
	fail(err)
	fleets, err := s.Cluster.Fleets(ctx)
	fail(err)
	st, err := c.readReleases(ctx)
	fail(err)
	data.Fleets = st.fleetRows(s.Name, fleets, rooms)
	events, err := s.Cluster.Events(ctx, "", "")
	fail(err)
	usage, usageErr := s.Cluster.Usage(ctx)

	for _, n := range nodes {
		row := nodeRow{Node: n}
		var requests cluster.Resources
		for _, pod := range pods {
			if pod.Node == n.Name && pod.Status != "Succeeded" && pod.Status != "Failed" {
				requests = requests.Add(pod.Requests)
			}
		}
		for _, room := range rooms {
			if room.Node == n.Name {
				st.tally(s.Name, room, &row.Allocated, &row.Ready)
			}
		}
		used, measured := usage.Nodes[n.Name]
		row.CPU = gaugePair{
			Used:      cpuGauge(used.MilliCPU, size(n).MilliCPU, usageErr == nil && measured),
			Requested: cpuGauge(requests.MilliCPU, n.Allocatable.MilliCPU, pods != nil),
		}
		row.Memory = gaugePair{
			Used:      memoryGauge(used.Memory, size(n).Memory, usageErr == nil && measured),
			Requested: memoryGauge(requests.Memory, n.Allocatable.Memory, pods != nil),
		}
		data.Nodes = append(data.Nodes, row)
	}
	data.Components = componentTable{Cluster: s.Name, Rows: c.components(pods, s.Cluster.Namespace(), usage), ByMachine: true,
		Machines: titles(nodes)}
	data.Stuck = c.stuck(data.Components.Rows)
	for _, e := range events {
		if e.Type == "Warning" && len(data.Warnings) < 10 {
			data.Warnings = append(data.Warnings, e)
		}
	}
	if len(nodes) > 0 {
		data.Kubelet = nodes[0].Kubelet
	}
	c.within(ctx, &p, s, nodes)
	p.Refresh = true
	p.Title = s.Name
	p.Nav = "cluster:" + s.Name
	p.Data = data
	c.render(w, http.StatusOK, "cluster", p)
}

// Node

// podRow is a pod on a machine: a game server, or a system component.
type podRow struct {
	cluster.Pod
	CPU, Memory string
	Room        *cluster.Room
	// Component is a system pod's name without the hashes its controller
	// added, and Healthy whether all its containers are ready.
	Component string
	Healthy   bool
}

type nodePage struct {
	Cluster          string
	Node             cluster.Node
	ControlPlane     bool
	Errs             []string
	CPU, Memory      gauge
	Requests         cluster.Resources
	Allocated, Ready int
	Servers          []podRow
	Components       componentTable
	Events           []cluster.Event
}

// componentTable is the system components of a cluster, or of one machine.
type componentTable struct {
	Cluster   string
	Rows      []podRow
	ByMachine bool              // show which machine each runs on
	Machines  map[string]string // what the console calls each machine
}

func (c *Console) node(w http.ResponseWriter, r *http.Request, p page) {
	s, ok := c.site(w, r, p, "node")
	if !ok {
		return
	}
	ctx := r.Context()
	name := r.PathValue("node")
	nodes, err := s.Cluster.Nodes(ctx)
	if err != nil {
		c.message(w, http.StatusBadGateway, p, "读不到集群", err.Error())
		return
	}
	i := slices.IndexFunc(nodes, func(n cluster.Node) bool { return n.Name == name })
	if i < 0 {
		c.message(w, http.StatusNotFound, p, "找不到", "集群里没有这个节点。")
		return
	}
	data := nodePage{Cluster: s.Name, Node: nodes[i],
		ControlPlane: slices.Contains(nodes[i].Roles, "control-plane") || slices.Contains(nodes[i].Roles, "master")}
	fail := func(err error) {
		if err != nil {
			data.Errs = append(data.Errs, err.Error())
		}
	}
	pods, err := s.Cluster.Pods(ctx, name)
	fail(err)
	rooms, err := s.Cluster.Rooms(ctx)
	fail(err)
	st, err := c.readReleases(ctx)
	fail(err)
	data.Events, err = s.Cluster.Events(ctx, "", name)
	fail(err)
	usage, usageErr := s.Cluster.Usage(ctx)

	used, measured := usage.Nodes[name]
	data.CPU = cpuGauge(used.MilliCPU, size(data.Node).MilliCPU, usageErr == nil && measured)
	data.Memory = memoryGauge(used.Memory, size(data.Node).Memory, usageErr == nil && measured)
	data.Components = componentTable{Cluster: s.Name, Rows: c.components(pods, s.Cluster.Namespace(), usage)}
	for _, pod := range pods {
		if pod.Status != "Succeeded" && pod.Status != "Failed" {
			data.Requests = data.Requests.Add(pod.Requests)
		}
		if !isServer(pod, s.Cluster.Namespace()) {
			continue
		}
		row := podRow{Pod: pod, CPU: "–", Memory: "–"}
		if u, ok := usage.Pods[pod.Namespace+"/"+pod.Name]; ok {
			row.CPU, row.Memory = formatCPU(u.MilliCPU), formatMemory(u.Memory)
		}
		for _, room := range rooms {
			if room.ID == pod.Name {
				row.Room = &room
				st.tally(s.Name, room, &data.Allocated, &data.Ready)
			}
		}
		data.Servers = append(data.Servers, row)
	}
	c.within(ctx, &p, s, nodes)
	p.Title = name
	p.Refresh = true
	p.Nav = "node:" + name
	p.Data = data
	c.render(w, http.StatusOK, "node", p)
}

// Game servers

type serverRow struct {
	Cluster string
	cluster.Room
	Machine string // what the console calls the machine it runs on
	Found   string // the match a search found on it
}

// matchRow is one running match, for lists of matches.
type matchRow struct {
	Cluster, Server, Node, Machine string
	cluster.Match
	Players int
}

func matchRows(clusterName string, room cluster.Room, machines map[string]string) []matchRow {
	rows := make([]matchRow, 0, len(room.Matches))
	for _, m := range room.Matches {
		rows = append(rows, matchRow{Cluster: clusterName, Server: room.ID, Node: room.Node, Machine: machines[room.Node],
			Match: m, Players: len(players(m.Players))})
	}
	return rows
}

// tally adds a game server to the counts the console shows: the matches it
// runs, and how many more it could start right away. Only a build that routes
// send matches to gets new ones, and not on a machine being retired.
func (st releaseState) tally(clusterName string, room cluster.Room, playing, free *int) {
	if room.State != "Allocated" && room.State != "Ready" {
		return
	}
	*playing += len(room.Matches)
	if d, ok := st.fleetOf(clusterName, room.Fleet); ok && d.State == store.StateServing && !room.Retiring {
		*free += room.Free()
	}
}

// shown is the state a game server is shown in: one that runs no match is
// idle, whatever Agones calls it.
func shown(room cluster.Room) string {
	if room.State == "Allocated" && len(room.Matches) == 0 {
		return "Ready"
	}
	return room.State
}

// newest is when the game server's latest match started, or zero.
func newest(room cluster.Room) time.Time {
	var at time.Time
	for _, m := range room.Matches {
		if m.Allocated.After(at) {
			at = m.Allocated
		}
	}
	return at
}

// filter is one choice in a row of filters on a list page.
type filter struct {
	Label  string
	Count  int
	URL    string
	Active bool
}

type serversPage struct {
	Rows []serverRow
	// Matches running, and how many more could start right away.
	Allocated, Ready int
	States, Clusters []filter
	Errs             []string
}

// Server states the list filters on: in a match, idle, or anything else,
// which is starting up, shutting down or broken.
var serverStates = []struct{ Key, Label string }{{"", "全部"}, {"allocated", "对局中"}, {"ready", "空闲"}, {"other", "启动中或出错"}}

func serverState(room cluster.Room) string {
	switch shown(room) {
	case "Allocated":
		return "allocated"
	case "Ready":
		return "ready"
	}
	return "other"
}

// servers lists the game servers of every cluster, filtered by state and
// cluster. The query keeps servers whose match ID or name contains it, so
// the start of a match ID is enough.
func (c *Console) servers(w http.ResponseWriter, r *http.Request, p page) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	needle := strings.ToLower(query)
	state, only := q.Get("state"), q.Get("cluster")
	link := func(state, cluster string) string {
		v := url.Values{}
		for key, value := range map[string]string{"q": query, "state": state, "cluster": cluster} {
			if value != "" {
				v.Set(key, value)
			}
		}
		if len(v) == 0 {
			return "/servers"
		}
		return "/servers?" + v.Encode()
	}
	var data serversPage
	st, err := c.readReleases(r.Context())
	if err != nil {
		data.Errs = append(data.Errs, err.Error())
	}
	counts := map[string]int{}
	data.Clusters = append(data.Clusters, filter{Label: "全部集群", URL: link(state, ""), Active: only == ""})
	for _, s := range c.sites.All() {
		data.Clusters = append(data.Clusters, filter{Label: s.Name, URL: link(state, s.Name), Active: only == s.Name})
		if only != "" && s.Name != only {
			continue
		}
		rooms, err := s.Cluster.Rooms(r.Context())
		if err != nil {
			data.Errs = append(data.Errs, s.Name+": "+err.Error())
			continue
		}
		nodes, _ := s.Cluster.Nodes(r.Context())
		machines := titles(nodes)
		for _, room := range rooms {
			found := ""
			if needle != "" {
				for _, m := range room.Matches {
					if strings.Contains(strings.ToLower(m.ID), needle) {
						found = m.ID
						break
					}
				}
				if found == "" && !strings.Contains(room.ID, needle) {
					continue
				}
			}
			counts[serverState(room)]++
			counts[""]++
			st.tally(s.Name, room, &data.Allocated, &data.Ready)
			if state == "" || serverState(room) == state {
				data.Rows = append(data.Rows, serverRow{Cluster: s.Name, Room: room, Machine: machines[room.Node], Found: found})
			}
		}
	}
	for _, st := range serverStates {
		data.States = append(data.States, filter{Label: st.Label, Count: counts[st.Key], URL: link(st.Key, only), Active: state == st.Key})
	}
	// Servers running matches first, the busiest first.
	sort.SliceStable(data.Rows, func(i, j int) bool {
		a, b := data.Rows[i], data.Rows[j]
		if len(a.Matches) != len(b.Matches) {
			return len(a.Matches) > len(b.Matches)
		}
		return newest(a.Room).After(newest(b.Room))
	})
	p.Title = "战斗服"
	p.Nav = "servers"
	p.Refresh = true
	p.Query = query
	p.Data = data
	c.render(w, http.StatusOK, "servers", p)
}

type player struct {
	UserID string `json:"userId"`
	Seat   int    `json:"seat"`
}

func players(raw json.RawMessage) []player {
	var list []player
	_ = json.Unmarshal(raw, &list)
	sort.Slice(list, func(i, j int) bool { return list[i].Seat < list[j].Seat })
	return list
}

type serverPage struct {
	Cluster      string
	Room         cluster.Room
	Machine      string // what the console calls the machine
	Matches      []matchView
	Pod          *cluster.Pod
	CPU, Memory  string
	Errs         []string
	Container    string
	Log          string
	LogNamespace string
	Events       []cluster.Event
}

// matchView is a match on its game server's page.
type matchView struct {
	cluster.Match
	Players []player
}

func (c *Console) server(w http.ResponseWriter, r *http.Request, p page) {
	s, ok := c.site(w, r, p, "server")
	if !ok {
		return
	}
	ctx := r.Context()
	room, err := s.Cluster.Get(ctx, r.PathValue("server"))
	if err != nil {
		c.message(w, http.StatusBadGateway, p, "读不到集群", err.Error())
		return
	}
	if room == nil {
		c.message(w, http.StatusNotFound, p, "找不到",
			"没有这个战斗服。对局结束后战斗服会被删除，已结束的对局请到你的游戏后端查结果。")
		return
	}
	ns := s.Cluster.Namespace()
	data := serverPage{Cluster: s.Name, Room: *room, CPU: "–", Memory: "–", LogNamespace: ns}
	for _, m := range room.Matches {
		data.Matches = append(data.Matches, matchView{Match: m, Players: players(m.Players)})
	}
	nodes, _ := s.Cluster.Nodes(ctx)
	data.Machine = titles(nodes)[room.Node]
	fail := func(err error) {
		if err != nil {
			data.Errs = append(data.Errs, err.Error())
		}
	}
	data.Pod, err = s.Cluster.Pod(ctx, ns, room.ID)
	fail(err)
	data.Events, err = s.Cluster.Events(ctx, ns, room.ID)
	fail(err)
	if usage, err := s.Cluster.Usage(ctx); err == nil {
		if u, ok := usage.Pods[ns+"/"+room.ID]; ok {
			data.CPU, data.Memory = formatCPU(u.MilliCPU), formatMemory(u.Memory)
		}
	}
	if data.Pod != nil && len(data.Pod.Containers) > 0 {
		data.Container = data.Pod.Containers[0]
		data.Log, err = s.Cluster.Log(ctx, ns, room.ID, data.Container, 100)
		fail(err)
	}
	p.Title = room.ID
	p.Nav = "servers"
	p.Data = data
	c.render(w, http.StatusOK, "server", p)
}

// Logs

type logPage struct {
	Cluster   string
	Pod       cluster.Pod
	Container string
	Lines     int
	Options   []int
	Log       string
	Err       string
}

func (c *Console) podLog(w http.ResponseWriter, r *http.Request, p page) {
	s, ok := c.site(w, r, p, "namespace", "pod")
	if !ok {
		return
	}
	ctx := r.Context()
	pod, err := s.Cluster.Pod(ctx, r.PathValue("namespace"), r.PathValue("pod"))
	if err != nil {
		c.message(w, http.StatusBadGateway, p, "读不到集群", err.Error())
		return
	}
	if pod == nil || len(pod.Containers) == 0 {
		c.message(w, http.StatusNotFound, p, "找不到", "没有这个 Pod。Pod 删除后，它的日志也一起没了。")
		return
	}
	data := logPage{Cluster: s.Name, Pod: *pod, Container: pod.Containers[0], Lines: 500, Options: logLines}
	if container := r.URL.Query().Get("container"); container != "" {
		if !slices.Contains(pod.Containers, container) {
			c.message(w, http.StatusNotFound, p, "找不到", "这个 Pod 没有这个容器。")
			return
		}
		data.Container = container
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && slices.Contains(logLines, n) {
		data.Lines = n
	}
	data.Log, err = s.Cluster.Log(ctx, pod.Namespace, pod.Name, data.Container, data.Lines)
	if err != nil {
		data.Err = err.Error()
	}
	p.Title = pod.Name + " 日志"
	p.Nav = "servers"
	if !isServer(*pod, s.Cluster.Namespace()) {
		c.within(ctx, &p, s, nil)
		p.Nav = "node:" + pod.Node
	}
	p.Data = data
	c.render(w, http.StatusOK, "log", p)
}

// Events

type eventsPage struct {
	Cluster  string
	Warnings bool
	Events   []cluster.Event
	Err      string
}

func (c *Console) events(w http.ResponseWriter, r *http.Request, p page) {
	s, ok := c.site(w, r, p)
	if !ok {
		return
	}
	data := eventsPage{Cluster: s.Name, Warnings: r.URL.Query().Get("type") == "Warning"}
	events, err := s.Cluster.Events(r.Context(), "", "")
	if err != nil {
		data.Err = err.Error()
	}
	for _, e := range events {
		if !data.Warnings || e.Type == "Warning" {
			data.Events = append(data.Events, e)
		}
	}
	c.within(r.Context(), &p, s, nil)
	p.Title = s.Name + " 事件"
	p.Refresh = true
	p.Nav = "cluster:" + s.Name
	p.Data = data
	c.render(w, http.StatusOK, "events", p)
}
