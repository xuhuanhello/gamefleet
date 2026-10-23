package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/clusters"
	"github.com/xuhuanhello/gamefleet/internal/remote"
)

func TestJoinAClusterFromTheConsole(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/clusters/new")
	if page := visible(body); !strings.Contains(page, "接入集群") || !strings.Contains(page, "开始安装") || !strings.Contains(page, "只要命令，我自己执行") {
		t.Fatalf("form:\n%s", page)
	}
	resp, body := e.post(t, "/clusters/new", url.Values{"name": {"sg-1"}, "region": {"us"}, "mode": {"manual"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(visible(body), "地区 us 已经由集群 us-1 服务") {
		t.Fatalf("taken region: %d\n%s", resp.StatusCode, visible(body))
	}
	resp, _ = e.post(t, "/clusters/new", url.Values{"name": {"sg-1"}, "region": {"sg"}, "mode": {"manual"}})
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(where, "/clusters/new?join=") {
		t.Fatalf("start: %d %s", resp.StatusCode, where)
	}
	_, body = e.get(t, where)
	code := regexp.MustCompile(`--code ([a-z2-7]+)`).FindStringSubmatch(body)
	if code == nil {
		t.Fatalf("no install command:\n%s", visible(body))
	}
	page := visible(body)
	for _, want := range []string{"接入 sg-1", "curl -sfL https://fleet.example.com/install/cluster.sh | sh -s -- --code", "TCP 6443",
		"UDP 20000-20999", "在新机器上运行命令", "加入码 59m"} {
		if !strings.Contains(page, want) {
			t.Errorf("join page lacks %q\n%s", want, page)
		}
	}
	if !strings.Contains(body, `data-refresh="3"`) {
		t.Error("the join page does not follow the script live")
	}

	// The install script reports as it goes.
	ctx := context.Background()
	if err := e.clusters.Report(ctx, code[1], "203.0.113.7", clusters.Report{Stage: clusters.StageAgones}); err != nil {
		t.Fatal(err)
	}
	_, body = e.get(t, where)
	if !regexp.MustCompile(`<li class="done">.*安装 K3s</span></li>\s*<li class="now">.*安装 Agones</span></li>`).MatchString(body) ||
		!strings.Contains(body, `<progress max="5" value="2">`) ||
		!strings.Contains(visible(body), "新机器 203.0.113.7") {
		t.Fatalf("progress:\n%s", body)
	}
	ca := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	err := e.clusters.Report(ctx, code[1], "203.0.113.7", clusters.Report{Stage: clusters.StageConnect, Connection: &clusters.Connection{
		Name: "sg-1", Region: "sg", Server: "https://203.0.113.7:6443", CABase64: ca, Token: "t", Namespace: clusters.Namespace,
		JoinURL: "https://203.0.113.7:6443", JoinToken: "agent-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	_, body = e.get(t, where)
	if page := visible(body); !strings.Contains(page, "sg-1 已经接入") || strings.Contains(body, "data-refresh") {
		t.Fatalf("done:\n%s", page)
	}
	// The new cluster is in the menu, and its page leads to adding machines,
	// which tells how machines join it.
	_, body = e.get(t, "/clusters/sg-1")
	if !strings.Contains(body, `href="/clusters/sg-1"`) || !strings.Contains(body, `href="/clusters/sg-1/nodes/new"`) {
		t.Fatalf("cluster page:\n%s", visible(body))
	}
	_, body = e.get(t, "/clusters/sg-1/nodes/new")
	if !strings.Contains(visible(body), "install/node.sh | sh -s -- --server https://203.0.113.7:6443 --token agent-secret") {
		t.Fatalf("add machine page:\n%s", visible(body))
	}
}

func TestRetireAMachineFromTheConsole(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/clusters/us-1/nodes/"+manager)
	if strings.Contains(body, "下线这台机器") {
		t.Error("the control plane offers to retire")
	}
	_, body = e.get(t, "/clusters/us-1/nodes/"+worker)
	if !strings.Contains(body, "下线这台机器") || !strings.Contains(body, "data-confirm=") {
		t.Fatal("the worker cannot be retired")
	}
	e.expectRedirect(t, "/clusters/us-1/nodes/"+worker+"/retire", nil, "/clusters/us-1/nodes/"+worker)
	_, body = e.get(t, "/clusters/us-1/nodes/"+worker)
	page := visible(body)
	if !strings.Contains(page, "正在下线") || !strings.Contains(page, "还有 1 局在打") || !strings.Contains(page, "取消下线") ||
		strings.Contains(page, "下线这台机器") {
		t.Fatalf("retiring:\n%s", page)
	}
	_, body = e.get(t, "/clusters/us-1")
	if !strings.Contains(visible(body), "下线中") {
		t.Error("the cluster page does not show the retiring machine")
	}
	e.expectRedirect(t, "/clusters/us-1/nodes/"+worker+"/keep", nil, "/clusters/us-1/nodes/"+worker)
	if _, body = e.get(t, "/clusters/us-1/nodes/"+worker); strings.Contains(visible(body), "正在下线") {
		t.Error("keeping did not cancel the retirement")
	}
	resp, body := e.post(t, "/clusters/us-1/nodes/"+manager+"/retire", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(visible(body), "是控制面，不能下线") {
		t.Errorf("retiring the control plane: %d %s", resp.StatusCode, visible(body))
	}
}

func TestRuntimeSettingsHideValues(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	e.expectRedirect(t, "/settings/runtime", url.Values{"game": {"billiards"}, "name": {"NAKAMA_HTTP_KEY"}, "value": {"very-secret-value"}},
		"/settings/runtime")
	_, body := e.get(t, "/settings/runtime")
	page := visible(body)
	if !strings.Contains(page, "billiards") || !strings.Contains(page, "NAKAMA_HTTP_KEY 已设置，不再显示") ||
		strings.Contains(body, "very-secret-value") {
		t.Fatalf("runtime page:\n%s", page)
	}
	resp, body := e.post(t, "/settings/runtime", url.Values{"game": {"billiards"}, "name": {"lower"}, "value": {"x"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(visible(body), "变量名只能用大写字母") {
		t.Errorf("bad name: %d %s", resp.StatusCode, visible(body))
	}
	e.expectRedirect(t, "/settings/runtime/delete", url.Values{"game": {"billiards"}, "name": {"NAKAMA_HTTP_KEY"}}, "/settings/runtime")
	if _, body = e.get(t, "/settings/runtime"); strings.Contains(body, "NAKAMA_HTTP_KEY") {
		t.Error("deleted variable still listed")
	}
	_, body = e.get(t, "/settings/audit")
	if page := visible(body); !strings.Contains(page, "admin 设置运行时配置 billiards NAKAMA_HTTP_KEY") {
		t.Errorf("audit:\n%s", page)
	}
}

func TestInstallAClusterOverSSHFromTheConsole(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	release := make(chan struct{})
	e.clusters.SetRunner(func(_ context.Context, target remote.Target, command string, out io.Writer) (string, error) {
		fmt.Fprintln(out, "GameFleet：在 "+target.Host+" 上安装集群 sg-1（地区 sg）")
		code := command[strings.LastIndex(command, " ")+1:]
		e.clusters.Report(context.Background(), code, target.Host,
			clusters.Report{Stage: clusters.StageAgones, Step: "下载 Agones 的镜像", Part: 1, Parts: 3})
		<-release
		fmt.Fprintln(out, "GameFleet 停下了：Agones 15 分钟还没装好")
		return "SHA256:abcdef", &remote.Error{Text: "命令在机器上失败了（退出码 1），原因见日志", Status: 1}
	})
	resp, body := e.post(t, "/clusters/new", url.Values{"name": {"sg-1"}, "region": {"sg"}, "host": {"203.0.113.7"}, "user": {"root"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(visible(body), "请填密码或者私钥") ||
		!strings.Contains(body, `value="203.0.113.7"`) {
		t.Fatalf("no password: %d\n%s", resp.StatusCode, visible(body))
	}
	resp, _ = e.post(t, "/clusters/new", url.Values{"name": {"sg-1"}, "region": {"sg"}, "host": {"203.0.113.7"}, "user": {"root"},
		"password": {"s3cret-root-password"}})
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	waitFor(t, func() bool { _, body := e.get(t, where); return strings.Contains(body, "上安装集群 sg-1") })
	_, body = e.get(t, where)
	page := visible(body)
	for _, want := range []string{"GameFleet 通过 SSH 在 root@203.0.113.7 上安装", "登录机器", "安装日志", "TCP 22", "也可以自己在那台机器上用 root 执行",
		"下载 Agones 的镜像 2 / 3 已用 0s"} {
		if !strings.Contains(page, want) {
			t.Errorf("progress page lacks %q\n%s", want, page)
		}
	}
	// Two stages done and a third of Agones.
	if !strings.Contains(body, `<progress max="5" value="2.33`) {
		t.Errorf("bar does not show the step: %s", body[strings.Index(body, "<progress"):][:60])
	}
	if strings.Contains(body, "s3cret-root-password") {
		t.Fatal("the password is in the page")
	}
	close(release)
	waitFor(t, func() bool { _, body := e.get(t, where); return strings.Contains(body, "退出码 1") })
	_, body = e.get(t, where)
	if page := visible(body); !strings.Contains(page, "Agones 15 分钟还没装好（退出码 1）") || !strings.Contains(page, "主机指纹 SHA256:abcdef") ||
		!strings.Contains(body, "/retry") || strings.Contains(body, "data-refresh") || strings.Contains(page, "已用") {
		t.Fatalf("failed page:\n%s", visible(body))
	}
	_, body = e.get(t, "/settings/audit")
	if page := visible(body); !strings.Contains(page, "通过 SSH 在 root@203.0.113.7 上安装，失败") || strings.Contains(body, "s3cret") {
		t.Errorf("audit:\n%s", page)
	}
}

func TestAddAMachineOverSSHFromTheConsole(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	ca := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	if err := e.clusters.Add(context.Background(), clusters.Connection{Name: "eu-1", Region: "eu", Server: "https://203.0.113.20:6443",
		CABase64: ca, Token: "t", Namespace: clusters.Namespace, JoinURL: "https://203.0.113.20:6443", JoinToken: "jt"}, "admin"); err != nil {
		t.Fatal(err)
	}
	site, _ := e.dir.Get("eu-1")
	eu := site.Cluster.(*clustertest.Fake)
	e.clusters.SetRunner(func(_ context.Context, target remote.Target, command string, out io.Writer) (string, error) {
		fmt.Fprintln(out, "GameFleet：以 vm-1（"+target.Host+"）的身份加入 https://203.0.113.20:6443")
		view := eu.View()
		view.Nodes = append(view.Nodes, cluster.Node{Name: "vm-1", Ready: true, ExternalIP: target.Host})
		eu.SetView(view)
		return "SHA256:xyz", nil
	})
	_, body := e.get(t, "/clusters/eu-1/nodes/new")
	if !strings.Contains(visible(body), "开始加入") || !strings.Contains(body, "install/node.sh | sh -s -- --server https://203.0.113.20:6443 --token jt") {
		t.Fatalf("add machine page:\n%s", visible(body))
	}
	resp, _ := e.post(t, "/clusters/eu-1/nodes/new", url.Values{"host": {"203.0.113.21"}, "user": {"root"}, "password": {"pw-of-the-vm"}})
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(where, "/clusters/eu-1/nodes/new?job=") {
		t.Fatalf("start: %d %s", resp.StatusCode, where)
	}
	waitFor(t, func() bool { _, body := e.get(t, where); return strings.Contains(body, "vm-1 已经加入 eu-1") })
	_, body = e.get(t, where)
	if !strings.Contains(body, `<progress max="4" value="4">`) || strings.Contains(body, "pw-of-the-vm") || !strings.Contains(visible(body), "SHA256:xyz") {
		t.Fatalf("done page:\n%s", visible(body))
	}
	resp, body = e.post(t, "/clusters/eu-1/nodes/new", url.Values{"host": {"203.0.113.21"}, "password": {"pw"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(visible(body), "已经是集群里的 vm-1 了") {
		t.Errorf("same machine twice: %d %s", resp.StatusCode, visible(body))
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// A cluster can keep its first machine for the control plane; the command
// says so, and the page says the next step is a machine for game servers.
func TestJoinAControlPlaneOnlyCluster(t *testing.T) {
	e := newEnv(t)
	e.signIn(t)
	_, body := e.get(t, "/clusters/new")
	contains(t, body, `name="controlPlaneOnly"`)
	resp, _ := e.post(t, "/clusters/new", url.Values{"name": {"eu-1"}, "region": {"eu"}, "controlPlaneOnly": {"1"}, "mode": {"manual"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	_, body = e.get(t, resp.Header.Get("Location"))
	contains(t, visible(body), "地区 eu · 只做控制面", "--control-plane-only")
	// A refused form keeps the box ticked.
	resp, body = e.post(t, "/clusters/new", url.Values{"name": {"EU 2"}, "region": {"eu"}, "controlPlaneOnly": {"1"}, "mode": {"manual"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cluster name: %d", resp.StatusCode)
	}
	contains(t, body, `value="1" checked`)
}
