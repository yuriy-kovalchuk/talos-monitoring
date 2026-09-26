// Package sysstat shares one SystemStat response between the collectors that
// need it.
//
// SystemStat carries both the per-core jiffies the cpu collector wants and the
// boot time and process counts nodeapi wants, so every round where both are due
// asked each node the identical question twice. They run at different cadences
// (cpu every 5 s, nodeapi every 30 s) and the scraper launches them as
// concurrent goroutines, so a plain TTL cache is not enough: the two requests
// overlap in time. Callers are therefore de-duplicated in flight as well.
package sysstat

import (
	"context"
	"sync"
	"time"

	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

// TTL is how long a response is reused. Deliberately short: the cpu collector
// derives usage from the difference between consecutive readings, so serving it
// a stale response would understate the delta. One second is the scheduler's
// own tick — long enough that collectors due in the same round share a read,
// short enough that no collector ever sees a reading from a previous round.
const TTL = time.Second

// Fetch performs the RPC for one node.
type Fetch func(ctx context.Context) (*machinepb.SystemStatResponse, error)

type entry struct {
	at   time.Time
	resp *machinepb.SystemStatResponse
	err  error
	done chan struct{} // closed when the in-flight fetch completes
}

// Cache holds the most recent SystemStat per node.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
}

// New returns an empty cache.
func New() *Cache {
	return &Cache{entries: make(map[string]*entry), now: time.Now}
}

// Get returns the node's SystemStat, calling fetch only when no fresh response
// is available. A concurrent caller for the same node waits for the in-flight
// fetch instead of issuing its own.
//
// A nil Cache calls fetch directly, so a collector constructed without one (as
// in tests) still works.
func (c *Cache) Get(ctx context.Context, node string, fetch Fetch) (*machinepb.SystemStatResponse, error) {
	if c == nil {
		return fetch(ctx)
	}

	c.mu.Lock()
	if e, ok := c.entries[node]; ok {
		if done := e.done; done != nil {
			// A fetch is running; wait for it rather than duplicating it. The
			// channel is captured under the lock because the fetcher clears
			// the field once it is finished.
			c.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			c.mu.Lock()
			resp, err := e.resp, e.err
			c.mu.Unlock()
			return resp, err
		}
		if c.now().Sub(e.at) < TTL {
			resp, err := e.resp, e.err
			c.mu.Unlock()
			return resp, err
		}
	}
	done := make(chan struct{})
	e := &entry{done: done}
	c.entries[node] = e
	c.mu.Unlock()

	resp, err := fetch(ctx)

	c.mu.Lock()
	e.resp, e.err, e.at = resp, err, c.now()
	e.done = nil // no longer in flight; at/resp/err are now authoritative
	c.mu.Unlock()
	close(done)

	return resp, err
}

// Prune drops cached responses for nodes no longer in the cluster. The
// collectors that share this cache call it through collector.Pruner.
func (c *Cache) Prune(live map[string]struct{}) {
	if c == nil {
		return
	}
	// An empty set means discovery has not synced yet, not that the cluster
	// is empty — the Pruner contract says implementations must prune nothing
	// in that case. app.prune() guards the call site too.
	if len(live) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for node := range c.entries {
		if _, ok := live[node]; !ok {
			delete(c.entries, node)
		}
	}
}
