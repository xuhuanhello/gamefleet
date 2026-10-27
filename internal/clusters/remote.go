package clusters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/remote"
)

// Runner runs a command on a machine over SSH; remote.Run is the real one.
type Runner func(ctx context.Context, t remote.Target, command string, out io.Writer) (fingerprint string, err error)

// SetRunner replaces the SSH runner; tests use it.
func (s *Service) SetRunner(r Runner) { s.run = r }

// HostKeyReader reads a machine's SSH host key without logging in;
// remote.ReadHostKey is the real one.
type HostKeyReader func(ctx context.Context, host string, port int) (remote.HostKey, error)

// SetHostKeyReader replaces the host key reader; tests use it.
func (s *Service) SetHostKeyReader(r HostKeyReader) { s.readHostKey = r }

// States of a run over SSH.
const (
	// RemoteChecking waits for the operator to check the machine's host key
	// on the machine; GameFleet has not logged in.
	RemoteChecking   = "checking"
	RemoteConnecting = "connecting"
	RemoteRunning    = "running"
	RemoteFailed     = "failed"
	RemoteFinished   = "finished"
)

// remoteTimeout bounds one run. Installing a cluster takes minutes; most of
// the rest is waiting for the operator to open the firewall.
const remoteTimeout = 45 * time.Minute

const logLines = 300

// Remote is GameFleet running an install command on a machine over SSH.
// The login itself is never part of it.
type Remote struct {
	Host        string
	User        string
	State       string
	Fingerprint string // of the machine's SSH host key
	KeyFile     string // where the machine keeps that key, for the operator to check it
	Error       string
	Log         []string // the last lines the command printed
	Started     time.Time
	Ended       time.Time
}

// auditRun writes the audit entry of a run over SSH. The run has happened
// either way, so a failure to write it goes into the run's log, which its
// page shows, and into GameFleet's.
func (s *Service) auditRun(ctx context.Context, r *Remote, actor, action, target, detail string) {
	err := s.audit(ctx, actor, action, target, detail)
	if err == nil {
		return
	}
	s.log.Error("audit failed", "action", action, "target", target, "error", err)
	s.mu.Lock()
	r.Log = append(r.Log, "GameFleet：审计日志没记上这次操作："+err.Error())
	s.mu.Unlock()
}

func (r *Remote) snapshot() Remote {
	c := *r
	c.Log = slices.Clone(r.Log)
	return c
}

// Busy reports whether the run is still going.
func (r Remote) Busy() bool { return r.State == RemoteConnecting || r.State == RemoteRunning }

// withCurl installs curl first on machines that lack it.
func withCurl(command string) string {
	return "command -v curl >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq curl; } >/dev/null 2>&1; " + command
}

// logWriter appends what a run prints to its Remote, under the service lock.
type logWriter struct {
	s *Service
	r *Remote
}

func (w logWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.r.Log = append(w.r.Log, line)
	}
	if over := len(w.r.Log) - logLines; over > 0 {
		w.r.Log = w.r.Log[over:]
	}
	if w.r.State == RemoteConnecting {
		w.r.State = RemoteRunning
	}
	return len(p), nil
}

// execute runs command for a job and records how it went in r.
func (s *Service) execute(r *Remote, target remote.Target, command string) (string, error) {
	s.mu.Lock()
	r.State, r.Error, r.Log, r.Started, r.Ended = RemoteConnecting, "", nil, s.now(), time.Time{}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()
	fingerprint, err := s.run(ctx, target, command, logWriter{s, r})
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Fingerprint, r.Ended = fingerprint, s.now()
	var failed *remote.Error
	switch {
	case errors.As(err, &failed) && failed.Status > 0 && reason(r.Log) != "":
		r.State, r.Error = RemoteFailed, fmt.Sprintf("%s（退出码 %d）", reason(r.Log), failed.Status)
	case err != nil:
		r.State, r.Error = RemoteFailed, err.Error()
	default:
		r.State = RemoteFinished
	}
	return fingerprint, err
}

// reason is why an install script stopped, in its own words.
func reason(log []string) string {
	for _, line := range slices.Backward(log) {
		if text, ok := strings.CutPrefix(line, "GameFleet 停下了："); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func checkTarget(t remote.Target) (remote.Target, error) {
	t, err := t.Check()
	if err != nil {
		return t, refuse("%s", err.Error())
	}
	return t, nil
}

// checking reads the machine's host key, without logging in, and returns the
// run that waits for the operator to check it on the machine.
func (s *Service) checking(ctx context.Context, t remote.Target) (*Remote, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	key, err := s.readHostKey(ctx, t.Host, t.Port)
	if err != nil {
		return nil, refuse("%s", err.Error())
	}
	return &Remote{Host: t.Host, User: t.User, State: RemoteChecking, Fingerprint: key.Fingerprint, KeyFile: key.File}, nil
}

// checked records that the operator checked the host key on the machine, and
// pins the login to it. fingerprint is the one the operator was shown. The
// caller holds the lock.
func checked(r *Remote, t *remote.Target, fingerprint string) error {
	switch {
	case r.State != RemoteChecking:
		return refuse("这次安装已经核对过主机指纹了")
	case fingerprint != r.Fingerprint:
		return refuse("核对的指纹和 GameFleet 读到的不一样，请刷新页面重新核对")
	}
	t.HostKey = r.Fingerprint
	r.State = RemoteConnecting
	return nil
}

// cancelled drops the login of a run the operator would not let start. The
// caller holds the lock.
func cancelled(r *Remote) {
	r.State, r.Error = RemoteFailed, "取消了：GameFleet 没有登录这台机器，登录信息已经丢掉"
}

// --- New clusters over SSH

// StartRemoteJoin makes a join code for GameFleet to run the install command
// on the machine itself. It reads the machine's host key and waits for the
// operator to check it on the machine (ConfirmJoin) before it logs in. The
// login stays in memory until the cluster has joined or the code expires, so
// a failed run can be retried.
func (s *Service) StartRemoteJoin(ctx context.Context, name, region string, controlPlaneOnly bool, t remote.Target, actor string) (Join, error) {
	t, err := checkTarget(t)
	if err != nil {
		return Join{}, err
	}
	r, err := s.checking(ctx, t)
	if err != nil {
		return Join{}, err
	}
	j, err := s.StartJoin(ctx, name, region, controlPlaneOnly, actor)
	if err != nil {
		return Join{}, err
	}
	s.mu.Lock()
	stored := s.joins[j.ID]
	stored.target = &t
	stored.Remote = r
	j = stored.snapshot()
	s.mu.Unlock()
	return j, nil
}

// StartRemoteRejoin is StartRejoin with GameFleet running the command on the
// control plane over SSH, once the operator checked its host key.
func (s *Service) StartRemoteRejoin(ctx context.Context, clusterName string, rotateToken bool, t remote.Target, actor string) (Join, error) {
	t, err := checkTarget(t)
	if err != nil {
		return Join{}, err
	}
	r, err := s.checking(ctx, t)
	if err != nil {
		return Join{}, err
	}
	j, err := s.StartRejoin(ctx, clusterName, rotateToken, actor)
	if err != nil {
		return Join{}, err
	}
	s.mu.Lock()
	stored := s.joins[j.ID]
	stored.target = &t
	stored.Remote = r
	j = stored.snapshot()
	s.mu.Unlock()
	return j, nil
}

// ConfirmJoin starts the install once the operator found the machine's host
// key to be fingerprint, the one GameFleet showed.
func (s *Service) ConfirmJoin(id, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.joins[id]
	switch {
	case !ok || j.Remote == nil:
		return refuse("没有这次安装")
	case j.target == nil || s.now().After(j.Expires):
		return refuse("登录信息已经丢了（加入码过期或 GameFleet 重启过），请重新填一次")
	}
	if err := checked(j.Remote, j.target, fingerprint); err != nil {
		return err
	}
	go s.runJoin(id)
	return nil
}

// CancelJoin drops the login of an install the operator would not let start.
func (s *Service) CancelJoin(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.joins[id]
	if !ok || j.Remote == nil {
		return refuse("没有这次安装")
	}
	if j.Remote.State != RemoteChecking {
		return refuse("安装已经开始了，不能取消")
	}
	j.target = nil
	cancelled(j.Remote)
	return nil
}

// RetryJoin runs the install again with the login given before.
func (s *Service) RetryJoin(id string) error {
	s.mu.Lock()
	j, ok := s.joins[id]
	switch {
	case !ok || j.Remote == nil:
		s.mu.Unlock()
		return refuse("没有这次安装")
	case j.target == nil || s.now().After(j.Expires):
		s.mu.Unlock()
		return refuse("登录信息已经丢了（加入码过期或 GameFleet 重启过），请重新填一次")
	case j.Remote.State == RemoteChecking:
		s.mu.Unlock()
		return refuse("先核对这台机器的主机指纹")
	case j.Remote.Busy():
		s.mu.Unlock()
		return nil
	}
	j.Remote.State = RemoteConnecting
	s.mu.Unlock()
	go s.runJoin(id)
	return nil
}

func (s *Service) runJoin(id string) {
	s.mu.Lock()
	j := s.joins[id]
	target, r, name, by := *j.target, j.Remote, j.Name, j.By
	command := withCurl(s.InstallCommand(*j))
	s.mu.Unlock()

	fingerprint, err := s.execute(r, target, command)
	result := "成功"
	if err != nil {
		result = "失败：" + err.Error()
	}
	s.auditRun(context.Background(), r, by, "cluster.install", name,
		fmt.Sprintf("通过 SSH 在 %s@%s 上安装，%s；主机指纹 %s", target.User, target.Host, result, orDash(fingerprint)))
	s.mu.Lock()
	if j.Stage == StageDone {
		j.target = nil
	}
	s.mu.Unlock()
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// --- New machines over SSH

// Stages of a machine joining a cluster.
const (
	NodeChecking   = "checking"   // the operator checks its host key; GameFleet has not logged in
	NodeInstalling = "installing" // GameFleet runs node.sh on it
	NodeAppearing  = "appearing"  // waiting for it to show up in the cluster
	NodeDone       = "done"
	NodeFailed     = "failed"
)

// NodeJob is a machine joining a cluster through GameFleet.
type NodeJob struct {
	ID      string
	Cluster string
	Stage   string
	Message string
	Node    string // its name in the cluster once it shows up
	Title   string // the name to give it in the console, if any
	Expires time.Time
	By      string
	Remote  *Remote

	target *remote.Target
}

func (n *NodeJob) snapshot() NodeJob {
	c := *n
	c.target = nil
	r := n.Remote.snapshot()
	c.Remote = &r
	return c
}

// StartNodeJoin has GameFleet log in to a machine and join it to the
// cluster, and give it title as its name in the console once it is in.
func (s *Service) StartNodeJoin(ctx context.Context, clusterName string, t remote.Target, title, actor string) (NodeJob, error) {
	t, err := checkTarget(t)
	if err != nil {
		return NodeJob{}, err
	}
	if title, err = checkTitle(title); err != nil {
		return NodeJob{}, err
	}
	site, ok := s.sites.Get(clusterName)
	if !ok {
		return NodeJob{}, refuse("没有叫 %s 的集群", clusterName)
	}
	if _, ok, err := s.Stored(ctx, clusterName); err != nil {
		return NodeJob{}, err
	} else if !ok {
		return NodeJob{}, refuse("集群 %s 的加入信息不在 GameFleet 里", clusterName)
	}
	if nodes, err := site.Cluster.Nodes(ctx); err == nil {
		for _, n := range nodes {
			if n.ExternalIP == t.Host || n.InternalIP == t.Host {
				return NodeJob{}, refuse("%s 已经是集群里的 %s 了", t.Host, n.Name)
			}
		}
	}
	r, err := s.checking(ctx, t)
	if err != nil {
		return NodeJob{}, err
	}
	now := s.now()
	job := &NodeJob{ID: random(10), Cluster: clusterName, Stage: NodeChecking, Title: title, Expires: now.Add(JoinCodeTTL), By: actor,
		Remote: r, target: &t}
	s.mu.Lock()
	s.forget(now)
	s.nodes[job.ID] = job
	snapshot := job.snapshot()
	s.mu.Unlock()
	return snapshot, nil
}

// ConfirmNode starts the join once the operator found the machine's host key
// to be fingerprint, the one GameFleet showed.
func (s *Service) ConfirmNode(id, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	switch {
	case !ok:
		return refuse("没有这次加入")
	case n.target == nil || s.now().After(n.Expires):
		return refuse("登录信息已经丢了（超过一小时或 GameFleet 重启过），请重新填一次")
	}
	if err := checked(n.Remote, n.target, fingerprint); err != nil {
		return err
	}
	n.Stage = NodeInstalling
	go s.runNode(id)
	return nil
}

// CancelNode drops the login of a join the operator would not let start.
func (s *Service) CancelNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return refuse("没有这次加入")
	}
	if n.Stage != NodeChecking {
		return refuse("加入已经开始了，不能取消")
	}
	n.target = nil
	cancelled(n.Remote)
	n.Stage, n.Message = NodeFailed, n.Remote.Error
	return nil
}

// NodeJobByID returns a machine's join for the console.
func (s *Service) NodeJobByID(id string) (NodeJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return NodeJob{}, false
	}
	return n.snapshot(), true
}

// RetryNode runs the join again with the login given before.
func (s *Service) RetryNode(id string) error {
	s.mu.Lock()
	n, ok := s.nodes[id]
	switch {
	case !ok:
		s.mu.Unlock()
		return refuse("没有这次加入")
	case n.target == nil || s.now().After(n.Expires):
		s.mu.Unlock()
		return refuse("登录信息已经丢了（超过一小时或 GameFleet 重启过），请重新填一次")
	case n.Stage == NodeChecking:
		s.mu.Unlock()
		return refuse("先核对这台机器的主机指纹")
	case n.Remote.Busy() || n.Stage == NodeAppearing || n.Stage == NodeDone:
		s.mu.Unlock()
		return nil
	}
	n.Stage, n.Message, n.Remote.State = NodeInstalling, "", RemoteConnecting
	s.mu.Unlock()
	go s.runNode(id)
	return nil
}

// appearWait is how long a joined machine has to show up in the cluster.
const appearWait = 3 * time.Minute

func (s *Service) runNode(id string) {
	s.mu.Lock()
	n := s.nodes[id]
	target, r, clusterName, title, by := *n.target, n.Remote, n.Cluster, n.Title, n.By
	s.mu.Unlock()
	ctx := context.Background()
	stored, ok, err := s.Stored(ctx, clusterName)
	site, inDir := s.sites.Get(clusterName)
	if err != nil || !ok || !inDir {
		s.nodeFailed(n, "集群 %s 不在了", clusterName)
		return
	}

	fingerprint, err := s.execute(r, target, withCurl(s.JoinCommand(stored)))
	if err != nil {
		s.nodeFailed(n, "%s", err.Error())
		s.auditRun(ctx, r, by, "node.install", target.Host+" @ "+clusterName,
			fmt.Sprintf("通过 SSH 加入失败；主机指纹 %s", orDash(fingerprint)))
		return
	}
	s.mu.Lock()
	n.Stage = NodeAppearing
	s.mu.Unlock()

	deadline := time.Now().Add(s.appearWait)
	for {
		if name := s.find(ctx, site.Cluster, target.Host); name != "" {
			named := ""
			if title != "" {
				if err := site.Cluster.NameNode(ctx, name, title); err != nil {
					s.log.Warn("could not name a new machine", "cluster", clusterName, "node", name, "error", err)
				} else {
					named = "，叫它 " + title
				}
			}
			s.mu.Lock()
			n.Stage, n.Node, n.target = NodeDone, name, nil
			s.mu.Unlock()
			s.auditRun(ctx, r, by, "node.add", name+" @ "+clusterName,
				fmt.Sprintf("通过 SSH 在 %s@%s 上安装%s；主机指纹 %s", target.User, target.Host, named, orDash(fingerprint)))
			return
		}
		if time.Now().After(deadline) {
			s.nodeFailed(n, "K3s 装好了，但 %s 之内这台机器没有出现在集群里。看日志，或者在那台机器上执行 journalctl -u k3s-agent", s.appearWait)
			return
		}
		time.Sleep(min(5*time.Second, s.appearWait/10+time.Millisecond))
	}
}

func (s *Service) find(ctx context.Context, api cluster.API, host string) string {
	nodes, err := api.Nodes(ctx)
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if n.ExternalIP == host || n.InternalIP == host {
			return n.Name
		}
	}
	return ""
}

func (s *Service) nodeFailed(n *NodeJob, format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n.Stage, n.Message = NodeFailed, fmt.Sprintf(format, args...)
}

// forget drops finished or expired jobs a day after they expire, and the
// logins of expired ones at once. The caller holds the lock.
func (s *Service) forget(now time.Time) {
	for id, j := range s.joins {
		if now.After(j.Expires) {
			j.target = nil
		}
		if now.After(j.Expires.Add(24 * time.Hour)) {
			delete(s.joins, id)
		}
	}
	for id, n := range s.nodes {
		if now.After(n.Expires) {
			n.target = nil
		}
		if now.After(n.Expires.Add(24 * time.Hour)) {
			delete(s.nodes, id)
		}
	}
}
