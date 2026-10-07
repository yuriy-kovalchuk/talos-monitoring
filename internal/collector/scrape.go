package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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

	// ScrapeInterval is the TTL for Live collectors: how often nodes are
	// actually queried for fresh data.
	ScrapeInterval time.Duration
	// InventoryInterval is the TTL for Inventory collectors.
	InventoryInterval time.Duration
	// TTLOverrides maps collector name → explicit TTL, overriding the
	// class-based default for that collector (per-scraper intervals; wired
	// to --collectors.<name>.ttl flags in phase 3 and Helm values in phase 5).
	TTLOverrides map[string]time.Duration
	// NodeTimeout bounds one node's collection round (0 → 10s). All of a
	// node's stale collectors run concurrently under this budget.
	NodeTimeout time.Duration
	// Now is overridable for tests (nil → time.Now).
	Now func() time.Time
}

// Scraper runs the enabled collectors across all nodes with a per
// (node, collector) TTL cache.
//
// Scraping is always-on: a scheduler ticks Refresh() every second and
// claimStale only selects (node, collector) pairs past their TTL, so the
// per-collector cadence is preserved exactly (an empty tick is a map scan).
// The scheduler is the only thing that talks to nodes — /metrics and the
// dashboard are pure readers of the cache and cannot cause an RPC. A failed
// collection keeps the last good data while the node is inside the
// reachability window; once the node is judged down its cached data is
// evicted, its series leave the exposition (Prometheus marks them stale) and
// only talos_node_up and talos_monitoring_node_scrape_errors_total keep the
// node visible. A failure never fails the whole scrape.
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
	refreshDur   prometheus.Gauge

	mu       sync.Mutex
	cache    map[key]*entry
	inflight map[key]struct{}
}

type key struct {
	node      string
	collector string
}

// entry is one cached collection.
//
// The exposition is memoised: gathered on first read and reused until the
// collector runs again and replaces the entry. Re-serialising unchanged data
// on every /metrics scrape was 45 % of all allocations in the gather path — an
// hourly inventory registry was rebuilt ~120 times per collection at a 30 s
// scrape interval.
//
// Lazy rather than eager, because the two cadences run in both directions: the
// cpu collector produces six collections per 30 s scrape, and gathering each
// one at store time would serialise five that nobody ever reads. This way the
// cost is once per collection *or* once per scrape, whichever is rarer.
//
// families is immutable once built: mergeFamilies rebuilds family headers
// rather than appending into these.
type entry struct {
	reg *prometheus.Registry
	ts  time.Time

	// Not sync.Once: a failed Gather must not be memoised. Once consumed the
	// attempt, the collector's series stayed absent from /metrics until the next
	// collection replaced the entry — up to an hour for an inventory collector —
	// while the dashboard went on showing the values from the snapshot. Two
	// renderers, one collection: the exposition has to come back on the next read.
	mu   sync.Mutex
	fams []*dto.MetricFamily
	got  bool // fams is valid; set only on a successful Gather
}

// families returns the entry's exposition, gathering it on first use. A failed
// gather is not cached, so the next read tries again.
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

// NewScraper creates a Scraper and registers its self-metrics on base.
// ScrapeInterval and InventoryInterval must be > 0.
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
	// How long collection takes and how often it runs.
	//
	// Without these there is no way to answer the two questions that decide
	// this exporter's deployment shape: how much load is it putting on the
	// Talos API, and at what fleet size does one replica stop keeping up.
	// A round is (nodes x due collectors) concurrent RPCs, and `sensors`
	// alone is ~80 file reads per node — the cost is real and was invisible.
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
	base.MustRegister(nodeUp, scrapeErrors, collectDur, collections, refreshDur)

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
		refreshDur:        refreshDur,
		cache:             make(map[key]*entry),
		inflight:          make(map[key]struct{}),
		up:                make(map[string]bool),
		lastSuccess:       make(map[string]time.Time),
	}
}

// Gatherer serves /metrics and the dashboard: it merges the base registry
// (go/process/self-metrics) with the cached per-node collector output.
//
// It is strictly read-only. Gathering never contacts a node: the always-on
// scheduler owns all collection, so a browser request or a Prometheus scrape
// can only ever read what the scheduler has already put in memory. Do not
// reintroduce a Refresh() here — it would let an HTTP request issue Talos RPCs
// inline and block for up to the per-node timeout.
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
	// Two sources with different owners, so a name in both is checked for
	// duplicate series; within each, its own producer already guaranteed none.
	return mergeFamilies(
		source{owner: "base", families: baseFamilies},
		source{owner: "scraper", families: cached},
	), nil
}

// Gather returns the cached per-node collector metrics, in deterministic
// (node, collector) order. It does not trigger collection.
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

// Refresh re-collects all stale (node, collector) pairs, in parallel per
// node. It is safe to call concurrently: in-flight keys are skipped.
//
// The app's always-on scheduler is its ONLY caller; a tick with nothing stale
// collects nothing and logs nothing. Serving an HTTP request must never call
// this — see the Gatherer doc comment.
func (s *Scraper) Refresh(ctx context.Context) {
	enabled := s.registry.Enabled()
	if len(enabled) == 0 {
		return
	}
	live := s.listNodes()
	if len(live) == 0 {
		// An empty list is almost always a not-yet-synced informer, not a
		// cluster that lost every node. Dropping the cache here would blank
		// /metrics and the UI on a transient blip; real removals are handled
		// by the dropGone(live) at the end of a round that actually ran.
		return
	}
	if !s.clientsReady() {
		s.log.Debug("talos clients not ready yet, skipping scrape round")
		return
	}

	// Most ticks have nothing to do: the scheduler runs at 1 s while the
	// fastest collector TTL is 5 s, so four ticks in five used to allocate a
	// waitgroup, spawn a goroutine per node and take the cache lock per
	// (node, collector) only to find nothing stale (finding 5.7). Ask the
	// cheap question first.
	if !s.anyDue(live, enabled) {
		// Only walk the caches when a node has actually gone, so an idle tick
		// really does cost nothing: dropGone allocates a set and scans every
		// cached entry, once a second, for a cluster that rarely changes.
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			collected.Add(int64(s.scrapeNode(ctx, n, enabled)))
		}()
	}
	wg.Wait()
	s.dropGone(live)
	if n := collected.Load(); n > 0 {
		// Only rounds that collected something are timed. Ticks that found
		// nothing due are near-instant and would drag the gauge to zero,
		// hiding the cost of the rounds that did work.
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

// anyDue reports whether any (node, collector) pair is stale, under a single
// lock and without allocating. It is the fast path for a tick with no work.
func (s *Scraper) anyDue(live []nodes.Node, enabled []Collector) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range live {
		if n.IP == "" {
			continue
		}
		for _, c := range enabled {
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

// scrapeNode collects the node's stale collectors and returns how many were
// claimed (0 = nothing was stale, or the node has no IP yet).
func (s *Scraper) scrapeNode(ctx context.Context, n nodes.Node, enabled []Collector) int {
	if n.IP == "" {
		return 0 // no IP assigned yet; not dialable, not an error
	}
	stale := s.claimStale(n.Name, enabled)
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
			// A panicking collector must not take the process down or keep its
			// in-flight key forever (only store and fail release it, so the
			// pair would never be scraped again). Route it through fail: a
			// buggy collector becomes a counted failure serving stale data,
			// which is the right property for a plugin framework.
			defer func() {
				if r := recover(); r != nil {
					s.fail(n.Name, c.Name(), panicError{fmt.Errorf("collector panicked: %v", r)})
				}
			}()
			sub := prometheus.NewRegistry()
			started := s.now()
			err := c.Collect(nodeCtx, nc, sub)
			// Timed whether it succeeded or not: a collector that is slow
			// because it times out is exactly the one worth seeing.
			s.collectDur.WithLabelValues(n.Name, c.Name()).Set(s.now().Sub(started).Seconds())
			s.collections.WithLabelValues(n.Name, c.Name()).Inc()
			if err != nil {
				s.fail(n.Name, c.Name(), err)
				return
			}
			s.store(n.Name, c.Name(), sub)
			succeeded.Add(1)
		}(c)
	}
	wg.Wait()
	s.noteRound(n.Name, succeeded.Load() > 0)
	return len(stale)
}

// upWindow is how long a node stays "up" after its last successful collection.
//
// Two Live intervals: long enough that a round in which only a slow
// Inventory collector was due — and failed — cannot flip a node whose Live
// collectors succeeded moments ago, short enough that a node which actually
// died is reported down within about a minute.
func (s *Scraper) upWindow() time.Duration {
	w := 2 * s.scrapeInterval
	if w < 2*s.nodeTimeout {
		w = 2 * s.nodeTimeout
	}
	return w
}

// noteRound records the outcome of one node's scrape round and recomputes
// reachability.
//
// Reachability is "did anything succeed recently", NOT "did everything due
// this round succeed". Collectors have very different cadences — Live at
// 30 s, Inventory at 1 h — so most rounds have exactly one collector due, and
// an hourly collector's transient RPC blip used to mark a reachable node down
// until the next Live round. talos_node_up is the single source of truth for
// reachability and fed an attention rule, so that was a false alarm on the
// most important signal in the system.
//
// A down verdict also evicts the node's cached data, so its series leave the
// exposition: a sample means "observed now", and re-serving a cached value
// would stamp it fresh on every scrape, so the series could never go stale in
// Prometheus and dashboards would draw a flat line at the last value. Absence
// is what the staleness model expects (the exporter target itself stays up);
// the data returns on the first successful collection, and talos_node_up plus
// the error counters — registered on the base registry — are deliberately not
// evicted.
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

// TTLs returns the effective scrape cadence for each enabled collector:
// the per-collector override, then the collector's own default, then the
// class default.
func (s *Scraper) TTLs() map[string]time.Duration {
	out := make(map[string]time.Duration)
	for _, c := range s.registry.Enabled() {
		out[c.Name()] = s.ttlFor(c)
	}
	return out
}

// claimStale returns the enabled collectors whose cached data is stale (or
// missing) and marks them in-flight so concurrent refreshes skip them.
func (s *Scraper) claimStale(node string, enabled []Collector) []Collector {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Collector
	for _, c := range enabled {
		k := key{node: node, collector: c.Name()}
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

// errClasses is every value the err label can take. Each is initialised to 0
// the first time a (node, collector) pair is scraped: a counter that springs
// into existence on first failure makes rate() return no data rather than
// zero, so an alert on a healthy cluster never arms and a panel reads
// "No data" instead of a reassuring flat line.
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
		// Steady state once the node is judged down: the data was evicted,
		// and Warn per collector per TTL would spam the log for hours.
		s.log.Debug("collector scrape failed, node down, no data served",
			"node", node, "collector", name, "err_class", class, "err", err)
	default:
		s.log.Warn("collector scrape failed, no data yet",
			"node", node, "collector", name, "err_class", class, "err", err)
	}
}

// NodeUp reports whether a node is reachable, and whether it has been scraped
// at all yet.
//
// The UI used to ask the client pool instead, which sets a verified flag once
// and never clears it — so a node that died after being verified rendered as
// up forever. There is one source of truth for reachability and this is it:
// "did any collector succeed within upWindow" — see noteRound for why it is a
// window and not "did this round succeed".
func (s *Scraper) NodeUp(node string) (up, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	up, known = s.up[node]
	return up, known
}

// dropGone removes cached (and in-flight) data for nodes that are no longer
// in the cluster.
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
			// The self-metrics are registered on the base registry, which the
			// drop above never touches: without this, every node that ever
			// left the cluster leaves its series frozen in the exposition
			// forever, and a churning node set grows them without bound
			// (invariant 6). Every node that reached these vectors also went
			// through noteRound, so s.up is the complete set of known nodes.
			s.scrapeErrors.DeletePartialMatch(prometheus.Labels{"node": node})
			s.collectDur.DeletePartialMatch(prometheus.Labels{"node": node})
			s.collections.DeletePartialMatch(prometheus.Labels{"node": node})
		}
	}
}

// ttlFor resolves the effective TTL for a collector: an explicit
// per-collector override wins, then the collector's own default (if it
// implements DefaultTTLer), then the class-based default.
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

// panicError wraps a recovered collector panic so classify can give it its
// own err_class instead of burying it in "other".
type panicError struct{ error }

// classify maps a collection error to a low-cardinality label value.
func classify(err error) string {
	var pe panicError
	if errors.As(err, &pe) {
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

// mergeFamilies combines metric families with the same name: the same
// collector metric collected on different nodes lives in different
// sub-registries and must be merged into one family (one series per node).
// source is one cached collection: the collector that produced it, and the
// families it gathered. The owner matters for de-duplication (see merge).
type source struct {
	owner    string
	families []*dto.MetricFamily
}

// mergeFamilies merges independently-gathered lists into one exposition,
// dropping any series that would appear twice.
//
// Duplicates matter because Prometheus rejects a body containing a repeated
// name+labels line **in full** — one bad pair would blank every series from
// every node. But checking every series costs a string key per series per
// scrape, which profiling put at 27 % of all allocations in this path, so the
// check is narrowed to where a collision is actually possible:
//
//   - A name contributed by only one source cannot collide: prometheus.Registry
//     already rejects duplicates within a registry.
//   - A name contributed by several sources of the *same* collector cannot
//     collide either — those are different nodes, and every per-node metric
//     carries a distinct `node` label. The one exception is a family with no
//     `node` label at all, which is treated as suspect.
//
// So the full check runs only for names emitted by two different collectors,
// or for node-less families. Everything else is concatenated.
func mergeFamilies(sources ...source) []*dto.MetricFamily {
	risky, total := planMerge(sources)

	// Pass 2: merge. Families are rebuilt rather than reused, so a cached
	// source list is never mutated (see entry.families).
	byName := make(map[string]*dto.MetricFamily, len(total))
	order := make([]string, 0, len(total))
	var seen map[string]struct{}
	if len(risky) > 0 {
		seen = make(map[string]struct{})
	}

	for _, src := range sources {
		for _, f := range src.families {
			name := f.GetName()
			metrics := f.Metric
			if _, check := risky[name]; check {
				metrics = dedupe(name, metrics, seen)
			}
			existing, ok := byName[name]
			if !ok {
				// Build the header field by field: dto.MetricFamily embeds a
				// protoimpl.MessageState containing a mutex, so copying the
				// struct by value is not safe (govet copylocks).
				byName[name] = &dto.MetricFamily{
					Name:   f.Name,
					Help:   f.Help,
					Type:   f.Type,
					Unit:   f.Unit,
					Metric: append(make([]*dto.Metric, 0, total[name]), metrics...),
				}
				order = append(order, name)
				continue
			}
			existing.Metric = append(existing.Metric, metrics...)
		}
	}
	out := make([]*dto.MetricFamily, 0, len(byName))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// planMerge is pass 1 of mergeFamilies: it decides which names need
// series-level de-duplication and counts the series each family will hold so
// its slice is allocated once. Growing it per contributing source made the
// merge quadratic: a 100-node cluster re-copied the CPU frequency family 100
// times.
//
// A name is risky when it is emitted by two different collectors, or when a
// family with that name carries no `node` label at all (such a family could
// be emitted identically by two nodes).
func planMerge(sources []source) (map[string]struct{}, map[string]int) {
	owners := make(map[string]string, 128)
	risky := make(map[string]struct{})
	total := make(map[string]int, 128)
	for _, src := range sources {
		for _, f := range src.families {
			name := f.GetName()
			if prev, ok := owners[name]; ok && prev != src.owner {
				risky[name] = struct{}{}
			} else if !ok {
				owners[name] = src.owner
			}
			if len(f.Metric) > 0 && !hasNodeLabel(f.Metric[0]) {
				risky[name] = struct{}{}
			}
			total[name] += len(f.Metric)
		}
	}
	return risky, total
}

// hasNodeLabel reports whether a metric carries the per-node label. A family
// without it could be emitted identically by two nodes.
func hasNodeLabel(m *dto.Metric) bool {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == "node" {
			return true
		}
	}
	return false
}

// dedupe returns the metrics whose (name, label set) has not been emitted yet,
// recording each one it keeps. Returns the input untouched when nothing is a
// duplicate, so the common case allocates nothing.
func dedupe(name string, metrics []*dto.Metric, seen map[string]struct{}) []*dto.Metric {
	var out []*dto.Metric
	for i, m := range metrics {
		key := seriesKey(name, m)
		if _, dup := seen[key]; dup {
			if out == nil { // first duplicate: copy what we kept so far
				out = append(make([]*dto.Metric, 0, len(metrics)), metrics[:i]...)
			}
			continue
		}
		seen[key] = struct{}{}
		if out != nil {
			out = append(out, m)
		}
	}
	if out == nil {
		return metrics
	}
	return out
}

// seriesKey identifies a series the way Prometheus does: metric name plus the
// full label set, label order normalised.
func seriesKey(name string, m *dto.Metric) string {
	pairs := make([]string, 0, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		pairs = append(pairs, lp.GetName()+"\x00"+lp.GetValue())
	}
	sort.Strings(pairs)
	return name + "\x00" + strings.Join(pairs, "\x00")
}
