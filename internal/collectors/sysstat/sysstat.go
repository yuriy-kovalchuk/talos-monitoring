// Package sysstat shares one SystemStat response between cpu and nodeapi, de-duplicating in flight.
package sysstat

import (
	"context"
	"sync"
	"time"

	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

// TTL is one scheduler tick: long enough to share a round, short enough to never be stale.
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

// Get returns a fresh SystemStat, waiting on an in-flight fetch instead of issuing another.
func (c *Cache) Get(ctx context.Context, node string, fetch Fetch) (*machinepb.SystemStatResponse, error) {
	if c == nil {
		return fetch(ctx)
	}

	c.mu.Lock()
	if e, ok := c.entries[node]; ok {
		if done := e.done; done != nil {
			// The channel is captured under the lock: the fetcher clears the field when done.
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

// Prune drops cached responses for departed nodes, via each collector's Pruner.
func (c *Cache) Prune(live map[string]struct{}) {
	if c == nil {
		return
	}
	// An empty live set means discovery has not synced, so the Pruner contract prunes nothing.
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
