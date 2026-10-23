package clusters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/sites"
)

// Interval is how often the loop runs when nothing asks it to run sooner.
const Interval = 15 * time.Second

// Kick makes the loop run soon.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run keeps every cluster's Secrets current and finishes retirements until ctx ends.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("clusters loop", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.kick:
		}
	}
}

// Reconcile makes one pass over every cluster: it writes each game's runtime
// settings, and for every retiring machine deletes its idle game servers and,
// once no match runs there any more, removes it from the cluster.
func (s *Service) Reconcile(ctx context.Context) error {
	var errs []error
	for _, site := range s.sites.All() {
		if err := s.applyVars(ctx, site.Cluster); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: runtime settings: %w", site.Name, err))
		}
		if err := s.retire(ctx, site); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", site.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) retire(ctx context.Context, site sites.Site) error {
	nodes, err := site.Cluster.Nodes(ctx)
	if err != nil {
		return err
	}
	var rooms []roomOnNode
	for _, n := range nodes {
		if n.Retiring.IsZero() || controlPlane(n) {
			continue
		}
		if rooms == nil {
			all, err := site.Cluster.Rooms(ctx)
			if err != nil {
				return err
			}
			rooms = []roomOnNode{}
			for _, r := range all {
				rooms = append(rooms, roomOnNode{id: r.ID, node: r.Node, state: r.State, matches: len(r.Matches)})
			}
		}
		playing := 0
		for _, r := range rooms {
			if r.node != n.Name {
				continue
			}
			if (r.state == "Allocated" || r.state == "Reserved") && r.matches > 0 {
				// Its matches finish here, but it takes no new ones.
				if err := site.Cluster.MarkRetiring(ctx, r.id, true); err != nil {
					return err
				}
				playing++
				continue
			}
			// Idle game servers, and running ones whose matches are all over,
			// go now; their Fleet starts new ones elsewhere.
			if err := site.Cluster.Shutdown(ctx, r.id); err != nil {
				return err
			}
		}
		if playing > 0 {
			continue
		}
		if err := site.Cluster.DeleteNode(ctx, n.Name); err != nil {
			return err
		}
		s.log.Info("node removed", "cluster", site.Name, "node", n.Name)
		if err := s.audit(ctx, "gamefleet", "node.remove", n.Name+" @ "+site.Name,
			fmt.Sprintf("下线 %s 后没有对局了", s.now().Sub(n.Retiring).Round(time.Second))); err != nil {
			return err
		}
	}
	return nil
}

type roomOnNode struct {
	id, node, state string
	matches         int
}
