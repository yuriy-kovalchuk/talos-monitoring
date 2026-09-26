package hwinfo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// memAPI is the part of the Talos client the memory collector uses for live
// usage (test seam).
type memAPI interface {
	Memory(ctx context.Context) (*machinepb.MemoryResponse, error)
}

type memTalosAPI struct{ client *talosclient.Client }

func (m memTalosAPI) Memory(ctx context.Context) (*machinepb.MemoryResponse, error) {
	return m.client.MachineClient.Memory(ctx, &empty.Empty{})
}

// memInventoryTTL is how often the DIMM inventory is re-listed. The collector
// itself runs on the live cadence for usage; the COSI list behind the DIMM
// table changes only when someone opens the machine.
const memInventoryTTL = time.Hour

// MemoryCollector reports both live memory usage (machine API Memory, every
// tick) and the DIMM inventory (COSI hardware.MemoryModule, re-listed at most
// every memInventoryTTL and re-registered from cache in between).
//
// The two live in one collector because they answer the same question from a
// user's point of view — "what is this machine's memory?" — but they change on
// completely different timescales, hence the internal throttle.
type MemoryCollector struct {
	*Collector
	newAPI func(node *collector.NodeClient) memAPI

	mu    sync.Mutex
	cache map[string]memInventory // keyed by node name
}

// memInventory is a node's last successful DIMM listing, kept so the metric can
// be re-registered on ticks that do not re-list.
type memInventory struct {
	ts      time.Time
	modules []moduleRow // identity labels plus measurements
}

// WithSnapshot points the collector at a snapshot store.
//
// Shadows the embedded Collector's method deliberately: that one returns
// *Collector, so NewMemory().WithSnapshot(s) would hand the registry the base
// collector and silently drop this type's Collect, taking /proc/meminfo and
// the DIMM inventory with it.
func (m *MemoryCollector) WithSnapshot(s *snapshot.Store) *MemoryCollector {
	m.snap = s
	return m
}

// NewMemory returns the memory collector: live usage plus DIMM inventory.
func NewMemory() *MemoryCollector {
	return &MemoryCollector{
		Collector: newCollector(NameMemory, "MemoryModule",
			hardware.MemoryModuleType, registerMemory),
		newAPI: func(node *collector.NodeClient) memAPI {
			return memTalosAPI{client: node.Client}
		},
		cache: make(map[string]memInventory),
	}
}

// Class implements collector.Collector: usage is live data.
func (m *MemoryCollector) Class() collector.Class { return collector.Live }

// DefaultTTL implements collector.DefaultTTLer: follow the scrape interval by
// returning 0, so --scrape-interval and --collectors.memory.ttl both apply.
func (m *MemoryCollector) DefaultTTL() time.Duration { return 0 }

// Collect registers the live memory gauges and the DIMM inventory.
func (m *MemoryCollector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	name := node.Node.Name

	resp, err := m.newAPI(node).Memory(ctx)
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if len(resp.GetMessages()) == 0 || resp.GetMessages()[0].GetMeminfo() == nil {
		return errors.New("memory response is empty")
	}
	mi := resp.GetMessages()[0].GetMeminfo()
	registerMemInfo(name, mi, reg)
	if m.snap != nil {
		// meminfo reports kibibytes; the snapshot carries bytes, like the
		// metrics.
		const kib = 1024
		mem := snapshot.Memory{
			Present:     true,
			Total:       float64(mi.GetMemtotal()) * kib,
			Free:        float64(mi.GetMemfree()) * kib,
			Available:   float64(mi.GetMemavailable()) * kib,
			Buffers:     float64(mi.GetBuffers()) * kib,
			Cached:      float64(mi.GetCached()) * kib,
			SwapTotal:   float64(mi.GetSwaptotal()) * kib,
			SwapFree:    float64(mi.GetSwapfree()) * kib,
			Committed:   float64(mi.GetCommittedas()) * kib,
			CommitLimit: float64(mi.GetCommitlimit()) * kib,
		}
		m.snap.Update(name, func(n *snapshot.Node) { n.Memory = &mem })
	}

	// DIMM inventory: re-list at most hourly, otherwise re-register the cache.
	m.mu.Lock()
	cached, ok := m.cache[name]
	m.mu.Unlock()
	if !ok || time.Since(cached.ts) >= memInventoryTTL {
		if fresh, err := m.listModules(ctx, node); err == nil {
			cached = memInventory{ts: time.Now(), modules: fresh}
			m.mu.Lock()
			m.cache[name] = cached
			m.mu.Unlock()
		} else if !ok {
			// Nothing cached and the list failed: usage still registered above,
			// so report the failure without discarding it.
			return err
		}
	}
	if c := m.snap; c != nil {
		mods := make([]snapshot.Module, 0, len(cached.modules))
		for _, row := range cached.modules {
			mods = append(mods, snapshot.Module{
				Slot:          row.Labels[1],
				Manufacturer:  row.Labels[2],
				Product:       row.Labels[3],
				DeviceLocator: row.Labels[4],
				BankLocator:   row.Labels[5],
				SerialNumber:  row.Labels[6],
				AssetTag:      row.Labels[7],
				SizeBytes:     row.Size,
				SpeedTransfer: row.Speed,
			})
		}
		sort.Slice(mods, func(i, j int) bool { return mods[i].Slot < mods[j].Slot })
		c.Update(name, func(n *snapshot.Node) { n.Modules = mods })
	}
	if len(cached.modules) > 0 {
		g := memoryModuleGauge()
		size, speed := memoryModuleMeasures()
		reg.MustRegister(g, size, speed)
		for _, row := range cached.modules {
			g.WithLabelValues(row.Labels...).Set(1)
			size.WithLabelValues(row.Node(), row.Slot()).Set(row.Size)
			speed.WithLabelValues(row.Node(), row.Slot()).Set(row.Speed)
		}
	}
	return nil
}

// listModules lists the node's DIMMs and returns their label values.
func (m *MemoryCollector) listModules(ctx context.Context, node *collector.NodeClient) ([]moduleRow, error) {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	list, err := listKind(ctx, m.state(node), m.kindName, m.kind)
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, nil // no DIMM inventory (common on VMs); not a failure
	}
	return memoryModuleValues(node.Node.Name, list)
}

// memInfoGauges maps a metric name to the /proc/meminfo field behind it.
// Everything the dashboard shows must be queryable in Prometheus too (§1.1), and
// the export deliberately goes wider than the UI: meminfo is bounded and
// low-cardinality, so there is no reason to withhold any of it.
//
// meminfo reports kibibytes; these are exported in bytes per the base-unit
// naming convention.
func registerMemInfo(node string, mi *machinepb.MemInfo, reg prometheus.Registerer) {
	fields := []struct {
		name string
		help string
		val  uint64
	}{
		{"talos_node_memory_total_bytes", "Total usable RAM, excluding what the kernel image itself reserves.", mi.GetMemtotal()},
		{"talos_node_memory_free_bytes", "Memory not used by anything.", mi.GetMemfree()},
		{"talos_node_memory_available_bytes", "Memory available for new workloads without swapping.", mi.GetMemavailable()},
		{"talos_node_memory_buffers_bytes", "Block-device buffer cache.", mi.GetBuffers()},
		{"talos_node_memory_cached_bytes", "Page cache, excluding swap cache.", mi.GetCached()},
		{"talos_node_memory_shared_bytes", "Memory used by tmpfs and shared mappings.", mi.GetShmem()},
		{"talos_node_memory_anon_bytes", "Anonymous (non-file-backed) pages.", mi.GetAnonpages()},
		{"talos_node_memory_mapped_bytes", "Files mapped into memory.", mi.GetMapped()},
		{"talos_node_memory_slab_bytes", "Kernel slab allocator memory.", mi.GetSlab()},
		{"talos_node_memory_slab_reclaimable_bytes", "Slab memory the kernel can reclaim.", mi.GetSreclaimable()},
		{"talos_node_memory_page_tables_bytes", "Memory used by page tables.", mi.GetPagetables()},
		{"talos_node_memory_kernel_stack_bytes", "Memory used by kernel stacks.", mi.GetKernelstack()},
		{"talos_node_memory_dirty_bytes", "Memory waiting to be written back to disk.", mi.GetDirty()},
		{"talos_node_memory_writeback_bytes", "Memory actively being written back.", mi.GetWriteback()},
		{"talos_node_memory_committed_bytes", "Memory committed to running workloads (Committed_AS).", mi.GetCommittedas()},
		{"talos_node_memory_commit_limit_bytes", "Total memory currently allocatable (CommitLimit).", mi.GetCommitlimit()},
		{"talos_node_swap_total_bytes", "Total swap space. 0 on a node with no swap configured, which is the Talos default.", mi.GetSwaptotal()},
		{"talos_node_swap_free_bytes", "Unused swap space. Equal to the total when nothing has been swapped out.", mi.GetSwapfree()},
		{"talos_node_swap_cached_bytes", "Swap pages also held in memory.", mi.GetSwapcached()},
	}
	for _, f := range fields {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: f.name, Help: f.help}, []string{"node"})
		reg.MustRegister(g)
		g.WithLabelValues(node).Set(float64(f.val) * 1024)
	}

	// Huge pages are counts and a size, not byte totals.
	hp := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_memory_hugepages",
		Help: "Huge page counts (state: total, free, reserved, surplus).",
	}, []string{"node", "state"})
	reg.MustRegister(hp)
	hp.WithLabelValues(node, "total").Set(float64(mi.GetHugepagestotal()))
	hp.WithLabelValues(node, "free").Set(float64(mi.GetHugepagesfree()))
	hp.WithLabelValues(node, "reserved").Set(float64(mi.GetHugepagesrsvd()))
	hp.WithLabelValues(node, "surplus").Set(float64(mi.GetHugepagessurp()))

	hs := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_memory_hugepage_size_bytes",
		Help: "Size of a single huge page.",
	}, []string{"node"})
	reg.MustRegister(hs)
	hs.WithLabelValues(node).Set(float64(mi.GetHugepagesize()) * 1024)
}

// Prune implements collector.Pruner: drop the cached DIMM list for departed
// nodes.
func (m *MemoryCollector) Prune(live map[string]struct{}) {
	// An empty set means discovery has not synced yet, not that the cluster
	// is empty — pruning here would drop every node's state on a transient
	// watch blip. app.prune() guards this too; the Pruner contract says
	// implementations must, so they do.
	if len(live) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for node := range m.cache {
		if _, ok := live[node]; !ok {
			delete(m.cache, node)
		}
	}
}
