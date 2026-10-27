// Package release turns registered builds into running game servers and
// decides which build new matches get (docs/releases.md): every build runs as its own Fleet, and switching versions only changes a
// route. A deployment no route uses any more drains to zero and is deleted
// once its last match ends and the retention time has passed.
package release

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/health"
	"github.com/xuhuanhello/gamefleet/internal/registry"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Channels. The matchmaker picks one per match: canary for testers, stable
// for everyone else.
const (
	Stable = "stable"
	Canary = "canary"
)

// Labels a build must carry, see docs/game-server.md.
const (
	LabelCompat   = "io.gamefleet.compat"
	LabelRevision = "org.opencontainers.image.revision"
	LabelVersion  = "org.opencontainers.image.version"
)

// Kinds of refusal. Every refusal is an *Error of one of these kinds; the
// console shows its text as it is, the API maps the kind to a status code.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	// ErrConflict means the change does not fit the current state, for
	// example routing to a build that is not deployed anywhere.
	ErrConflict = errors.New("conflict")
	// ErrNotReady refuses a route to a build that has no game server ready
	// in some cluster. The operator may move the route anyway.
	ErrNotReady = errors.New("not ready")
	// ErrNoRoute means no deployed build serves the match's channel and
	// compat version in its region.
	ErrNoRoute = errors.New("route_not_found")
)

// compatValue is what a compat version may look like.
var compatValue = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Defaults for a game's first deployment in a cluster. Later deployments
// start from the previous one's settings.
var Defaults = store.Deployment{
	// One idle game server is room for Rooms more matches.
	Buffer:      1,
	MaxReplicas: 10,
	Rooms:       50,
	Resources:   store.Resources{CPURequest: 250, CPULimit: 2000, MemoryRequest: 256, MemoryLimit: 1024},
}

// Registry reads pushed images.
type Registry interface {
	Inspect(ctx context.Context, repo, ref string) (registry.Image, error)
}

// Service releases builds.
type Service struct {
	monitor   *health.Monitor
	store     *store.Store
	registry  Registry
	host      string // where clusters pull images from
	sites     *sites.Directory
	retention time.Duration
	log       *slog.Logger
	kick      chan struct{}
}

// New returns the service. host is the registry's public host name.
func New(s *store.Store, reg Registry, host string, dir *sites.Directory, retention time.Duration, log *slog.Logger) *Service {
	return &Service{store: s, registry: reg, host: host, sites: dir, retention: retention, log: log, kick: make(chan struct{}, 1)}
}

func (s *Service) site(name string) (sites.Site, bool) { return s.sites.Get(name) }

// SetMonitor has the loop tell the monitor when it gets through a pass, and
// whether it reached each cluster.
func (s *Service) SetMonitor(m *health.Monitor) { s.monitor = m }

// Error is a refusal written for the operator, in the console's language.
type Error struct {
	Kind error
	Text string
}

func (e *Error) Error() string { return e.Text }
func (e *Error) Unwrap() error { return e.Kind }

func invalid(format string, args ...any) error {
	return &Error{ErrInvalid, fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &Error{ErrConflict, fmt.Sprintf(format, args...)}
}

// --- Builds

// Registration names a pushed image by tag, digest or both.
type Registration struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	Note       string `json:"note"`
}

// Register records a pushed image as a build. Registering the same image
// again returns the existing build with created false.
func (s *Service) Register(ctx context.Context, r Registration, actor string) (b store.Build, created bool, err error) {
	if r.Tag == "" && r.Digest == "" {
		return store.Build{}, false, invalid("请给出镜像的 tag 或 digest")
	}
	if !printable(r.Note, 500, true) {
		return store.Build{}, false, invalid("备注最多 500 个字符")
	}
	ref := r.Digest
	if r.Tag != "" {
		ref = r.Tag
	}
	img, err := s.registry.Inspect(ctx, r.Repository, ref)
	if err != nil {
		return store.Build{}, false, err
	}
	if r.Digest != "" && img.Digest != r.Digest {
		return store.Build{}, false, invalid("tag %s 指向 %s，不是 %s", r.Tag, img.Digest, r.Digest)
	}
	for _, label := range []string{LabelCompat, LabelRevision, LabelVersion} {
		if img.Labels[label] == "" {
			return store.Build{}, false, fmt.Errorf("%w：缺少标签 %s", registry.ErrUnusable, label)
		}
	}
	compat := img.Labels[LabelCompat]
	if !compatValue.MatchString(compat) {
		return store.Build{}, false, fmt.Errorf("%w：标签 %s 只能是 1 到 128 个字母、数字、点、横线或下划线", registry.ErrUnusable, LabelCompat)
	}

	err = s.store.Update(ctx, func(tx *store.Tx) error {
		existing, err := tx.BuildByDigest(r.Repository, img.Digest)
		if err == nil {
			b = existing
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		b, err = tx.InsertBuild(store.Build{
			Game: r.Repository, Digest: img.Digest, Tag: r.Tag, Compat: compat, Labels: img.Labels,
			Port: img.Port, Protocol: img.Protocol, Note: strings.TrimSpace(r.Note), RegisteredBy: actor,
		})
		if err != nil {
			return err
		}
		created = true
		return tx.Audit(actor, "build.register", buildName(b), fmt.Sprintf("版本 %s，联机版本 %s，%s", b.Version(), b.Compat, b.Digest[:min(len(b.Digest), len("sha256:")+12)]))
	})
	return b, created, err
}

func buildName(b store.Build) string { return fmt.Sprintf("%s #%d", b.Game, b.ID) }

// --- Deployments

// DeployRequest asks for a build to run in a cluster.
type DeployRequest struct {
	BuildID     int64
	Cluster     string
	Buffer      int
	MaxReplicas int
	// Rooms is how many matches one game server runs at once.
	Rooms     int
	Resources store.Resources
}

// DefaultsFor returns the settings a new deployment of the game in the
// cluster starts from.
func (s *Service) DefaultsFor(ctx context.Context, game, clusterName string) (store.Deployment, error) {
	var d store.Deployment
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		d, err = tx.LastDeployment(game, clusterName)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return Defaults, nil
	}
	return d, err
}

// Deploy starts the build in the cluster. The background loop creates the
// Fleet within seconds.
func (s *Service) Deploy(ctx context.Context, req DeployRequest, actor string) (store.Deployment, error) {
	if _, ok := s.site(req.Cluster); !ok {
		return store.Deployment{}, invalid("没有叫 %s 的集群", req.Cluster)
	}
	r := req.Resources
	switch {
	case req.Buffer < 1 || req.Buffer > 50:
		return store.Deployment{}, invalid("空闲战斗服要在 1 到 50 个之间")
	case req.MaxReplicas < req.Buffer || req.MaxReplicas > 500:
		return store.Deployment{}, invalid("上限要不小于空闲数，最多 500")
	case req.Rooms < 1 || req.Rooms > cluster.MaxRooms:
		return store.Deployment{}, invalid("每个战斗服的局数要在 1 到 %d 之间", cluster.MaxRooms)
	case r.CPURequest < 10 || r.CPULimit < r.CPURequest || r.CPULimit > 16000:
		return store.Deployment{}, invalid("CPU 请求至少 10m，上限要在请求和 16000m 之间")
	case r.MemoryRequest < 16 || r.MemoryLimit < r.MemoryRequest || r.MemoryLimit > 65536:
		return store.Deployment{}, invalid("内存请求至少 16Mi，上限要在请求和 65536Mi 之间")
	}
	var d store.Deployment
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		b, err := tx.Build(req.BuildID)
		if err != nil {
			return notFound(err, "构建")
		}
		routed, err := routedBuilds(tx)
		if err != nil {
			return err
		}
		state := store.StateNew
		if routed[b.ID] {
			state = store.StateServing
		}
		fleet := fleetName(b)
		if len(fleet) > maxFleetName {
			return invalid("游戏名 %s 太长：Fleet 名 %s 超过了 %d 个字符", b.Game, fleet, maxFleetName)
		}
		d, err = tx.InsertDeployment(store.Deployment{
			BuildID: b.ID, Cluster: req.Cluster, Fleet: fleet,
			Buffer: req.Buffer, MaxReplicas: req.MaxReplicas, Rooms: req.Rooms, Resources: r, State: state, CreatedBy: actor,
		})
		if errors.Is(err, store.ErrDuplicate) {
			return conflict("%s 已经部署在 %s 了", buildName(b), req.Cluster)
		}
		if err != nil {
			return err
		}
		return tx.Audit(actor, "deployment.create", d.Fleet+" @ "+d.Cluster,
			fmt.Sprintf("%s，每个战斗服 %d 局，保持 %d 个空闲，最多 %d 个", buildName(b), d.Rooms, d.Buffer, d.MaxReplicas))
	})
	if err == nil {
		s.Kick()
	}
	return d, err
}

// UndeployWait is how long a deployment must be out of service before it can
// be deleted by hand. An allocation that read the route while the deployment
// still served may reach the cluster this late: the cluster client waits 10
// seconds for an answer.
const UndeployWait = time.Minute

// Undeploy deletes a deployment that no route uses, with its Fleet. It
// refuses while matches still run on it; those end on their own.
func (s *Service) Undeploy(ctx context.Context, id int64, actor string) error {
	var d store.Deployment
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		d, err = tx.Deployment(id)
		return notFound(err, "部署")
	})
	if err != nil {
		return err
	}
	if d.State == store.StateDeleted {
		return nil
	}
	if err := s.undeployable(d); err != nil {
		return err
	}
	site, ok := s.site(d.Cluster)
	if !ok {
		return conflict("配置里已经没有集群 %s", d.Cluster)
	}
	fleets, err := site.Cluster.ManagedFleets(ctx)
	if err != nil {
		return err
	}
	for _, f := range fleets {
		if owns(d, f) && f.Allocated+f.Reserved > 0 {
			return conflict("%s 上还有 %d 局在打，打完才能删", d.Fleet, f.Allocated+f.Reserved)
		}
	}
	// Record the deletion before deleting anything. A route that moved to the
	// build since the check above keeps the deployment; once recorded, no
	// route can move to it.
	err = s.store.Update(ctx, func(tx *store.Tx) error {
		current, err := tx.Deployment(id)
		if err != nil {
			return notFound(err, "部署")
		}
		if current.State == store.StateDeleted {
			return nil
		}
		if err := s.undeployable(current); err != nil {
			return err
		}
		if err := tx.SetDeploymentState(id, store.StateDeleted); err != nil {
			return err
		}
		return tx.Audit(actor, "deployment.delete", d.Fleet+" @ "+d.Cluster, "")
	})
	if err != nil {
		return err
	}
	if err := errors.Join(site.Cluster.DeleteAutoscaler(ctx, d.Fleet), site.Cluster.DeleteFleet(ctx, d.Fleet)); err != nil {
		// The loop deletes it as a leftover.
		return fmt.Errorf("部署记为删除了，但集群里的 %s 还没删掉，后台会接着删: %w", d.Fleet, err)
	}
	return nil
}

// undeployable refuses to delete a deployment by hand while a route uses it,
// or for UndeployWait after the last one stopped.
func (s *Service) undeployable(d store.Deployment) error {
	switch {
	case d.State == store.StateServing:
		return conflict("还有路由把对局分到这个构建，先改路由")
	case d.State == store.StateDraining && s.store.Now().Sub(d.Since) < UndeployWait:
		left := UndeployWait - s.store.Now().Sub(d.Since)
		return conflict("%s 刚停止接新局，可能还有分配请求在路上。%s 后再删", d.Fleet, left.Round(time.Second))
	}
	return nil
}

// canTakeAMatch reports whether the deployment's Fleet has a game server
// that allocation can give a match to: a Ready one not taken out of
// allocation, or a running one with room left. A Fleet being deleted has
// none.
func canTakeAMatch(d store.Deployment, fleets []cluster.ManagedFleet, rooms []cluster.Room) bool {
	i := slices.IndexFunc(fleets, func(f cluster.ManagedFleet) bool { return owns(d, f) })
	if i < 0 || fleets[i].Deleting {
		return false
	}
	ready := fleets[i].Ready
	for _, r := range rooms {
		switch {
		case r.Fleet != d.Fleet:
		case r.State == "Ready" && r.Retiring:
			// Counted Ready by its Fleet, but allocation passes it by.
			ready--
		case r.State == "Allocated" && !r.Retiring && r.Free() > 0:
			return true
		}
	}
	return ready > 0
}

// maxFleetName is the longest Fleet name: Agones puts it in a label value.
const maxFleetName = 63

// fleetName names a new deployment's Fleet: the game, the build, and four
// random characters. A database restored from a backup hands out build and
// deployment IDs again, while the Fleets of the deployments it lost may still
// run. The random part keeps a new deployment off such a Fleet: the loop takes
// that one for a leftover and drains it, and the new build serves from a Fleet
// of its own.
func fleetName(b store.Build) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	suffix := make([]byte, 4)
	for i := range suffix {
		suffix[i] = chars[rand.IntN(len(chars))]
	}
	return fmt.Sprintf("%s-%d-%s", b.Game, b.ID, suffix)
}

// owns reports whether the Fleet is the deployment's: it has the
// deployment's name and carries its ID. A Fleet with only one of the two
// belongs to a deployment that is gone.
func owns(d store.Deployment, f cluster.ManagedFleet) bool {
	return f.Name == d.Fleet && f.Deployment == d.ID
}

// SetBuffer changes how many idle game servers a deployment keeps.
func (s *Service) SetBuffer(ctx context.Context, id int64, buffer, maxReplicas int, actor string) error {
	if buffer < 1 || buffer > 50 || maxReplicas < buffer || maxReplicas > 500 {
		return invalid("空闲战斗服要在 1 到 50 个之间，上限不小于空闲数，最多 500")
	}
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		d, err := tx.Deployment(id)
		if err != nil {
			return notFound(err, "部署")
		}
		if d.State == store.StateDeleted {
			return conflict("这个部署已经删除了")
		}
		if err := tx.SetDeploymentBuffer(id, buffer, maxReplicas); err != nil {
			return err
		}
		return tx.Audit(actor, "deployment.buffer", d.Fleet+" @ "+d.Cluster,
			fmt.Sprintf("保持 %d 个空闲，最多 %d 个（原来 %d、%d）", buffer, maxReplicas, d.Buffer, d.MaxReplicas))
	})
	if err == nil {
		s.Kick()
	}
	return err
}

// --- Routes

// SetRoute sends new matches of the build's game and compat version on the
// channel to the build. The build must be deployed somewhere, and, unless
// force, have a game server ready in each cluster it is deployed in.
func (s *Service) SetRoute(ctx context.Context, channel string, buildID int64, actor string, force bool) error {
	if !force {
		if err := s.ready(ctx, buildID); err != nil {
			return err
		}
	}
	return s.route(ctx, "route.set", channel, buildID, actor)
}

// ready refuses a build that has no game server ready to take a match in
// some cluster it is deployed in: a Fleet not made yet, game servers still
// pulling their image on new machines, or a Fleet scaled to zero. A route
// moved there would fail that region's new matches until they start. It
// reads the clusters outside any transaction.
func (s *Service) ready(ctx context.Context, buildID int64) error {
	var b store.Build
	var live []store.Deployment
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		if b, err = tx.Build(buildID); err != nil {
			return notFound(err, "构建")
		}
		live, err = liveDeploymentsOf(tx, buildID)
		return err
	})
	if err != nil {
		return err
	}
	var waiting []string
	for _, d := range live {
		site, ok := s.site(d.Cluster)
		if !ok {
			continue
		}
		fleets, err := site.Cluster.ManagedFleets(ctx)
		var rooms []cluster.Room
		if err == nil {
			rooms, err = site.Cluster.Rooms(ctx)
		}
		if err != nil {
			return &Error{ErrNotReady, fmt.Sprintf("读不到 %s 上的战斗服，没法确认 %s 在那里能接新局：%v", d.Cluster, buildName(b), err)}
		}
		if !canTakeAMatch(d, fleets, rooms) {
			waiting = append(waiting, d.Cluster)
		}
	}
	if len(waiting) > 0 {
		return &Error{ErrNotReady, fmt.Sprintf("%s 在 %s 上还没有能接新局的战斗服（刚部署，或者新机器还在下载镜像），现在切过去，那里的新对局会分配失败。等它就绪再切", buildName(b), strings.Join(waiting, "、"))}
	}
	return nil
}

// Promote points stable at the build canary uses for the compat version. The
// build must be ready as for SetRoute, unless force.
func (s *Service) Promote(ctx context.Context, game, compat, actor string, force bool) error {
	var canary store.Route
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		canary, err = tx.Route(game, Canary, compat)
		return notFound(err, "灰度路由")
	})
	if err != nil {
		return err
	}
	if !force {
		if err := s.ready(ctx, canary.BuildID); err != nil {
			return err
		}
	}
	return s.route(ctx, "route.promote", Stable, canary.BuildID, actor)
}

// Rollback points the route back at the build it used before. It does not
// wait for the build to be ready: a rollback is for when the current build
// is broken. A drained build's Fleet starts its game servers again then,
// which takes from seconds to minutes; matches fail meanwhile.
func (s *Service) Rollback(ctx context.Context, game, channel, compat, actor string) error {
	var r store.Route
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		r, err = tx.Route(game, channel, compat)
		return notFound(err, "路由")
	})
	if err != nil {
		return err
	}
	if r.PreviousBuildID == 0 {
		return conflict("这条路由之前没有指向过别的构建")
	}
	return s.route(ctx, "route.rollback", channel, r.PreviousBuildID, actor)
}

func (s *Service) route(ctx context.Context, action, channel string, buildID int64, actor string) error {
	if channel != Stable && channel != Canary {
		return invalid("通道只能是 %s 或 %s", Stable, Canary)
	}
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		b, err := tx.Build(buildID)
		if err != nil {
			return notFound(err, "构建")
		}
		live, err := liveDeploymentsOf(tx, b.ID)
		if err != nil {
			return err
		}
		if len(live) == 0 {
			return conflict("%s 没有部署在任何集群，先部署", buildName(b))
		}
		r := store.Route{Game: b.Game, Channel: channel, Compat: b.Compat, BuildID: b.ID, UpdatedBy: actor}
		old, err := tx.Route(b.Game, channel, b.Compat)
		switch {
		case err == nil && old.BuildID == b.ID:
			return nil
		case err == nil:
			r.PreviousBuildID = old.BuildID
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if r.PreviousBuildID != 0 {
			if err := s.keepsClusters(tx, b, live, r.PreviousBuildID); err != nil {
				return err
			}
		}
		if err := tx.PutRoute(r); err != nil {
			return err
		}
		detail := fmt.Sprintf("→ #%d", b.ID)
		if r.PreviousBuildID != 0 {
			detail = fmt.Sprintf("#%d → #%d", r.PreviousBuildID, b.ID)
		}
		if err := tx.Audit(actor, action, routeName(r.Game, channel, r.Compat), detail); err != nil {
			return err
		}
		if err := endCanaryLikeStable(tx, b, actor); err != nil {
			return err
		}
		return refreshStates(tx)
	})
	if err == nil {
		s.Kick()
	}
	return err
}

// keepsClusters refuses to move a route onto a build that is missing from a
// cluster the route's current build is deployed in: that region's players
// would get no game server.
func (s *Service) keepsClusters(tx *store.Tx, b store.Build, live []store.Deployment, fromBuildID int64) error {
	from, err := liveDeploymentsOf(tx, fromBuildID)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for _, d := range live {
		has[d.Cluster] = true
	}
	var missing []string
	for _, d := range from {
		if _, configured := s.site(d.Cluster); configured && !has[d.Cluster] {
			missing = append(missing, d.Cluster)
		}
	}
	if len(missing) > 0 {
		return conflict("%s 没有部署在 %s，切过去以后那里的玩家分不到战斗服。先把它部署到那里", buildName(b), strings.Join(missing, "、"))
	}
	return nil
}

// endCanaryLikeStable deletes the canary route of the build's compat version
// when it points where stable does. Such a route is no canary, yet it would
// keep testers matching only each other. So promoting a build ends its
// canary, and testers play with everyone until the next one.
func endCanaryLikeStable(tx *store.Tx, b store.Build, actor string) error {
	stable, err := tx.Route(b.Game, Stable, b.Compat)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	canary, err := tx.Route(b.Game, Canary, b.Compat)
	if errors.Is(err, store.ErrNotFound) || err == nil && canary.BuildID != stable.BuildID {
		return nil
	}
	if err != nil {
		return err
	}
	if err := tx.DeleteRoute(b.Game, Canary, b.Compat); err != nil {
		return err
	}
	return tx.Audit(actor, "route.delete", routeName(b.Game, Canary, b.Compat), fmt.Sprintf("原来是 #%d，和正式一样，灰度结束", canary.BuildID))
}

// DeleteRoute stops sending matches on the channel and compat version
// anywhere. Players on the canary channel go back to stable.
func (s *Service) DeleteRoute(ctx context.Context, game, channel, compat, actor string) error {
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		r, err := tx.Route(game, channel, compat)
		if err != nil {
			return notFound(err, "路由")
		}
		if err := tx.DeleteRoute(game, channel, compat); err != nil {
			return err
		}
		if err := refreshStates(tx); err != nil {
			return err
		}
		return tx.Audit(actor, "route.delete", routeName(game, channel, compat), fmt.Sprintf("原来是 #%d", r.BuildID))
	})
	if err == nil {
		s.Kick()
	}
	return err
}

func routeName(game, channel, compat string) string { return game + " " + channel + " " + compat }

// RouteView is a route as the game backend sees it: which compat versions
// can play on which channel, in which regions.
type RouteView struct {
	Game    string   `json:"game"`
	Channel string   `json:"channel"`
	Compat  string   `json:"compat"`
	Build   int64    `json:"build"`
	Regions []string `json:"regions"`
}

// Routes lists the routes of one game, or of every game when game is empty.
func (s *Service) Routes(ctx context.Context, game string) ([]RouteView, error) {
	views := []RouteView{}
	err := s.store.View(ctx, func(tx *store.Tx) error {
		routes, err := tx.Routes(game)
		if err != nil {
			return err
		}
		deployments, err := tx.Deployments()
		if err != nil {
			return err
		}
		for _, r := range routes {
			v := RouteView{Game: r.Game, Channel: r.Channel, Compat: r.Compat, Build: r.BuildID, Regions: []string{}}
			for _, d := range deployments {
				if site, ok := s.site(d.Cluster); ok && d.BuildID == r.BuildID && !slices.Contains(v.Regions, site.Region) {
					v.Regions = append(v.Regions, site.Region)
				}
			}
			slices.Sort(v.Regions)
			views = append(views, v)
		}
		return nil
	})
	return views, err
}

// Target is where a match goes: a Fleet in a cluster.
type Target struct {
	Cluster string
	Fleet   string
	Build   int64
}

// Resolve finds the Fleet for a new match.
func (s *Service) Resolve(ctx context.Context, game, channel, compat, region string) (Target, error) {
	var t Target
	err := s.store.View(ctx, func(tx *store.Tx) error {
		r, err := tx.Route(game, channel, compat)
		if errors.Is(err, store.ErrNotFound) {
			return ErrNoRoute
		}
		if err != nil {
			return err
		}
		live, err := liveDeploymentsOf(tx, r.BuildID)
		if err != nil {
			return err
		}
		for _, d := range live {
			if site, ok := s.site(d.Cluster); ok && site.Region == region {
				t = Target{Cluster: d.Cluster, Fleet: d.Fleet, Build: d.BuildID}
				return nil
			}
		}
		return ErrNoRoute
	})
	return t, err
}

// --- State

// routedBuilds returns the builds some route points at.
func routedBuilds(tx *store.Tx) (map[int64]bool, error) {
	routes, err := tx.Routes("")
	if err != nil {
		return nil, err
	}
	routed := map[int64]bool{}
	for _, r := range routes {
		routed[r.BuildID] = true
	}
	return routed, nil
}

func liveDeploymentsOf(tx *store.Tx, buildID int64) ([]store.Deployment, error) {
	all, err := tx.Deployments()
	if err != nil {
		return nil, err
	}
	var live []store.Deployment
	for _, d := range all {
		if d.BuildID == buildID {
			live = append(live, d)
		}
	}
	return live, nil
}

// refreshStates moves deployments between serving and draining after the
// routes changed: a routed build serves, a build that lost its last route drains.
func refreshStates(tx *store.Tx) error {
	routed, err := routedBuilds(tx)
	if err != nil {
		return err
	}
	deployments, err := tx.Deployments()
	if err != nil {
		return err
	}
	for _, d := range deployments {
		next := d.State
		switch {
		case routed[d.BuildID]:
			next = store.StateServing
		case d.State == store.StateServing:
			next = store.StateDraining
		}
		if next != d.State {
			if err := tx.SetDeploymentState(d.ID, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func notFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return &Error{ErrNotFound, "没有这个" + what}
	}
	return err
}

func printable(s string, max int, empty bool) bool {
	if s == "" {
		return empty
	}
	return len(s) <= max && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' })
}

// Record adds an entry to the audit log for something done elsewhere, such
// as a GM ending a room.
func (s *Service) Record(ctx context.Context, actor, action, target, detail string) error {
	return s.store.Update(ctx, func(tx *store.Tx) error { return tx.Audit(actor, action, target, detail) })
}

// Retention is how long a drained deployment is kept for a quick rollback.
func (s *Service) Retention() time.Duration { return s.retention }
