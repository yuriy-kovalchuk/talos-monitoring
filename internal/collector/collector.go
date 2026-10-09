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

// Degradation reasons: the fixed set of things a collector can report as
// given up on. They are label values on
// talos_monitoring_collector_degraded, so the set is part of the metric
// contract and lives here rather than in each collector.
const (
	// ReasonPermission is a file-read path switched off because the monitor
	// ServiceAccount lacks os:admin.
	ReasonPermission = "permission"
	// ReasonInventory is a cached inventory that has not been refreshed since
	// its last success: the data is still the best available, but it is no
	// longer observed.
	ReasonInventory = "inventory"
)

// Degradation is one thing a collector gave up on for one node while still
// returning usable data.
type Degradation struct {
	Reason string
	// Stopped is true when the collector stopped contacting the node at all -
	// a whole-collector self-disable. Such a round is not evidence that the
	// node is reachable: Collect returns nil without making a single RPC, and
	// the scraper must not let it vote on talos_node_up.
	Stopped bool
}

// DegradedReporter is an optional Collector interface for collectors that can
// serve partial data.
//
// Collect has one error value, so "I ran, I exported something, and part of it
// is missing" is otherwise inexpressible: the scrape counts as a success, no
// error counter moves, and the only trace is a log line that fired once hours
// ago. The scraper calls Degraded after a nil Collect and publishes the result
// as talos_monitoring_collector_degraded.
//
// Implementations report state they already keep - a self-disable flag, a
// cached inventory's last failed refresh - and must be safe to call
// concurrently for different nodes.
type DegradedReporter interface {
	Degraded(node string) []Degradation
}
