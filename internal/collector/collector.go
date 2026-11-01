// Package collector turns per-node Talos data into Prometheus metrics via a per-node TTL cache.
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
	// Live data (sensors, CPU frequency, load) is re-collected every ScrapeInterval.
	Live Class = iota
	// Inventory data (hardware, disk layout) changes rarely and is re-collected every InventoryInterval.
	Inventory
)

// DefaultTTLer overrides the class TTL with the collector's own; a --collectors.<name>.ttl flag still wins.
type DefaultTTLer interface {
	DefaultTTL() time.Duration
}

// Collector collects one domain per node into reg: safe to run concurrently, every series labelled with the node.
type Collector interface {
	// Name is the stable id used for enable/disable selection and the error counters.
	Name() string
	// Class picks the refresh interval: Live or Inventory.
	Class() Class
	// Collect's error fails only this collection: cached data is kept and the failure counted.
	Collect(ctx context.Context, node *NodeClient, reg prometheus.Registerer) error
}

// NodeClient is what collectors receive: node metadata plus the per-node Talos API client.
type NodeClient struct {
	Node   nodes.Node
	Client *talosclient.Client
}

// Pruner frees per-node state that has no natural expiry; an empty live set (discovery not synced) prunes nothing.
type Pruner interface {
	Prune(live map[string]struct{})
}

// Degradation reasons are label values on talos_monitoring_collector_degraded, so they are contract.
const (
	// ReasonPermission: a file-read path switched off because the monitor ServiceAccount lacks os:admin.
	ReasonPermission = "permission"
	// ReasonInventory: a cached inventory that stopped refreshing, so the data is no longer observed.
	ReasonInventory = "inventory"
)

// Degradation is one thing a collector gave up on for one node while still returning usable data.
type Degradation struct {
	Reason string
	// Stopped is a whole-collector self-disable: the round made no RPC, so it must not vote on talos_node_up.
	Stopped bool
}

// DegradedReporter reports partial data after a nil Collect, which one error value cannot express.
type DegradedReporter interface {
	Degraded(node string) []Degradation
}
