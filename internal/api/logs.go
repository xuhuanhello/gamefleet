package api

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/logs"
)

// readsPerMinute is how many reads of logs one key may make in a minute.
const readsPerMinute = 60

// limiter counts each key's reads in the current minute.
type limiter struct {
	mu     sync.Mutex
	minute time.Time
	reads  map[string]int
}

func (l *limiter) allow(name string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if minute := now.Truncate(time.Minute); !minute.Equal(l.minute) {
		l.minute, l.reads = minute, map[string]int{}
	}
	l.reads[name]++
	return l.reads[name] <= readsPerMinute
}

// logRoutes adds the endpoints of the logs scope.
func (h *handler) logRoutes(mux *http.ServeMux) {
	read := func(next func(http.ResponseWriter, *http.Request, auth.Caller)) http.Handler {
		return h.require(func(w http.ResponseWriter, r *http.Request, caller auth.Caller) {
			if h.logs == nil {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			if !h.limits.allow(caller.Name, time.Now()) {
				w.Header().Set("Retry-After", "60")
				writeError(w, http.StatusTooManyRequests, "rate_limited", "每把 key 每分钟最多读 60 次")
				return
			}
			// The query as far as a real one goes: the caller chooses it.
			query := r.URL.RawQuery
			if len(query) > 200 {
				query = query[:200] + "…"
			}
			h.log.Info("logs read", "caller", caller.Name, "path", r.URL.Path, "query", query)
			next(w, r, caller)
		}, auth.ScopeLogs)
	}
	mux.Handle("GET /api/v1/pods", read(h.pods))
	mux.Handle("GET /api/v1/events", read(h.events))
	mux.Handle("GET /api/v1/logs/pods/{cluster}/{namespace}/{pod}", read(h.podLog))
	mux.Handle("GET /api/v1/logs/matches/{matchId}", read(h.matchLog))
	mux.Handle("GET /api/v1/logs/gamefleet", read(h.gamefleetLog))
}

func (h *handler) pods(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	pods, err := h.logs.Pods(r.Context(), r.URL.Query().Get("cluster"))
	if err != nil {
		h.logsFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pods": pods})
}

func (h *handler) events(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	q, ok := logQuery(w, r)
	if !ok {
		return
	}
	events, err := h.logs.Events(r.Context(), r.URL.Query().Get("cluster"), q.Since)
	if err != nil {
		h.logsFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (h *handler) podLog(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	q, ok := logQuery(w, r)
	if !ok {
		return
	}
	text, err := h.logs.PodLog(r.Context(), r.PathValue("cluster"), r.PathValue("namespace"), r.PathValue("pod"), q)
	if err != nil {
		h.logsFailed(w, err)
		return
	}
	writeText(w, text)
}

func (h *handler) matchLog(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	q, ok := logQuery(w, r)
	if !ok {
		return
	}
	room, text, err := h.logs.MatchLog(r.Context(), r.PathValue("matchId"), q)
	if err != nil {
		h.logsFailed(w, err)
		return
	}
	w.Header().Set("GameFleet-Room", room.Cluster+"/"+room.ID)
	writeText(w, text)
}

func (h *handler) gamefleetLog(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	q, ok := logQuery(w, r)
	if !ok {
		return
	}
	text, err := h.logs.GameFleetLog(r.Context(), q)
	if err != nil {
		h.logsFailed(w, err)
		return
	}
	writeText(w, text)
}

// logQuery reads since, lines, container, previous and level from the URL.
func logQuery(w http.ResponseWriter, r *http.Request) (logs.Query, bool) {
	v := r.URL.Query()
	q := logs.Query{Container: v.Get("container"), Previous: v.Get("previous") == "true", Warnings: v.Get("level") == "warn"}
	var err error
	if s := v.Get("since"); s != "" {
		if q.Since, err = time.ParseDuration(s); err != nil || q.Since <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "since 是一段时间，比如 30m、2h，最多 24h")
			return q, false
		}
	}
	if s := v.Get("lines"); s != "" {
		if q.Lines, err = strconv.Atoi(s); err != nil || q.Lines <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "lines 是 1 到 2000 之间的整数")
			return q, false
		}
	}
	return q, true
}

func (h *handler) logsFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, logs.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, logs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		// The error may name what the logs scope does not show; GameFleet's
		// own log, redacted, has it.
		h.log.Warn("logs read failed", "error", err)
		writeError(w, http.StatusBadGateway, "cluster_unavailable", "读不到集群，原因在 GameFleet 自己的日志里")
	}
}

// writeText sends redacted log lines. The caller should treat them as data:
// players control some of what game servers write.
func writeText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}
