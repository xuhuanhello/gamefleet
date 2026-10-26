package sites

import (
	"fmt"
	"sync"
	"testing"
)

// Clusters that join at the same time all end up in the directory.
func TestConcurrentAddsKeepEverySite(t *testing.T) {
	d := New()
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Add(Site{Name: fmt.Sprintf("c-%02d", i), Region: fmt.Sprintf("r-%02d", i)})
		}()
	}
	wg.Wait()
	all := d.All()
	if len(all) != n {
		t.Fatalf("directory has %d sites, want %d", len(all), n)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Name >= all[i].Name {
			t.Fatalf("sites out of order: %s before %s", all[i-1].Name, all[i].Name)
		}
	}
}

func TestAddReplacesTheSiteWithTheSameName(t *testing.T) {
	d := New(Site{Name: "us-1", Region: "us"})
	d.Add(Site{Name: "us-1", Region: "us-west"})
	all := d.All()
	if len(all) != 1 || all[0].Region != "us-west" {
		t.Fatalf("sites = %+v, want only us-1 in us-west", all)
	}
}
