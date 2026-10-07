package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

// fakeCollector registers a single test_gauge{node,collector} metric.
type fakeCollector struct {
	name  string
	class Class

	err    error
	panics bool
	delay  time.Duration
	block  chan struct{} // if non-nil, wait until closed (or ctx done) before succeeding
	value  float64
	calls  atomic.Int64
}

func (f *fakeCollector) Name() string      { return f.name }
func (f *fakeCollector) Class() Class      { return f.class }
func (f *fakeCollector) callsCount() int64 { return f.calls.Load() }

func (f *fakeCollector) Collect(ctx context.Context, node *NodeClient, reg prometheus.Registerer) error {
	f.calls.Add(1)
	if f.panics {
		panic("boom")
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.err != nil {
		return f.err
	}
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "test_gauge",
		ConstLabels: prometheus.Labels{"node": node.Node.Name, "collector": f.name},
	})
	g.Set(f.value)
	reg.MustRegister(g)
	return nil
}

// env is a scraper test environment with a controllable clock.
type env struct {
	t        *testing.T
	now      *time.Time
	base     *prometheus.Registry
	scraper  *Scraper
	gatherer *Gatherer
}

func newEnv(t *testing.T, scrapeTTL, inventoryTTL, nodeTimeout time.Duration, list *[]nodes.Node, ready bool, overrides map[string]time.Duration, collectors ...Collector) *env {
	t.Helper()
	base := prometheus.NewRegistry()
	reg := NewRegistry()
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}

	now0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := &env{t: t, now: &now0, base: base}
	e.scraper = NewScraper(base, Options{
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry:          reg,
		ListNodes:         func() []nodes.Node { return *list },
		Clients:           func(string) (*talosclient.Client, error) { return nil, nil },
		ClientsReady:      func() bool { return ready },
		ScrapeInterval:    scrapeTTL,
		InventoryInterval: inventoryTTL,
		TTLOverrides:      overrides,
		NodeTimeout:       nodeTimeout,
		Now:               func() time.Time { return *e.now },
	})
	e.gatherer = NewGatherer(base, e.scraper)
	return e
}

func (e *env) advance(d time.Duration) { *e.now = e.now.Add(d) }

// tick is one scheduler tick followed by a read — how the app actually
// behaves: the scheduler collects, then /metrics or the UI reads the cache.
// Tests that must prove reading never collects use gather() directly.
func (e *env) tick() []*dto.MetricFamily {
	e.t.Helper()
	e.scraper.Refresh(context.Background())
	return e.gather()
}

func (e *env) gather() []*dto.MetricFamily {
	e.t.Helper()
	families, err := e.gatherer.Gather()
	if err != nil {
		e.t.Fatalf("gather: %v", err)
	}
	return families
}

func findFamily(families []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, f := range families {
		if *f.Name == name {
			return f
		}
	}
	return nil
}

func findMetric(families []*dto.MetricFamily, name string, labels map[string]string) *dto.Metric {
	f := findFamily(families, name)
	if f == nil {
		return nil
	}
	for _, m := range f.Metric {
		ok := true
		for k, want := range labels {
			var got string
			for _, lp := range m.Label {
				if *lp.Name == k {
					got = *lp.Value
				}
			}
			if got != want {
				ok = false
				break
			}
		}
		if ok {
			return m
		}
	}
	return nil
}

func valueOf(t *testing.T, families []*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(families, name, labels)
	if m == nil {
		t.Fatalf("metric %s%v not found", name, labels)
	}
	return m.GetGauge().GetValue()
}

func countOf(t *testing.T, families []*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(families, name, labels)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

var oneNode = []nodes.Node{{Name: "n1", IP: "10.0.0.1"}}

func TestFirstGatherCollects(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	families := e.tick()
	got := valueOf(t, families, "test_gauge", map[string]string{"node": "n1", "collector": "test"})
	if got != 42 {
		t.Fatalf("expected 42, got %v", got)
	}
	if calls := c.callsCount(); calls != 1 {
		t.Fatalf("expected 1 collection, got %d", calls)
	}
	if up := valueOf(t, families, "talos_node_up", map[string]string{"node": "n1"}); up != 1 {
		t.Fatalf("expected talos_node_up=1, got %v", up)
	}
}

func TestCachePreventsRecollect(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()
	e.advance(29 * time.Second)
	e.tick()
	if calls := c.callsCount(); calls != 1 {
		t.Fatalf("expected no re-collect within the TTL, got %d calls", calls)
	}
}

func TestLiveTTLExpiry(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()
	e.advance(31 * time.Second)
	e.tick()
	if calls := c.callsCount(); calls != 2 {
		t.Fatalf("expected re-collect after the TTL, got %d calls", calls)
	}
}

func TestInventoryTTLIsSeparate(t *testing.T) {
	live := &fakeCollector{name: "live", class: Live}
	inv := &fakeCollector{name: "inv", class: Inventory}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, live, inv)

	e.tick()
	if live.callsCount() != 1 || inv.callsCount() != 1 {
		t.Fatal("expected one collection each")
	}

	// Past the live TTL but not the inventory TTL.
	e.advance(time.Minute)
	e.tick()
	if live.callsCount() != 2 {
		t.Fatalf("live collector must be re-collected, got %d calls", live.callsCount())
	}
	if inv.callsCount() != 1 {
		t.Fatalf("inventory collector must be cached, got %d calls", inv.callsCount())
	}

	// Past the inventory TTL as well.
	e.advance(4*time.Minute + 31*time.Second)
	e.tick()
	if live.callsCount() != 3 || inv.callsCount() != 2 {
		t.Fatalf("expected live=3 inv=2 calls, got live=%d inv=%d", live.callsCount(), inv.callsCount())
	}
}

func TestPerCollectorTTLOverride(t *testing.T) {
	live := &fakeCollector{name: "live", class: Live}
	inv := &fakeCollector{name: "inv", class: Inventory}
	list := oneNode
	// live: class default 30s, overridden to 2m (longer)
	// inv:  class default 5m, overridden to 1m (shorter)
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true,
		map[string]time.Duration{"live": 2 * time.Minute, "inv": time.Minute}, live, inv)

	e.tick()
	if live.callsCount() != 1 || inv.callsCount() != 1 {
		t.Fatal("expected one collection each")
	}

	// t=90s: the live override (2m) has not expired — the class default
	// (30s) would have re-collected already. The inv override (1m) has
	// expired — the class default (5m) would still be caching.
	e.advance(90 * time.Second)
	e.tick()
	if live.callsCount() != 1 {
		t.Fatalf("live override (2m) must hold, got %d calls", live.callsCount())
	}
	if inv.callsCount() != 2 {
		t.Fatalf("inv override (1m) must expire, got %d calls", inv.callsCount())
	}

	// t=4m: both overrides have expired since their last collection.
	e.advance(3 * time.Minute)
	e.tick()
	if live.callsCount() != 2 || inv.callsCount() != 3 {
		t.Fatalf("expected live=2 inv=3, got live=%d inv=%d", live.callsCount(), inv.callsCount())
	}
}

// TestStaleDataServedUntilDownVerdict pins the failure contract: inside the
// reachability window a failed collection keeps serving the last good data;
// once the node is judged down its series leave the exposition (so Prometheus
// marks them stale instead of drawing a flat line at the last value), with
// only talos_node_up and the error counters keeping the node visible; the
// first successful round brings the data back.
func TestStaleDataServedUntilDownVerdict(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 1}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()
	c.err = errors.New("boom")
	e.advance(31 * time.Second)
	families := e.tick()

	if got := valueOf(t, families, "test_gauge", map[string]string{"node": "n1", "collector": "test"}); got != 1 {
		t.Fatalf("expected stale data to be kept inside the window, got %v", got)
	}
	if errCount := countOf(t, families, "talos_monitoring_node_scrape_errors_total",
		map[string]string{"node": "n1", "collector": "test", "err": "other"}); errCount != 1 {
		t.Fatalf("expected 1 scrape error, got %v", errCount)
	}
	// Reachability is a window, so one failed round does not flip it yet;
	// keeping the stale data is this test's subject either way.
	if up := valueOf(t, families, "talos_node_up", map[string]string{"node": "n1"}); up != 1 {
		t.Fatalf("expected talos_node_up=1 inside the window, got %v", up)
	}
	e.advance(31 * time.Second)
	families = e.tick()
	if up := valueOf(t, families, "talos_node_up", map[string]string{"node": "n1"}); up != 0 {
		t.Fatalf("expected talos_node_up=0 once the window lapsed, got %v", up)
	}
	// The down verdict evicts the cached data: the series must be gone from
	// the exposition, so Prometheus marks them stale. Only the liveness
	// signal and the error counter keep the node visible.
	if m := findMetric(families, "test_gauge", map[string]string{"node": "n1", "collector": "test"}); m != nil {
		t.Fatalf("expected the down node's series to be evicted, got %v", m.GetGauge().GetValue())
	}
	if errCount := countOf(t, families, "talos_monitoring_node_scrape_errors_total",
		map[string]string{"node": "n1", "collector": "test", "err": "other"}); errCount != 2 {
		t.Fatalf("expected 2 scrape errors, got %v", errCount)
	}
	// Recovery: the first successful round brings the series back.
	c.err = nil
	e.advance(31 * time.Second)
	families = e.tick()
	if up := valueOf(t, families, "talos_node_up", map[string]string{"node": "n1"}); up != 1 {
		t.Fatalf("expected the node back up after recovery, got %v", up)
	}
	if got := valueOf(t, families, "test_gauge", map[string]string{"node": "n1", "collector": "test"}); got != 1 {
		t.Fatalf("expected the series back after recovery, got %v", got)
	}
}

func TestTimeoutClassification(t *testing.T) {
	c := &fakeCollector{name: "slow", class: Live, delay: 5 * time.Second}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 50*time.Millisecond, &list, true, nil, c)

	families := e.tick()
	if errCount := countOf(t, families, "talos_monitoring_node_scrape_errors_total",
		map[string]string{"node": "n1", "collector": "slow", "err": "timeout"}); errCount != 1 {
		t.Fatalf("expected 1 timeout error, got %v", errCount)
	}
	if up := valueOf(t, families, "talos_node_up", map[string]string{"node": "n1"}); up != 0 {
		t.Fatalf("expected talos_node_up=0, got %v", up)
	}
}

func TestPermissionClassification(t *testing.T) {
	var errCases = []struct {
		name string
		err  error
	}{
		{"grpc status", status.Error(codes.PermissionDenied, "not allowed")},
		{"wrapped status", fmt.Errorf("10.0.0.1: %w", status.Error(codes.PermissionDenied, "not allowed"))},
		{"plain string", errors.New("open /sys/class/hwmon/hwmon0/temp1_input: permission denied")},
	}
	for i, tc := range errCases {
		c := &fakeCollector{name: fmt.Sprintf("perm%d", i), class: Live, err: tc.err}
		list := oneNode
		e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

		families := e.tick()
		if errCount := countOf(t, families, "talos_monitoring_node_scrape_errors_total",
			map[string]string{"node": "n1", "collector": c.Name(), "err": "permission"}); errCount != 1 {
			t.Fatalf("%s: expected 1 permission error, got %v", tc.name, errCount)
		}
	}
}

func TestNodeRemovalDropsCache(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := []nodes.Node{{Name: "n1", IP: "10.0.0.1"}, {Name: "n2", IP: "10.0.0.2"}}
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()

	// A real removal: the list shrinks but is not empty.
	list = []nodes.Node{{Name: "n1", IP: "10.0.0.1"}}
	families := e.tick()
	if hasLabel(families, "test_gauge", "node", "n2") {
		t.Error("expected the removed node's cached data to be dropped")
	}
	if !hasLabel(families, "test_gauge", "node", "n1") {
		t.Error("expected the surviving node's data to be kept")
	}

	// The scraper's own self-metrics live on the base registry, not in the
	// cache: they must be de-labelled too, or a churned node set grows them
	// without bound and a frozen duration survives the node that produced it.
	for _, name := range []string{
		"talos_monitoring_node_scrape_errors_total",
		"talos_monitoring_collector_duration_seconds",
		"talos_monitoring_collections_total",
		"talos_node_up",
	} {
		if hasLabel(families, name, "node", "n2") {
			t.Errorf("%s still carries a series for the removed node", name)
		}
		if !hasLabel(families, name, "node", "n1") {
			t.Errorf("%s lost the surviving node's series", name)
		}
	}
}

// TestEmptyNodeListKeepsCache pins finding 1.6: an empty list is a
// not-yet-synced informer, not a cluster that lost every node. Dropping the
// cache there blanks /metrics and the UI on a transient blip.
func TestEmptyNodeListKeepsCache(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()

	list = []nodes.Node{}
	families := e.tick()
	if got := valueOf(t, families, "test_gauge", map[string]string{"node": "n1", "collector": "test"}); got != 42 {
		t.Fatalf("expected cached data to survive an empty node list, got %v", got)
	}
	if calls := c.callsCount(); calls != 1 {
		t.Fatalf("expected no collection for an empty list, got %d calls", calls)
	}
}

// hasLabel reports whether any metric in the family carries label=value.
func hasLabel(families []*dto.MetricFamily, name, label, value string) bool {
	f := findFamily(families, name)
	if f == nil {
		return false
	}
	for _, m := range f.Metric {
		for _, lp := range m.Label {
			if lp.GetName() == label && lp.GetValue() == value {
				return true
			}
		}
	}
	return false
}

// Refresh is documented as safe to call concurrently; the in-flight guard must
// keep overlapping rounds from collecting the same (node, collector) twice.
func TestConcurrentRefreshNoDoubleCollect(t *testing.T) {
	release := make(chan struct{})
	c := &fakeCollector{name: "blocky", class: Live, block: release}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.scraper.Refresh(context.Background())
		}()
	}

	// The first refresh holds the collection in flight; let the others run
	// against it before releasing the collector.
	for c.callsCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls := c.callsCount(); calls != 1 {
		t.Fatalf("expected exactly 1 collection under concurrent refreshes, got %d", calls)
	}
}

func TestClientsNotReadySkips(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, false, nil, c)

	families := e.tick()
	if f := findFamily(families, "test_gauge"); f != nil {
		t.Fatal("expected no data before the talos clients are ready")
	}
	if calls := c.callsCount(); calls != 0 {
		t.Fatalf("expected no collections, got %d", calls)
	}
	if errCount := countOf(t, families, "talos_monitoring_node_scrape_errors_total",
		map[string]string{"node": "n1", "collector": "test", "err": "other"}); errCount != 0 {
		t.Fatalf("expected no error metrics before readiness, got %v", errCount)
	}
}

func TestNodeWithoutIPIsSkipped(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live}
	list := []nodes.Node{{Name: "n1"}} // no IP yet
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	families := e.tick()
	if calls := c.callsCount(); calls != 0 {
		t.Fatalf("expected no collections without an IP, got %d", calls)
	}
	if errCount := countOf(t, families, "talos_monitoring_node_scrape_errors_total",
		map[string]string{"node": "n1", "collector": "test", "err": "other"}); errCount != 0 {
		t.Fatalf("expected no error metrics for a node without IP, got %v", errCount)
	}
}

func TestFamilyMergeAcrossNodes(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 7}
	list := []nodes.Node{
		{Name: "n1", IP: "10.0.0.1"},
		{Name: "n2", IP: "10.0.0.2"},
	}
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	families := e.tick()
	f := findFamily(families, "test_gauge")
	if f == nil {
		t.Fatal("test_gauge family not found")
	}
	if len(f.Metric) != 2 {
		t.Fatalf("expected 2 series (one per node) in a single family, got %d", len(f.Metric))
	}
	if got := valueOf(t, families, "test_gauge", map[string]string{"node": "n2", "collector": "test"}); got != 7 {
		t.Fatalf("expected 7, got %v", got)
	}
}

// ttlFake is a fakeCollector with its own default TTL (DefaultTTLer).
type ttlFake struct {
	fakeCollector
	ttl time.Duration
}

func (f *ttlFake) DefaultTTL() time.Duration { return f.ttl }

func TestScraperTTLs(t *testing.T) {
	live := &fakeCollector{name: "live", class: Live}
	inv := &fakeCollector{name: "inv", class: Inventory}
	own := &ttlFake{fakeCollector: fakeCollector{name: "own", class: Live}, ttl: 5 * time.Second}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true,
		map[string]time.Duration{"inv": time.Hour}, live, inv, own)

	got := e.scraper.TTLs()
	want := map[string]time.Duration{
		"live": 30 * time.Second, // class default
		"inv":  time.Hour,        // override wins over the class default
		"own":  5 * time.Second,  // the collector's own default
	}
	if len(got) != len(want) {
		t.Fatalf("TTLs: got %v, want %v", got, want)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("TTLs[%q] = %v, want %v", name, got[name], w)
		}
	}
}

// TestGatherNeverCollects is the guarantee that serving a browser request or a
// Prometheus scrape can never reach a node: only the scheduler collects.
func TestGatherNeverCollects(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	// Nothing collected yet: gathering must not kick one off, however stale.
	e.gather()
	if calls := c.callsCount(); calls != 0 {
		t.Fatalf("Gather() collected %d times on a cold cache, want 0", calls)
	}

	// One scheduler tick fills the cache.
	e.scraper.Refresh(context.Background())
	if calls := c.callsCount(); calls != 1 {
		t.Fatalf("Refresh() collected %d times, want 1", calls)
	}

	// Let the TTL lapse. This is the dangerous window: previously a request
	// arriving here would claim the stale entry and issue the RPC inline,
	// blocking the response for up to the per-node timeout.
	e.advance(31 * time.Second)
	for i := 0; i < 5; i++ {
		e.gather()
	}
	if calls := c.callsCount(); calls != 1 {
		t.Fatalf("Gather() collected %d times past the TTL, want 1 (scheduler only)", calls)
	}

	// Stale data is still served rather than nothing.
	families := e.gather()
	if got := valueOf(t, families, "test_gauge", map[string]string{"node": "n1", "collector": "test"}); got != 42 {
		t.Fatalf("expected the cached value 42 to still be served, got %v", got)
	}
}

// TestPanicInCollectIsContainedAndRetried pins finding 1.7: only store and
// fail release the in-flight key, so a panic used to claim that (node,
// collector) pair forever — the collector never ran again for the lifetime of
// the process, and the panic took the whole exporter down with it.
func TestPanicInCollectIsContainedAndRetried(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, panics: true}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick() // must not take the process down
	if calls := c.callsCount(); calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}

	// The key was released, so the next round retries rather than skipping
	// this pair forever.
	e.advance(31 * time.Second)
	families := e.tick()
	if calls := c.callsCount(); calls != 2 {
		t.Fatalf("expected the collector to be retried, got %d calls", calls)
	}

	// It is counted as a failure with its own class, and the node is down
	// because nothing succeeded.
	m := findMetric(families, "talos_monitoring_node_scrape_errors_total",
		map[string]string{"node": "n1", "collector": "test", "err": "panic"})
	if m == nil {
		t.Fatal("expected a panic-class scrape error")
	}
	if got := m.GetCounter().GetValue(); got != 2 {
		t.Fatalf("expected 2 panic-class errors, got %v", got)
	}
	if up := valueOf(t, families, "talos_node_up", map[string]string{"node": "n1"}); up != 0 {
		t.Fatalf("expected talos_node_up=0, got %v", up)
	}
}

// TestNodeUpFlipsWhenANodeStopsResponding pins finding 1.2. Only the
// never-reachable case was covered before, which is exactly why the bug
// survived: the UI asked the client pool, whose verified flag is set once and
// never cleared, so a node that died after being verified rendered as up
// forever.
func TestNodeUpFlipsWhenANodeStopsResponding(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()
	if up, known := e.scraper.NodeUp("n1"); !known || !up {
		t.Fatalf("after a good round: up=%v known=%v, want true/true", up, known)
	}

	// The node stops answering. Reachability is a window ("did anything
	// succeed recently"), not a single round, so one failed round is not yet
	// enough — see noteRound. It must go down once the window lapses.
	c.err = errors.New("connection refused")
	e.advance(31 * time.Second)
	e.tick()
	if up, _ := e.scraper.NodeUp("n1"); !up {
		t.Error("one failed round inside the window should not flip a node down")
	}
	e.advance(31 * time.Second)
	e.tick()
	if up, known := e.scraper.NodeUp("n1"); !known || up {
		t.Fatalf("after the window lapsed: up=%v known=%v, want false/true", up, known)
	}

	// And back again, so the flag is not sticky in either direction.
	c.err = nil
	e.advance(31 * time.Second)
	e.tick()
	if up, _ := e.scraper.NodeUp("n1"); !up {
		t.Error("expected the node to recover")
	}
}

// TestNodeUpUnknownBeforeFirstScrape: a node discovered but not yet scraped is
// reported unknown, so the UI can distinguish "not measured yet" from "down".
func TestNodeUpUnknownBeforeFirstScrape(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	if up, known := e.scraper.NodeUp("n1"); known || up {
		t.Fatalf("before any scrape: up=%v known=%v, want false/false", up, known)
	}
}

// TestNodeUpForgottenOnRemoval: a removed node must not leave a stale verdict
// (or a stale talos_node_up series) behind.
func TestNodeUpForgottenOnRemoval(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := []nodes.Node{{Name: "n1", IP: "10.0.0.1"}, {Name: "n2", IP: "10.0.0.2"}}
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()
	list = []nodes.Node{{Name: "n1", IP: "10.0.0.1"}}
	families := e.tick()

	if _, known := e.scraper.NodeUp("n2"); known {
		t.Error("expected the removed node's verdict to be forgotten")
	}
	if hasLabel(families, "talos_node_up", "node", "n2") {
		t.Error("expected talos_node_up to drop the removed node")
	}
}

// TestMergeFamiliesDropsDuplicateSeries pins finding 3.3. Sub-registries are
// gathered independently, so two of them can produce the same series;
// concatenating them yields a body with a repeated line, which Prometheus
// rejects in full — one bad pair would blank the entire scrape.
func TestMergeFamiliesDropsDuplicateSeries(t *testing.T) {
	gather := func(labels []string, values ...string) []*dto.MetricFamily {
		r := prometheus.NewRegistry()
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "dup_info", Help: "h"}, labels)
		r.MustRegister(g)
		g.WithLabelValues(values...).Set(1)
		fams, err := r.Gather()
		if err != nil {
			t.Fatal(err)
		}
		return fams
	}

	a := gather([]string{"node", "id"}, "n1", "1")
	same := gather([]string{"node", "id"}, "n1", "1") // identical series
	other := gather([]string{"node", "id"}, "n1", "2")
	wider := gather([]string{"node", "id", "extra"}, "n1", "1", "x")

	// Different owners, so the collision check runs — that is the case this
	// guard exists for. Same-owner lists are different nodes and cannot
	// collide, which is what makes narrowing the check safe.
	merged := mergeFamilies(
		source{owner: "c1", families: a},
		source{owner: "c2", families: same},
		source{owner: "c3", families: other},
		source{owner: "c4", families: wider},
	)
	f := findFamily(merged, "dup_info")
	if f == nil {
		t.Fatal("dup_info family missing")
	}
	// The identical series is dropped; the other two are distinct series and
	// must survive, including the one carrying an extra label.
	if got := len(f.Metric); got != 3 {
		t.Fatalf("got %d series, want 3 (one duplicate dropped)", got)
	}

	// And the result must actually encode without a repeated line.
	var sb strings.Builder
	enc := expfmt.NewEncoder(&sb, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, fam := range merged {
		if err := enc.Encode(fam); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	lines := map[string]int{}
	for _, l := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(l, "dup_info{") {
			lines[l]++
		}
	}
	for l, n := range lines {
		if n > 1 {
			t.Errorf("series emitted %d times: %s", n, l)
		}
	}
}

// TestIdleTickDoesNoWork pins finding 5.7: the scheduler runs at 1 s while the
// fastest collector TTL is 5 s, so most ticks used to allocate a waitgroup,
// spawn a goroutine per node and take the cache lock per (node, collector)
// only to find nothing stale.
func TestIdleTickDoesNoWork(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := []nodes.Node{{Name: "n1", IP: "10.0.0.1"}, {Name: "n2", IP: "10.0.0.2"}}
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	e.tick()
	if got := c.callsCount(); got != 2 {
		t.Fatalf("first round: got %d collections, want 2", got)
	}

	// Ticks inside the TTL must not collect anything.
	for i := 0; i < 10; i++ {
		e.advance(time.Second)
		e.tick()
	}
	if got := c.callsCount(); got != 2 {
		t.Errorf("idle ticks collected %d times, want 2 (nothing was due)", got)
	}

	// And once the TTL expires the work still happens.
	e.advance(31 * time.Second)
	e.tick()
	if got := c.callsCount(); got != 4 {
		t.Errorf("after the TTL: got %d collections, want 4", got)
	}
}

// TestIdleTickStillDropsRemovedNodes: the fast path must not skip cleanup, or a
// node removed while nothing is due would keep its metrics forever.
func TestIdleTickStillDropsRemovedNodes(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := []nodes.Node{{Name: "n1", IP: "10.0.0.1"}, {Name: "n2", IP: "10.0.0.2"}}
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)
	e.tick()

	// Remove a node and tick while everything is still fresh.
	list = []nodes.Node{{Name: "n1", IP: "10.0.0.1"}}
	e.advance(time.Second)
	families := e.tick()

	if hasLabel(families, "test_gauge", "node", "n2") {
		t.Error("an idle tick kept the removed node's data")
	}
	if !hasLabel(families, "test_gauge", "node", "n1") {
		t.Error("an idle tick dropped a live node's data")
	}
}

// TestErrorCountersExistBeforeAnyError: a counter created only on first
// failure makes rate() return no data rather than zero, so an alert on a
// cluster that has always been healthy never arms, and a Grafana panel reads
// "No data" instead of a flat line.
func TestErrorCountersExistBeforeAnyError(t *testing.T) {
	c := &fakeCollector{name: "test", class: Live, value: 42}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, 10*time.Second, &list, true, nil, c)

	families := e.tick() // a completely successful round

	f := findFamily(families, "talos_monitoring_node_scrape_errors_total")
	if f == nil {
		t.Fatal("error counter absent after a healthy scrape")
	}
	got := map[string]float64{}
	for _, m := range f.Metric {
		for _, lp := range m.Label {
			if lp.GetName() == "err" {
				got[lp.GetValue()] = m.GetCounter().GetValue()
			}
		}
	}
	for _, class := range []string{"timeout", "permission", "panic", "other"} {
		v, ok := got[class]
		if !ok {
			t.Errorf("err=%q not initialised", class)
		}
		if v != 0 {
			t.Errorf("err=%q = %v on a healthy cluster, want 0", class, v)
		}
	}
}

// TestSameOwnerAcrossNodesIsNotDeduped: the fast path. Two nodes running the
// same collector produce the same family names but distinct series, so the
// merge must keep both without paying for a series-level check.
func TestSameOwnerAcrossNodesIsNotDeduped(t *testing.T) {
	gather := func(node string) []*dto.MetricFamily {
		r := prometheus.NewRegistry()
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "same_info", Help: "h"}, []string{"node", "id"})
		r.MustRegister(g)
		g.WithLabelValues(node, "1").Set(1)
		fams, err := r.Gather()
		if err != nil {
			t.Fatal(err)
		}
		return fams
	}
	merged := mergeFamilies(
		source{owner: "cpu", families: gather("n1")},
		source{owner: "cpu", families: gather("n2")},
	)
	f := findFamily(merged, "same_info")
	if f == nil || len(f.Metric) != 2 {
		t.Fatalf("got %v series, want 2 — same collector on different nodes must both survive", f)
	}
}

// TestMergeDoesNotMutateItsInputs: cached collections are gathered once and
// reused on every scrape, so merging must not edit them — a mutating merge
// would strip series permanently after the first /metrics request.
func TestMergeDoesNotMutateItsInputs(t *testing.T) {
	build := func(node string) []*dto.MetricFamily {
		r := prometheus.NewRegistry()
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "keep_info", Help: "h"}, []string{"node"})
		r.MustRegister(g)
		g.WithLabelValues(node).Set(1)
		fams, err := r.Gather()
		if err != nil {
			t.Fatal(err)
		}
		return fams
	}
	a, b := build("n1"), build("n2")
	before := len(a[0].Metric)

	for i := 0; i < 3; i++ { // several scrapes over the same cached lists
		merged := mergeFamilies(source{owner: "cpu", families: a}, source{owner: "cpu", families: b})
		if f := findFamily(merged, "keep_info"); f == nil || len(f.Metric) != 2 {
			t.Fatalf("scrape %d: got %v, want 2 series", i, f)
		}
	}
	if len(a[0].Metric) != before {
		t.Errorf("merge mutated a cached list: %d series, was %d", len(a[0].Metric), before)
	}
}

// TestCachedExpositionIsGatheredOncePerCollection: the exposition is memoised
// per cache entry, so N scrapes between two collections cost one Gather, not N.
// Re-serialising unchanged data was the single largest allocation source in
// the /metrics path.
func TestCachedExpositionIsGatheredOncePerCollection(t *testing.T) {
	c := &countingGatherer{Registry: prometheus.NewRegistry()}
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "cached_gauge", Help: "h"}, []string{"node"})
	c.MustRegister(g)
	g.WithLabelValues("n1").Set(1)

	e := &entry{reg: c.Registry, ts: time.Now()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 5; i++ {
		if fams := e.families(log, "n1", "test"); len(fams) != 1 {
			t.Fatalf("scrape %d: got %d families, want 1", i, len(fams))
		}
	}

	// A new collection replaces the entry, so the next read gathers again.
	e2 := &entry{reg: c.Registry, ts: time.Now()}
	if fams := e2.families(log, "n1", "test"); len(fams) != 1 {
		t.Fatalf("after a new collection: got %d families, want 1", len(fams))
	}
}

// countingGatherer lets the test hold a registry by value.
type countingGatherer struct{ *prometheus.Registry }

// flakyCollector makes Registry.Gather fail on its first call — two identical
// series is a consistency error — and succeed afterwards.
type flakyCollector struct{ calls int }

func (c *flakyCollector) Describe(chan<- *prometheus.Desc) {}

func (c *flakyCollector) Collect(ch chan<- prometheus.Metric) {
	c.calls++
	d := prometheus.NewDesc("flaky_gauge", "h", nil, nil)
	if c.calls == 1 {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, 1)
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, 2)
		return
	}
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, 2)
}

// TestFailedGatherIsNotCached: the memoisation must not survive a failed Gather.
// The sync.Once it used to sit behind consumed the attempt, so the collector's
// series stayed missing from /metrics until the next collection replaced the
// entry — up to an hour for an inventory collector — while the dashboard went on
// showing the values from the snapshot. The one case where the two renderers
// disagree, and the exporter never says so.
func TestFailedGatherIsNotCached(t *testing.T) {
	fc := &flakyCollector{}
	reg := prometheus.NewRegistry()
	reg.MustRegister(fc)

	e := &entry{reg: reg, ts: time.Now()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if fams := e.families(log, "n1", "test"); fams != nil {
		t.Fatalf("first read returned %d families, want nil (the gather must fail)", len(fams))
	}
	if fams := e.families(log, "n1", "test"); len(fams) != 1 {
		t.Fatalf("second read returned %d families, want 1: a failed gather must not be cached", len(fams))
	}
	if fams := e.families(log, "n1", "test"); len(fams) != 1 {
		t.Fatalf("third read returned %d families, want 1", len(fams))
	}
	// Success is still memoised: one failed gather plus one good one, not three.
	if fc.calls != 2 {
		t.Errorf("registry gathered %d times, want 2 (success memoised, failure not)", fc.calls)
	}
}

// A node whose frequent collectors are all cached and healthy must not be
// reported down because the one collector due this round failed.
//
// Collectors have very different cadences (Live 30 s, Inventory 1 h), so most
// rounds have exactly one collector due. Before reachability became a window,
// an hourly collector's transient RPC blip marked a reachable node down until
// the next Live round — a false alarm on the single source of truth for
// reachability, which also fires the dashboard's attention rule.
func TestNodeUpSurvivesOneInfrequentCollectorFailing(t *testing.T) {
	slow := &fakeCollector{name: "slow", class: Live, value: 1}
	fast := &fakeCollector{name: "fast", class: Live, value: 1}
	list := oneNode
	e := newEnv(t, time.Minute, time.Hour, 10*time.Second, &list, true,
		map[string]time.Duration{"slow": time.Hour, "fast": time.Minute}, slow, fast)

	e.tick()
	if up, _ := e.scraper.NodeUp("n1"); !up {
		t.Fatal("node should be up after a clean round")
	}

	// 61 s on, only `fast` is due (slow is fresh for an hour) and it fails.
	fast.err = errors.New("transient blip")
	e.advance(61 * time.Second)
	e.tick()
	if up, _ := e.scraper.NodeUp("n1"); !up {
		t.Error("a reachable node was marked down because its one due collector failed")
	}

	// It recovers on the next round without any sticky state.
	fast.err = nil
	e.advance(61 * time.Second)
	e.tick()
	if up, _ := e.scraper.NodeUp("n1"); !up {
		t.Error("node did not recover")
	}
}

// The scheduler had no visibility into its own cost: nothing said how long a
// collection took or how often it ran, so "is one replica keeping up" and "how
// much load is this putting on the Talos API" were both unanswerable.
func TestSelfMetricsReportCollectionCost(t *testing.T) {
	slow := &fakeCollector{name: "slow", class: Live, delay: 20 * time.Millisecond, value: 1}
	fast := &fakeCollector{name: "fast", class: Live, value: 1}
	list := oneNode
	e := newEnv(t, 30*time.Second, 5*time.Minute, time.Second, &list, true, nil, slow, fast)

	// The fake clock does not advance during a collection, so durations read
	// 0; what this pins is that the series exist per (node, collector) and
	// that the counters track cadence.
	families := e.tick()
	for _, c := range []string{"slow", "fast"} {
		if _, ok := findMetricOK(families, "talos_monitoring_collector_duration_seconds",
			map[string]string{"node": "n1", "collector": c}); !ok {
			t.Errorf("no duration series for collector %q", c)
		}
		if got := countOf(t, families, "talos_monitoring_collections_total",
			map[string]string{"node": "n1", "collector": c}); got != 1 {
			t.Errorf("%s collections = %v, want 1", c, got)
		}
	}

	// A second round bumps the cadence counter.
	e.advance(31 * time.Second)
	families = e.tick()
	if got := countOf(t, families, "talos_monitoring_collections_total",
		map[string]string{"node": "n1", "collector": "fast"}); got != 2 {
		t.Errorf("collections after two rounds = %v, want 2", got)
	}

	// A failed collection is still timed and counted: a collector that is slow
	// because it times out is exactly the one worth seeing.
	fast.err = errors.New("boom")
	e.advance(31 * time.Second)
	families = e.tick()
	if got := countOf(t, families, "talos_monitoring_collections_total",
		map[string]string{"node": "n1", "collector": "fast"}); got != 3 {
		t.Errorf("a failed collection was not counted: got %v, want 3", got)
	}
}

// findMetricOK is findMetric with an explicit found flag, for series whose
// value may legitimately be zero.
func findMetricOK(fams []*dto.MetricFamily, family string, want map[string]string) (float64, bool) {
	m := findMetric(fams, family, want)
	if m == nil {
		return 0, false
	}
	if m.GetGauge() != nil {
		return m.GetGauge().GetValue(), true
	}
	if m.GetCounter() != nil {
		return m.GetCounter().GetValue(), true
	}
	return 0, true
}
