package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

const defaultNodeTimeout = 10 * time.Second

// Options configures a Scraper.
type Options struct {
	Log          *slog.Logger
	Registry     *Registry
	ListNodes    func() []nodes.Node
	Clients      func(ip string) (*talosclient.Client, error)
	ClientsReady func() bool

	// ScrapeInterval is the TTL for Live collectors: how often nodes are actually queried.
	ScrapeInterval time.Duration
	// InventoryInterval is the TTL for Inventory collectors.
	InventoryInterval time.Duration
	// TTLOverrides maps collector name to a TTL that overrides that collector's class-based default.
	TTLOverrides map[string]time.Duration
	// NodeTimeout bounds one node's collection round (0 → 10s); its collectors run concurrently.
	NodeTimeout time.Duration
	// Now is overridable for tests (nil → time.Now).
	Now func() time.Time
}

// Scraper runs the enabled collectors per node with a (node, collector) TTL cache; only it talks to nodes.
type Scraper struct {
	log               *slog.Logger
	registry          *Registry
	listNodes         func() []nodes.Node
	clients           func(ip string) (*talosclient.Client, error)
	clientsReady      func() bool
	scrapeInterval    time.Duration
	inventoryInterval time.Duration
	ttlOverrides      map[string]time.Duration
	nodeTimeout       time.Duration
	now               func() time.Time

	nodeUp       *prometheus.GaugeVec
	up           map[string]bool      // reachability verdict per node, guarded by mu
	lastSuccess  map[string]time.Time // last successful collection per node, guarded by mu
	scrapeErrors *prometheus.CounterVec
	collectDur   *prometheus.GaugeVec
	collections  *prometheus.CounterVec
	degraded     *prometheus.GaugeVec
	// degradedOn is which reasons are currently 1 per (node, collector), so a cleared one returns to 0.
	degradedOn map[key]map[string]bool
	refreshDur prometheus.Gauge

	mu       sync.Mutex
	cache    map[key]*entry
	inflight map[key]struct{}
}

type key struct {
	node      string
	collector string
}

// entry caches one collection; its exposition is memoised until the collection replaces it.
type entry struct {
	reg *prometheus.Registry
	ts  time.Time

	// Not sync.Once: a failed Gather must not be memoised, or the series stay absent until the next collection.
	mu   sync.Mutex
	fams []*dto.MetricFamily
	got  bool // fams is valid; set only on a successful Gather
}

// families returns the cached exposition, gathering on first use; a failed gather is not cached.
func (e *entry) families(log *slog.Logger, node, collector string) []*dto.MetricFamily {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.got {
		return e.fams
	}
	f, err := e.reg.Gather()
	if err != nil {
		log.Error("gathering cached collector data failed",
			"node", node, "collector", collector, "err", err)
		return nil
	}
	e.fams, e.got = f, true
	return e.fams
}

// NewScraper registers the scraper's self-metrics on base; the intervals must be > 0.
func NewScraper(base prometheus.Registerer, opts Options) *Scraper {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NodeTimeout <= 0 {
		opts.NodeTimeout = defaultNodeTimeout
	}
	nodeUp := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_up",
		Help: "Reachability of the node: 1 if any collector succeeded within the reachability " +
			"window, 0 otherwise. A down node exports no other metrics (its series are stale).",
	}, []string{"node"})
	scrapeErrors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_monitoring_node_scrape_errors_total",
		Help: "Total failed per-node, per-collector collections by error class " +
			"(timeout, permission, panic, other). Zero-initialised for every " +
			"(node, collector, class) once the pair has been scraped, so rate() " +
			"and alerts work on a cluster that has never failed.",
	}, []string{"node", "collector", "err"})
	// Collection duration and counts: the only view of the load this exporter puts on the Talos API.
	collectDur := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_monitoring_collector_duration_seconds",
		Help: "Wall time of the most recent collection for a (node, collector) pair, in seconds. " +
			"A gauge of the last observation rather than a histogram: the interesting question " +
			"is which collector or node is slow now, and a histogram would cost buckets per pair.",
	}, []string{"node", "collector"})
	collections := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_monitoring_collections_total",
		Help: "Total collections run per (node, collector), successful or not. " +
			"rate() gives the effective cadence, which is what a TTL override actually changed.",
	}, []string{"node", "collector"})
	refreshDur := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "talos_monitoring_refresh_duration_seconds",
		Help: "Wall time of the most recent scheduler tick that collected anything, in seconds. " +
			"Approaching the scrape interval means one replica is no longer keeping up.",
	})
	degraded := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_monitoring_collector_degraded",
		Help: "1 while a collector serves partial data for a node, 0 once it recovers. " +
			"reason=permission is a file-read path switched off because the monitor ServiceAccount " +
			"lacks os:admin; reason=inventory is a cached inventory that has not been refreshed since " +
			"its last success. Series appear on the first degradation, so a healthy cluster carries " +
			"none and an alert written == 1 cannot arm on absence of data.",
	}, []string{"node", "collector", "reason"})
	base.MustRegister(nodeUp, scrapeErrors, collectDur, collections, degraded, refreshDur)

	return &Scraper{
		log:               opts.Log,
		registry:          opts.Registry,
		listNodes:         opts.ListNodes,
		clients:           opts.Clients,
		clientsReady:      opts.ClientsReady,
		scrapeInterval:    opts.ScrapeInterval,
		inventoryInterval: opts.InventoryInterval,
		ttlOverrides:      opts.TTLOverrides,
		nodeTimeout:       opts.NodeTimeout,
		now:               opts.Now,
		nodeUp:            nodeUp,
		scrapeErrors:      scrapeErrors,
		collectDur:        collectDur,
		collections:       collections,
		degraded:          degraded,
		refreshDur:        refreshDur,
		cache:             make(map[key]*entry),
		inflight:          make(map[key]struct{}),
		degradedOn:        make(map[key]map[string]bool),
		up:                make(map[string]bool),
		lastSuccess:       make(map[string]time.Time),
	}
}

// Gatherer serves /metrics and the dashboard from the cache; never call Refresh from a request path.
type Gatherer struct {
	base    prometheus.Gatherer
	scraper *Scraper
}

// NewGatherer wraps the base registry and the Scraper into one Gatherer.
func NewGatherer(base prometheus.Gatherer, scraper *Scraper) *Gatherer {
	return &Gatherer{base: base, scraper: scraper}
}

// Gather implements prometheus.Gatherer. It reads cached data only.
func (g *Gatherer) Gather() ([]*dto.MetricFamily, error) {
	baseFamilies, err := g.base.Gather()
	if err != nil {
		return nil, err
	}
	cached, err := g.scraper.Gather()
	if err != nil {
		return nil, err
	}
	// Different owners, so a name present in both is checked for duplicate series.
	return mergeFamilies(
		source{owner: "base", families: baseFamilies},
		source{owner: "scraper", families: cached},
	), nil
}

// Gather returns the cached per-node metrics in (node, collector) order; it never triggers collection.
func (s *Scraper) Gather() ([]*dto.MetricFamily, error) {
	s.mu.Lock()
	snaps := make([]struct {
		key key
		e   *entry
	}, 0, len(s.cache))
	for k, e := range s.cache {
		snaps = append(snaps, struct {
			key key
			e   *entry
		}{k, e})
	}
	s.mu.Unlock()
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].key.node != snaps[j].key.node {
			return snaps[i].key.node < snaps[j].key.node
		}
		return snaps[i].key.collector < snaps[j].key.collector
	})

	lists := make([]source, 0, len(snaps))
	for _, sn := range snaps {
		fams := sn.e.families(s.log, sn.key.node, sn.key.collector)
		if len(fams) == 0 {
			continue
		}
		lists = append(lists, source{owner: sn.key.collector, families: fams})
	}
	return mergeFamilies(lists...), nil
}

// Refresh collects the stale (node, collector) pairs per node; the app scheduler is its only caller.
func (s *Scraper) Refresh(ctx context.Context) {
	enabled := s.registry.Enabled()
	if len(enabled) == 0 {
		return
	}
	live := s.listNodes()
	if len(live) == 0 {
		// An empty node list is almost always a not-yet-synced informer; dropping the cache would blank the UI.
		return
	}
	if !s.clientsReady() {
		s.log.Debug("talos clients not ready yet, skipping scrape round")
		return
	}

	// Most ticks have nothing due, so ask the cheap question before allocating a waitgroup and a goroutine per node.
	if !s.anyDue(live, enabled) {
		// Only walk the caches when a node has actually gone: dropGone allocates and scans every entry.
		if s.cachedNodeCount() > len(live) {
			s.dropGone(live)
		}
		return
	}

	roundStart := s.now()
	var collected atomic.Int64
	var wg sync.WaitGroup
	for i := range live {
		n := live[i]
		wg.Go(func() {
			collected.Add(int64(s.scrapeNode(ctx, n, enabled)))
		})
	}
	wg.Wait()
	s.dropGone(live)
	if n := collected.Load(); n > 0 {
		// Only rounds that collected something are timed; idle ticks would flatten the gauge.
		s.refreshDur.Set(s.now().Sub(roundStart).Seconds())
		s.log.Debug("scrape round", "nodes", len(live), "collections", n)
	}
}

// cachedNodeCount is how many distinct nodes hold cached data.
func (s *Scraper) cachedNodeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(s.up))
	for k := range s.cache {
		seen[k.node] = struct{}{}
	}
	for node := range s.up {
		seen[node] = struct{}{}
	}
	return len(seen)
}

// anyDue is the fast path for a tick with no work: one lock, no allocation.
func (s *Scraper) anyDue(live []nodes.Node, enabled []Collector) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range live {
		if n.IP == "" {
			continue
		}
		for _, c := range enabled {
			if !applies(c, n) {
				continue
			}
			k := key{node: n.Name, collector: c.Name()}
			if e, ok := s.cache[k]; ok && now.Sub(e.ts) < s.ttlFor(c) {
				continue // fresh
			}
			if _, busy := s.inflight[k]; busy {
				continue
			}
			return true
		}
	}
	return false
}

// scrapeNode collects a node's stale collectors, returning how many were claimed.
func (s *Scraper) scrapeNode(ctx context.Context, n nodes.Node, enabled []Collector) int {
	if n.IP == "" {
		return 0 // no IP assigned yet; not dialable, not an error
	}
	stale := s.claimStale(n, enabled)
	if len(stale) == 0 {
		return 0
	}

	client, err := s.clients(n.IP)
	if err != nil {
		for _, c := range stale {
			s.fail(n.Name, c.Name(), err)
		}
		s.noteRound(n.Name, false)
		return len(stale)
	}

	nc := &NodeClient{Node: n, Client: client}
	nodeCtx, cancel := context.WithTimeout(ctx, s.nodeTimeout)
	defer cancel()

	var succeeded atomic.Int32
	var wg sync.WaitGroup
	for _, c := range stale {
		wg.Add(1)
		go func(c Collector) {
			defer wg.Done()
			// A panic must not kill the process or leak its in-flight key, so it goes through fail.
			defer func() {
				if r := recover(); r != nil {
					s.fail(n.Name, c.Name(), panicError{fmt.Errorf("collector panicked: %v", r)})
				}
			}()
			sub := prometheus.NewRegistry()
			started := s.now()
			err := c.Collect(nodeCtx, nc, sub)
			// Timed either way: a collector that is slow because it times out is the one worth seeing.
			s.collectDur.WithLabelValues(n.Name, c.Name()).Set(s.now().Sub(started).Seconds())
			s.collections.WithLabelValues(n.Name, c.Name()).Inc()
			if err != nil {
				s.fail(n.Name, c.Name(), err)
				return
			}
			// A nil Collect is not the same as a complete collection: Degraded says which part is missing.
			var deg []Degradation
			if dr, ok := c.(DegradedReporter); ok {
				deg = dr.Degraded(n.Name)
			}
			s.noteDegraded(n.Name, c.Name(), deg)
			s.store(n.Name, c.Name(), sub)
			if !stopped(deg) {
				succeeded.Add(1)
			}
		}(c)
	}
	wg.Wait()
	s.noteRound(n.Name, succeeded.Load() > 0)
	return len(stale)
}

// upWindow is how long a node stays up after its last success: two Live intervals.
func (s *Scraper) upWindow() time.Duration {
	return max(2*s.scrapeInterval, 2*s.nodeTimeout)
}

// noteRound: reachability is "anything succeeded recently", not "everything due this round succeeded".
func (s *Scraper) noteRound(node string, succeeded bool) {
	now := s.now()
	s.mu.Lock()
	if succeeded {
		s.lastSuccess[node] = now
	}
	last, seen := s.lastSuccess[node]
	// A node never collected from is down, not "recently fine".
	up := seen && now.Sub(last) < s.upWindow()
	s.up[node] = up
	if !up {
		// A down node's cache is dropped so its series go stale in Prometheus instead of being re-stamped.
		for k := range s.cache {
			if k.node == node {
				delete(s.cache, k)
			}
		}
	}
	s.mu.Unlock()

	v := float64(0)
	if up {
		v = 1
	}
	s.nodeUp.WithLabelValues(node).Set(v)
}

// TTLs gives the effective cadence per enabled collector: override, then its own default, then the class default.
func (s *Scraper) TTLs() map[string]time.Duration {
	out := make(map[string]time.Duration)
	for _, c := range s.registry.Enabled() {
		out[c.Name()] = s.ttlFor(c)
	}
	return out
}

// claimStale returns the stale enabled collectors and marks them in-flight so concurrent refreshes skip them.
func (s *Scraper) claimStale(n nodes.Node, enabled []Collector) []Collector {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Collector
	for _, c := range enabled {
		k := key{node: n.Name, collector: c.Name()}
		if !applies(c, n) {
			// Never scheduled, so it neither fails nor votes on talos_node_up; data from before a role change goes too.
			delete(s.cache, k)
			continue
		}
		if e, ok := s.cache[k]; ok && now.Sub(e.ts) < s.ttlFor(c) {
			continue
		}
		if _, busy := s.inflight[k]; busy {
			continue
		}
		s.inflight[k] = struct{}{}
		out = append(out, c)
	}
	return out
}

// errClasses seeds every err label value at 0 so rate() reads as a flat line, not no data.
var errClasses = [...]string{"timeout", "permission", "panic", "other"}

// initErrors makes the error counters exist at 0 for one (node, collector).
func (s *Scraper) initErrors(node, name string) {
	for _, class := range errClasses {
		s.scrapeErrors.WithLabelValues(node, name, class)
	}
}

func (s *Scraper) store(node, name string, reg *prometheus.Registry) {
	s.initErrors(node, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key{node: node, collector: name})
	s.cache[key{node: node, collector: name}] = &entry{reg: reg, ts: s.now()}
}

func (s *Scraper) fail(node, name string, err error) {
	class := classify(err)
	s.initErrors(node, name)

	s.mu.Lock()
	delete(s.inflight, key{node: node, collector: name})
	hadCached := s.cache[key{node: node, collector: name}] != nil
	_, known := s.up[node]
	down := known && !s.up[node]
	s.mu.Unlock()

	s.scrapeErrors.WithLabelValues(node, name, class).Inc()
	switch {
	case hadCached:
		s.log.Debug("collector scrape failed, serving stale data",
			"node", node, "collector", name, "err_class", class, "err", err)
	case down:
		// Steady state for a down node: the data is gone, so Warn would spam the log for hours.
		s.log.Debug("collector scrape failed, node down, no data served",
			"node", node, "collector", name, "err_class", class, "err", err)
	default:
		s.log.Warn("collector scrape failed, no data yet",
			"node", node, "collector", name, "err_class", class, "err", err)
	}
}

// noteDegraded writes to the base registry: the per-collection sub-registry is replaced every run.
func (s *Scraper) noteDegraded(node, name string, deg []Degradation) {
	k := key{node: node, collector: name}
	active := make(map[string]bool, len(deg))
	for _, d := range deg {
		active[d.Reason] = true
	}

	s.mu.Lock()
	prev := s.degradedOn[k]
	if len(active) == 0 {
		delete(s.degradedOn, k)
	} else {
		s.degradedOn[k] = active
	}
	for r := range prev {
		if !active[r] {
			s.degraded.WithLabelValues(node, name, r).Set(0)
			s.log.Info("collector partial data cleared",
				"node", node, "collector", name, "reason", r)
		}
	}
	for r := range active {
		s.degraded.WithLabelValues(node, name, r).Set(1)
		if !prev[r] {
			s.log.Warn("collector serving partial data",
				"node", node, "collector", name, "reason", r, "hint", degradedHint(r))
		}
	}
	s.mu.Unlock()
}

// degradedHint is the remediation for one reason; an inventory gap is not fixed by a role.
func degradedHint(reason string) string {
	if reason == ReasonPermission {
		return "the monitor ServiceAccount needs the os:admin role (Helm value talos.roles); " +
			"a granted role is picked up within " + PermissionRetry.String() + ", no restart"
	}
	return "the cached inventory is re-served while its refresh keeps failing; see the scrape errors for this collector"
}

// stopped is true when the collector made no RPC, so the round must not vote on talos_node_up.
func stopped(deg []Degradation) bool {
	for _, d := range deg {
		if d.Stopped {
			return true
		}
	}
	return false
}

// NodeUp is the single source of reachability truth: any collector succeeded within upWindow.
func (s *Scraper) NodeUp(node string) (up, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	up, known = s.up[node]
	return up, known
}

// dropGone removes cached and in-flight data for nodes that left the cluster.
func (s *Scraper) dropGone(live []nodes.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()

	liveSet := make(map[string]bool, len(live))
	for _, n := range live {
		liveSet[n.Name] = true
	}
	for k := range s.cache {
		if !liveSet[k.node] {
			delete(s.cache, k)
		}
	}
	for k := range s.inflight {
		if !liveSet[k.node] {
			delete(s.inflight, k)
		}
	}
	for node := range s.up {
		if !liveSet[node] {
			delete(s.up, node)
			delete(s.lastSuccess, node)
			s.nodeUp.DeleteLabelValues(node)
			// The base registry holds per-node self-metric series the cache drop never touches, so delete them here.
			s.scrapeErrors.DeletePartialMatch(prometheus.Labels{"node": node})
			s.collectDur.DeletePartialMatch(prometheus.Labels{"node": node})
			s.collections.DeletePartialMatch(prometheus.Labels{"node": node})
			s.degraded.DeletePartialMatch(prometheus.Labels{"node": node})
			for k := range s.degradedOn {
				if k.node == node {
					delete(s.degradedOn, k)
				}
			}
		}
	}
}

// ttlFor: explicit per-collector override, then the collector's own default, then the class default.
func (s *Scraper) ttlFor(c Collector) time.Duration {
	if ttl, ok := s.ttlOverrides[c.Name()]; ok && ttl > 0 {
		return ttl
	}
	if d, ok := c.(DefaultTTLer); ok {
		if ttl := d.DefaultTTL(); ttl > 0 {
			return ttl
		}
	}
	if c.Class() == Inventory {
		return s.inventoryInterval
	}
	return s.scrapeInterval
}

// panicError wraps a recovered collector panic so classify can label it apart from "other".
type panicError struct{ error }

// classify maps a collection error to a low-cardinality label value.
func classify(err error) string {
	if _, ok := errors.AsType[panicError](err); ok {
		return "panic" // its own class: a bug here is not a cluster problem
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if IsPermissionError(err) {
		return "permission"
	}
	return "other"
}
