package history

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestStore(window time.Duration) (*Store, *time.Time) {
	s := New(window)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestAppendAndPointsOrder(t *testing.T) {
	s, now := newTestStore(10 * time.Minute)

	s.Append("n1", "usage.all", *now, 10)
	(*now) = now.Add(5 * time.Second)
	s.Append("n1", "usage.all", *now, 20)
	s.Append("n1", "freq.current.0", *now, 3500000)

	pts, ok := s.Points("n1")
	if !ok {
		t.Fatal("Points: ok=false, want data")
	}
	if len(pts["usage.all"]) != 2 {
		t.Fatalf("usage.all: got %d points, want 2", len(pts["usage.all"]))
	}
	if pts["usage.all"][0].V != 10 || pts["usage.all"][1].V != 20 {
		t.Errorf("usage.all order: got %v, want oldest-first [10 20]", pts["usage.all"])
	}
	want := float64(now.Unix()) // the clock was advanced before appending point 2
	if pts["usage.all"][1].T != want {
		t.Errorf("T: got %v, want unix seconds %v", pts["usage.all"][1].T, want)
	}
	if got := len(pts["freq.current.0"]); got != 1 {
		t.Errorf("freq.current.0: got %d points, want 1", got)
	}
}

func TestWindowTrimming(t *testing.T) {
	s, now := newTestStore(10 * time.Minute)

	s.Append("n1", "x", *now, 1)       // t=0
	(*now) = now.Add(11 * time.Minute) // t=11m
	s.Append("n1", "x", *now, 2)       // t=11m
	(*now) = now.Add(2 * time.Minute)  // t=13m

	pts, ok := s.Points("n1")
	if !ok {
		t.Fatal("Points: ok=false, want data")
	}
	if len(pts["x"]) != 1 || pts["x"][0].V != 2 {
		t.Errorf("window trim: got %v, want only the in-window point [2]", pts["x"])
	}

	// Everything aged out of the window → no data.
	(*now) = now.Add(20 * time.Minute)
	if _, ok := s.Points("n1"); ok {
		t.Error("Points: ok=true after all points aged out, want false")
	}
}

func TestRingEvictsOldestAtTheCeiling(t *testing.T) {
	s, now := newTestStore(10 * time.Minute)

	// Below the ceiling the ring grows instead of evicting, so the configured
	// window survives a fast cadence (finding 5.6). Past it, the oldest point
	// goes: append maxCap+2 samples 1 ms apart, all inside the window.
	max := s.maxCap
	for i := 0; i < max+2; i++ {
		s.Append("n1", "x", *now, float64(i))
		(*now) = now.Add(time.Millisecond)
	}
	pts, ok := s.Points("n1")
	if !ok {
		t.Fatal("Points: ok=false, want data")
	}
	if len(pts["x"]) != max {
		t.Fatalf("ring: got %d points, want the ceiling %d", len(pts["x"]), max)
	}
	if pts["x"][0].V != 2 || pts["x"][len(pts["x"])-1].V != float64(max+1) {
		t.Errorf("ring eviction: first=%v last=%v, want the two oldest evicted", pts["x"][0].V, pts["x"][len(pts["x"])-1].V)
	}
}

func TestUnknownNode(t *testing.T) {
	s, _ := newTestStore(10 * time.Minute)
	if _, ok := s.Points("missing"); ok {
		t.Error("Points for an unknown node: ok=true, want false")
	}
}

func TestPrune(t *testing.T) {
	s, now := newTestStore(10 * time.Minute)
	s.Append("n1", "x", *now, 1)
	s.Append("n2", "x", *now, 2)

	s.Prune(map[string]struct{}{"n1": {}})

	if _, ok := s.Points("n2"); ok {
		t.Error("pruned node n2 still has data")
	}
	if _, ok := s.Points("n1"); !ok {
		t.Error("live node n1 lost its data")
	}
}

func TestConcurrentAppendAndRead(t *testing.T) {
	s, now := newTestStore(10 * time.Minute)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Append(fmt.Sprintf("n%d", g%2), "x", *now, 1)
				s.Points(fmt.Sprintf("n%d", g%2))
			}
		}(g)
	}
	wg.Wait()
}

// The incremental `since` query went with the uPlot panels it served. What
// still matters is the window boundary and the unknown-node case.
func TestPointsHonoursTheWindowAndUnknownNodes(t *testing.T) {
	s := New(10 * time.Second)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		s.Append("n1", "freq.current.0", base.Add(time.Duration(i)*time.Second), float64(i))
	}

	// Read 3s after the last sample: the whole window is still in range.
	s.now = func() time.Time { return base.Add(7 * time.Second) }
	all, ok := s.Points("n1")
	if !ok || len(all["freq.current.0"]) != 5 {
		t.Fatalf("full window: ok=%v points=%d, want ok=true points=5", ok, len(all["freq.current.0"]))
	}

	// Read far enough on that every sample has aged out. A node whose data
	// has all expired reports ok=false, the same as one that never had any —
	// the caller has nothing to render either way.
	s.now = func() time.Time { return base.Add(time.Hour) }
	if _, ok := s.Points("n1"); ok {
		t.Error("samples older than the window were returned")
	}

	if _, ok := s.Points("ghost"); ok {
		t.Error("unknown node must report ok=false")
	}
}

// TestWindowIsHonouredAtFastCadence pins finding 5.6: the ring was sized for
// 2-second samples, so a faster collector TTL silently delivered less history
// than --history.window asked for — at 1 s the graph showed half the window.
func TestWindowIsHonouredAtFastCadence(t *testing.T) {
	s := New(10 * time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s.now = func() time.Time { return now }

	// Ten minutes of one-second samples: 600 points, twice what the initial
	// ring holds.
	const n = 600
	for i := 0; i < n; i++ {
		now = base.Add(time.Duration(i) * time.Second)
		s.Append("n1", "usage.all", now, float64(i))
	}

	pts, ok := s.Points("n1")
	if !ok {
		t.Fatal("no points")
	}
	got := pts["usage.all"]
	if len(got) < n-1 {
		t.Fatalf("got %d points, want ~%d: the window was truncated", len(got), n)
	}
	// The oldest point must still be the start of the window, not halfway in.
	span := got[len(got)-1].T - got[0].T
	if span < 590 {
		t.Errorf("span %.0fs, want ~599s of history", span)
	}
	if got[0].V != 0 {
		t.Errorf("oldest sample is %v, want 0: earlier points were evicted", got[0].V)
	}
}

// TestRingDoesNotGrowPastTheWindow: growth is bounded, so a fast series cannot
// consume memory without limit.
func TestRingDoesNotGrowPastTheWindow(t *testing.T) {
	s := New(time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s.now = func() time.Time { return now }

	// Append far faster than the scheduler can actually tick.
	for i := 0; i < 5000; i++ {
		now = base.Add(time.Duration(i) * 10 * time.Millisecond)
		s.Append("n1", "usage.all", now, float64(i))
	}
	s.mu.RLock()
	size := len(s.nodes["n1"]["usage.all"].ring)
	s.mu.RUnlock()
	if size > s.maxCap {
		t.Errorf("ring grew to %d, past the %d ceiling", size, s.maxCap)
	}
}

// TestRingStartsSmallAndReachesTheWindow: sizing every ring for the full
// window up front allocated ~68 MB across a 100-node cluster on the first
// sample, before any history existed. Growth must still reach the full window.
func TestRingStartsSmallAndReachesTheWindow(t *testing.T) {
	s := New(10 * time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s.now = func() time.Time { return now }

	s.Append("n1", "usage.all", now, 0)
	s.mu.RLock()
	first := len(s.nodes["n1"]["usage.all"].ring)
	s.mu.RUnlock()
	if first > minCap {
		t.Errorf("first sample allocated %d points, want at most %d", first, minCap)
	}

	// Ten minutes at the 2 s cadence the window is sized for.
	for i := 1; i < 300; i++ {
		now = base.Add(time.Duration(i) * 2 * time.Second)
		s.Append("n1", "usage.all", now, float64(i))
	}
	pts, ok := s.Points("n1")
	if !ok {
		t.Fatal("no points")
	}
	if got := len(pts["usage.all"]); got < 299 {
		t.Errorf("got %d points, want the full window (~300)", got)
	}
	if pts["usage.all"][0].V != 0 {
		t.Errorf("oldest sample is %v, want 0 — the window was truncated", pts["usage.all"][0].V)
	}
}
