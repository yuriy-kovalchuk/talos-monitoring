// Package collector is the framework that turns per-node Talos data into
// Prometheus metrics.
//
// A Collector knows how to collect one data domain (COSI resources, machine
// API calls, or files read through the Talos API) from one node and register
// the resulting metrics on a per-node registry. The Scraper orchestrates
// collections across all nodes with a TTL cache: nodes are only actually
// queried when their cached data is stale, so /metrics can be scraped more
// often than nodes are hit.
package collector

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

// Class selects which refresh interval the Scraper applies to a collector.
type Class int

const (
	// Live data (sensors, CPU frequency, load) is re-collected every
	// ScrapeInterval.
	Live Class = iota
	// Inventory data (hardware, disk layout) changes rarely and is
	// re-collected every InventoryInterval.
	Inventory
)

// DefaultTTLer is an optional Collector interface: a collector that knows
// its own natural refresh period declares it here. It overrides the
// class-based default, but a --collectors.<name>.ttl flag still wins.
type DefaultTTLer interface {
	DefaultTTL() time.Duration
}

// Collector collects one data domain from one node and registers the
// resulting metrics on reg.
//
// Implementations must be safe to run concurrently for different nodes and
// must label every metric with the node name (convention: "node").
// A returned error marks only this collection as failed: cached data (if
// any) is kept, the failure is counted, and other collectors and nodes are
// unaffected.
type Collector interface {
	// Name is the stable identifier used for enable/disable selection and
	// in talos_monitoring_node_scrape_errors_total.
	Name() string
	// Class picks the refresh interval: Live or Inventory.
	Class() Class
	// Collect gathers the data domain for one node and registers metrics
	// on reg.
	Collect(ctx context.Context, node *NodeClient, reg prometheus.Registerer) error
}

// NodeClient is what collectors receive: node metadata plus the per-node
// Talos API client.
type NodeClient struct {
	Node   nodes.Node
	Client *talosclient.Client
}

// Pruner is an optional interface for collectors that keep per-node state.
//
// Most do: a previous-jiffies sample, a cached disk list, a discovered sensor
// layout. All of it is keyed by node name and none of it has a natural
// expiry, so on a cluster whose node names churn — autoscaling, rebuilds —
// it grows without bound. The scheduler calls Prune on the same cadence it
// already prunes the snapshot and history stores.
//
// live is the current node set. An empty set means discovery has not synced
// and must prune nothing, the same rule the scraper and both stores follow.
type Pruner interface {
	Prune(live map[string]struct{})
}
