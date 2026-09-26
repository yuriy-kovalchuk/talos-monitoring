package snapshot

import (
	"sync"
	"testing"
)

// TestUpdateIsPerSection: collectors run concurrently and at different TTLs,
// so one writing its section must not clear another's.
func TestUpdateIsPerSection(t *testing.T) {
	s := New()
	s.Update("n1", func(n *Node) { n.System = &System{Product: "NUC"} })
	s.Update("n1", func(n *Node) { n.Disks = []Disk{{Device: "/dev/sda"}} })

	got := s.Node("n1")
	if got.System == nil || got.System.Product != "NUC" {
		t.Error("the disk update cleared the system section")
	}
	if len(got.Disks) != 1 {
		t.Error("disks not recorded")
	}
	if got.Updated.IsZero() {
		t.Error("Updated not stamped")
	}
}

// TestNodeAbsentIsNil: a node nothing has collected for reads as absent, not
// as a zero-valued snapshot the UI would render as real data.
func TestNodeAbsentIsNil(t *testing.T) {
	if got := New().Node("nope"); got != nil {
		t.Errorf("got %+v, want nil for an uncollected node", got)
	}
}

// TestPruneKeepsLiveDropsGone mirrors the scraper's cache behaviour.
func TestPruneKeepsLiveDropsGone(t *testing.T) {
	s := New()
	s.Update("n1", func(n *Node) { n.System = &System{} })
	s.Update("n2", func(n *Node) { n.System = &System{} })

	s.Prune(map[string]struct{}{"n1": {}})
	if s.Node("n1") == nil {
		t.Error("live node was pruned")
	}
	if s.Node("n2") != nil {
		t.Error("removed node was kept")
	}
}

// TestPruneEmptySetKeepsEverything pins the same rule as scraper finding 1.6:
// an empty node list is an unsynced informer, not an empty cluster.
func TestPruneEmptySetKeepsEverything(t *testing.T) {
	s := New()
	s.Update("n1", func(n *Node) { n.System = &System{} })
	s.Prune(nil)
	if s.Node("n1") == nil {
		t.Error("an empty live set must prune nothing")
	}
}

// TestNilStoreIsSafe: collectors take the store by pointer and tests pass nil
// when they do not care about it, exactly as they do with history.Store.
func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	s.Update("n1", func(*Node) { t.Error("fn must not run on a nil store") })
	if s.Node("n1") != nil {
		t.Error("nil store returned a node")
	}
	s.Prune(map[string]struct{}{"n1": {}})
}

// TestConcurrentUpdates: every collector for a node runs as its own goroutine.
func TestConcurrentUpdates(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Update("n1", func(n *Node) { n.Sockets = []Socket{{ID: "CPU0"}} })
			s.Node("n1")
		}(i)
	}
	wg.Wait()
	if got := s.Node("n1"); got == nil || len(got.Sockets) != 1 {
		t.Errorf("got %+v, want one socket", got)
	}
}

// TestConcurrentReadWriteIsRaceFree reproduces production: the scheduler
// goroutine collects while HTTP handlers render. Node() hands the caller a
// pointer whose fields are then read outside the lock, so mutating a
// published *Node in place was a data race on every dashboard request — one
// the rest of the suite could not catch, because nothing else runs a
// collector and a handler at the same time.
func TestConcurrentReadWriteIsRaceFree(t *testing.T) {
	s := New()
	s.Update("n1", func(n *Node) { n.Disks = []Disk{{Device: "/dev/sda"}} })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.Update("n1", func(n *Node) {
				n.Disks = make([]Disk, i%8+1)
				n.CPU = &CPU{UsagePct: float64(i)}
				n.Sensors = make([]Sensor, i%4+1)
			})
		}
	}()

	for i := 0; i < 20000; i++ {
		n := s.Node("n1")
		if n == nil {
			continue
		}
		// Exactly what the dashboard does: read fields without the lock.
		_ = len(n.Disks)
		_ = len(n.Sensors)
		if n.CPU != nil {
			_ = n.CPU.UsagePct
		}
	}
	close(stop)
	wg.Wait()
}

// TestPublishedSnapshotIsImmutable: a reader holding an older *Node must keep
// seeing what it had, which is what makes lock-free reads safe.
func TestPublishedSnapshotIsImmutable(t *testing.T) {
	s := New()
	s.Update("n1", func(n *Node) { n.Disks = []Disk{{Device: "/dev/sda"}} })
	before := s.Node("n1")

	s.Update("n1", func(n *Node) { n.Disks = []Disk{{Device: "/dev/sdb"}, {Device: "/dev/sdc"}} })

	if len(before.Disks) != 1 || before.Disks[0].Device != "/dev/sda" {
		t.Errorf("a published snapshot changed under its reader: %+v", before.Disks)
	}
	if after := s.Node("n1"); len(after.Disks) != 2 {
		t.Errorf("the new snapshot is wrong: %+v", after.Disks)
	}
	if before == s.Node("n1") {
		t.Error("Update must publish a new *Node, not edit the old one")
	}
}
