// Package history keeps a bounded in-RAM ring of recent samples for the dashboard's live graphs.
package history

import (
	"sync"
	"time"
)

// defaultWindow is the history depth when no window is configured.
const defaultWindow = 10 * time.Minute

// minResolution sizes the ring initially; rings grow on overflow so --history.window means what it says.
const minResolution = 2 * time.Second

// maxResolution bounds growth: the scheduler ticks once a second, so nothing appends faster.
const maxResolution = time.Second

// minCap is the smallest ring, for tiny windows.
const minCap = 8

// Point is one sample; when is the sortable instant behind T (unix seconds).
type Point struct {
	// T is Unix seconds, the same encoding the window filter compares against.
	T float64 `json:"t"`
	V float64 `json:"v"`
}

// unixSeconds is Point.T's encoding, shared by Append and the window filter so they cannot drift.
func unixSeconds(t time.Time) float64 { return float64(t.UnixMilli()) / 1000.0 }

// grow re-lays the ring oldest-first so the head/count arithmetic stays valid.
func (rs *series) grow(size int) {
	bigger := make([]Point, size)
	start := (rs.head - rs.count + len(rs.ring)) % len(rs.ring)
	for i := 0; i < rs.count; i++ {
		bigger[i] = rs.ring[(start+i)%len(rs.ring)]
	}
	rs.ring, rs.head = bigger, rs.count
}

// series is a ring written in time order that grows when sampled faster than it was sized.
type series struct {
	ring  []Point
	head  int // index of the next write
	count int // points stored (<= len(ring))
}

// Store is the per-node history; one RWMutex suffices because appends are O(1) and reads copy few points.
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
	points := max(int(window/minResolution)+1, minCap)
	return &Store{
		window: window,
		cap:    points,
		maxCap: int(window/maxResolution) + 1,
		nodes:  make(map[string]map[string]*series),
		now:    time.Now,
	}
}

// Window returns the configured history depth.
func (s *Store) Window() time.Duration { return s.window }

// Append adds a sample, evicting the oldest when the ring is full; safe for concurrent use.
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
		// Start small and grow: sizing every ring for the full window cost ~68 MB on a 100-node cluster.
		rs = &series{ring: make([]Point, min(minCap, s.cap))}
		byNode[name] = rs
	}
	// Full but the evicted point is still in the window? The ring is undersized, so grow instead.
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

// Points returns a node's in-window samples oldest first, and false when the node has none.
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
	// An empty live set means discovery has not synced; the Pruner contract says that prunes nothing.
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
