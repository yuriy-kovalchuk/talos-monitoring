package sysstat

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

func resp() *machinepb.SystemStatResponse {
	return &machinepb.SystemStatResponse{
		Messages: []*machinepb.SystemStat{{BootTime: 42}},
	}
}

// TestSecondCallerWithinTTLReusesTheResponse is the point of the cache: cpu and
// nodeapi both want SystemStat, so a round where both are due asked the node
// twice (finding 5.9).
func TestSecondCallerWithinTTLReusesTheResponse(t *testing.T) {
	c := New()
	now := time.Now()
	c.now = func() time.Time { return now }

	var calls atomic.Int64
	fetch := func(context.Context) (*machinepb.SystemStatResponse, error) {
		calls.Add(1)
		return resp(), nil
	}

	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), "n1", fetch); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("got %d RPCs, want 1", got)
	}

	// Past the TTL the next caller pays again: the cpu collector derives usage
	// from consecutive readings and must never see a previous round's numbers.
	now = now.Add(TTL)
	if _, err := c.Get(context.Background(), "n1", fetch); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("got %d RPCs after the TTL, want 2", got)
	}
}

// TestConcurrentCallersIssueOneRPC: the scraper launches every due collector
// for a node as its own goroutine, so the two callers overlap in time and a
// plain TTL check would let both through.
func TestConcurrentCallersIssueOneRPC(t *testing.T) {
	c := New()
	var calls atomic.Int64
	release := make(chan struct{})
	fetch := func(context.Context) (*machinepb.SystemStatResponse, error) {
		calls.Add(1)
		<-release // hold the fetch open so the callers genuinely overlap
		return resp(), nil
	}

	var wg sync.WaitGroup
	got := make([]*machinepb.SystemStatResponse, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := c.Get(context.Background(), "n1", fetch)
			if err != nil {
				t.Error(err)
			}
			got[i] = r
		}(i)
	}
	// Give the goroutines time to pile up on the in-flight fetch.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("got %d RPCs from 8 concurrent callers, want 1", n)
	}
	for i, r := range got {
		if r == nil || r.GetMessages()[0].GetBootTime() != 42 {
			t.Errorf("caller %d got %v, want the shared response", i, r)
		}
	}
}

// TestErrorsAreSharedAndNotCachedPastTTL: a failed fetch must reach every
// waiter, so neither collector silently reports success.
func TestErrorsAreShared(t *testing.T) {
	c := New()
	boom := errors.New("boom")
	_, err := c.Get(context.Background(), "n1", func(context.Context) (*machinepb.SystemStatResponse, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	// Within the TTL the error is reused rather than hammering a down node.
	_, err = c.Get(context.Background(), "n1", func(context.Context) (*machinepb.SystemStatResponse, error) {
		t.Error("fetch called again within the TTL")
		return resp(), nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the cached boom", err)
	}
}

// TestPerNode: one node's reading must not serve another's.
func TestPerNode(t *testing.T) {
	c := New()
	var calls atomic.Int64
	fetch := func(context.Context) (*machinepb.SystemStatResponse, error) {
		calls.Add(1)
		return resp(), nil
	}
	for _, n := range []string{"n1", "n2", "n1"} {
		if _, err := c.Get(context.Background(), n, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("got %d RPCs for 2 nodes, want 2", got)
	}
}

// TestNilCacheFetchesDirectly: collectors built without a cache still work.
func TestNilCacheFetchesDirectly(t *testing.T) {
	var c *Cache
	r, err := c.Get(context.Background(), "n1", func(context.Context) (*machinepb.SystemStatResponse, error) {
		return resp(), nil
	})
	if err != nil || r == nil {
		t.Fatalf("got %v %v, want the response", r, err)
	}
}

// TestPruneEmptySetKeepsEverything pins the Pruner contract: an empty live
// set means discovery has not synced yet, never "the cluster is empty" — the
// cache is shared by five collectors, so a wipe is expensive.
func TestPruneEmptySetKeepsEverything(t *testing.T) {
	c := New()
	c.entries = map[string]*entry{"gone": {}, "live": {}}

	c.Prune(map[string]struct{}{})
	if len(c.entries) != 2 {
		t.Fatalf("Prune(empty) dropped cached state")
	}

	c.Prune(map[string]struct{}{"live": {}})
	if _, ok := c.entries["gone"]; ok {
		t.Error("Prune kept a departed node")
	}
	if _, ok := c.entries["live"]; !ok {
		t.Error("Prune dropped a live node")
	}
}
