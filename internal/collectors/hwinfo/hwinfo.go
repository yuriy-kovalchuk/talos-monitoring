// Package hwinfo collects hardware inventory from the Talos COSI "hardware"
// namespace and exposes it as Prometheus info metrics.
//
// It provides four per-resource collectors — system (SystemInformation, one
// per node), processor (one per socket), memory (one per DIMM slot) and
// pci (one per BDF) — so each resource can be scraped at its own frequency.
package hwinfo

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Collector names (used in the --collectors.<name>.enabled and TTL flags).
const (
	NameSystem    = "system"
	NameProcessor = "processor"
	NameMemory    = "memory"
	NamePCI       = "pci"
)

// defaultTTL: hardware inventory effectively never changes.
const defaultTTL = time.Hour

// Collector lists one COSI hardware resource kind per node and registers
// it as talos_hw_*_info (value 1, identity fields as labels).
type Collector struct {
	name     string
	kindName string // display name for errors, e.g. "SystemInformation"
	kind     resource.Type
	register func(nodeName string, list resource.List, reg prometheus.Registerer) error

	// state returns the COSI state to query for a node; overridable in tests.
	state func(node *collector.NodeClient) state.CoreState

	// snap receives the typed inventory alongside the metrics, so the UI
	// reads structs instead of re-parsing labels. nil disables it.
	snap *snapshot.Store
}

// WithSnapshot points the collector at a snapshot store. Returns the receiver
// so it can be chained in the registry list.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// NewSystem returns the system collector (SystemInformation).
func NewSystem() *Collector {
	return newCollector(NameSystem, "SystemInformation",
		hardware.SystemInformationType, registerSystem)
}

// NewProcessor returns the processor collector (one per socket).
func NewProcessor() *Collector {
	return newCollector(NameProcessor, "Processor",
		hardware.ProcessorType, registerProcessor)
}

func newCollector(name, kindName string, kind resource.Type,
	register func(nodeName string, list resource.List, reg prometheus.Registerer) error) *Collector {
	return &Collector{
		name:     name,
		kindName: kindName,
		kind:     kind,
		register: register,
		state:    collector.DefaultState,
	}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return c.name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Inventory }

// DefaultTTL implements collector.DefaultTTLer.
func (c *Collector) DefaultTTL() time.Duration { return defaultTTL }

// Collect lists the resource kind on the node and registers it as
// talos_hw_*_info. An empty list is NOT an error — a VM with no DMI tables or
// no PCI bus simply has none of this resource, and reporting that as a failure
// made the error counter climb forever and could pull talos_node_up to 0 on a
// healthy machine. Only the RPC itself failing is an error.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	list, err := listKind(ctx, c.state(node), c.kindName, c.kind)
	if err != nil {
		return err
	}
	if len(list.Items) == 0 {
		// The RPC succeeded and the node simply has none of this resource — a
		// VM with no DMI tables, no DIMM inventory, no PCI bus. Reporting that
		// as an error made the counter climb forever, logged at Warn every
		// interval (there is never cached data to fall back to) and could pull
		// talos_node_up to 0 for a healthy machine.
		return nil
	}
	if err := c.register(node.Node.Name, list, reg); err != nil {
		return err
	}
	c.snapshot(node.Node.Name, list)
	return nil
}

// listKind lists a whole kind from the node's COSI state (empty-ID metadata
// means "all resources of this kind").
func listKind(ctx context.Context, st state.CoreState, name string, kind resource.Type) (resource.List, error) {
	md := resource.NewMetadata(hardware.NamespaceName, kind, "", resource.VersionUndefined)
	list, err := st.List(ctx, md)
	if err != nil {
		return resource.List{}, fmt.Errorf("list %s: %w", name, err)
	}
	return list, nil
}

// snapshot mirrors the listed resources into the typed store. Each kind knows
// its own shape, so this switches on the collector's kind rather than adding a
// second function pointer beside register.
func (c *Collector) snapshot(nodeName string, list resource.List) {
	if c.snap == nil {
		return
	}
	switch c.kind {
	case hardware.SystemInformationType:
		for _, r := range list.Items {
			si, ok := r.(*hardware.SystemInformation)
			if !ok {
				continue
			}
			spec := si.TypedSpec()
			sys := snapshot.System{
				Manufacturer: spec.Manufacturer,
				Product:      spec.ProductName,
				Version:      spec.Version,
				SerialNumber: spec.SerialNumber,
				UUID:         spec.UUID,
				SKUNumber:    spec.SKUNumber,
				WakeUpType:   spec.WakeUpType,
			}
			c.snap.Update(nodeName, func(n *snapshot.Node) { n.System = &sys })
		}
	case hardware.ProcessorType:
		sockets := make([]snapshot.Socket, 0, len(list.Items))
		for _, r := range list.Items {
			p, ok := r.(*hardware.Processor)
			if !ok {
				continue
			}
			spec := p.TypedSpec()
			sockets = append(sockets, snapshot.Socket{
				ID:           p.Metadata().ID(),
				Product:      spec.ProductName,
				PartNumber:   spec.PartNumber,
				SerialNumber: spec.SerialNumber,
				AssetTag:     spec.AssetTag,
				Cores:        int(spec.CoreCount),
				CoresEnabled: int(spec.CoreEnabled),
				Threads:      int(spec.ThreadCount),
				MaxHertz:     float64(spec.MaxSpeed) * 1e6,
				BootHertz:    float64(spec.BootSpeed) * 1e6,
				Status:       int(spec.Status),
			})
		}
		sort.Slice(sockets, func(i, j int) bool { return sockets[i].ID < sockets[j].ID })
		c.snap.Update(nodeName, func(n *snapshot.Node) { n.Sockets = sockets })
	}
}

func registerSystem(nodeName string, list resource.List, reg prometheus.Registerer) error {
	siG := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_system_info",
		Help: "System hardware identity (value 1), one series per node.",
	}, []string{"node", "manufacturer", "product", "version", "serial_number", "uuid", "sku_number", "wake_up_type"})
	reg.MustRegister(siG)
	for _, r := range list.Items {
		si, ok := r.(*hardware.SystemInformation)
		if !ok {
			return fmt.Errorf("unexpected resource %T in SystemInformation list", r)
		}
		spec := si.TypedSpec()
		siG.WithLabelValues(
			nodeName,
			spec.Manufacturer,
			spec.ProductName,
			spec.Version,
			spec.SerialNumber,
			spec.UUID,
			spec.SKUNumber,
			spec.WakeUpType,
		).Set(1)
	}
	return nil
}

func registerProcessor(nodeName string, list resource.List, reg prometheus.Registerer) error {
	cpuG := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_processor_info",
		Help: "CPU socket identity (value 1), one series per socket.",
	}, []string{"node", "socket", "product", "part_number", "serial_number", "asset_tag"})

	// The counts and speeds used to ride as labels on the _info metric, where
	// PromQL could not sum or compare them. Each is its own gauge keyed by
	// (node, socket). SMBIOS reports speeds in MHz; these are hertz per the
	// base-unit convention.
	g := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help},
			[]string{"node", "socket"})
	}
	cores := g("talos_hw_processor_cores", "Physical cores on the socket.")
	coresEnabled := g("talos_hw_processor_cores_enabled", "Cores enabled on the socket.")
	threads := g("talos_hw_processor_threads", "Hardware threads on the socket.")
	maxHz := g("talos_hw_processor_max_hertz", "Maximum rated clock of the socket, in hertz.")
	bootHz := g("talos_hw_processor_boot_hertz", "Clock at boot, in hertz.")
	// The raw SMBIOS status word is an opaque bitfield (65 = populated +
	// enabled), unqueryable without the spec to hand. Export the two bits that
	// carry meaning as booleans, and keep the raw value with its encoding
	// documented for anyone who wants the rest.
	populated := g("talos_hw_processor_socket_populated", "1 when the socket holds a CPU (SMBIOS status bit 6).")
	enabled := g("talos_hw_processor_enabled", "1 when the CPU is enabled (SMBIOS status bits 0-2 = 1).")
	status := g("talos_hw_processor_status",
		"Raw SMBIOS processor status word: bit 6 = socket populated, bits 0-2 = CPU status "+
			"(1 enabled, 2 disabled by user, 3 disabled by BIOS, 4 idle). 65 = populated and enabled.")
	reg.MustRegister(cpuG, cores, coresEnabled, threads, maxHz, bootHz, status, populated, enabled)

	for _, r := range list.Items {
		p, ok := r.(*hardware.Processor)
		if !ok {
			return fmt.Errorf("unexpected resource %T in Processor list", r)
		}
		spec := p.TypedSpec()
		socket := p.Metadata().ID()
		cpuG.WithLabelValues(
			nodeName,
			socket,
			spec.ProductName,
			spec.PartNumber,
			spec.SerialNumber,
			spec.AssetTag,
		).Set(1)
		cores.WithLabelValues(nodeName, socket).Set(float64(spec.CoreCount))
		coresEnabled.WithLabelValues(nodeName, socket).Set(float64(spec.CoreEnabled))
		threads.WithLabelValues(nodeName, socket).Set(float64(spec.ThreadCount))
		maxHz.WithLabelValues(nodeName, socket).Set(float64(spec.MaxSpeed) * 1e6)
		bootHz.WithLabelValues(nodeName, socket).Set(float64(spec.BootSpeed) * 1e6)
		status.WithLabelValues(nodeName, socket).Set(float64(spec.Status))
		populated.WithLabelValues(nodeName, socket).Set(collector.BoolValue(spec.Status&0x40 != 0))
		enabled.WithLabelValues(nodeName, socket).Set(collector.BoolValue(spec.Status&0x07 == 1))
	}
	return nil
}

// memoryModuleGauge builds the DIMM identity metric.
func memoryModuleGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_memory_module_info",
		Help: "Memory module identity (value 1), one series per DIMM.",
	}, []string{"node", "slot", "manufacturer", "product", "device_locator", "bank_locator",
		"serial_number", "asset_tag"})
}

// memoryModuleMeasures are the per-DIMM numeric metrics, keyed by (node, slot).
//
// Speed is transfers per second, not hertz: SMBIOS reports DDR4-3200 as 3200,
// which is 3200 MT/s on a 1600 MHz clock. Naming that "_hertz" would be wrong
// by a factor of two, so it is _speed_transfers_per_second.
func memoryModuleMeasures() (size, speed *prometheus.GaugeVec) {
	size = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_memory_module_size_bytes",
		Help: "Capacity of the memory module in bytes.",
	}, []string{"node", "slot"})
	speed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_memory_module_speed_transfers_per_second",
		Help: "Rated transfer rate of the memory module (MT/s x 1e6); DDR4-3200 reports 3.2e9.",
	}, []string{"node", "slot"})
	return size, speed
}

// moduleRow is one DIMM: identity labels plus its measurements.
type moduleRow struct {
	Labels []string // in memoryModuleGauge's label order
	Size   float64  // bytes
	Speed  float64  // transfers per second
}

// Node and Slot are the row's key.
func (r moduleRow) Node() string { return r.Labels[0] }
func (r moduleRow) Slot() string { return r.Labels[1] }

// memoryModuleValues extracts one label-value row per DIMM, in the gauge's
// label order. Kept separate from registration so the memory collector can
// re-register a cached listing without re-querying the node.
func memoryModuleValues(nodeName string, list resource.List) ([]moduleRow, error) {
	out := make([]moduleRow, 0, len(list.Items))
	for _, r := range list.Items {
		m, ok := r.(*hardware.MemoryModule)
		if !ok {
			return nil, fmt.Errorf("unexpected resource %T in MemoryModule list", r)
		}
		spec := m.TypedSpec()
		out = append(out, moduleRow{
			Labels: []string{
				nodeName,
				m.Metadata().ID(),
				spec.Manufacturer,
				spec.ProductName,
				spec.DeviceLocator,
				spec.BankLocator,
				spec.SerialNumber,
				spec.AssetTag,
			},
			Size:  float64(spec.Size) * 1024 * 1024, // SMBIOS reports MiB
			Speed: float64(spec.Speed) * 1e6,        // MT/s
		})
	}
	return out, nil
}

// registerMemory keeps the generic Collector signature working for tests and
// any caller that lists and registers in one step.
func registerMemory(nodeName string, list resource.List, reg prometheus.Registerer) error {
	vals, err := memoryModuleValues(nodeName, list)
	if err != nil {
		return err
	}
	g := memoryModuleGauge()
	size, speed := memoryModuleMeasures()
	reg.MustRegister(g, size, speed)
	for _, v := range vals {
		g.WithLabelValues(v.Labels...).Set(1)
		size.WithLabelValues(v.Node(), v.Slot()).Set(v.Size)
		speed.WithLabelValues(v.Node(), v.Slot()).Set(v.Speed)
	}
	return nil
}
