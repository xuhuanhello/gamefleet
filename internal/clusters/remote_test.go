package clusters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/cluster/clustertest"
	"github.com/xuhuanhello/gamefleet/internal/remote"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// script plays the install scripts on the far side of SSH.
type script struct {
	mu       sync.Mutex
	calls    []string
	failNext error
	act      func(command string) // what the machine does when the command runs
}

func (s *script) run(_ context.Context, t remote.Target, command string, out io.Writer) (string, error) {
	s.mu.Lock()
	if t.HostKey != testHostKey.Fingerprint {
		s.mu.Unlock()
		return "", fmt.Errorf("logged in with host key %q, not the one checked", t.HostKey)
	}
	s.calls = append(s.calls, t.User+"@"+t.Host+" "+command)
	fail := s.failNext
	s.failNext = nil
	s.mu.Unlock()
	fmt.Fprintln(out, "GameFleet：开始")
	if fail != nil {
		return "SHA256:host", fail
	}
	if s.act != nil {
		s.act(command)
	}
	fmt.Fprintln(out, "GameFleet：好了")
	return "SHA256:host", nil
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestInstallAClusterOverSSH(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sc := &script{failNext: &remote.Error{Text: "连不上 203.0.113.7:22"}}
	f.svc.SetRunner(sc.run)
	sc.act = func(command string) {
		code := command[strings.Index(command, "--code ")+len("--code "):]
		for _, stage := range []string{StageK3s, StageAgones} {
			f.svc.Report(ctx, code, "203.0.113.7", Report{Stage: stage})
		}
		f.svc.Report(ctx, code, "203.0.113.7", Report{Stage: StageConnect, Connection: connection("sg-1", "sg")})
	}

	if _, err := f.svc.StartRemoteJoin(ctx, "sg-1", "sg", false, remote.Target{Host: "203.0.113.7"}, "admin"); err == nil {
		t.Fatal("a login without a password or key was accepted")
	}
	j, err := f.svc.StartRemoteJoin(ctx, "sg-1", "sg", false, remote.Target{Host: "203.0.113.7", Password: "pw"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// GameFleet logs in only once the operator checked the host key.
	if j.Remote.State != RemoteChecking || j.Remote.Fingerprint != "SHA256:host" || j.Remote.KeyFile != testHostKey.File || len(sc.calls) != 0 {
		t.Fatalf("before the check: %+v, calls %v", j.Remote, sc.calls)
	}
	if err := f.svc.RetryJoin(j.ID); err == nil {
		t.Fatal("ran before the host key was checked")
	}
	if err := f.svc.ConfirmJoin(j.ID, "SHA256:other"); err == nil {
		t.Fatal("confirmed another host key")
	}
	if err := f.svc.ConfirmJoin(j.ID, "SHA256:host"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ConfirmJoin(j.ID, "SHA256:host"); err == nil {
		t.Fatal("confirmed twice")
	}
	waitFor(t, "the first run to fail", func() bool { got, _ := f.svc.JoinByID(j.ID); return got.Remote.State == RemoteFailed })
	got, _ := f.svc.JoinByID(j.ID)
	if !strings.Contains(got.Remote.Error, "连不上") || got.Remote.Fingerprint != "SHA256:host" || got.Remote.Log[0] != "GameFleet：开始" {
		t.Fatalf("failed run = %+v", got.Remote)
	}

	// The retry uses the login given before.
	if err := f.svc.RetryJoin(j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cluster to join", func() bool { got, _ := f.svc.JoinByID(j.ID); return got.Stage == StageDone && !got.Remote.Busy() })
	got, _ = f.svc.JoinByID(j.ID)
	if got.Remote.State != RemoteFinished || len(sc.calls) != 2 {
		t.Fatalf("join = %+v, calls %v", got.Remote, sc.calls)
	}
	if !strings.HasPrefix(sc.calls[1], "root@203.0.113.7 command -v curl") || !strings.Contains(sc.calls[1], "install/cluster.sh | sh -s -- --code "+j.Code) {
		t.Errorf("command = %s", sc.calls[1])
	}
	if _, ok := f.dir.Get("sg-1"); !ok {
		t.Fatal("sg-1 not added")
	}
	// The login is gone once the cluster has joined.
	if err := f.svc.RetryJoin(j.ID); err == nil || !strings.Contains(err.Error(), "登录信息已经丢了") {
		t.Errorf("retry after done: %v", err)
	}
	f.svc.mu.Lock()
	kept := f.svc.joins[j.ID].target
	f.svc.mu.Unlock()
	if kept != nil {
		t.Error("the login outlived the join")
	}
}

func TestAddAMachineOverSSH(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.fakes["us-1"] = clustertest.New("us-1", 0)
	us := f.fakes["us-1"]
	us.SetView(clustertest.View{Nodes: []cluster.Node{{Name: "mgr", Roles: []string{"control-plane"}, InternalIP: "10.0.0.4"}}})
	if err := f.svc.Add(ctx, *connection("us-1", "us"), "admin"); err != nil {
		t.Fatal(err)
	}
	sc := &script{}
	f.svc.SetRunner(sc.run)
	sc.act = func(string) {
		view := us.View()
		view.Nodes = append(view.Nodes, cluster.Node{Name: "vm-0-3-debian", InternalIP: "10.0.0.3", ExternalIP: "203.0.113.9"})
		us.SetView(view)
	}
	if _, err := f.svc.StartNodeJoin(ctx, "us-1", remote.Target{Host: "10.0.0.4", Password: "pw"}, "", "admin"); err == nil {
		t.Error("a machine already in the cluster was accepted")
	}
	if _, err := f.svc.StartNodeJoin(ctx, "us-1", remote.Target{Host: "203.0.113.9", Password: "pw"}, "a\nb", "admin"); err == nil {
		t.Error("a name with a line break was accepted")
	}
	job, err := f.svc.StartNodeJoin(ctx, "us-1", remote.Target{Host: "203.0.113.9", User: "debian", Password: "pw"}, " 美西 2 号机 ", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if job.Stage != NodeChecking || job.Remote.Fingerprint != "SHA256:host" || len(sc.calls) != 0 {
		t.Fatalf("before the check: %+v, calls %v", job, sc.calls)
	}
	if err := f.svc.RetryNode(job.ID); err == nil {
		t.Fatal("ran before the host key was checked")
	}
	if err := f.svc.ConfirmNode(job.ID, "SHA256:other"); err == nil {
		t.Fatal("confirmed another host key")
	}
	if err := f.svc.ConfirmNode(job.ID, "SHA256:host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the machine to appear", func() bool { got, _ := f.svc.NodeJobByID(job.ID); return got.Stage == NodeDone })
	got, _ := f.svc.NodeJobByID(job.ID)
	if got.Node != "vm-0-3-debian" || !strings.Contains(sc.calls[0], "install/node.sh | sh -s -- --server https://203.0.113.7:6443 --token agent-token") {
		t.Fatalf("job = %+v, calls %v", got, sc.calls)
	}
	title := func() string {
		nodes, _ := us.Nodes(ctx)
		return nodes[slices.IndexFunc(nodes, func(n cluster.Node) bool { return n.Name == "vm-0-3-debian" })].Title()
	}
	// It gets the name it was added with; the name can change, and go back
	// to the node's own name.
	if title() != "美西 2 号机" {
		t.Errorf("new machine is called %q", title())
	}
	if err := f.svc.Rename(ctx, "us-1", "vm-0-3-debian", "us worker", "admin"); err != nil || title() != "us worker" {
		t.Errorf("rename: %v, called %q", err, title())
	}
	if err := f.svc.Rename(ctx, "us-1", "vm-0-3-debian", strings.Repeat("长", 41), "admin"); err == nil {
		t.Error("a 41-character name was accepted")
	}
	if err := f.svc.Rename(ctx, "us-1", "no-such-node", "x", "admin"); err == nil {
		t.Error("renamed a machine that is not there")
	}
	if err := f.svc.Rename(ctx, "us-1", "vm-0-3-debian", "  ", "admin"); err != nil || title() != "vm-0-3-debian" {
		t.Errorf("clearing the name: %v, called %q", err, title())
	}
	var log []store.Entry
	_ = f.store.View(ctx, func(tx *store.Tx) error {
		var err error
		log, err = tx.AuditLog(0, 10)
		return err
	})
	var details []string
	for _, e := range log {
		if e.Action == "node.add" || e.Action == "node.rename" {
			details = append(details, e.Action+" "+e.Detail)
		}
	}
	if len(details) != 3 || !strings.HasPrefix(details[0], "node.rename 去掉名称") || !strings.HasPrefix(details[1], "node.rename 改叫 us worker") ||
		!strings.Contains(details[2], "叫它 美西 2 号机") {
		t.Errorf("audit = %q", details)
	}

	// A run that fails can be retried; a machine that never shows up fails.
	sc.failNext = errors.New("boom")
	sc.act = nil
	f.svc.appearWait = 50 * time.Millisecond
	job, _ = f.svc.StartNodeJoin(ctx, "us-1", remote.Target{Host: "203.0.113.10", Password: "pw"}, "", "admin")
	if err := f.svc.ConfirmNode(job.ID, "SHA256:host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to fail", func() bool { got, _ := f.svc.NodeJobByID(job.ID); return got.Stage == NodeFailed })
	if err := f.svc.RetryNode(job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the machine to time out", func() bool {
		got, _ := f.svc.NodeJobByID(job.ID)
		return got.Stage == NodeFailed && strings.Contains(got.Message, "没有出现在集群里")
	})
}

func TestInstallStepsAndWhyItStopped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	j, err := f.svc.StartJoin(ctx, "sg-1", "sg", false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	report := func(r Report) Join {
		t.Helper()
		if err := f.svc.Report(ctx, j.Code, "203.0.113.7", r); err != nil {
			t.Fatal(err)
		}
		got, _ := f.svc.JoinByID(j.ID)
		return got
	}
	got := report(Report{Stage: StageAgones, Step: "下载 Agones 的镜像", Part: 1, Parts: 3})
	if got.Step != "下载 Agones 的镜像" || got.Part != 1 || got.Parts != 3 || !got.StepStarted.Equal(f.now) || got.Message != "" {
		t.Fatalf("step = %+v", got)
	}
	// A problem on the same step keeps the time the step started.
	started := f.now
	f.now = f.now.Add(time.Minute)
	got = report(Report{Stage: StageAgones, Step: "下载 Agones 的镜像", Part: 1, Parts: 3, Problem: "下载镜像出错，K3s 在重试：429"})
	if !got.StepStarted.Equal(started) || got.Message != "下载镜像出错，K3s 在重试：429" {
		t.Fatalf("problem = %+v", got)
	}
	got = report(Report{Stage: StageAgones, Step: strings.Repeat("长", 400), Part: 5, Parts: 3})
	if !got.StepStarted.Equal(f.now) || got.Part != 0 || got.Parts != 0 || len([]rune(got.Step)) != reportText+1 || got.Message != "" {
		t.Fatalf("bad counts and a long step = %d %d %d runes, %q", got.Part, got.Parts, len([]rune(got.Step)), got.Message)
	}
	// Scripts from before steps existed still work.
	if got = report(Report{Stage: StageK3s}); got.Stage != StageK3s || got.Step != "" || got.Parts != 0 {
		t.Fatalf("plain stage = %+v", got)
	}

	// When the script stops itself, the console says why instead of an exit status.
	f.svc.SetRunner(func(_ context.Context, _ remote.Target, _ string, out io.Writer) (string, error) {
		fmt.Fprintln(out, "GameFleet：下载并安装 Agones 的安装包")
		fmt.Fprintln(out, "GameFleet 停下了：Agones 的安装包装了 3 次都失败了：Error: INSTALLATION FAILED")
		fmt.Fprintln(out, "")
		return "SHA256:host", &remote.Error{Text: "命令在机器上失败了（退出码 1），原因见日志", Status: 1}
	})
	r, err := f.svc.StartRemoteJoin(ctx, "jp-1", "jp", false, remote.Target{Host: "203.0.113.8", Password: "pw"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ConfirmJoin(r.ID, "SHA256:host"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to stop", func() bool { got, _ := f.svc.JoinByID(r.ID); return got.Remote.State == RemoteFailed })
	got, _ = f.svc.JoinByID(r.ID)
	if got.Remote.Error != "Agones 的安装包装了 3 次都失败了：Error: INSTALLATION FAILED（退出码 1）" {
		t.Fatalf("error = %q", got.Remote.Error)
	}
}

// An operator whose machine shows another host key cancels: GameFleet drops
// the login without having used it, and runs nothing.
func TestCancelBeforeLoggingIn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sc := &script{}
	f.svc.SetRunner(sc.run)
	j, err := f.svc.StartRemoteJoin(ctx, "sg-1", "sg", false, remote.Target{Host: "203.0.113.7", Password: "pw"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelJoin(j.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.JoinByID(j.ID)
	f.svc.mu.Lock()
	kept := f.svc.joins[j.ID].target
	f.svc.mu.Unlock()
	if got.Remote.State != RemoteFailed || kept != nil || len(sc.calls) != 0 {
		t.Fatalf("cancelled install: %+v, login kept %v, calls %v", got.Remote, kept != nil, sc.calls)
	}
	if err := f.svc.ConfirmJoin(j.ID, "SHA256:host"); err == nil {
		t.Fatal("confirmed a cancelled install")
	}

	f.fakes["us-1"] = clustertest.New("us-1", 0)
	if err := f.svc.Add(ctx, *connection("us-1", "us"), "admin"); err != nil {
		t.Fatal(err)
	}
	job, err := f.svc.StartNodeJoin(ctx, "us-1", remote.Target{Host: "203.0.113.9", Password: "pw"}, "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelNode(job.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.NodeJobByID(job.ID); got.Stage != NodeFailed || len(sc.calls) != 0 {
		t.Fatalf("cancelled join: %+v, calls %v", got, sc.calls)
	}
	if err := f.svc.RetryNode(job.ID); err == nil {
		t.Fatal("retried a cancelled join")
	}
}

// A machine whose host key cannot be read is refused before anything starts.
func TestAMachineWithoutAHostKeyIsRefused(t *testing.T) {
	f := newFixture(t)
	f.svc.SetHostKeyReader(func(context.Context, string, int) (remote.HostKey, error) {
		return remote.HostKey{}, errors.New("连不上 203.0.113.7:22")
	})
	if _, err := f.svc.StartRemoteJoin(context.Background(), "sg-1", "sg", false, remote.Target{Host: "203.0.113.7", Password: "pw"}, "admin"); err == nil {
		t.Fatal("started an install on a machine whose host key could not be read")
	}
	if joins := len(f.svc.joins); joins != 0 {
		t.Fatalf("%d join codes made for it", joins)
	}
}
