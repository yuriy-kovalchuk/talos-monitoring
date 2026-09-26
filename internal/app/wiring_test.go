package app

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/block"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/cpu"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/hwinfo"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/network"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/sensors"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/sysstat"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// TestWithSnapshotKeepsTheSpecialisedCollector pins a bug the type system
// allowed: MemoryCollector and PCICollector embed *Collector, so calling the
// embedded WithSnapshot returned *Collector and handed the registry the base
// type, silently dropping /proc/meminfo and the PCI sysfs enrichment. The
// shadowing methods must return the concrete type.
func TestWithSnapshotKeepsTheSpecialisedCollector(t *testing.T) {
	s := snapshot.New()
	if _, ok := any(hwinfo.NewMemory().WithSnapshot(s)).(*hwinfo.MemoryCollector); !ok {
		t.Error("NewMemory().WithSnapshot() lost the MemoryCollector type")
	}
	if _, ok := any(hwinfo.NewPCI(discardLog()).WithSnapshot(s)).(*hwinfo.PCICollector); !ok {
		t.Error("NewPCI(discardLog()).WithSnapshot() lost the PCICollector type")
	}
}

// TestEveryStatefulCollectorPrunes: each collector keyed by node name must
// implement collector.Pruner, or its cache grows without bound on a cluster
// whose node names churn. Nothing else catches this — a static cluster never
// exercises it.
func TestEveryStatefulCollectorPrunes(t *testing.T) {
	hist := history.New(time.Minute)
	snap := snapshot.New()
	stats := sysstat.New()

	stateful := []collector.Collector{
		cpu.New(discardLog(), hist, 0, false).WithSnapshot(snap).WithSysStat(stats),
		sensors.New(discardLog()).WithSnapshot(snap),
		block.New().WithSnapshot(snap),
		network.New(discardLog()).WithSnapshot(snap),
		hwinfo.NewMemory().WithSnapshot(snap),
	}
	for _, c := range stateful {
		if _, ok := c.(collector.Pruner); !ok {
			t.Errorf("%s keeps per-node state but does not implement collector.Pruner", c.Name())
		}
	}

	// And the shared caches prune too.
	stats.Prune(map[string]struct{}{"live": {}})
	snap.Prune(map[string]struct{}{"live": {}})
	hist.Prune(map[string]struct{}{"live": {}})
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testCollectorNames are the registered collector names, sorted.
var testCollectorNames = []string{
	"block", "cpu", "diskio", "gpu", "health", "identity", "kernel", "memory",
	"network", "nodeapi", "pci", "processor", "sensors", "system", "time", "volumes",
}

// New is the whole wiring: registry, collectors, scraper, stores, router. It
// had no test at all, so a mis-registered collector or a bad default only
// surfaced by running the binary against a cluster.
func TestNewWiresEveryCollectorAndAppliesDefaults(t *testing.T) {
	a, err := New(discardLog(), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Every collector registered, and the names the --collectors.<name>.enabled
	// flags accept.
	got := a.collectors.Names()
	if len(got) != len(testCollectorNames) {
		t.Fatalf("registered %d collectors %v, want %d %v", len(got), got, len(testCollectorNames), testCollectorNames)
	}
	for i := range testCollectorNames {
		if got[i] != testCollectorNames[i] {
			t.Errorf("collector %d = %q, want %q", i, got[i], testCollectorNames[i])
		}
	}

	// Zero intervals fall back to the documented defaults rather than
	// producing a scraper that re-collects on every 1s tick.
	ttls := a.scraper.TTLs()
	// A collector's own DefaultTTL wins over the class default: cpu samples
	// faster than the scrape interval, inventory far slower.
	if ttls["cpu"] >= defaultScrapeInterval {
		t.Errorf("cpu TTL = %v, want its own faster default (< %v)", ttls["cpu"], defaultScrapeInterval)
	}
	if ttls["system"] != time.Hour {
		t.Errorf("system TTL = %v, want 1h", ttls["system"])
	}
	// health is Live with no override, so it follows the scrape interval.
	if ttls["health"] != defaultScrapeInterval {
		t.Errorf("health TTL = %v, want the scrape-interval default %v", ttls["health"], defaultScrapeInterval)
	}

	// The listener is unauthenticated, so header reads get their own
	// deadline alongside ReadTimeout/WriteTimeout.
	if a.srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero; header reads need their own deadline")
	}
}

// A name that was never registered is skipped, not an error: both the flag
// set and the registry are fixed at build time, so a stale entry must not
// fail startup.
func TestNewSkipsUnregisteredCollectorNames(t *testing.T) {
	a, err := New(discardLog(), Options{CollectorEnabled: map[string]bool{"nosuchthing": false}})
	if err != nil {
		t.Fatalf("New rejected an unregistered collector name: %v", err)
	}
	if got := len(a.collectors.Enabled()); got != len(testCollectorNames) {
		t.Fatalf("enabled %d collectors, want all %d", got, len(testCollectorNames))
	}
}

// An explicit false keeps the collector registered (so Prune still reaches
// its state) while taking it out of the scrape loop; un-named collectors run.
func TestNewDisablesOnlyTheRequestedCollectors(t *testing.T) {
	decisions := make(map[string]bool, len(testCollectorNames))
	for _, name := range testCollectorNames {
		decisions[name] = true
	}
	decisions["cpu"] = false
	decisions["sensors"] = false

	a, err := New(discardLog(), Options{CollectorEnabled: decisions})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	enabled := a.collectors.Enabled()
	if len(enabled) != len(testCollectorNames)-2 {
		t.Fatalf("enabled %d collectors, want %d", len(enabled), len(testCollectorNames)-2)
	}
	for _, c := range enabled {
		if c.Name() == "cpu" || c.Name() == "sensors" {
			t.Errorf("disabled collector %q still enabled", c.Name())
		}
	}
	if got := len(a.collectors.All()); got != len(testCollectorNames) {
		t.Errorf("registered %d collectors, want %d; disabled state would never be pruned", got, len(testCollectorNames))
	}
}

// prune must not touch anything while discovery is empty: an unsynced watch
// would otherwise drop every node's cached state on startup.
func TestPruneIsANoOpBeforeDiscoverySyncs(t *testing.T) {
	a, err := New(discardLog(), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.snap.Update("ghost", func(n *snapshot.Node) { n.Runtime = &snapshot.Runtime{Version: "v1"} })

	a.prune() // node list is empty: discovery has not synced

	if a.snap.Node("ghost") == nil {
		t.Error("prune dropped cached state while the node list was empty")
	}
}

// nodeStatus reports reachability from the scraper, not the client pool: the
// pool sets its verified flag once and never clears it, so a node that died
// after being verified rendered as up forever.
func TestNodeStatusReportsUnknownNodesAsDown(t *testing.T) {
	a, err := New(discardLog(), Options{CollectorEnabled: map[string]bool{"cpu": true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	status := nodeStatus(a.talosPool, a.scraper)
	if _, up := status("never-seen"); up {
		t.Error("a node the scraper has never reached reported as up")
	}
}
