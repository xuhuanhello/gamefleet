// Package sites is the list of clusters GameFleet works with right now.
// Allocation, releases and the console all read it; it changes when a cluster
// joins, without a restart.
package sites

import (
	"slices"
	"strings"
	"sync"

	"github.com/xuhuanhello/gamefleet/internal/cluster"
)

// Site is one cluster and the region it serves.
type Site struct {
	Name    string
	Region  string
	Cluster cluster.API
}

// Directory holds the current sites. It is safe for concurrent use.
type Directory struct {
	mu   sync.RWMutex
	list []Site
}

// New returns a directory holding the sites.
func New(list ...Site) *Directory {
	d := &Directory{}
	d.Set(list)
	return d
}

// Set replaces every site.
func (d *Directory) Set(list []Site) {
	list = slices.Clone(list)
	slices.SortFunc(list, func(a, b Site) int { return strings.Compare(a.Name, b.Name) })
	d.mu.Lock()
	defer d.mu.Unlock()
	d.list = list
}

// Add adds a site, or replaces the one with the same name.
func (d *Directory) Add(s Site) {
	list := d.All()
	list = slices.DeleteFunc(list, func(x Site) bool { return x.Name == s.Name })
	d.Set(append(list, s))
}

// All returns the sites by name.
func (d *Directory) All() []Site {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return slices.Clone(d.list)
}

// Get returns the site with this name.
func (d *Directory) Get(name string) (Site, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.list {
		if s.Name == name {
			return s, true
		}
	}
	return Site{}, false
}

// InRegion returns the site that serves the region.
func (d *Directory) InRegion(region string) (Site, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.list {
		if s.Region == region {
			return s, true
		}
	}
	return Site{}, false
}
