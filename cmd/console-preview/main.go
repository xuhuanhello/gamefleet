// Command console-preview serves the console with made-up data shaped like a
// small real cluster in the middle of a release, already signed in, for
// looking at the pages and taking screenshots. /login still shows the sign-in
// page. It only listens on loopback, and keeps its database in a temporary
// directory.
//
//	go run ./cmd/console-preview [-addr 127.0.0.1:8091]
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/clusters"
	"github.com/xuhuanhello/gamefleet/internal/config"
	"github.com/xuhuanhello/gamefleet/internal/registry"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/remote"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
	"github.com/xuhuanhello/gamefleet/internal/update"
	"github.com/xuhuanhello/gamefleet/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8091", "loopback address to listen on")
	updateState := flag.String("update", "downloading", "what the update page shows: "+strings.Join(previewUpdateStates, ", "))
	flag.Parse()
	if !slices.Contains(previewUpdateStates, *updateState) {
		log.Fatalf("-update must be one of %s", strings.Join(previewUpdateStates, ", "))
	}
	host, _, err := net.SplitHostPort(*addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		log.Fatalf("-addr must be a loopback address, got %q", *addr)
	}

	password := "preview-password"
	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Fatal(err)
	}
	secret := auth.NewTOTPSecret()
	users, err := auth.New([]config.User{{Name: "admin", PasswordHash: hash, TOTPSecret: secret}})
	if err != nil {
		log.Fatal(err)
	}
	code, _ := auth.Code(secret, time.Now())
	token, err := users.Login("admin", password, code)
	if err != nil {
		log.Fatal(err)
	}

	dir, err := os.MkdirTemp("", "console-preview-")
	if err != nil {
		log.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "gamefleet.db"))
	if err != nil {
		log.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fake := fakeCluster(time.Now())
	directory := sites.New()
	keys := auth.NewKeys(db)
	connect := func(store.Cluster) (cluster.API, error) { return fake, nil }
	fleets := clusters.New(db, directory, keys, connect, "fleet.example.com", logger)
	releases := release.New(db, images, "fleet.example.com", directory, 24*time.Hour, logger)
	if err := history(db, releases, keys, fake, fleets); err != nil {
		log.Fatal(err)
	}
	console, err := web.New(web.Options{
		Host: "fleet.example.com", Sites: directory, Clusters: fleets, Auth: users, Keys: keys,
		Store: db, Releases: releases, Updates: previewUpdates{*updateState}, Log: logger,
	})
	if err != nil {
		log.Fatal(err)
	}
	go releases.Run(context.Background())
	go fleets.Run(context.Background())
	pages := console.Handler()
	log.Printf("console preview on http://%s/", *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			r.AddCookie(&http.Cookie{Name: web.SessionCookie, Value: token})
		}
		pages.ServeHTTP(w, r)
	})))
}

const (
	manager = "vm-0-4-debian"
	worker  = "vm-0-3-debian"
	compat  = "dm-v7-closed-frame-v3-cached-0"
	matchID = "1208c143-4262-41c5-9640-c628f9ca689a"
	// A match that started on the previous build and is still running.
	oldMatch = "9d0e66f1-5b0a-4f8e-a6f3-2c7d14e0b9aa"
)

func digest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// images holds three CI builds of billiards: runs 3, 4 and 5.
var images = previewRegistry{
	"3": image(digest("3"), "3", "4f1c2e9a7b6d5c3e1f0a9b8c7d6e5f4a3b2c1d0e"),
	"4": image(digest("4"), "4", "a87d3c1e5f2b4a6d8c0e1f3a5b7d9c2e4f6a8b0c"),
	"5": image(digest("5"), "5", "c03b5e7d9f1a3c5e7b9d1f3a5c7e9b1d3f5a7c9e"),
}

func image(d, version, revision string) registry.Image {
	return registry.Image{Digest: d, Port: 7770, Protocol: "UDP", Labels: map[string]string{
		release.LabelCompat: compat, release.LabelVersion: version, release.LabelRevision: revision,
		"io.gamefleet.physics": "event-contact-v7-closed-frame-v3-cached-window-0",
		"io.gamefleet.unity":   "2022.3.62f3", "io.gamefleet.netcode": "fishnet",
	}}
}

type previewRegistry map[string]registry.Image

func (p previewRegistry) Inspect(_ context.Context, _, ref string) (registry.Image, error) {
	for tag, img := range p {
		if ref == tag || ref == img.Digest {
			return img, nil
		}
	}
	return registry.Image{}, registry.ErrNotFound
}

// history replays a release: build 3 went out, build 4 was tested on canary
// and promoted, and build 5 is on canary now. A match that started on build 3
// is still running, so build 3 has not been reclaimed yet.
func history(db *store.Store, releases *release.Service, keys *auth.Keys, fake *clustertest.Fake, fleets *clusters.Service) error {
	ctx := context.Background()
	start := time.Now()
	at := start.Add(-72 * time.Hour)
	db.SetClock(func() time.Time { return at })
	ca := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	if err := fleets.Add(ctx, clusters.Connection{Name: "us-1", Region: "us", Server: "https://10.4.0.4:6443", CABase64: ca,
		Token: "preview", Namespace: clusters.Namespace, JoinURL: "https://10.4.0.4:6443",
		JoinToken: "preview-join-token-not-real"}, "gamefleet cluster"); err != nil {
		return err
	}
	for name, value := range map[string]string{"NAKAMA_URL": "https://api.example.com", "NAKAMA_HTTP_KEY": "preview",
		"TICKET_PUBLIC_KEYS": "k1:preview"} {
		if err := fleets.SetVar(ctx, "billiards", name, value, "admin"); err != nil {
			return err
		}
	}
	step := func(d time.Duration) { at = at.Add(d) }
	for _, k := range []struct {
		name  string
		scope string
	}{{"ci", auth.ScopePush}, {"nakama", auth.ScopeAllocate}} {
		if _, err := keys.Create(ctx, k.name, []string{k.scope}, "admin"); err != nil {
			return err
		}
	}
	deploy := func(build int64) error {
		_, err := releases.Deploy(ctx, release.DeployRequest{BuildID: build, Cluster: "us-1", Buffer: 1, MaxReplicas: 10, Rooms: 50,
			Resources: release.Defaults.Resources}, "admin")
		if err != nil {
			return err
		}
		return releases.Reconcile(ctx)
	}
	for i, tag := range []string{"3", "4", "5"} {
		step(time.Duration(10+i*7) * time.Hour)
		if _, _, err := releases.Register(ctx, release.Registration{Repository: "billiards", Tag: tag}, "key:ci"); err != nil {
			return err
		}
		step(20 * time.Minute)
		if err := deploy(int64(i + 1)); err != nil {
			return err
		}
		step(5 * time.Minute)
		channel := release.Canary
		if i == 0 {
			channel = release.Stable
		}
		if err := releases.SetRoute(ctx, channel, int64(i+1), "admin", true); err != nil {
			return err
		}
		if i == 1 {
			step(3 * time.Hour)
			if err := releases.Promote(ctx, "billiards", compat, "admin", true); err != nil {
				return err
			}
			fake.SetAllocated("billiards-1", 1)
			if err := releases.Reconcile(ctx); err != nil {
				return err
			}
		}
	}
	fake.SetAllocated("billiards-2", 1)
	db.SetClock(time.Now)
	if err := releases.Reconcile(ctx); err != nil {
		return err
	}
	// Singapore is on its way in: GameFleet is installing it over SSH and
	// the install script has got to Agones.
	fleets.SetRunner(func(ctx context.Context, t remote.Target, command string, out io.Writer) (string, error) {
		code := command[strings.LastIndex(command, " ")+1:]
		for _, line := range []string{
			"GameFleet：在 " + t.Host + " 上安装集群 sg-1（地区 sg）",
			"GameFleet：下载并安装 K3s " + clusters.K3sVersion,
			"GameFleet：等 K3s 启动",
			"GameFleet：安装 Agones " + clusters.AgonesVersion,
			"GameFleet：下载并安装 Agones 的安装包",
			"GameFleet：下载 Agones 的镜像",
			"GameFleet：下载好了镜像 agones-extensions:" + clusters.AgonesVersion + "：31 MB，用了 9.68s",
		} {
			fmt.Fprintln(out, line)
		}
		fleets.Report(ctx, code, t.Host, clusters.Report{Stage: clusters.StageK3s, Step: "等 K3s 启动", Part: 1, Parts: 2})
		fleets.Report(ctx, code, t.Host, clusters.Report{Stage: clusters.StageAgones, Step: "下载 Agones 的镜像", Part: 1, Parts: 3})
		<-ctx.Done()
		return "SHA256:Qm8l2s6tYwP0xk7c3zRrJm9eN4aHq1vB5uLdT2fG8sE", ctx.Err()
	})
	j, err := fleets.StartRemoteJoin(ctx, "sg-1", "sg", false, remote.Target{Host: "203.0.113.30", Password: "preview"}, "admin")
	if err != nil {
		return err
	}
	log.Printf("join in progress: /clusters/new?join=%s", j.ID)
	return nil
}

func imageOf(tag string) string { return "fleet.example.com/billiards@" + images[tag].Digest }

func fakeCluster(now time.Time) *clustertest.Fake {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour
	fake := clustertest.New("us-1", 0)
	system := func(ns, name, owner string, cpu, memMi int64) cluster.Pod {
		return cluster.Pod{Namespace: ns, Name: name, Node: manager, Owner: owner, Status: "Running", Ready: 1,
			Containers: []string{strings.Split(name, "-")[0]}, Requests: cluster.Resources{MilliCPU: cpu, Memory: memMi << 20},
			Created: ago(6*day + 15*time.Hour)}
	}
	game := func(name string, created time.Time) cluster.Pod {
		return cluster.Pod{Namespace: clustertest.Namespace, Name: name, Node: worker, Owner: "GameServer", Status: "Running",
			Ready: 2, Containers: []string{"billiards", "agones-gameserver-sidecar"},
			Requests: cluster.Resources{MilliCPU: 280, Memory: 256 << 20}, Created: created}
	}
	var log strings.Builder
	for i := range 60 {
		fmt.Fprintf(&log, `DM_MEMORY {"utc":%d,"processAgeSeconds":%d,"rssBytes":168079360,"allocated":true,"active":true}`+"\n",
			ago(10*time.Minute).UnixMilli()+int64(i)*10000, 600+i*10)
		log.WriteString("DM.Net.Online.DedicatedServer:ObserveMemory()\nDM.Net.Online.DedicatedServer:Update()\n\n")
		if i == 3 {
			fmt.Fprintf(&log, "DM_ALLOCATED match=%s physics=event-contact-v7-closed-frame-v3-cached-window-0\n", matchID)
		}
	}
	fake.SetView(clustertest.View{
		Nodes: []cluster.Node{
			{Name: manager, Ready: true, Roles: []string{"control-plane", "master"}, InternalIP: "10.4.0.4",
				Kubelet: "v1.35.8+k3s1", OS: "Debian GNU/Linux 13 (trixie)", Capacity: cluster.Resources{MilliCPU: 2000, Memory: 3900 << 20},
				Allocatable: cluster.Resources{MilliCPU: 2000, Memory: 3800 << 20},
				Given:       "us-west-ctrl", Created: ago(6*day + 15*time.Hour)},
			{Name: worker, Ready: true, InternalIP: "10.4.0.3", ExternalIP: "203.0.113.20",
				Kubelet: "v1.35.8+k3s1", OS: "Debian GNU/Linux 13 (trixie)", Capacity: cluster.Resources{MilliCPU: 2000, Memory: 1990 << 20},
				Allocatable: cluster.Resources{MilliCPU: 2000, Memory: 1940 << 20},
				Given:       "us-worker-1", Created: ago(6*day + 15*time.Hour)},
		},
		Pods: []cluster.Pod{
			system("agones-system", "agones-controller-9c49bcbdc-c4vwx", "ReplicaSet", 0, 0),
			system("agones-system", "agones-extensions-5645847598-7pbqz", "ReplicaSet", 0, 0),
			system("kube-system", "coredns-c5fdd76cf-m5ph7", "ReplicaSet", 100, 70),
			system("kube-system", "local-path-provisioner-d9d8769b6-vjll2", "ReplicaSet", 0, 0),
			system("kube-system", "metrics-server-7f4b6d9bd7-99xk4", "ReplicaSet", 100, 70),
			game("billiards-2-wrw85-jnscl", ago(25*time.Minute)),
			game("billiards-2-wrw85-shx6v", ago(13*time.Hour)),
			game("billiards-2-wrw85-k8p2d", ago(13*time.Hour)),
			game("billiards-1-q7r2m-x5n4c", ago(day)),
			game("billiards-3-hp4tz-2mf8s", ago(9*time.Hour)),
			game("billiards-3-hp4tz-9kqrw", ago(9*time.Hour)),
		},
		Fleets: []cluster.Fleet{
			{Name: "billiards-1", Image: imageOf("3"), Replicas: 1, Allocated: 1, Created: ago(3 * day)},
			{Name: "billiards-2", Image: imageOf("4"), Replicas: 3, Ready: 2, Allocated: 1, Created: ago(2 * day)},
			{Name: "billiards-3", Image: imageOf("5"), Replicas: 2, Ready: 2, Created: ago(day)},
		},
		Events: []cluster.Event{
			{Namespace: clustertest.Namespace, Kind: "GameServer", Name: "billiards-2-wrw85-jnscl", Type: "Normal", Reason: "Allocated",
				Message: "Allocated", Count: 1, Last: ago(10 * time.Minute)},
			{Namespace: clustertest.Namespace, Kind: "Pod", Name: "billiards-2-wrw85-hlgvd", Type: "Warning", Reason: "Unhealthy",
				Message: `Liveness probe failed: Get "http://10.42.1.111:8080/healthz": dial tcp 10.42.1.111:8080: connect: connection refused`,
				Count:   1, Last: ago(25 * time.Minute)},
			{Namespace: clustertest.Namespace, Kind: "GameServer", Name: "billiards-2-wrw85-jnscl", Type: "Normal", Reason: "Ready",
				Message: "SDK.Ready() complete", Count: 1, Last: ago(25 * time.Minute)},
			{Namespace: clustertest.Namespace, Kind: "Pod", Name: "billiards-2-wrw85-jnscl", Type: "Normal", Reason: "Started",
				Message: "Container started", Count: 2, Last: ago(25 * time.Minute)},
		},
		Logs: map[string]string{
			clustertest.Namespace + "/billiards-2-wrw85-jnscl/billiards":                 log.String(),
			clustertest.Namespace + "/billiards-2-wrw85-jnscl/agones-gameserver-sidecar": "{\"message\":\"Starting SDKServer grpc service...\",\"severity\":\"info\"}\n",
		},
		Usage: cluster.Usage{
			Nodes: map[string]cluster.Resources{manager: {MilliCPU: 70, Memory: 1942 << 20}, worker: {MilliCPU: 85, Memory: 938 << 20}},
			Pods: map[string]cluster.Resources{
				clustertest.Namespace + "/billiards-2-wrw85-jnscl": {MilliCPU: 21, Memory: 123 << 20},
				clustertest.Namespace + "/billiards-2-wrw85-shx6v": {MilliCPU: 3, Memory: 164 << 20},
				"kube-system/coredns-c5fdd76cf-m5ph7":              {MilliCPU: 2, Memory: 18 << 20},
			},
		},
	})
	// Players of the preview's matches: two made-up user IDs per match.
	players := func(n int) json.RawMessage {
		raw, _ := json.Marshal([]map[string]any{
			{"userId": fmt.Sprintf("%08x-ae18-4ec3-83b7-9d64a95b2ac1", 0x4ef51ac5+n), "seat": 0},
			{"userId": fmt.Sprintf("%08x-95a3-4e78-81b4-e72506542dec", 0x68f1a152+n), "seat": 1},
		})
		return raw
	}
	port := 20137
	room := func(id, build, state string, created time.Time) cluster.Room {
		port += 211
		return cluster.Room{ID: id, State: state, Address: "203.0.113.20", Port: port, Capacity: 50,
			Fleet: id[:strings.LastIndex(id[:strings.LastIndex(id, "-")], "-")], Build: build, Node: worker, Game: "billiards",
			Image: imageOf(map[string]string{"1": "3", "2": "4", "3": "5"}[build]), Created: created}
	}
	// with gives the game server n matches, the first one with the given ID,
	// started minutes apart up to first ago.
	n := 0
	with := func(r cluster.Room, first string, count int, firstAgo time.Duration) cluster.Room {
		for i := range count {
			id := first
			if i > 0 || first == "" {
				n++
				id = fmt.Sprintf("%08x-%04x-4%03x-a%03x-%012x", 0x2b7c0d11*n%0xffffffff, n*37%0xffff, n*91%0xfff, n*53%0xfff, n*7919)
			}
			channel := "stable"
			if r.Build == "3" {
				channel = "canary"
			}
			r.Matches = append(r.Matches, cluster.Match{ID: id, MatchInfo: cluster.MatchInfo{
				Players: players(n), Channel: channel, Compat: compat, Allocated: ago(firstAgo - time.Duration(i)*firstAgo/time.Duration(count+1)),
			}})
		}
		r.State = "Allocated"
		return r
	}
	fake.AddRoom(with(room("billiards-2-wrw85-jnscl", "2", "Ready", ago(25*time.Minute)), matchID, 23, 10*time.Minute))
	fake.AddRoom(with(room("billiards-2-wrw85-shx6v", "2", "Ready", ago(13*time.Hour)), "", 0, 0))
	fake.AddRoom(with(room("billiards-1-q7r2m-x5n4c", "1", "Ready", ago(day)), oldMatch, 1, 40*time.Minute))
	fake.AddRoom(with(room("billiards-3-hp4tz-2mf8s", "3", "Ready", ago(9*time.Hour)), "", 4, 30*time.Minute))
	fake.AddRoom(room("billiards-2-wrw85-k8p2d", "2", "Ready", ago(13*time.Hour)))
	fake.AddRoom(room("billiards-3-hp4tz-9kqrw", "3", "Ready", ago(9*time.Hour)))
	return fake
}

// previewUpdateStates are the states of the update page the preview can show.
var previewUpdateStates = []string{"latest", "applied", "check-failed", "available", "downloading", "download-failed",
	"downloaded", "applying", "apply-failed", "no-updater", "dev"}

// previewUpdates is GameFleet in one of previewUpdateStates; in most of them
// it is v2.0.0 with v2.1.0 out.
type previewUpdates struct{ state string }

var previewRelease = &update.Release{
	Version: "2.1.0", Name: "GameFleet v2.1.0", URL: "https://github.com/xuhuanhello/gamefleet/releases/tag/v2.1.0",
	Published: time.Now().Add(-26 * time.Hour),
	Notes:     "- 路由页可以停止服务一个联机版本，旧客户端会被提示更新\n- 总览里每个构建下面写着它的联机版本\n- 修复：晋升以后测试账号仍被隔离在灰度通道",
}

func (u previewUpdates) Current() string {
	switch u.state {
	case "dev":
		return "dev"
	case "latest", "applied", "check-failed":
		return "2.1.0"
	}
	return "2.0.0"
}
func (u previewUpdates) Released() bool                                    { return u.state != "dev" }
func (u previewUpdates) HasUpdater() bool                                  { return u.state != "no-updater" }
func (previewUpdates) Download(string) error                               { return nil }
func (previewUpdates) Apply(context.Context, string) error                 { return nil }
func (previewUpdates) CheckSoon(time.Duration)                             {}
func (u previewUpdates) Check(context.Context, time.Duration) update.Check { return u.Checked() }
func (u previewUpdates) Checked() update.Check {
	switch u.state {
	case "dev":
		return update.Check{}
	case "check-failed":
		return update.Check{Latest: previewRelease, At: time.Now().Add(-3 * time.Second), Err: errors.New("连不上 GitHub")}
	}
	return update.Check{Latest: previewRelease, At: time.Now().Add(-3 * time.Second)}
}
func (u previewUpdates) Available() *update.Release {
	if update.Newer(previewRelease.Version, u.Current()) {
		return previewRelease
	}
	return nil
}
func (u previewUpdates) Status() (update.Status, error) {
	at := time.Now().Add(-time.Minute)
	st := update.Status{Action: "download", Version: "2.1.0", At: at}
	switch u.state {
	case "downloading":
		st.State, st.Done, st.Total = update.Downloading, 31<<20, 50<<20
	case "download-failed":
		st.State, st.Error = update.Failed, "镜像没下载下来，原因在管理机的 /opt/gamefleet/update/pull.jsonl 里"
	case "downloaded":
		st.State = update.Downloaded
	case "applying":
		st.Action, st.State = "apply", update.Applying
	case "apply-failed":
		st.Action, st.State, st.Error = "apply", update.Failed, "新版本没有启动起来，已经回到原来的版本。经过见管理机的 /opt/gamefleet/update/apply.log"
	case "applied":
		st.Action, st.State = "apply", update.Applied
	default:
		return update.Status{}, nil
	}
	return st, nil
}
