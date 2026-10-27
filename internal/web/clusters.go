package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/xuhuanhello/gamefleet/internal/clusters"
	"github.com/xuhuanhello/gamefleet/internal/remote"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Clusters is what the console does through clusters.Service.
type Clusters interface {
	StartJoin(ctx context.Context, name, region string, controlPlaneOnly bool, actor string) (clusters.Join, error)
	StartRemoteJoin(ctx context.Context, name, region string, controlPlaneOnly bool, t remote.Target, actor string) (clusters.Join, error)
	RetryJoin(id string) error
	ConfirmJoin(id, fingerprint string) error
	CancelJoin(id string) error
	JoinByID(id string) (clusters.Join, bool)
	StartNodeJoin(ctx context.Context, cluster string, t remote.Target, title, actor string) (clusters.NodeJob, error)
	NodeJobByID(id string) (clusters.NodeJob, bool)
	RetryNode(id string) error
	ConfirmNode(id, fingerprint string) error
	CancelNode(id string) error
	InstallCommand(j clusters.Join) string
	Stored(ctx context.Context, name string) (store.Cluster, bool, error)
	JoinCommand(c store.Cluster) string
	Retire(ctx context.Context, cluster, node, actor string) error
	Keep(ctx context.Context, cluster, node, actor string) error
	Rename(ctx context.Context, cluster, node, title, actor string) error
	SetVar(ctx context.Context, game, key, value, actor string) error
	DeleteVar(ctx context.Context, game, key, actor string) error
	Vars(ctx context.Context) ([]store.Var, error)
}

// --- Joining a new cluster

type joinStep struct {
	Label string
	State string // done, now, failed or later
}

// progress is the bar and the steps of a job.
type progress struct {
	Steps []joinStep
	Done  int     // steps finished
	Value float64 // for the bar: Done and how far the current step has got
	// Detail is what the current step is doing, Count which part of it that
	// is, and Since how long it has been doing it.
	Detail, Count, Since string
}

// at adds where the current step is: part of parts done.
func (p *progress) at(detail string, part, parts int, since string) {
	p.Detail, p.Since = detail, since
	if parts > 0 {
		p.Value += float64(part) / float64(parts)
		p.Count = fmt.Sprintf("%d / %d", min(part+1, parts), parts)
	}
}

func steps(labels []string, current int, finished, failed bool) progress {
	var p progress
	for i, label := range labels {
		state := "later"
		switch {
		case finished || i < current:
			state, p.Done = "done", p.Done+1
		case i == current && failed:
			state = "failed"
		case i == current:
			state = "now"
		}
		p.Steps = append(p.Steps, joinStep{label, state})
	}
	p.Value = float64(p.Done)
	return p
}

type joinPage struct {
	Join      *clusters.Join
	Command   string
	Progress  progress
	Expired   bool
	GameFleet string // the public IP that has to reach TCP 6443
	Form      machineForm
	Err       string
}

// machineForm is what the operator typed, minus any password or key.
type machineForm struct {
	Name, Region, Host, User string
	ControlPlaneOnly         bool
}

var joinStages = []string{clusters.StageWaiting, clusters.StageK3s, clusters.StageAgones, clusters.StageConnect, clusters.StageDone}

func (c *Console) newCluster(w http.ResponseWriter, r *http.Request, p page) {
	data := joinPage{GameFleet: c.publicIP()}
	if id := r.URL.Query().Get("join"); id != "" {
		j, ok := c.clusters.JoinByID(id)
		if !ok {
			c.message(w, http.StatusNotFound, p, "找不到", "没有这次接入。GameFleet 重启后要重新生成加入码。")
			return
		}
		data.Join = &j
		data.Command = c.clusters.InstallCommand(j)
		data.Expired = j.Stage != clusters.StageDone && c.now().After(j.Expires)
		labels := []string{"在新机器上运行命令", "安装 K3s", "安装 Agones", "GameFleet 连接集群", "接入完成"}
		failed, checking := false, false
		if j.Remote != nil {
			labels[0] = "登录机器"
			failed = j.Remote.State == clusters.RemoteFailed
			checking = j.Remote.State == clusters.RemoteChecking
		}
		data.Progress = steps(labels, slices.Index(joinStages, j.Stage), j.Stage == clusters.StageDone, failed)
		// Nothing moves while the operator checks the host key.
		p.Refresh = j.Stage != clusters.StageDone && !data.Expired && !failed && !checking
		p.RefreshSeconds = 3
		if j.Step != "" && (j.Stage == clusters.StageK3s || j.Stage == clusters.StageAgones) {
			since := ""
			if p.Refresh {
				since = c.age(j.StepStarted)
			}
			data.Progress.at(j.Step, j.Part, j.Parts, since)
		}
	}
	p.Title = "接入集群"
	p.Nav = "newcluster"
	p.Data = data
	c.render(w, http.StatusOK, "join", p)
}

// target reads a machine's login from a form. It is used for one job and
// never stored by the console.
func target(r *http.Request) remote.Target {
	return remote.Target{
		Host: strings.TrimSpace(r.PostForm.Get("host")), User: strings.TrimSpace(r.PostForm.Get("user")),
		Password: r.PostForm.Get("password"), Key: r.PostForm.Get("key"), Passphrase: r.PostForm.Get("passphrase"),
	}
}

func (c *Console) startJoin(w http.ResponseWriter, r *http.Request, p page) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name, region := strings.TrimSpace(r.PostForm.Get("name")), strings.TrimSpace(r.PostForm.Get("region"))
	controlPlaneOnly := r.PostForm.Get("controlPlaneOnly") != ""
	var j clusters.Join
	var err error
	if r.PostForm.Get("mode") == "manual" {
		j, err = c.clusters.StartJoin(r.Context(), name, region, controlPlaneOnly, p.User)
	} else {
		j, err = c.clusters.StartRemoteJoin(r.Context(), name, region, controlPlaneOnly, target(r), p.User)
	}
	var refusal *clusters.Error
	if errors.As(err, &refusal) {
		p.Title, p.Nav = "接入集群", "newcluster"
		t := target(r)
		p.Data = joinPage{GameFleet: c.publicIP(), Form: machineForm{name, region, t.Host, t.User, controlPlaneOnly}, Err: refusal.Text}
		c.render(w, http.StatusBadRequest, "join", p)
		return
	}
	if err != nil {
		c.fail(w, p, err)
		return
	}
	c.log.Info("cluster join started", "user", p.User, "cluster", name, "region", region, "ssh", j.Remote != nil)
	http.Redirect(w, r, "/clusters/new?join="+j.ID, http.StatusSeeOther)
}

func (c *Console) retryJoin(w http.ResponseWriter, r *http.Request, p page) {
	id := r.PathValue("id")
	c.act(w, r, p, "/clusters/new?join="+id, func(context.Context, formValues) error { return c.clusters.RetryJoin(id) })
}

func (c *Console) confirmJoin(w http.ResponseWriter, r *http.Request, p page) {
	id := r.PathValue("id")
	c.act(w, r, p, "/clusters/new?join="+id, func(_ context.Context, f formValues) error {
		return c.clusters.ConfirmJoin(id, f.text("fingerprint"))
	})
}

func (c *Console) cancelJoin(w http.ResponseWriter, r *http.Request, p page) {
	id := r.PathValue("id")
	c.act(w, r, p, "/clusters/new?join="+id, func(context.Context, formValues) error { return c.clusters.CancelJoin(id) })
}

// --- New machines

type nodeJobPage struct {
	Cluster  string
	Job      clusters.NodeJob
	Progress progress
}

type addMachinePage struct {
	Cluster     string
	JoinCommand string // how a machine joins by hand; empty when GameFleet lacks it
	Form        machineForm
	Title       string // the name typed for the machine
	Err         string
}

// addMachine is the form that has GameFleet install a machine into a cluster.
func (c *Console) addMachine(w http.ResponseWriter, r *http.Request, p page, s Site, data addMachinePage) {
	data.Cluster = s.Name
	if stored, ok, err := c.clusters.Stored(r.Context(), s.Name); err == nil && ok {
		data.JoinCommand = c.clusters.JoinCommand(stored)
	}
	status := http.StatusOK
	if data.Err != "" {
		status = http.StatusBadRequest
	}
	c.within(r.Context(), &p, s, nil)
	p.Title = "给 " + s.Name + " 加机器"
	p.Nav = "cluster:" + s.Name
	p.Data = data
	c.render(w, status, "addnode", p)
}

func (c *Console) startNodeJoin(w http.ResponseWriter, r *http.Request, p page) {
	s, ok := c.site(w, r, p)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	title := r.PostForm.Get("title")
	job, err := c.clusters.StartNodeJoin(r.Context(), s.Name, target(r), title, p.User)
	var refusal *clusters.Error
	if errors.As(err, &refusal) {
		t := target(r)
		c.addMachine(w, r, p, s, addMachinePage{Form: machineForm{Host: t.Host, User: t.User}, Title: title, Err: refusal.Text})
		return
	}
	if err != nil {
		c.fail(w, p, err)
		return
	}
	c.log.Info("machine join started", "user", p.User, "cluster", s.Name, "host", job.Remote.Host)
	http.Redirect(w, r, "/clusters/"+s.Name+"/nodes/new?job="+job.ID, http.StatusSeeOther)
}

func (c *Console) nodeJob(w http.ResponseWriter, r *http.Request, p page) {
	s, ok := c.site(w, r, p)
	if !ok {
		return
	}
	id := r.URL.Query().Get("job")
	if id == "" {
		c.addMachine(w, r, p, s, addMachinePage{})
		return
	}
	job, ok := c.clusters.NodeJobByID(id)
	if !ok || job.Cluster != s.Name {
		c.message(w, http.StatusNotFound, p, "找不到", "没有这次加入。GameFleet 重启后要重新填一次。")
		return
	}
	current := map[string]int{clusters.NodeAppearing: 2, clusters.NodeDone: 3}[job.Stage]
	if job.Stage == clusters.NodeInstalling || job.Stage == clusters.NodeFailed {
		current = 0
		if job.Remote.State != clusters.RemoteConnecting && len(job.Remote.Log) > 0 {
			current = 1
		}
		if job.Stage == clusters.NodeFailed && job.Remote.State == clusters.RemoteFinished {
			current = 2
		}
	}
	data := nodeJobPage{Cluster: s.Name, Job: job,
		Progress: steps([]string{"登录机器", "安装 K3s", "出现在集群里", "完成"}, current, job.Stage == clusters.NodeDone,
			job.Stage == clusters.NodeFailed)}
	c.within(r.Context(), &p, s, nil)
	p.Title = "加机器 " + job.Remote.Host
	p.Nav = "cluster:" + s.Name
	p.Refresh = job.Stage == clusters.NodeInstalling || job.Stage == clusters.NodeAppearing
	p.RefreshSeconds = 3
	p.Data = data
	c.render(w, http.StatusOK, "nodejob", p)
}

func (c *Console) retryNode(w http.ResponseWriter, r *http.Request, p page) {
	cluster, id := r.PathValue("cluster"), r.PathValue("id")
	c.act(w, r, p, "/clusters/"+cluster+"/nodes/new?job="+id, func(context.Context, formValues) error {
		return c.clusters.RetryNode(id)
	})
}

func (c *Console) confirmNode(w http.ResponseWriter, r *http.Request, p page) {
	cluster, id := r.PathValue("cluster"), r.PathValue("id")
	c.act(w, r, p, "/clusters/"+cluster+"/nodes/new?job="+id, func(_ context.Context, f formValues) error {
		return c.clusters.ConfirmNode(id, f.text("fingerprint"))
	})
}

func (c *Console) cancelNode(w http.ResponseWriter, r *http.Request, p page) {
	cluster, id := r.PathValue("cluster"), r.PathValue("id")
	c.act(w, r, p, "/clusters/"+cluster+"/nodes/new?job="+id, func(context.Context, formValues) error {
		return c.clusters.CancelNode(id)
	})
}

// publicIP is the address GameFleet's host name points at, which is where
// GameFleet reaches clusters from.
func (c *Console) publicIP() string {
	addrs, err := net.LookupHost(c.host)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return addrs[0]
}

// --- Machines

func (c *Console) retireNode(w http.ResponseWriter, r *http.Request, p page) {
	cluster, node := r.PathValue("cluster"), r.PathValue("node")
	c.act(w, r, p, "/clusters/"+cluster+"/nodes/"+node, func(ctx context.Context, _ formValues) error {
		return c.clusters.Retire(ctx, cluster, node, p.User)
	})
}

func (c *Console) renameNode(w http.ResponseWriter, r *http.Request, p page) {
	cluster, node := r.PathValue("cluster"), r.PathValue("node")
	c.act(w, r, p, "/clusters/"+cluster+"/nodes/"+node, func(ctx context.Context, f formValues) error {
		return c.clusters.Rename(ctx, cluster, node, f.r.PostForm.Get("title"), p.User)
	})
}

func (c *Console) keepNode(w http.ResponseWriter, r *http.Request, p page) {
	cluster, node := r.PathValue("cluster"), r.PathValue("node")
	c.act(w, r, p, "/clusters/"+cluster+"/nodes/"+node, func(ctx context.Context, _ formValues) error {
		return c.clusters.Keep(ctx, cluster, node, p.User)
	})
}

// --- Runtime settings

type runtimeGame struct {
	Game string
	Vars []store.Var
}

type runtimePage struct {
	Games []runtimeGame
	// Known lists games with builds, for the form.
	Known []string
}

func (c *Console) runtime(w http.ResponseWriter, r *http.Request, p page) {
	ctx := r.Context()
	vars, err := c.clusters.Vars(ctx)
	if err != nil {
		c.fail(w, p, err)
		return
	}
	var data runtimePage
	for _, v := range vars {
		v.Value = "" // never shown again
		if n := len(data.Games); n == 0 || data.Games[n-1].Game != v.Game {
			data.Games = append(data.Games, runtimeGame{Game: v.Game})
		}
		g := &data.Games[len(data.Games)-1]
		g.Vars = append(g.Vars, v)
	}
	err = c.store.View(ctx, func(tx *store.Tx) error {
		builds, err := tx.Builds("", 1000)
		for _, b := range builds {
			if !slices.Contains(data.Known, b.Game) {
				data.Known = append(data.Known, b.Game)
			}
		}
		return err
	})
	if err != nil {
		c.fail(w, p, err)
		return
	}
	for _, g := range data.Games {
		if !slices.Contains(data.Known, g.Game) {
			data.Known = append(data.Known, g.Game)
		}
	}
	slices.Sort(data.Known)
	p.Title = "运行时配置"
	p.Nav = "settings"
	p.Data = data
	c.render(w, http.StatusOK, "runtime", p)
}

func (c *Console) setVar(w http.ResponseWriter, r *http.Request, p page) {
	c.act(w, r, p, "/settings/runtime", func(ctx context.Context, f formValues) error {
		// Values may hold line breaks or spaces on purpose; only the name is trimmed.
		return c.clusters.SetVar(ctx, f.text("game"), f.text("name"), f.r.PostForm.Get("value"), p.User)
	})
}

func (c *Console) deleteVar(w http.ResponseWriter, r *http.Request, p page) {
	c.act(w, r, p, "/settings/runtime", func(ctx context.Context, f formValues) error {
		return c.clusters.DeleteVar(ctx, f.text("game"), f.text("name"), p.User)
	})
}
