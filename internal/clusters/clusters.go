// Package clusters adds clusters to GameFleet, retires their machines and
// keeps the Secrets game servers need in every cluster. See docs/clusters.md.
//
// A new cluster joins in three moves: the console makes a join code, the
// install script run on the new machine reports its progress with that code,
// and when K3s and Agones are up GameFleet connects to the cluster itself.
// Nothing has to be pasted back.
package clusters

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/remote"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Versions and settings every cluster GameFleet installs gets.
const (
	K3sVersion    = "v1.35.8+k3s1"
	AgonesVersion = "1.60.0"
	Namespace     = "agones-games"
	MinPort       = 20000
	MaxPort       = 20999
)

// JoinCodeTTL is how long a join code works.
const JoinCodeTTL = time.Hour

// Error is a refusal written for the operator, in the console's language.
type Error struct{ Text string }

func (e *Error) Error() string { return e.Text }

func refuse(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

// Connect opens a cluster from its stored details. Tests replace it.
type Connect func(store.Cluster) (cluster.API, error)

// Service manages clusters.
type Service struct {
	store   *store.Store
	sites   *sites.Directory
	keys    *auth.Keys
	connect Connect
	host    string // the registry's public host, for pull Secrets
	log     *slog.Logger
	now     func() time.Time
	kick    chan struct{}

	run        Runner
	appearWait time.Duration

	// retiring is held by a pass that works on retiring machines, and by
	// Keep: the loop never removes a machine that Keep just said it kept.
	retiring sync.Mutex

	mu    sync.Mutex
	joins map[string]*Join    // by ID
	nodes map[string]*NodeJob // by ID
}

// New returns the service. Connect is usually ConnectClient.
func New(s *store.Store, dir *sites.Directory, keys *auth.Keys, connect Connect, host string, log *slog.Logger) *Service {
	return &Service{store: s, sites: dir, keys: keys, connect: connect, host: host, log: log, now: time.Now,
		kick: make(chan struct{}, 1), run: remote.Run, appearWait: appearWait, joins: map[string]*Join{}, nodes: map[string]*NodeJob{}}
}

// ConnectClient opens a real cluster.
func ConnectClient(c store.Cluster) (cluster.API, error) {
	return cluster.Connect(c.Name, c.Server, []byte(c.CA), c.Token, c.Namespace)
}

// Load fills the directory with the stored clusters. A cluster whose details
// do not parse is logged and left out.
func (s *Service) Load(ctx context.Context) error {
	var stored []store.Cluster
	if err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		stored, err = tx.Clusters()
		return err
	}); err != nil {
		return err
	}
	var list []sites.Site
	for _, c := range stored {
		api, err := s.connect(c)
		if err != nil {
			s.log.Error("cluster unusable", "cluster", c.Name, "error", err)
			continue
		}
		list = append(list, sites.Site{Name: c.Name, Region: c.Region, Cluster: api})
	}
	s.sites.Set(list)
	return nil
}

// Stored returns the stored details of a cluster.
func (s *Service) Stored(ctx context.Context, name string) (store.Cluster, bool, error) {
	var found store.Cluster
	ok := false
	err := s.store.View(ctx, func(tx *store.Tx) error {
		list, err := tx.Clusters()
		for _, c := range list {
			if c.Name == name {
				found, ok = c, true
			}
		}
		return err
	})
	return found, ok, err
}

// --- Joining

var (
	name  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	codes = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// Stages a join goes through, in order. The install script reports the first
// three; GameFleet sets the others.
const (
	StageWaiting = "waiting" // the command has not been run yet
	StageK3s     = "k3s"     // installing K3s
	StageAgones  = "agones"  // installing Agones
	StageConnect = "connect" // GameFleet is trying to reach the cluster
	StageDone    = "done"
)

// Join is a cluster on its way in.
type Join struct {
	ID      string // shown in console URLs
	Code    string // in the install command; whoever has it can add the cluster
	Name    string
	Region  string
	Expires time.Time
	// ControlPlaneOnly keeps game servers off the first machine: they run
	// on machines added later.
	ControlPlaneOnly bool
	Stage            string
	// Step is where the stage is, in the install script's words, with Part
	// of Parts done; StepStarted is when the script got there.
	Step        string
	Part, Parts int
	StepStarted time.Time
	Message     string // the last problem, for the console and the script
	IP          string // where the install script runs
	Updated     time.Time
	By          string
	// Remote is set when GameFleet runs the install over SSH.
	Remote *Remote

	target *remote.Target // the login, kept in memory for retries only
}

func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return strings.ToLower(codes.EncodeToString(b))
}

// StartJoin makes a join code for a new cluster.
func (s *Service) StartJoin(ctx context.Context, clusterName, region string, controlPlaneOnly bool, actor string) (Join, error) {
	if !name.MatchString(clusterName) {
		return Join{}, refuse("集群名只能用小写字母、数字和横线，例如 sg-1")
	}
	if !name.MatchString(region) {
		return Join{}, refuse("地区只能用小写字母、数字和横线，例如 sg")
	}
	for _, site := range s.sites.All() {
		if site.Name == clusterName {
			return Join{}, refuse("已经有叫 %s 的集群", clusterName)
		}
		if site.Region == region {
			return Join{}, refuse("地区 %s 已经由集群 %s 服务；一个地区只有一个集群，要扩容就给它加机器", region, site.Name)
		}
	}
	now := s.now()
	j := &Join{ID: random(10), Code: random(20), Name: clusterName, Region: region, Expires: now.Add(JoinCodeTTL),
		ControlPlaneOnly: controlPlaneOnly, Stage: StageWaiting, Updated: now, By: actor}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(now)
	s.joins[j.ID] = j
	return *j, nil
}

// JoinByID returns a join for the console.
func (s *Service) JoinByID(id string) (Join, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.joins[id]
	if !ok {
		return Join{}, false
	}
	return j.snapshot(), true
}

// snapshot copies the join for readers outside the lock, without the login.
func (j *Join) snapshot() Join {
	c := *j
	c.target = nil
	if j.Remote != nil {
		r := j.Remote.snapshot()
		c.Remote = &r
	}
	return c
}

// Lookup returns the join a code belongs to while the code works.
func (s *Service) Lookup(code string) (Join, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.joins {
		if j.Code == code && code != "" && (j.Stage == StageDone || !s.now().After(j.Expires)) {
			return j.snapshot(), true
		}
	}
	return Join{}, false
}

// Report is what the install script sends.
type Report struct {
	Stage string `json:"stage"`
	// Step, Part and Parts say where the stage is; Problem is what holds it up.
	Step       string      `json:"step,omitempty"`
	Part       int         `json:"part,omitempty"`
	Parts      int         `json:"parts,omitempty"`
	Problem    string      `json:"problem,omitempty"`
	Connection *Connection `json:"connection,omitempty"`
}

// reportText is the longest step or problem the console shows, in runes.
const reportText = 300

func clip(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > reportText {
		return string(r[:reportText]) + "…"
	}
	return s
}

// Connection is how GameFleet reaches a cluster and how machines join it.
type Connection struct {
	Name      string `json:"name"`
	Region    string `json:"region"`
	Server    string `json:"server"`
	CABase64  string `json:"caBase64"`
	Token     string `json:"token"`
	Namespace string `json:"namespace"`
	JoinURL   string `json:"joinUrl"`
	JoinToken string `json:"joinToken"`
}

// ErrBadCode means the join code is unknown or expired.
var ErrBadCode = errors.New("join code unknown or expired")

// Report records the install script's progress. With a connection, GameFleet
// connects to the cluster and adds it; an error says why that failed, and the
// script tries again.
func (s *Service) Report(ctx context.Context, code, ip string, r Report) error {
	s.mu.Lock()
	var j *Join
	for _, candidate := range s.joins {
		if candidate.Code == code && code != "" {
			j = candidate
		}
	}
	if j == nil || (s.now().After(j.Expires) && j.Stage != StageDone) {
		s.mu.Unlock()
		return ErrBadCode
	}
	if j.Stage == StageDone {
		s.mu.Unlock()
		return nil
	}
	j.IP, j.Updated = ip, s.now()
	switch r.Stage {
	case StageK3s, StageAgones:
		step := clip(r.Step)
		if r.Stage != j.Stage || step != j.Step {
			j.StepStarted = s.now()
		}
		j.Stage, j.Step, j.Message = r.Stage, step, clip(r.Problem)
		j.Part, j.Parts = r.Part, r.Parts
		if r.Parts < 1 || r.Parts > 10 || r.Part < 0 || r.Part > r.Parts {
			j.Part, j.Parts = 0, 0
		}
		s.mu.Unlock()
		return nil
	case StageConnect:
	default:
		s.mu.Unlock()
		return refuse("不认识的阶段 %q", r.Stage)
	}
	if j.Stage != StageConnect {
		j.StepStarted = s.now()
	}
	j.Stage, j.Step, j.Part, j.Parts = StageConnect, "", 0, 0
	clusterName, region, actor := j.Name, j.Region, j.By
	s.mu.Unlock()

	err := errors.New("没有连接信息")
	if r.Connection != nil {
		c := *r.Connection
		if c.Name != clusterName || c.Region != region {
			err = refuse("安装脚本报的是 %s/%s，和接入码的 %s/%s 不一致", c.Name, c.Region, clusterName, region)
		} else {
			err = s.Add(ctx, c, actor)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		j.Message = err.Error()
		return err
	}
	j.Stage, j.Message = StageDone, ""
	j.target = nil
	return nil
}

// Add connects to a cluster, gives it a pull key and stores it. Adding a
// cluster that is already stored with the same server succeeds and changes
// nothing.
func (s *Service) Add(ctx context.Context, c Connection, actor string) error {
	ca, err := base64.StdEncoding.DecodeString(c.CABase64)
	switch {
	case !name.MatchString(c.Name) || !name.MatchString(c.Region) || !name.MatchString(c.Namespace):
		return refuse("集群名、地区和命名空间只能用小写字母、数字和横线")
	case !httpsURL(c.Server) || !httpsURL(c.JoinURL):
		return refuse("server 和 joinUrl 要是 https 地址")
	case err != nil || !strings.Contains(string(ca), "BEGIN CERTIFICATE"):
		return refuse("caBase64 不是证书")
	case c.Token == "" || c.JoinToken == "":
		return refuse("缺少 token 或 joinToken")
	}
	stored := store.Cluster{Name: c.Name, Region: c.Region, Server: c.Server, CA: string(ca), Token: c.Token,
		Namespace: c.Namespace, JoinURL: c.JoinURL, JoinToken: c.JoinToken, CreatedBy: actor}
	if old, ok, err := s.Stored(ctx, c.Name); err != nil {
		return err
	} else if ok {
		if old.Server == c.Server {
			return nil
		}
		return refuse("已经有叫 %s 的集群", c.Name)
	}
	api, err := s.connect(stored)
	if err != nil {
		return refuse("连接信息不对：%v", err)
	}
	if err := api.Check(ctx); err != nil {
		return refuse("GameFleet 连不上 %s：%v。确认云防火墙对 GameFleet 所在的机器放行了 TCP 6443", c.Server, err)
	}

	// Each cluster pulls with its own key, so one can be revoked alone. A
	// key left from an earlier attempt is replaced.
	keyName := "pull-" + c.Name
	_ = s.keys.Revoke(ctx, keyName, actor)
	key, err := s.keys.Create(ctx, keyName, []string{auth.ScopePull}, actor)
	if err != nil {
		return err
	}
	if err := api.ApplySecret(ctx, cluster.PullSecret(s.host, key)); err != nil {
		return refuse("写不进拉取镜像用的 Secret：%v", err)
	}
	if err := s.applyVars(ctx, api); err != nil {
		return refuse("写不进战斗服的运行时配置：%v", err)
	}
	err = s.store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.InsertCluster(stored); err != nil {
			if errors.Is(err, store.ErrDuplicate) {
				return refuse("集群名或地区已经被占用")
			}
			return err
		}
		return tx.Audit(actor, "cluster.add", c.Name, fmt.Sprintf("地区 %s，%s", c.Region, c.Server))
	})
	if err != nil {
		return err
	}
	s.sites.Add(sites.Site{Name: c.Name, Region: c.Region, Cluster: api})
	s.log.Info("cluster added", "cluster", c.Name, "region", c.Region, "server", c.Server)
	return nil
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && (u.Path == "" || u.Path == "/")
}

// JoinCommand is what to run on a new machine to make it a worker of the cluster.
func (s *Service) JoinCommand(c store.Cluster) string {
	return fmt.Sprintf("curl -sfL https://%s/install/node.sh | sh -s -- --server %s --token %s", s.host, c.JoinURL, c.JoinToken)
}

// InstallCommand is what to run on the first machine of a new cluster.
func (s *Service) InstallCommand(j Join) string {
	command := fmt.Sprintf("curl -sfL https://%s/install/cluster.sh | sh -s -- --code %s", s.host, j.Code)
	if j.ControlPlaneOnly {
		command += " --control-plane-only"
	}
	return command
}

// --- Retiring machines

// Retire stops new game servers from going to the node. The loop deletes its
// idle game servers and removes the node once its last match has ended.
func (s *Service) Retire(ctx context.Context, clusterName, node, actor string) error {
	site, n, err := s.node(ctx, clusterName, node)
	if err != nil {
		return err
	}
	if controlPlane(n) {
		return refuse("%s 是控制面，不能下线", node)
	}
	if !n.Retiring.IsZero() {
		return nil
	}
	if err := site.Cluster.RetireNode(ctx, node, s.now()); err != nil {
		return err
	}
	s.Kick()
	return doneButNotAudited("已经开始下线", s.audit(ctx, actor, "node.retire", node+" @ "+clusterName, ""))
}

// maxTitle is the longest name a machine may be given, in characters.
const maxTitle = 40

// checkTitle tidies a name given to a machine and checks it.
func checkTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > maxTitle {
		return "", refuse("名称最多 %d 个字", maxTitle)
	}
	if strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return "", refuse("名称里不能有换行和控制字符")
	}
	return title, nil
}

// Rename sets the name the console shows for a machine. The node keeps its
// own name, which comes from the host name; an empty title shows that again.
func (s *Service) Rename(ctx context.Context, clusterName, node, title, actor string) error {
	title, err := checkTitle(title)
	if err != nil {
		return err
	}
	site, n, err := s.node(ctx, clusterName, node)
	if err != nil {
		return err
	}
	if n.Given == title {
		return nil
	}
	if err := site.Cluster.NameNode(ctx, node, title); err != nil {
		return err
	}
	detail := "改叫 " + title
	if title == "" {
		detail = "去掉名称，显示节点名"
	}
	return doneButNotAudited("已经改好名字", s.audit(ctx, actor, "node.rename", node+" @ "+clusterName, detail))
}

// Keep cancels a retirement that has not finished yet. It waits for a pass
// that works on retiring machines; if that pass removed the machine, Keep
// says the machine is gone.
func (s *Service) Keep(ctx context.Context, clusterName, node, actor string) error {
	s.retiring.Lock()
	defer s.retiring.Unlock()
	site, n, err := s.node(ctx, clusterName, node)
	if err != nil {
		return err
	}
	if n.Retiring.IsZero() {
		return nil
	}
	if err := site.Cluster.KeepNode(ctx, node); err != nil {
		return err
	}
	// Its game servers take new matches again.
	rooms, err := site.Cluster.Rooms(ctx)
	if err != nil {
		return err
	}
	for _, r := range rooms {
		if r.Node == node {
			if err := site.Cluster.MarkRetiring(ctx, r.ID, false); err != nil {
				return err
			}
		}
	}
	return doneButNotAudited("已经取消下线", s.audit(ctx, actor, "node.keep", node+" @ "+clusterName, ""))
}

func (s *Service) node(ctx context.Context, clusterName, node string) (sites.Site, cluster.Node, error) {
	site, ok := s.sites.Get(clusterName)
	if !ok {
		return sites.Site{}, cluster.Node{}, refuse("没有叫 %s 的集群", clusterName)
	}
	nodes, err := site.Cluster.Nodes(ctx)
	if err != nil {
		return sites.Site{}, cluster.Node{}, err
	}
	for _, n := range nodes {
		if n.Name == node {
			return site, n, nil
		}
	}
	return sites.Site{}, cluster.Node{}, refuse("集群 %s 里没有 %s 这台机器", clusterName, node)
}

func controlPlane(n cluster.Node) bool {
	return slices.Contains(n.Roles, "control-plane") || slices.Contains(n.Roles, "master")
}

// --- Runtime settings

var varName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// SetVar sets one environment variable of a game's servers in every cluster.
// Game servers that already run keep the old value; new ones get the new one.
func (s *Service) SetVar(ctx context.Context, game, key, value, actor string) error {
	err := SaveVar(ctx, s.store, game, key, value, actor)
	if err == nil {
		s.Kick()
	}
	return err
}

// SaveVar is SetVar for a process without the loop, such as the command
// line: a running GameFleet writes the value into the clusters on its next
// pass, within Interval.
func SaveVar(ctx context.Context, db *store.Store, game, key, value, actor string) error {
	if !name.MatchString(game) {
		return refuse("游戏名只能用小写字母、数字和横线")
	}
	if !varName.MatchString(key) {
		return refuse("变量名只能用大写字母、数字和下划线，例如 BACKEND_URL")
	}
	if len(value) > 16<<10 {
		return refuse("值太长")
	}
	return db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.PutVar(store.Var{Game: game, Name: key, Value: value, UpdatedBy: actor}); err != nil {
			return err
		}
		return tx.Audit(actor, "runtime.set", game+" "+key, "")
	})
}

// DeleteVar removes a variable from the game's settings. The loop takes it
// out of the Secrets within Interval; game servers that already run keep it.
func (s *Service) DeleteVar(ctx context.Context, game, key, actor string) error {
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		if err := tx.DeleteVar(game, key); err != nil {
			return err
		}
		return tx.Audit(actor, "runtime.delete", game+" "+key, "")
	})
	if errors.Is(err, store.ErrNotFound) {
		return refuse("没有这个变量")
	}
	return err
}

// Vars lists the variables of every game, values included. The console shows
// only the names.
func (s *Service) Vars(ctx context.Context) ([]store.Var, error) {
	var vars []store.Var
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		vars, err = tx.Vars("")
		return err
	})
	return vars, err
}

// applyVars writes each game's settings into the cluster as <game>-runtime.
// Every game with a build gets its Secret, an empty one when it has no
// settings: GameFleet owns the keys it writes, so a Secret written without a
// key loses it, and a deleted last variable leaves no old value behind.
//
// A game with no build yet that loses its last variable keeps the old values
// until the first pass after its first build; no game server of it runs
// before that build is deployed, which takes someone in the console.
func (s *Service) applyVars(ctx context.Context, api cluster.API) error {
	var games []string
	var vars []store.Var
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		if games, err = tx.RuntimeGames(); err != nil {
			return err
		}
		vars, err = tx.Vars("")
		return err
	})
	if err != nil {
		return err
	}
	data := map[string]map[string][]byte{}
	for _, game := range games {
		data[game] = map[string][]byte{}
	}
	for _, v := range vars {
		data[v.Game][v.Name] = []byte(v.Value)
	}
	for _, game := range games {
		if err := api.ApplySecret(ctx, cluster.Secret{Name: game + cluster.RuntimeSecretSuffix, Data: data[game]}); err != nil {
			return err
		}
	}
	return nil
}

// doneButNotAudited tells an operator that what they asked for happened in
// the cluster, but its audit entry could not be written: the error alone
// would read as if nothing had happened.
func doneButNotAudited(done string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s，但审计日志没记上：%w", done, err)
}

func (s *Service) audit(ctx context.Context, actor, action, target, detail string) error {
	return s.store.Update(ctx, func(tx *store.Tx) error { return tx.Audit(actor, action, target, detail) })
}
