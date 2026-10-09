package hwinfo

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// pciFileAPI is the part of the Talos client used to read PCI sysfs (test seam).
type pciFileAPI interface {
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// pciSysfsRoot is where the kernel exposes per-device PCI attributes.
const pciSysfsRoot = "/sys/bus/pci/devices/"

// pciReadConcurrency bounds the sysfs reads. The pci collector runs hourly, so
// the wall time hardly matters; the limit is there to avoid opening dozens of
// concurrent streams against apid on a machine with many devices.
const pciReadConcurrency = 8

// pciAttrs are the per-device files read for every device. Absent files are not
// an error: a root-complex device has no link attributes, and AER files exist
// only where the port supports error reporting.
var pciAttrs = []string{
	"current_link_speed", "current_link_width",
	"max_link_speed", "max_link_width",
	"numa_node", "irq", "power_state", "enable",
	"revision", "subsystem_vendor", "subsystem_device",
	"aer_dev_correctable", "aer_dev_fatal", "aer_dev_nonfatal",
}

// PCICollector reports the COSI PCIDevice inventory enriched with the sysfs
// attributes that carry the diagnostic value: negotiated versus maximum link
// speed and width, NUMA locality, power state and AER error counters.
//
// COSI alone gives identity only — nine string fields, all of which the older
// collector already exported. A device negotiating x1 on an x16 slot, or
// accumulating correctable AER errors, is invisible without sysfs.
type PCICollector struct {
	*Collector
	log    *slog.Logger
	newAPI func(node *collector.NodeClient) pciFileAPI

	// disabled is set on the first permission error: file reads need os:admin,
	// so retrying every hour would only repeat the failure. The COSI inventory
	// keeps being reported.
	disabled atomic.Bool
}

// WithSnapshot points the collector at a snapshot store. Shadows the embedded
// Collector's method for the same reason as MemoryCollector's: returning
// *Collector would drop the sysfs enrichment.
func (c *PCICollector) WithSnapshot(s *snapshot.Store) *PCICollector {
	c.snap = s
	return c
}

// NewPCI returns the pci collector.
func NewPCI(log *slog.Logger) *PCICollector {
	return &PCICollector{
		// registerPCIEnriched, not a plain identity registration: Collect
		// below always overrides the embedded method, and having a second
		// function define talos_hw_pcidevice_info with a different label set
		// was a duplicate-series hazard (finding 3.3) that only looked live.
		Collector: newCollector(NamePCI, "PCIDevice",
			hardware.PCIDeviceType, func(node string, list resource.List, reg prometheus.Registerer) error {
				return registerPCIEnriched(node, list, nil, reg)
			}),
		log:    log,
		newAPI: func(node *collector.NodeClient) pciFileAPI { return node.Client },
	}
}

// snapshotPCI mirrors the device list and its sysfs enrichment into the store.
func (c *PCICollector) snapshotPCI(nodeName string, list resource.List, details map[string]pciDetail) {
	if c.snap == nil {
		return
	}
	devs := make([]snapshot.PCIDevice, 0, len(list.Items))
	for _, r := range list.Items {
		p, ok := r.(*hardware.PCIDevice)
		if !ok {
			continue
		}
		spec := p.TypedSpec()
		bdf := p.Metadata().ID()
		d := details[bdf]
		devs = append(devs, snapshot.PCIDevice{
			BDF:                bdf,
			Class:              spec.Class,
			Subclass:           spec.Subclass,
			Vendor:             spec.Vendor,
			Product:            spec.Product,
			ClassID:            spec.ClassID,
			SubclassID:         spec.SubclassID,
			VendorID:           spec.VendorID,
			ProductID:          spec.ProductID,
			Driver:             spec.Driver,
			Revision:           d.revision,
			SubsystemVendorID:  d.subsystemVendorID,
			SubsystemProductID: d.subsystemProductID,
			HasLink:            d.curSpeed > 0 || d.maxSpeed > 0,
			LinkSpeedGTps:      d.curSpeed,
			LinkWidth:          d.curWidth,
			MaxSpeedGTps:       d.maxSpeed,
			MaxWidth:           d.maxWidth,
			PowerState:         d.powerState,
			HasNUMA:            d.hasNUMA,
			NUMANode:           d.numaNode,
			HasIRQ:             d.hasIRQ,
			IRQ:                d.irq,
			HasEnabled:         d.hasEnabled,
			Enabled:            d.enabled,
			HasAER:             d.hasAER,
			AERCorrectable:     d.aerCorrectable,
			AERFatal:           d.aerFatal,
			AERNonFatal:        d.aerNonFatal,
		})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].BDF < devs[j].BDF })
	c.snap.Update(nodeName, func(n *snapshot.Node) { n.PCI = devs })
}

// pciDetail is one device's sysfs attributes.
type pciDetail struct {
	curSpeed, maxSpeed float64 // GT/s; 0 when the device reports none
	curWidth, maxWidth float64 // lanes
	numaNode           float64
	hasNUMA            bool
	irq                float64
	hasIRQ             bool
	powerState         string
	enabled            float64
	hasEnabled         bool
	revision           string
	subsystemVendorID  string
	subsystemProductID string
	aerCorrectable     float64
	aerFatal           float64
	aerNonFatal        float64
	hasAER             bool
}

// Collect lists the node's PCI devices and enriches them from sysfs.
func (c *PCICollector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	list, err := listKind(ctx, c.state(node), c.kindName, c.kind)
	if err != nil {
		return err
	}
	if len(list.Items) == 0 {
		return nil // no PCI bus reported; not a failure
	}

	details := map[string]pciDetail{}
	if !c.disabled.Load() {
		details = c.readAll(ctx, node, list)
	}
	if err := registerPCIEnriched(node.Node.Name, list, details, reg); err != nil {
		return err
	}
	c.snapshotPCI(node.Node.Name, list, details)
	return nil
}

type errPCIList string

func (e errPCIList) Error() string { return string(e) }

// readAll reads every device's attributes with bounded concurrency.
func (c *PCICollector) readAll(ctx context.Context, node *collector.NodeClient, list resource.List) map[string]pciDetail {
	api := c.newAPI(node)
	out := make(map[string]pciDetail, len(list.Items))

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, pciReadConcurrency)
	for _, r := range list.Items {
		bdf := r.Metadata().ID()
		wg.Add(1)
		go func(bdf string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, ok := c.readDevice(ctx, api, bdf, node.Node.Name)
			if !ok {
				return
			}
			mu.Lock()
			out[bdf] = d
			mu.Unlock()
		}(bdf)
	}
	wg.Wait()
	return out
}

// readDevice reads one device's attributes. ok is false only when nothing was
// readable, which on a permission error also disables the enrichment.
func (c *PCICollector) readDevice(ctx context.Context, api pciFileAPI, bdf, nodeName string) (pciDetail, bool) {
	var d pciDetail
	any := false
	for _, attr := range pciAttrs {
		raw, err := readPCIFile(ctx, api, pciSysfsRoot+bdf+"/"+attr)
		if err != nil {
			if collector.IsPermissionError(err) {
				c.disable(nodeName)
				return d, false
			}
			continue // absent attribute: normal for bridges and root ports
		}
		any = true
		applyPCIAttr(&d, attr, raw)
	}
	return d, any
}

// applyPCIAttr parses one attribute into the detail struct.
func applyPCIAttr(d *pciDetail, attr, raw string) {
	switch attr {
	case "current_link_speed":
		d.curSpeed = parseLinkSpeed(raw)
	case "max_link_speed":
		d.maxSpeed = parseLinkSpeed(raw)
	case "current_link_width":
		d.curWidth = parseFloat(raw)
	case "max_link_width":
		d.maxWidth = parseFloat(raw)
	case "numa_node":
		d.numaNode, d.hasNUMA = parseFloat(raw), true
	case "irq":
		d.irq, d.hasIRQ = parseFloat(raw), true
	case "power_state":
		d.powerState = raw
	case "enable":
		d.enabled, d.hasEnabled = parseFloat(raw), true
	case "revision":
		d.revision = raw
	case "subsystem_vendor":
		d.subsystemVendorID = raw
	case "subsystem_device":
		d.subsystemProductID = raw
	case "aer_dev_correctable":
		d.aerCorrectable, d.hasAER = sumAERCounters(raw), true
	case "aer_dev_fatal":
		d.aerFatal, d.hasAER = sumAERCounters(raw), true
	case "aer_dev_nonfatal":
		d.aerNonFatal, d.hasAER = sumAERCounters(raw), true
	}
}

// parseLinkSpeed turns "8.0 GT/s PCIe" into 8.0. "Unknown" yields 0.
func parseLinkSpeed(raw string) float64 {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

func parseFloat(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return v
}

// sumAERCounters totals an AER file, whose lines are "Name Count". The
// per-cause breakdown is deliberately not exported: it would be ~10 extra
// series per device per severity, and the total is what an alert watches.
func sumAERCounters(raw string) float64 {
	total := 0.0
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[1], 64); err == nil {
			total += v
		}
	}
	return total
}

func readPCIFile(ctx context.Context, api pciFileAPI, path string) (string, error) {
	s, err := collector.ReadFile(ctx, api, path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func (c *PCICollector) disable(nodeName string) {
	if c.disabled.CompareAndSwap(false, true) {
		c.log.Warn("pci sysfs enrichment disabled: file reads need the os:admin role (got a permission error)",
			"node", nodeName,
			"hint", "give the monitor ServiceAccount the os:admin role (Helm value talos.roles) and restart the exporter to re-enable; the COSI inventory keeps being reported")
	}
}

// Degraded implements collector.DegradedReporter. Not Stopped: the COSI device
// list is still collected, so the round still reaches the node. Only the sysfs
// enrichment - the negotiated link width and the AER counters - is off.
func (c *PCICollector) Degraded(string) []collector.Degradation {
	if c.disabled.Load() {
		return []collector.Degradation{{Reason: collector.ReasonPermission}}
	}
	return nil
}

// registerPCIEnriched registers the device inventory together with whatever
// sysfs detail was readable.
func registerPCIEnriched(nodeName string, list resource.List, details map[string]pciDetail, reg prometheus.Registerer) error {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_info",
		Help: "PCI device identity (value 1), one series per BDF.",
	}, []string{"node", "bdf", "class", "subclass", "vendor", "product", "class_id", "subclass_id",
		"vendor_id", "product_id", "driver", "revision", "subsystem_vendor_id", "subsystem_product_id"})
	// Negotiated state and the device's own maximum are separate families, not
	// one keyed by a `link` label: as a single family, avg() mixed what a link
	// is doing with what it could do.
	//
	// Transfers per second, not GT/s: base units, and consistent with
	// talos_hw_memory_module_speed_transfers_per_second, which had already
	// made this choice.
	speed := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_link_speed_transfers_per_second",
		Help: "Negotiated PCIe link speed in transfers per second (PCIe 3.0 x1 is 8e9). Below the max means the device negotiated a slower link than it supports.",
	}, []string{"node", "bdf"})
	maxSpeed := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_link_max_speed_transfers_per_second",
		Help: "Highest PCIe link speed the device supports, in transfers per second.",
	}, []string{"node", "bdf"})
	width := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_link_width_lanes",
		Help: "Negotiated PCIe link width in lanes.",
	}, []string{"node", "bdf"})
	maxWidth := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_link_max_width_lanes",
		Help: "Highest PCIe link width the device supports, in lanes.",
	}, []string{"node", "bdf"})
	// NUMA node and IRQ are identifiers, not measurements: avg(irq) is
	// arithmetic on a name, and "which devices share IRQ 16" is answerable
	// only if it is a label. Both are info metrics, and neither is emitted
	// when the platform reports nothing — no -1 sentinel.
	numa := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_numa_node_info",
		Help: "NUMA node the device is attached to (value 1). Absent when the platform reports none.",
	}, []string{"node", "bdf", "numa_node"})
	irq := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_irq_info",
		Help: "Interrupt line assigned to the device (value 1). Group by irq to find devices sharing one.",
	}, []string{"node", "bdf", "irq"})
	enabled := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_enabled",
		Help: "1 when the device is enabled.",
	}, []string{"node", "bdf"})
	power := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_power_state_info",
		Help: "Current PCI power state (value 1). Kept out of the identity metric because it changes as devices suspend.",
	}, []string{"node", "bdf", "state"})
	aer := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_hw_pcidevice_aer_errors_total",
		Help: "Advanced Error Reporting counters since boot, summed over causes (severity: correctable, fatal, nonfatal).",
	}, []string{"node", "bdf", "severity"})

	for _, c := range []prometheus.Collector{info, speed, maxSpeed, width, maxWidth, numa, irq, enabled, power, aer} {
		reg.MustRegister(c)
	}

	for _, r := range list.Items {
		p, ok := r.(*hardware.PCIDevice)
		if !ok {
			return errPCIList("unexpected resource in PCIDevice list")
		}
		spec := p.TypedSpec()
		bdf := p.Metadata().ID()
		d := details[bdf]

		info.WithLabelValues(nodeName, bdf, spec.Class, spec.Subclass, spec.Vendor, spec.Product,
			spec.ClassID, spec.SubclassID, spec.VendorID, spec.ProductID, spec.Driver,
			d.revision, d.subsystemVendorID, d.subsystemProductID).Set(1)

		// sysfs reports GT/s; the metric is transfers per second.
		if d.curSpeed > 0 {
			speed.WithLabelValues(nodeName, bdf).Set(d.curSpeed * 1e9)
			width.WithLabelValues(nodeName, bdf).Set(d.curWidth)
		}
		if d.maxSpeed > 0 {
			maxSpeed.WithLabelValues(nodeName, bdf).Set(d.maxSpeed * 1e9)
			maxWidth.WithLabelValues(nodeName, bdf).Set(d.maxWidth)
		}
		// -1 is the kernel's "no NUMA node"; an absent series says that better
		// than a magic number that pollutes min() and avg().
		if d.hasNUMA && d.numaNode >= 0 {
			numa.WithLabelValues(nodeName, bdf, strconv.FormatFloat(d.numaNode, 'f', -1, 64)).Set(1)
		}
		if d.hasIRQ {
			irq.WithLabelValues(nodeName, bdf, strconv.FormatFloat(d.irq, 'f', -1, 64)).Set(1)
		}
		if d.hasEnabled {
			enabled.WithLabelValues(nodeName, bdf).Set(d.enabled)
		}
		if d.powerState != "" {
			power.WithLabelValues(nodeName, bdf, d.powerState).Set(1)
		}
		if d.hasAER {
			aer.WithLabelValues(nodeName, bdf, "correctable").Add(d.aerCorrectable)
			aer.WithLabelValues(nodeName, bdf, "fatal").Add(d.aerFatal)
			aer.WithLabelValues(nodeName, bdf, "nonfatal").Add(d.aerNonFatal)
		}
	}
	return nil
}
