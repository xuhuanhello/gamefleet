package install

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/xuhuanhello/gamefleet/internal/clusters"
)

type joins struct {
	reports []clusters.Report
	err     error
}

func (j *joins) Lookup(code string) (clusters.Join, bool) {
	return clusters.Join{Name: "sg-1", Region: "sg"}, code == "goodcode"
}

func (j *joins) Report(_ context.Context, code, ip string, r clusters.Report) error {
	if code != "goodcode" {
		return clusters.ErrBadCode
	}
	j.reports = append(j.reports, r)
	return j.err
}

func serve(t *testing.T, j *joins) *httptest.Server {
	t.Helper()
	h, err := Handler("https://fleet.example.com", j, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string, header ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestScriptsRenderAndParse(t *testing.T) {
	srv := serve(t, &joins{})
	for _, name := range []string{"cluster.sh", "node.sh"} {
		status, body := get(t, srv.URL+"/install/"+name)
		if status != http.StatusOK || strings.Contains(body, "{{") {
			t.Fatalf("%s: %d, unrendered template", name, status)
		}
		for _, want := range []string{clusters.K3sVersion, "https://fleet.example.com", "20000"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		path := filepath.Join(t.TempDir(), name)
		os.WriteFile(path, []byte(body), 0o600)
		for _, shell := range []string{"sh", "bash"} {
			if out, err := exec.Command(shell, "-n", path).CombinedOutput(); err != nil {
				t.Errorf("%s -n %s: %v\n%s", shell, name, err, out)
			}
		}
	}
	_, script := get(t, srv.URL+"/install/cluster.sh")
	for _, want := range []string{"kind: ClusterRole", "namespace: agones-games", "version: $AGONES_VERSION", clusters.AgonesVersion,
		`resources: ["secrets"]`, `verbs: ["create", "patch"]`,
		// K3s' Helm installs new charts with server-side apply, which rejects Agones' CRDs.
		`serverSide: "false"`,
		// Pods cannot reach the API server at a public IP that is NAT.
		"advertise-address: $private_ip",
		// Game servers go only to machines labelled for them, which a
		// control plane kept for itself is not.
		`labels="\"gamefleet.io/pool=game\", \"gamefleet.io/cluster=$name\""`, "--control-plane-only) control_plane_only=1"} {
		if !strings.Contains(script, want) {
			t.Errorf("cluster.sh lacks %q", want)
		}
	}
	if strings.Contains(script, `resources: ["secrets"]
    verbs: ["get"`) {
		t.Error("GameFleet must not read Secrets")
	}
	if status, body := get(t, srv.URL+"/install/rbac.yaml"); status != http.StatusOK || !strings.Contains(body, "name: gamefleet-token") {
		t.Errorf("rbac.yaml: %d", status)
	}
}

func TestIPAndReports(t *testing.T) {
	j := &joins{}
	srv := serve(t, j)
	if _, body := get(t, srv.URL+"/install/ip", "X-Forwarded-For", "203.0.113.7, 10.0.0.1"); body != "203.0.113.7" {
		t.Errorf("ip = %q", body)
	}
	if status, body := get(t, srv.URL+"/install/clusters/goodcode"); status != http.StatusOK || !strings.Contains(body, `"region":"sg"`) {
		t.Errorf("lookup: %d %s", status, body)
	}
	if status, _ := get(t, srv.URL+"/install/clusters/badcode"); status != http.StatusForbidden {
		t.Errorf("bad code lookup: %d", status)
	}
	post := func(code, body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+"/install/clusters/"+code, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, _ := post("goodcode", `{"stage":"k3s"}`); status != http.StatusOK || len(j.reports) != 1 || j.reports[0].Stage != "k3s" {
		t.Errorf("report: %d %+v", status, j.reports)
	}
	if status, _ := post("badcode", `{"stage":"k3s"}`); status != http.StatusForbidden {
		t.Errorf("bad code: %d", status)
	}
	j.err = &clusters.Error{Text: "GameFleet 连不上 6443"}
	if status, out := post("goodcode", `{"stage":"connect","connection":{"name":"sg-1"}}`); status != http.StatusConflict || out["error"] != "GameFleet 连不上 6443" {
		t.Errorf("refused: %d %v", status, out)
	}
	if status, _ := post("goodcode", `not json`); status != http.StatusBadRequest {
		t.Errorf("not json: %d", status)
	}
}

// block returns the lines of script from the one that is first, up to and
// including the first line after it that is last.
func block(t *testing.T, script, first, last string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	for i, l := range lines {
		if l != first {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == last {
				return strings.Join(lines[i:j+1], "\n")
			}
		}
	}
	t.Fatalf("cluster.sh has no block from %q to %q", first, last)
	return ""
}

// systemPath matches a path into the machine's own directories.
var systemPath = regexp.MustCompile(`(^|[\s>"'=(])/(usr|etc|var|tmp|opt|run|lib|bin|sbin|root|home|srv)/[^\s"']*`)

// runBlock runs part of cluster.sh with its helpers stubbed, and the commands
// it runs recorded. The part must name no file outside dir but those it
// replaces here with stand-ins: it runs on the machine of whoever runs the
// tests. It returns what the part printed and what it ran.
func runBlock(t *testing.T, dir, part, setup string, paths ...string) (out, ran string) {
	t.Helper()
	log := filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		filepath.Join(bin, "systemctl"):      "systemctl",
		filepath.Join(bin, "curl"):           "curl",
		filepath.Join(bin, "kubectl"):        "kubectl",
		filepath.Join(dir, "k3s-killall.sh"): "k3s-killall.sh",
	}
	for path, name := range stubs {
		stub := "#!/bin/sh\necho \"" + name + " $*\" >>" + log + "\n"
		if name == "curl" {
			stub += "exit 1\n" // no download: the install stops there
		}
		if err := os.WriteFile(path, []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// One replacer, which never looks again at what it put in: dir may itself
	// be under /tmp.
	pairs := append([]string{"/usr/local/bin/k3s-killall.sh", filepath.Join(dir, "k3s-killall.sh")}, paths...)
	part = strings.NewReplacer(append(pairs, "/tmp/", dir+"/")...).Replace(part)
	// The test's own directory may be under /tmp: leave it out of the check.
	if m := systemPath.FindString(strings.ReplaceAll(part, dir, "TESTDIR")); m != "" {
		t.Fatalf("the part names %q, a file of the machine running the tests:\n%s", m, part)
	}
	script := "set -u\nsay() { :; }\nstatus() { :; }\nfail() { echo \"fail: $*\"; exit 1; }\nK3S_VERSION=v0\n" +
		setup + "\n" + part + "\necho \"configured=${configured:-} installed=${installed:-}\"\n"
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b, _ := cmd.CombinedOutput()
	r, _ := os.ReadFile(log)
	return string(b), string(r)
}

// A retry of the control plane install goes on from what is really there: a
// config written by an earlier run is not K3s installed, and K3s installed is
// not K3s started.
func TestClusterScriptResumesFromWhatIsThere(t *testing.T) {
	srv := serve(t, &joins{})
	_, script := get(t, srv.URL+"/install/cluster.sh")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k3s, unit, config := filepath.Join(dir, "k3s"), filepath.Join(dir, "k3s.service"), filepath.Join(dir, "config.yaml")
	detect := block(t, script, "configured='' installed=''", "fi")
	paths := []string{"/usr/local/bin/k3s", k3s, "/etc/systemd/system/k3s.service", unit}
	setup := "CONFIG=" + config
	cases := []struct {
		name                    string
		marked, binary, service bool
		want                    string
	}{
		{"fresh machine", false, false, false, "configured= installed="},
		{"config written, K3s not downloaded", true, false, false, "configured=1 installed="},
		{"K3s downloaded, service not made", true, true, false, "configured=1 installed="},
		{"K3s installed", true, true, true, "configured=1 installed=1"},
		{"another K3s", false, true, true, "fail: 这台机器已经装了别的 K3s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, f := range []string{k3s, unit, config} {
				if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			for _, f := range []struct {
				want       bool
				path, data string
				mode       os.FileMode
			}{{c.marked, config, "# gamefleet cluster sg-1\n", 0o600}, {c.binary, k3s, "#!/bin/sh\n", 0o755}, {c.service, unit, "[Unit]\n", 0o644}} {
				if f.want {
					if err := os.WriteFile(f.path, []byte(f.data), f.mode); err != nil {
						t.Fatal(err)
					}
				}
			}
			if out, _ := runBlock(t, dir, detect, setup, paths...); !strings.Contains(out, c.want) {
				t.Fatalf("got %q, want %q", out, c.want)
			}
		})
	}

	start := block(t, script, `if [ -z "$installed" ]; then`, "fi")
	for _, c := range []struct{ name, setup, want string }{
		{"not installed", "installed='' config_changed=''", "curl -sfL --max-time 60 https://get.k3s.io"},
		{"config changed", "installed=1 config_changed=1", "k3s-killall.sh \nsystemctl start --no-block k3s"},
		// An earlier run may have stopped before it started K3s.
		{"config unchanged", "installed=1 config_changed=''", "systemctl start --no-block k3s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if out, ran := runBlock(t, run, start, c.setup); !strings.Contains(ran, c.want) {
				t.Fatalf("ran %q (printed %q), want %q", ran, out, c.want)
			}
		})
	}
}

// A rejoin runs only on the control plane of the cluster it names, and keeps
// that machine's setup: game servers on it or not, as its config says.
func TestClusterScriptRejoinsAsTheControlPlaneWas(t *testing.T) {
	srv := serve(t, &joins{})
	_, script := get(t, srv.URL+"/install/cluster.sh")
	rejoin := block(t, script, `if [ -n "$rejoin" ]; then`, "fi")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.yaml")
	for _, c := range []struct {
		name, installed, config, want string
	}{
		{"not installed", "", "# gamefleet cluster sg-1\n", "fail: 这台机器上没有 GameFleet 装的集群"},
		{"another cluster", "1", "# gamefleet cluster jp-1\n", "fail: 这台机器装的是别的集群"},
		{"runs game servers", "1", "# gamefleet cluster sg-1\nnode-label: [\"gamefleet.io/pool=game\", \"gamefleet.io/cluster=sg-1\"]\n", "control_plane_only=."},
		{"control plane only", "1", "# gamefleet cluster sg-1\nnode-label: [\"gamefleet.io/cluster=sg-1\"]\n", "control_plane_only=1."},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(config, []byte(c.config), 0o600); err != nil {
				t.Fatal(err)
			}
			// The command line said the opposite of what the machine has.
			setup := "CONFIG=" + config + " name=sg-1 rejoin=1 installed=" + c.installed + " control_plane_only=x"
			out, _ := runBlock(t, dir, rejoin+"\necho \"control_plane_only=$control_plane_only.\"", setup)
			if !strings.Contains(out, c.want) {
				t.Fatalf("got %q, want %q", out, c.want)
			}
		})
	}
}

// Rotating the token deletes GameFleet's token Secret, which ends the old
// token, and applies the manifests to make a new one.
func TestClusterScriptRotatesTheToken(t *testing.T) {
	srv := serve(t, &joins{})
	_, script := get(t, srv.URL+"/install/cluster.sh")
	rotate := block(t, script, `if [ -n "$rotate_token" ]; then`, "fi")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, ran := runBlock(t, dir, rotate, "rotate_token=1 NAMESPACE=agones-games MANIFESTS="+dir)
	want := "kubectl -n agones-games delete secret gamefleet-token --ignore-not-found\nkubectl apply -f " + dir + "/gamefleet.yaml\n"
	if ran != want {
		t.Fatalf("ran %q, want %q", ran, want)
	}
}
