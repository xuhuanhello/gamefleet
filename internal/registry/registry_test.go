package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/xuhuanhello/gamefleet/internal/auth"
)

// fakeRegistry serves manifests and blobs from memory and records requests.
type fakeRegistry struct {
	manifests map[string][]byte // by tag and by digest
	types     map[string]string
	blobs     map[string][]byte
	requests  []string
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{manifests: map[string][]byte{}, types: map[string]string{}, blobs: map[string][]byte{}}
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// addImage stores an image config and its manifest under tag, and returns the manifest digest.
func (f *fakeRegistry) addImage(tag string, labels map[string]string, ports ...string) string {
	exposed := map[string]struct{}{}
	for _, p := range ports {
		exposed[p] = struct{}{}
	}
	config, _ := json.Marshal(map[string]any{"config": map[string]any{"Labels": labels, "ExposedPorts": exposed}})
	f.blobs[sum(config)] = config
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": mediaDockerManifest,
		"config": map[string]any{"digest": sum(config)},
	})
	return f.addManifest(tag, mediaDockerManifest, manifest)
}

func (f *fakeRegistry) addManifest(tag, mediaType string, body []byte) string {
	d := sum(body)
	for _, ref := range []string{tag, d} {
		if ref != "" {
			f.manifests[ref], f.types[ref] = body, mediaType
		}
	}
	return d
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
	switch {
	case len(parts) == 3 && parts[1] == "manifests" && f.manifests[parts[2]] != nil:
		w.Header().Set("Content-Type", f.types[parts[2]])
		w.Write(f.manifests[parts[2]])
	case len(parts) == 3 && parts[1] == "blobs" && f.blobs[parts[2]] != nil:
		w.Write(f.blobs[parts[2]])
	case r.URL.Path == "/v2/" || r.Method != http.MethodGet:
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

func serve(t *testing.T, h http.Handler) *url.URL {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u
}

var labels = map[string]string{
	"io.gamefleet.compat":               "net7",
	"org.opencontainers.image.version":  "42",
	"org.opencontainers.image.revision": "abc",
}

func TestInspectReadsLabelsAndPort(t *testing.T) {
	reg := newFakeRegistry()
	d := reg.addImage("42", labels, "7770/udp")
	c := NewClient(serve(t, reg))
	for _, ref := range []string{"42", d} {
		img, err := c.Inspect(context.Background(), "billiards", ref)
		if err != nil {
			t.Fatal(err)
		}
		if img.Digest != d || img.Labels["io.gamefleet.compat"] != "net7" || img.Port != 7770 || img.Protocol != "UDP" {
			t.Errorf("Inspect(%s) = %+v", ref, img)
		}
	}
}

func TestInspectPicksLinuxAmd64FromAnIndex(t *testing.T) {
	reg := newFakeRegistry()
	amd64 := reg.addImage("", labels, "7770/udp")
	arm64 := reg.addImage("", map[string]string{"io.gamefleet.compat": "wrong"}, "7770/udp")
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": mediaOCIIndex,
		"manifests": []map[string]any{
			{"digest": arm64, "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
			{"digest": amd64, "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
		},
	})
	d := reg.addManifest("43", mediaOCIIndex, index)
	img, err := NewClient(serve(t, reg)).Inspect(context.Background(), "billiards", "43")
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != d || img.Labels["io.gamefleet.compat"] != "net7" {
		t.Errorf("image = %+v", img)
	}
}

func TestInspectRejects(t *testing.T) {
	reg := newFakeRegistry()
	reg.addImage("noport", labels)
	reg.addImage("twoports", labels, "7770/udp", "8080/tcp")
	tampered := reg.addImage("ok", labels, "7770/udp")
	c := NewClient(serve(t, reg))
	ctx := context.Background()
	for ref, want := range map[string]error{"missing": ErrNotFound, "noport": ErrUnusable, "twoports": ErrUnusable, "bad tag!": ErrUnusable} {
		if _, err := c.Inspect(ctx, "billiards", ref); !errors.Is(err, want) {
			t.Errorf("Inspect(%s) = %v, want %v", ref, err, want)
		}
	}
	if _, err := c.Inspect(ctx, "Billiards", "ok"); !errors.Is(err, ErrUnusable) {
		t.Errorf("uppercase repository: %v", err)
	}
	// A manifest whose content does not hash to the digest asked for.
	reg.manifests[tampered] = []byte(`{"schemaVersion":2}`)
	if _, err := c.Inspect(ctx, "billiards", tampered); err == nil {
		t.Error("tampered manifest accepted")
	}
}

type keys map[string]auth.Caller

func (k keys) Check(_ context.Context, key string) (auth.Caller, bool) {
	c, ok := k[key]
	return c, ok
}

func TestProxyChecksKeysAndScopes(t *testing.T) {
	reg := newFakeRegistry()
	upstream := serve(t, reg)
	proxy := serve(t, NewProxy(upstream, keys{
		"push-key": {Name: "ci", Scopes: []string{auth.ScopePush}},
		"pull-key": {Name: "cluster", Scopes: []string{auth.ScopePull}},
		"nakama":   {Name: "nakama", Scopes: []string{auth.ScopeAllocate}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil))))

	cases := []struct {
		method, key string
		want        int
	}{
		{http.MethodGet, "", http.StatusUnauthorized},
		{http.MethodGet, "wrong", http.StatusUnauthorized},
		{http.MethodGet, "nakama", http.StatusForbidden},
		{http.MethodGet, "pull-key", http.StatusAccepted},
		{http.MethodHead, "push-key", http.StatusAccepted},
		{http.MethodPost, "pull-key", http.StatusForbidden},
		{http.MethodPost, "push-key", http.StatusAccepted},
		{http.MethodDelete, "push-key", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, proxy.String()+"/v2/", nil)
		if c.key != "" {
			req.SetBasicAuth("anyone", c.key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s with %q: %d, want %d", c.method, c.key, resp.StatusCode, c.want)
		}
		if resp.StatusCode == http.StatusUnauthorized && !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("401 without a Basic challenge")
		}
	}
	for _, r := range reg.requests {
		if !strings.HasSuffix(r, "auth=") {
			t.Errorf("the key reached the registry: %s", r)
		}
	}
	if len(reg.requests) != 3 {
		t.Errorf("registry saw %d requests, want 3", len(reg.requests))
	}
}
