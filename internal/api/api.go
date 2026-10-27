// Package api serves GameFleet's machine-facing HTTP API: builds, routes,
// allocations and rooms. Callers authenticate with a bearer key, and each
// key's scopes decide which endpoints it may use. The console has its own
// endpoints and is not part of this contract.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/allocate"
	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/buildinfo"
	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/health"
	"github.com/xuhuanhello/gamefleet/internal/registry"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Keys checks API keys; auth.Keys is the real one.
type Keys interface {
	Check(ctx context.Context, key string) (auth.Caller, bool)
}

// Releases is what the API needs from release.Service.
type Releases interface {
	Register(ctx context.Context, r release.Registration, actor string) (store.Build, bool, error)
	Routes(ctx context.Context, game string) ([]release.RouteView, error)
	Record(ctx context.Context, actor, action, target, detail string) error
}

type handler struct {
	svc      *allocate.Service
	releases Releases
	keys     Keys
	monitor  *health.Monitor
	log      *slog.Logger
}

// Handler returns the HTTP handler for the machine API, /healthz and
// /status. monitor may be nil: /status then answers 404.
func Handler(svc *allocate.Service, releases Releases, keys Keys, monitor *health.Monitor, log *slog.Logger) http.Handler {
	h := &handler{svc: svc, releases: releases, keys: keys, monitor: monitor, log: log}
	mux := http.NewServeMux()
	// For an outside monitor, without a key: what needs a person, with no
	// secret in it. 503 when something does.
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		if h.monitor == nil {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		st := h.monitor.Status()
		status := http.StatusOK
		if !st.OK {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, st)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": buildinfo.Version})
	})
	mux.Handle("POST /api/v1/builds", h.require(h.register, auth.ScopePush))
	mux.Handle("GET /api/v1/routes", h.require(h.routes, auth.ScopeAllocate))
	mux.Handle("POST /api/v1/allocations", h.require(h.allocate, auth.ScopeAllocate))
	mux.Handle("GET /api/v1/rooms", h.require(h.roomsByMatch, auth.ScopeAllocate, auth.ScopeGM))
	mux.Handle("GET /api/v1/rooms/{id}", h.require(h.room, auth.ScopeAllocate, auth.ScopeGM))
	mux.Handle("DELETE /api/v1/rooms/{id}", h.require(h.shutdown, auth.ScopeGM))
	mux.Handle("POST /api/v1/matches/{matchId}/end", h.require(h.endMatch, auth.ScopeGM))
	return mux
}

// require authenticates the bearer key and checks that it has one of the scopes.
func (h *handler) require(next func(http.ResponseWriter, *http.Request, auth.Caller), scopes ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		caller, ok := h.keys.Check(r.Context(), token)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gamefleet"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !caller.Has(scopes...) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r, caller)
	})
}

// register records an image CI pushed as a build.
func (h *handler) register(w http.ResponseWriter, r *http.Request, caller auth.Caller) {
	var req release.Registration
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "请求体要是 JSON，字段为 repository、tag 或 digest、note")
		return
	}
	b, created, err := h.releases.Register(r.Context(), req, caller.Actor())
	if err != nil {
		for _, m := range []struct {
			err    error
			status int
			code   string
		}{
			{release.ErrInvalid, http.StatusBadRequest, "invalid_request"},
			{registry.ErrNotFound, http.StatusNotFound, "image_not_found"},
			{registry.ErrUnusable, http.StatusUnprocessableEntity, "image_unusable"},
		} {
			if errors.Is(err, m.err) {
				writeError(w, m.status, m.code, err.Error())
				return
			}
		}
		h.log.Error("build registration failed", "caller", caller.Name, "repository", req.Repository, "error", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		h.log.Info("build registered", "caller", caller.Name, "build", b.ID, "game", b.Game, "digest", b.Digest)
	}
	writeJSON(w, status, map[string]any{
		"build": b.ID, "game": b.Game, "version": b.Version(), "compat": b.Compat,
		"digest": b.Digest, "labels": b.Labels,
	})
}

func (h *handler) routes(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	routes, err := h.releases.Routes(r.Context(), r.URL.Query().Get("game"))
	if err != nil {
		h.log.Error("routes failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": routes})
}

func (h *handler) allocate(w http.ResponseWriter, r *http.Request, caller auth.Caller) {
	var req allocate.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, allocate.ErrInvalid.Error())
		return
	}
	started := time.Now()
	result, err := h.svc.Allocate(r.Context(), req)
	// A request that was wrong, or for a route there is not, is the
	// caller's, and is left out; anything else that failed is GameFleet's or
	// a cluster's.
	if !errors.Is(err, allocate.ErrInvalid) && !errors.Is(err, allocate.ErrRouteNotFound) {
		h.monitor.Allocation(time.Since(started), err != nil)
	}
	if err != nil {
		h.fail(w, err, "allocation", "caller", caller.Name, "matchId", req.MatchID)
		return
	}
	h.log.Info("allocation", "caller", caller.Name, "matchId", req.MatchID,
		"room", result.Room.ID, "cluster", result.Room.Cluster, "build", result.Room.Build, "created", result.Created)
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result.Room)
}

func (h *handler) room(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	room, err := h.svc.Room(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err, "room lookup", "room", r.PathValue("id"))
		return
	}
	if room == nil {
		writeError(w, http.StatusNotFound, "room_not_found")
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (h *handler) roomsByMatch(w http.ResponseWriter, r *http.Request, _ auth.Caller) {
	matchID := r.URL.Query().Get("matchId")
	room, err := h.svc.RoomByMatch(r.Context(), matchID)
	if err != nil {
		h.fail(w, err, "room lookup", "matchId", matchID)
		return
	}
	rooms := []cluster.Room{}
	if room != nil {
		rooms = append(rooms, *room)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

func (h *handler) shutdown(w http.ResponseWriter, r *http.Request, caller auth.Caller) {
	id := r.PathValue("id")
	found, err := h.svc.Shutdown(r.Context(), id)
	if err != nil {
		h.fail(w, err, "room shutdown", "caller", caller.Name, "room", id)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "room_not_found")
		return
	}
	h.log.Info("room shutdown", "caller", caller.Name, "room", id)
	audited := h.record(r.Context(), caller, "room.shutdown", id, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"roomId": id, "audited": audited})
}

func (h *handler) endMatch(w http.ResponseWriter, r *http.Request, caller auth.Caller) {
	matchID := r.PathValue("matchId")
	room, err := h.svc.EndMatch(r.Context(), matchID)
	if err != nil {
		h.fail(w, err, "match end", "caller", caller.Name, "matchId", matchID)
		return
	}
	if room == nil {
		writeError(w, http.StatusNotFound, "room_not_found")
		return
	}
	h.log.Info("match end", "caller", caller.Name, "matchId", matchID, "room", room.ID)
	audited := h.record(r.Context(), caller, "match.end", matchID, room.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"roomId": room.ID, "matchId": matchID, "audited": audited})
}

// record writes the audit entry of a GM action that has happened in the
// cluster. The action stands either way; the caller learns from the answer
// whether the audit log has it.
func (h *handler) record(ctx context.Context, caller auth.Caller, action, target, detail string) bool {
	if err := h.releases.Record(ctx, caller.Actor(), action, target, detail); err != nil {
		h.log.Error("audit failed", "caller", caller.Name, "action", action, "target", target, "error", err)
		return false
	}
	return true
}

// fail maps service errors onto status codes and error codes.
func (h *handler) fail(w http.ResponseWriter, err error, op string, attrs ...any) {
	for _, m := range []struct {
		err    error
		status int
	}{
		{allocate.ErrInvalid, http.StatusBadRequest},
		{allocate.ErrRouteNotFound, http.StatusNotFound},
		{allocate.ErrNoCapacity, http.StatusConflict},
		{allocate.ErrUnavailable, http.StatusServiceUnavailable},
	} {
		if errors.Is(err, m.err) {
			if m.status >= 500 {
				h.log.Warn(op+" failed", append(attrs, "error", err)...)
			}
			writeError(w, m.status, m.err.Error())
			return
		}
	}
	h.log.Error(op+" failed", append(attrs, "error", err)...)
	writeError(w, http.StatusInternalServerError, "internal")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends an error code and, for callers like CI that show it to a
// person, an optional message.
func writeError(w http.ResponseWriter, status int, code string, message ...string) {
	body := map[string]string{"error": code}
	if len(message) > 0 {
		body["message"] = message[0]
	}
	writeJSON(w, status, body)
}
