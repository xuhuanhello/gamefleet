package release

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
	"github.com/xuhuanhello/gamefleet/internal/health"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
)

// Interval is how often the loop compares the clusters with the database
// when nothing asks it to run sooner.
const Interval = 15 * time.Second

// Kick makes the loop run soon, for example right after a route changed.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run keeps every cluster in line with the database until ctx ends.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		err := s.Reconcile(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("release loop", "error", err)
		}
		s.monitor.Pass("release", err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.kick:
		}
	}
}

// Reconcile makes one pass over every cluster:
//
//   - new and serving deployments get their Fleet and an autoscaler that
//     keeps the buffer of idle game servers;
//   - draining deployments lose the autoscaler and scale to zero, and are
//     deleted once no match runs on them and the retention time has passed;
//   - Fleets GameFleet created for deployments that are gone are deleted
//     once no match runs on them.
//
// A cluster that cannot be reached is skipped; the others still get done.
func (s *Service) Reconcile(ctx context.Context) error {
	var deployments []store.Deployment
	builds := map[int64]store.Build{}
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		if deployments, err = tx.Deployments(); err != nil {
			return err
		}
		for _, d := range deployments {
			if _, ok := builds[d.BuildID]; !ok {
				if builds[d.BuildID], err = tx.Build(d.BuildID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, site := range s.sites.All() {
		var mine []store.Deployment
		for _, d := range deployments {
			if d.Cluster == site.Name {
				mine = append(mine, d)
			}
		}
		if err := s.reconcileSite(ctx, site, mine, builds); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", site.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) reconcileSite(ctx context.Context, site sites.Site, deployments []store.Deployment, builds map[int64]store.Build) error {
	fleets, err := site.Cluster.ManagedFleets(ctx)
	if err != nil {
		s.monitor.Contact(site.Name, err)
		return err
	}
	byName := map[string]cluster.ManagedFleet{}
	for _, f := range fleets {
		byName[f.Name] = f
	}
	rooms, err := site.Cluster.Rooms(ctx)
	s.monitor.Contact(site.Name, err)
	if err != nil {
		return err
	}
	s.monitor.Starting(site.Name, starting(rooms))
	// The deployments' own Fleets, by name.
	live := map[string]bool{}
	var errs []error
	for _, room := range rooms {
		if len(room.Stale) > 0 {
			if err := site.Cluster.ForgetMatches(ctx, room.ID, room.Stale); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", room.ID, err))
			}
		}
		if room.Notes > cluster.FullNotes && !room.Retiring {
			s.log.Warn("game server notes near the Kubernetes limit; it takes no new matches", "cluster", site.Name,
				"room", room.ID, "bytes", room.Notes, "stale", len(room.Stale))
			if err := site.Cluster.MarkRetiring(ctx, room.ID, true); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", room.ID, err))
			}
		}
	}
	for _, d := range deployments {
		f, exists := byName[d.Fleet]
		if exists && !owns(d, f) {
			// An older deployment's Fleet of the same name is still going away.
			continue
		}
		live[d.Fleet] = true
		var err error
		switch d.State {
		case store.StateNew, store.StateServing:
			err = errors.Join(s.keepRunning(ctx, site, d, builds[d.BuildID], f, exists), s.stopIdle(ctx, site, d.Fleet, rooms))
		case store.StateDraining:
			err = errors.Join(s.stopEmpty(ctx, site, d.Fleet, rooms), s.drain(ctx, site, d, f, exists))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Fleet, err))
		}
	}
	for _, f := range fleets {
		if live[f.Name] || f.Deleting {
			continue
		}
		if err := errors.Join(s.stopEmpty(ctx, site, f.Name, rooms), s.removeLeftover(ctx, site, f)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) keepRunning(ctx context.Context, site sites.Site, d store.Deployment, b store.Build, f cluster.ManagedFleet, exists bool) error {
	if exists && f.Deleting {
		return nil
	}
	if !exists {
		r := d.Resources
		err := site.Cluster.CreateFleet(ctx, cluster.FleetSpec{
			Name: d.Fleet, Deployment: d.ID, Build: b.ID, Game: b.Game,
			Image: s.host + "/" + b.Game + "@" + b.Digest, Port: b.Port, Protocol: b.Protocol,
			Replicas: d.Buffer, RoomsCapacity: d.RoomsCapacity, CPURequest: r.CPURequest, CPULimit: r.CPULimit,
			MemoryRequest: r.MemoryRequest, MemoryLimit: r.MemoryLimit,
		})
		if err != nil {
			return err
		}
		s.log.Info("fleet created", "cluster", site.Name, "fleet", d.Fleet, "build", b.ID)
	}
	return site.Cluster.ApplyAutoscaler(ctx, d.Fleet, d.ID, d.Buffer, d.MaxReplicas)
}

// stopEmpty stops the Fleet's game servers that run no match any more. A
// game server outlives its matches to take the next ones, so a Fleet that
// gets no new matches would otherwise keep them for ever. A rollback may route
// a match to one right then, so it first takes them out of allocation, and
// stops them on a later pass if they are still empty.
func (s *Service) stopEmpty(ctx context.Context, site sites.Site, fleet string, rooms []cluster.Room) error {
	var errs []error
	for _, room := range rooms {
		if room.Fleet != fleet || room.State != "Allocated" || len(room.Matches) > 0 {
			continue
		}
		if room.Retiring {
			errs = append(errs, site.Cluster.Shutdown(ctx, room.ID))
		} else {
			errs = append(errs, site.Cluster.MarkRetiring(ctx, room.ID, true))
		}
	}
	return errors.Join(errs...)
}

// IdleTimeout is how long a game server of a serving deployment may go
// without a new match before GameFleet stops it. Allocation fills the fullest
// game servers first, so one that got none for this long is not needed: after
// a burst the Fleet shrinks back to the idle ones its autoscaler keeps.
const IdleTimeout = 5 * time.Minute

// stopIdle stops the Fleet's empty game servers that got no match for
// IdleTimeout. Allocation may be giving one a match right then, so it first
// takes them out of allocation, and stops them on a later pass if they are
// still empty. One that got a match after all is stopped once that is over.
func (s *Service) stopIdle(ctx context.Context, site sites.Site, fleet string, rooms []cluster.Room) error {
	var errs []error
	for _, room := range rooms {
		if room.Fleet != fleet || room.State != "Allocated" || len(room.Matches) > 0 {
			continue
		}
		last := room.LastAllocated
		if last.IsZero() {
			last = room.Created
		}
		switch {
		case room.Retiring:
			errs = append(errs, site.Cluster.Shutdown(ctx, room.ID))
		case s.store.Now().Sub(last) >= IdleTimeout:
			errs = append(errs, site.Cluster.MarkRetiring(ctx, room.ID, true))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) drain(ctx context.Context, site sites.Site, d store.Deployment, f cluster.ManagedFleet, exists bool) error {
	if err := site.Cluster.DeleteAutoscaler(ctx, d.Fleet); err != nil {
		return err
	}
	if !exists {
		// Someone deleted the Fleet by hand; there is nothing left to drain.
		_, err := s.markDeleted(ctx, d, "它的 Fleet 已经不在了")
		return err
	}
	if f.Replicas != 0 {
		if err := site.Cluster.ScaleFleet(ctx, d.Fleet, 0); err != nil {
			return err
		}
	}
	if f.Allocated+f.Reserved > 0 || s.store.Now().Sub(d.Since) < s.retention {
		return nil
	}
	// Record the deletion first: a route that moved back to the build since
	// this pass read the database keeps the deployment, and once recorded no
	// route can move to it.
	deleted, err := s.markDeleted(ctx, d, fmt.Sprintf("排空 %s，没有对局了", s.store.Now().Sub(d.Since).Round(time.Minute)))
	if err != nil || !deleted {
		return err
	}
	if err := site.Cluster.DeleteFleet(ctx, d.Fleet); err != nil {
		// The next pass finds the Fleet a leftover and deletes it.
		return err
	}
	s.log.Info("fleet reclaimed", "cluster", site.Name, "fleet", d.Fleet)
	return nil
}

// markDeleted records the deletion of a draining deployment, unless a route
// used its build in the meantime: it serves again, or drains since a later
// time and gets its retention again. It reports whether it did.
func (s *Service) markDeleted(ctx context.Context, d store.Deployment, why string) (bool, error) {
	deleted := false
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		current, err := tx.Deployment(d.ID)
		if err != nil || current.State != store.StateDraining || !current.Since.Equal(d.Since) {
			return err
		}
		if err := tx.SetDeploymentState(d.ID, store.StateDeleted); err != nil {
			return err
		}
		deleted = true
		return tx.Audit("gamefleet", "deployment.reclaim", d.Fleet+" @ "+d.Cluster, why)
	})
	return deleted, err
}

func (s *Service) removeLeftover(ctx context.Context, site sites.Site, f cluster.ManagedFleet) error {
	if err := site.Cluster.DeleteAutoscaler(ctx, f.Name); err != nil {
		return err
	}
	if f.Allocated+f.Reserved > 0 {
		if f.Replicas == 0 {
			return nil
		}
		return site.Cluster.ScaleFleet(ctx, f.Name, 0)
	}
	s.log.Info("leftover fleet deleted", "cluster", site.Name, "fleet", f.Name)
	return site.Cluster.DeleteFleet(ctx, f.Name)
}

// starting lists the game servers that are not Ready yet: in the states
// Agones passes through before it.
func starting(rooms []cluster.Room) []health.Starting {
	var out []health.Starting
	for _, r := range rooms {
		switch r.State {
		case "", "PortAllocation", "Creating", "Starting", "Scheduled", "RequestReady":
			out = append(out, health.Starting{State: r.State, Created: r.Created})
		}
	}
	return out
}
