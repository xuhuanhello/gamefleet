// Package registry puts GameFleet's API keys in front of the image registry
// and reads the labels of pushed images. The registry itself is the official
// registry container, reachable only from GameFleet.
//
// Docker, crane and containerd log in with Basic authentication. The user
// name is ignored; the password is a GameFleet API key. Reading needs scope
// pull or push, writing needs push. Nobody deletes through GameFleet.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/auth"
)

// transferTime bounds one request through the proxy. Game server layers are
// hundreds of megabytes; the server's usual timeouts would cut them off.
const transferTime = 30 * time.Minute

// Keys is what the proxy needs from auth.Keys.
type Keys interface {
	Check(ctx context.Context, key string) (auth.Caller, bool)
}

// Proxy checks the caller's key and passes the request on to the registry.
type Proxy struct {
	keys  Keys
	proxy *httputil.ReverseProxy
	log   *slog.Logger
}

// NewProxy returns the handler for /v2/ in front of the registry at upstream.
func NewProxy(upstream *url.URL, keys Keys, log *slog.Logger) *Proxy {
	return &Proxy{
		keys: keys,
		log:  log,
		proxy: &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(upstream)
				r.Out.Host = r.In.Host
				r.Out.Header.Del("Authorization")
			},
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		},
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	_, key, _ := r.BasicAuth()
	caller, ok := p.keys.Check(r.Context(), key)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="GameFleet"`)
		registryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "a GameFleet API key is required")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !caller.Has(auth.ScopePull, auth.ScopePush) {
			registryError(w, http.StatusForbidden, "DENIED", "this key cannot pull images")
			return
		}
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if !caller.Has(auth.ScopePush) {
			registryError(w, http.StatusForbidden, "DENIED", "this key cannot push images")
			return
		}
	default:
		registryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "not available through GameFleet")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		p.log.Info("registry write", "caller", caller.Name, "method", r.Method, "path", r.URL.Path)
	}
	deadline := time.Now().Add(transferTime)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	p.proxy.ServeHTTP(w, r)
}

func registryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}

// Errors from Inspect.
var (
	// ErrNotFound means the registry has no such repository, tag or digest.
	ErrNotFound = errors.New("镜像仓库里没有这个镜像")
	// ErrUnusable means the image exists but GameFleet cannot run it as a game server.
	ErrUnusable = errors.New("这个镜像不能当战斗服用")
)

// Image is what GameFleet needs to know about a pushed image.
type Image struct {
	Digest string // of what was pushed: a manifest or an index
	Labels map[string]string
	// Port and Protocol are the one port the image EXPOSEs.
	Port     int
	Protocol string
}

// Client reads images straight from the registry, without a key.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the registry at upstream.
func NewClient(upstream *url.URL) *Client {
	return &Client{base: strings.TrimRight(upstream.String(), "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

var (
	repository = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tag        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

const (
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
)

// Inspect reads the image repo:ref, where ref is a tag or a digest. For a
// multi-platform index it reads the linux/amd64 image, which is what the
// cluster runs; the returned digest is still the index's.
func (c *Client) Inspect(ctx context.Context, repo, ref string) (Image, error) {
	if !repository.MatchString(repo) {
		return Image{}, fmt.Errorf("%w：仓库名只能是一段小写名字", ErrUnusable)
	}
	if !tag.MatchString(ref) && !digest.MatchString(ref) {
		return Image{}, fmt.Errorf("%w：%q 不是 tag，也不是 sha256 digest", ErrUnusable, ref)
	}
	body, mediaType, pushed, err := c.manifest(ctx, repo, ref)
	if err != nil {
		return Image{}, err
	}
	var m struct {
		Config    struct{ Digest string } `json:"config"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return Image{}, fmt.Errorf("manifest of %s:%s: %w", repo, ref, err)
	}
	if mediaType == mediaOCIIndex || mediaType == mediaDockerList {
		var platform string
		for _, entry := range m.Manifests {
			if entry.Platform.OS == "linux" && entry.Platform.Architecture == "amd64" {
				platform = entry.Digest
				break
			}
		}
		if platform == "" {
			return Image{}, fmt.Errorf("%w：%s:%s 没有 linux/amd64 的镜像", ErrUnusable, repo, ref)
		}
		if body, _, _, err = c.manifest(ctx, repo, platform); err != nil {
			return Image{}, err
		}
		m.Config.Digest = ""
		if err := json.Unmarshal(body, &m); err != nil {
			return Image{}, fmt.Errorf("manifest of %s@%s: %w", repo, platform, err)
		}
	}
	if !digest.MatchString(m.Config.Digest) {
		return Image{}, fmt.Errorf("%w：%s:%s 没有镜像配置", ErrUnusable, repo, ref)
	}

	data, err := c.fetch(ctx, "/v2/"+repo+"/blobs/"+m.Config.Digest, "")
	if err != nil {
		return Image{}, err
	}
	var config struct {
		Config struct {
			Labels       map[string]string   `json:"Labels"`
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data.body, &config); err != nil {
		return Image{}, fmt.Errorf("config of %s:%s: %w", repo, ref, err)
	}
	image := Image{Digest: pushed, Labels: config.Config.Labels}
	if image.Labels == nil {
		image.Labels = map[string]string{}
	}
	if len(config.Config.ExposedPorts) != 1 {
		return Image{}, fmt.Errorf("%w：镜像要 EXPOSE 正好一个端口，现在是 %d 个", ErrUnusable, len(config.Config.ExposedPorts))
	}
	for exposed := range config.Config.ExposedPorts {
		number, protocol, _ := strings.Cut(exposed, "/")
		port, err := strconv.Atoi(number)
		if err != nil || port < 1 || port > 65535 {
			return Image{}, fmt.Errorf("%w：端口 %q 不对", ErrUnusable, exposed)
		}
		switch strings.ToLower(protocol) {
		case "", "tcp":
			image.Protocol = "TCP"
		case "udp":
			image.Protocol = "UDP"
		default:
			return Image{}, fmt.Errorf("%w：端口 %q 不对", ErrUnusable, exposed)
		}
		image.Port = port
	}
	return image, nil
}

// manifest fetches a manifest or index and checks it against its digest.
func (c *Client) manifest(ctx context.Context, repo, ref string) (body []byte, mediaType, sum string, err error) {
	accept := strings.Join([]string{mediaOCIIndex, mediaDockerList, mediaOCIManifest, mediaDockerManifest}, ", ")
	got, err := c.fetch(ctx, "/v2/"+repo+"/manifests/"+ref, accept)
	if err != nil {
		return nil, "", "", err
	}
	hash := sha256.Sum256(got.body)
	sum = "sha256:" + hex.EncodeToString(hash[:])
	if digest.MatchString(ref) && ref != sum {
		return nil, "", "", fmt.Errorf("manifest of %s@%s has digest %s", repo, ref, sum)
	}
	mediaType = got.contentType
	if mediaType == "" || mediaType == "application/json" {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(got.body, &probe)
		mediaType = probe.MediaType
	}
	return got.body, mediaType, sum, nil
}

type fetched struct {
	body        []byte
	contentType string
}

func (c *Client) fetch(ctx context.Context, path, accept string) (fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fetched{}, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fetched{}, fmt.Errorf("registry: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fetched{}, fmt.Errorf("registry: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fetched{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return fetched{}, fmt.Errorf("registry: GET %s returned %d", path, resp.StatusCode)
	}
	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return fetched{body: body, contentType: strings.TrimSpace(contentType)}, nil
}
