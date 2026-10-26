package update

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests here run deploy/updater.sh against stand-ins: every image is on
// the machine, and the install script that an image's deploy files carry
// exits with the code in the file exit-<version>, 0 when there is none.

// dockerStub stands in for the docker commands the updater runs.
const dockerStub = `#!/bin/sh
case "$1" in
image) exit 0 ;;
create) echo "c-$(echo "$2" | tr ':/' '__')" ;;
cp)
	mkdir -p "$3"
	tag=${2%%:/deploy/.}
	tag=${tag##*_}
	printf '#!/bin/sh\nexit $(cat "%s/exit-%s" 2>/dev/null || echo 0)\n' "$ROOT" "$tag" >"$3/install.sh"
	;;
esac
`

type updater struct {
	root string
	path string // PATH, with the stand-ins first
}

func newUpdater(t *testing.T) *updater {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join("..", "..", "deploy", "updater.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	for name, data := range map[string]string{
		"updater.sh": string(script),
		".env": "GAMEFLEET_DOMAIN=fleet.example.com\nACME_EMAIL=ops@example.com\n" +
			"GAMEFLEET_IMAGE=repo:2.0.0\nGAMEFLEET_IMAGE_REPO=repo\n",
		"bin/docker": dockerStub,
		"bin/curl":   "#!/bin/sh\nexit 0\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "update"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &updater{root: root, path: bin + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// write puts a file in the updater's directory.
func (u *updater) write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(u.root, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// start runs the updater until it exits or the context ends.
func (u *updater) start(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(u.root, "updater.sh"))
	cmd.Env = append(os.Environ(), "PATH="+u.path, "ROOT="+u.root)
	return cmd
}

// status reads status.json, which must be valid JSON.
func (u *updater) status(t *testing.T) Status {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(u.root, "update", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("status.json is not JSON: %v\n%s", err, data)
	}
	return st
}

// apply asks the updater to put 2.1.0 in place and waits for it to finish.
func (u *updater) apply(t *testing.T) Status {
	t.Helper()
	u.write(t, "update/request.json",
		`{"action":"apply","at":"2026-10-26T10:10:10Z","backup":"gamefleet-2.0.0-20261026-101010.db","version":"2.1.0"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := u.start(ctx).CombinedOutput(); err != nil {
		t.Fatalf("updater: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(u.root, "update", "request.json")); err == nil {
		t.Fatal("the request is still there")
	}
	return u.status(t)
}

func TestUpdaterApplies(t *testing.T) {
	u := newUpdater(t)
	if st := u.apply(t); st.State != Applied || st.Version != "2.1.0" {
		t.Fatalf("status = %+v", st)
	}
}

func TestUpdaterGoesBackWhenTheNewVersionFails(t *testing.T) {
	u := newUpdater(t)
	u.write(t, "exit-2.1.0", "1")
	st := u.apply(t)
	if st.State != Failed || !strings.Contains(st.Error, "已经回到原来的版本") {
		t.Fatalf("status = %+v", st)
	}
}

// When going back fails too, the updater says so, and which backup to restore.
func TestUpdaterSaysWhenGoingBackFails(t *testing.T) {
	u := newUpdater(t)
	u.write(t, "exit-2.1.0", "1")
	u.write(t, "exit-2.0.0", "1")
	st := u.apply(t)
	if st.State != Failed || strings.Contains(st.Error, "已经回到原来的版本") ||
		!strings.Contains(st.Error, "换回原来的版本也没有成功") ||
		!strings.Contains(st.Error, "backups/gamefleet-2.0.0-20261026-101010.db") {
		t.Fatalf("status = %+v", st)
	}
}

// A download or an update under way when the updater stopped did not finish;
// on its next start the updater says so, and takes requests again.
func TestUpdaterEndsWhatAStopCutShort(t *testing.T) {
	for _, state := range []string{Downloading, Applying} {
		t.Run(state, func(t *testing.T) {
			u := newUpdater(t)
			u.write(t, "update/status.json", `{"action":"apply","version":"2.1.0","state":"`+state+`","error":"","at":"2026-10-26T10:10:10Z"}`)
			ctx, cancel := context.WithCancel(context.Background())
			cmd := u.start(ctx)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); cmd.Wait() }()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if data, err := os.ReadFile(filepath.Join(u.root, "update", "status.json")); err == nil && strings.Contains(string(data), `"failed"`) {
					if st := u.status(t); st.Version != "2.1.0" || !strings.Contains(st.Error, "可以再试一次") {
						t.Fatalf("status = %+v", st)
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("still %s after a restart", state)
		})
	}
}
