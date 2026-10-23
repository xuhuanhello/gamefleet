// Package install serves what new machines fetch without an account: the
// scripts that make a cluster or join one, GameFleet's RBAC manifest, the
// caller's public IP, and the endpoint install scripts report progress to.
// None of it holds a secret; a join code authorizes the report.
package install

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"text/template"

	"github.com/xuhuanhello/gamefleet/internal/clusters"
)

//go:embed cluster.sh node.sh rbac.yaml
var files embed.FS

// Joins is what the handler needs from clusters.Service.
type Joins interface {
	Lookup(code string) (clusters.Join, bool)
	Report(ctx context.Context, code, ip string, r clusters.Report) error
}

type values struct {
	Base, K3s, Agones, Namespace string
	MinPort, MaxPort             int
	Manifests                    string
}

// Manifests renders GameFleet's RBAC for a cluster whose game servers run in namespace.
func Manifests(namespace string) (string, error) {
	return render("rbac.yaml", values{Namespace: namespace})
}

func render(name string, v values) (string, error) {
	t, err := template.ParseFS(files, name)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Handler serves /install/. base is GameFleet's public address, https://host.
func Handler(base string, joins Joins, log *slog.Logger) (http.Handler, error) {
	manifests, err := Manifests(clusters.Namespace)
	if err != nil {
		return nil, err
	}
	v := values{Base: base, K3s: clusters.K3sVersion, Agones: clusters.AgonesVersion, Namespace: clusters.Namespace,
		MinPort: clusters.MinPort, MaxPort: clusters.MaxPort, Manifests: strings.TrimRight(manifests, "\n")}
	scripts := map[string]string{}
	for _, name := range []string{"cluster.sh", "node.sh"} {
		if scripts[name], err = render(name, v); err != nil {
			return nil, err
		}
	}

	mux := http.NewServeMux()
	text := func(body, contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(body))
		}
	}
	mux.Handle("GET /install/cluster.sh", text(scripts["cluster.sh"], "text/x-shellscript; charset=utf-8"))
	mux.Handle("GET /install/node.sh", text(scripts["node.sh"], "text/x-shellscript; charset=utf-8"))
	mux.Handle("GET /install/rbac.yaml", text(manifests, "application/yaml"))
	mux.HandleFunc("GET /install/ip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(remoteAddr(r)))
	})
	mux.HandleFunc("GET /install/clusters/{code}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := joins.Lookup(r.PathValue("code"))
		if !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "加入码不对或已过期，到控制台重新生成"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": j.Name, "region": j.Region})
	})
	mux.HandleFunc("POST /install/clusters/{code}", func(w http.ResponseWriter, r *http.Request) {
		var report clusters.Report
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&report); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求不是 JSON"})
			return
		}
		ip := remoteAddr(r)
		err := joins.Report(r.Context(), r.PathValue("code"), ip, report)
		var refusal *clusters.Error
		switch {
		case err == nil:
			log.Info("cluster install report", "stage", report.Stage, "ip", ip)
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case errors.Is(err, clusters.ErrBadCode):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "加入码不对或已过期，到控制台重新生成"})
		case errors.As(err, &refusal):
			log.Warn("cluster install report refused", "stage", report.Stage, "ip", ip, "error", err)
			writeJSON(w, http.StatusConflict, map[string]string{"error": refusal.Text})
		default:
			log.Error("cluster install report failed", "stage", report.Stage, "ip", ip, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "GameFleet 出错了，稍后会重试"})
		}
	})
	return mux, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// remoteAddr is the caller's address. Caddy, the only way in, puts it first
// in X-Forwarded-For.
func remoteAddr(r *http.Request) string {
	if forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); forwarded != "" {
		return strings.TrimSpace(forwarded)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}
