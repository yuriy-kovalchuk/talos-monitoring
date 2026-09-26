// Package history keeps a bounded in-memory ring of recent samples for
// fast-updating per-node series (CPU usage, per-core frequency), feeding the
// dashboard graph endpoint.
//
// History is RAM-only by design: it exists so the UI can draw a 10-minute
// live graph at the fast collector's cadence without hammering /metrics.
// Durable long-term history stays with Prometheus/Mimir, which keeps
// scraping /metrics at a normal interval.
package history

import (
	"sort"
	"sync"
	"time"
)

// defaultWindow is the history depth when no window is configured.
const defaultWindow = 10 * time.Minute

// minResolution is the sample spacing the ring is sized for initially: a
// 10-minute window holds points at 2-second resolution, which covers the
// default cadences.
//
// A faster collector TTL used to mean the ring silently held less than the
// configured window — at --collectors.cpu.ttl=1s a 10-minute window delivered
// five minutes of graph (finding 5.6). Rings now grow when they overflow while
// still inside the window, so --history.window means what it says whatever the
// cadence.
const minResolution = 2 * time.Second

// maxResolution bounds that growth. The scheduler ticks once a second, so no
// series can be appended to faster than that; a ring sized for 1 Hz can always
// hold the full window.
const maxResolution = time.Second

// minCap is the smallest ring, for tiny windows.
const minCap = 8

// Point is one sample. T is unix seconds with sub-second precision; when is
// the sortable instant (unexported, not marshaled).
type Point struct {
	// T is Unix seconds. The time.Time it came from used to be stored
	// alongside it, so every point in every ring carried the same instant
	// twice; the window filter now compares in these same seconds.
	T float64 `json:"t"`
	V float64 `json:"v"`
}

// unixSeconds is Point.T's encoding, shared by Append and the window filter so
// the two cannot drift apart.
func unixSeconds(t time.Time) float64 { return float64(t.UnixMilli()) / 1000.0 }

// grow re-lays the ring into a larger one, oldest first, so head/count
// arithmetic stays valid.
func (rs *series) grow(size int) {
	bigger := make([]Point, size)
	start := (rs.head - rs.count + len(rs.ring)) % len(rs.ring)
	for i := 0; i < rs.count; i++ {
		bigger[i] = rs.ring[(start+i)%len(rs.ring)]
	}
	rs.ring, rs.head = bigger, rs.count
}

// series is a ring of points, written in time order. It grows if the series is
// sampled faster than the store was sized for.
type series struct {
	ring  []Point
	head  int // index of the next write
	count int // points stored (<= len(ring))
}

// Store is the per-node history. One RWMutex guards everything: appends are
// O(1) and reads copy at most a few hundred points.
type Store struct {
	mu     sync.RWMutex
	window time.Duration
	cap    int                           // initial ring size
	maxCap int                           // ceiling for a ring that grows (see maxResolution)
	nodes  map[string]map[string]*series // node → series name → ring
	now    func() time.Time
}

// New creates a Store keeping samples from the last window (<= 0 → 10m).
func New(window time.Duration) *Store {
	if window <= 0 {
		window = defaultWindow
	}
	cap := int(window/minResolution) + 1
	if cap < minCap {
		cap = minCap
	}
	return &Store{
		window: window,
		cap:    cap,
		maxCap: int(window/maxResolution) + 1,
		nodes:  make(map[string]map[string]*series),
		now:    time.Now,
	}
}

// Window returns the configured history depth.
func (s *Store) Window() time.Duration { return s.window }

// Append adds one sample to a node's series, evicting the oldest point when
// the ring is full. Safe for concurrent use.
func (s *Store) Append(node, name string, t time.Time, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byNode, ok := s.nodes[node]
	if !ok {
		byNode = make(map[string]*series)
		s.nodes[node] = byNode
	}
	rs, ok := byNode[name]
	if !ok {
		// Start small and grow. Sizing every ring for the full window up
		// front allocated ~68 MB across a 100-node cluster on the first
		// sample, before a single point of history existed; grow() (already
		// needed for fast cadences) reaches the same size as data arrives.
		rs = &series{ring: make([]Point, min(minCap, s.cap))}
		byNode[name] = rs
	}
	// Full, and the point about to be evicted is still inside the window?
	// Then the ring is smaller than the window needs — either because it
	// started small, or because this series is sampled faster than
	// minResolution. Either way, grow instead of serving a short window.
	if rs.count == len(rs.ring) && len(rs.ring) < s.maxCap {
		oldest := rs.ring[rs.head]
		if oldest.T > unixSeconds(t.Add(-s.window)) {
			rs.grow(min(len(rs.ring)*2, s.maxCap))
		}
	}

	rs.ring[rs.head] = Point{T: unixSeconds(t), V: v}
	rs.head = (rs.head + 1) % len(rs.ring)
	if rs.count < len(rs.ring) {
		rs.count++
	}
}

// Points returns every in-window sample for a node, oldest first, keyed by
// series name. The second result is false when the node has no data at all.
//
// It used to take a `since` cut-off so the removed uPlot panels could poll
// incrementally; every caller now passes the zero time, so the parameter and
// its exclusive/inclusive branch are gone.
func (s *Store) Points(node string) (map[string][]Point, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byNode, ok := s.nodes[node]
	if !ok || len(byNode) == 0 {
		return nil, false
	}
	cutoffSec := unixSeconds(s.now().Add(-s.window))
	out := make(map[string][]Point, len(byNode))
	for name, rs := range byNode {
		start := (rs.head - rs.count + len(rs.ring)) % len(rs.ring)
		pts := make([]Point, 0, rs.count)
		for i := 0; i < rs.count; i++ {
			p := rs.ring[(start+i)%len(rs.ring)]
			if p.T >= cutoffSec {
				pts = append(pts, p)
			}
		}
		if len(pts) > 0 {
			out[name] = pts
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// Prune drops all data for nodes no longer in the cluster.
func (s *Store) Prune(live map[string]struct{}) {
	// An empty set means discovery has not synced yet, not that the cluster
	// is empty — pruning here would drop every node's state on a transient
	// watch blip. app.prune() guards this too; the Pruner contract says
	// implementations must, so they do.
	if len(live) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for name := range s.nodes {
		if _, ok := live[name]; !ok {
			delete(s.nodes, name)
		}
	}
}

// NodeNames returns the nodes currently holding data, sorted.
func (s *Store) NodeNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]string, 0, len(s.nodes))
	for name := range s.nodes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
