// Package block collects block device inventory from the Talos COSI
// "block" namespace and exposes it as Prometheus info metrics.
//
// Currently covers the Disk resource (one per disk); other block types
// (volumes, filesystems, mounts) are out of scope for the hardware report.
package block

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosmachine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosblock "github.com/siderolabs/talos/pkg/machinery/resources/block"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the registry name of the block collector.
const Name = "block"

// localTransports are the transports that mean "attached to this machine". An
// allowlist, not a denylist: this is a hardware monitor, so an unrecognised
// transport is reported as network-attached rather than silently counted as
// local hardware.
//
// Everything else is storage the node reaches over a network — iSCSI above all
// (Kubernetes PersistentVolumes appear as "iSCSI Disk" / "VIRTUAL-DISK"), plus
// fibre channel, NBD and RBD. On the reference cluster that is 19 of 23
// transport-carrying disks, so counting them as local made a 4-disk fleet look
// like a 23-disk one.
var localTransports = map[string]bool{
	"nvme": true, "sata": true, "ata": true, "ide": true,
	"sas": true, "usb": true, "mmc": true, "virtio": true,
}

// Attachment classifies a block device by how the node reaches it. It is
// exported as a label so the distinction the dashboard draws is queryable in
// PromQL too, rather than something only the UI knows how to derive.
func Attachment(transport string) string {
	switch {
	case transport == "":
		return "virtual" // loop / device-mapper: no transport at all
	case localTransports[transport]:
		return "local"
	default:
		return "network" // iscsi, fc, nbd, rbd, nvme-tcp, ...
	}
}

// diskInventoryTTL is how often the COSI Disk list is refreshed. The collector
// runs on the live cadence for filesystem usage; disk topology does not move.
const diskInventoryTTL = time.Hour

// Collector implements collector.Collector for block devices: the disk
// inventory (COSI, effectively static) and filesystem/volume usage (machine
// API, live). They answer the same question — "what storage does this node
// have?" — on very different timescales, so the COSI list is throttled
// internally while usage is collected every tick.
type Collector struct {
	state  func(node *collector.NodeClient) state.CoreState
	newAPI func(node *collector.NodeClient) mountAPI

	mu    sync.Mutex
	cache map[string]diskInventory // keyed by node name

	// snap receives the typed inventory alongside the metrics.
	snap *snapshot.Store
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// diskInventory is a node's last successful Disk listing, kept so the metric
// can be re-registered on ticks that do not re-list. The scraper hands out a
// fresh registry per collection, so a skipped list would otherwise mean a
// missing metric rather than a cached one.
type diskInventory struct {
	ts   time.Time
	rows []diskRow
}

// New returns the block collector.
func New() *Collector {
	return &Collector{
		state: collector.DefaultState,
		newAPI: func(node *collector.NodeClient) mountAPI {
			return mountTalosAPI{client: node.Client}
		},
		cache: make(map[string]diskInventory),
	}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector: filesystem usage is live data.
func (c *Collector) Class() collector.Class { return collector.Live }

// DefaultTTL implements collector.DefaultTTLer: 0 follows the scrape interval,
// so --scrape-interval and --collectors.block.ttl both apply.
func (c *Collector) DefaultTTL() time.Duration { return 0 }

// Collect lists the node's block.Disk resources and registers them as
// talos_block_disk_info (value 1, identity fields as labels). An empty list is
// NOT an error: a node with no disks is unusual, not broken. Only the RPC
// itself failing is an error, and then the scraper keeps the last good data.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	name := node.Node.Name

	// Live: filesystem and PersistentVolume usage.
	resp, err := c.newAPI(node).Mounts(ctx)
	if err != nil {
		return fmt.Errorf("mounts: %w", err)
	}
	var mounts []*talosmachine.MountStat
	for _, m := range resp.GetMessages() {
		mounts = append(mounts, m.GetStats()...)
	}
	filesystems := registerMounts(name, mounts, reg)
	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.Filesystems = filesystems })
	}

	// Inventory: the disk list, re-listed at most hourly.
	c.mu.Lock()
	cached, ok := c.cache[name]
	c.mu.Unlock()
	if !ok || time.Since(cached.ts) >= diskInventoryTTL {
		if rows, err := c.listDisks(ctx, node); err == nil {
			cached = diskInventory{ts: time.Now(), rows: rows}
			c.mu.Lock()
			c.cache[name] = cached
			c.mu.Unlock()
		} else if !ok {
			return err
		}
	}
	if c.snap != nil {
		disks := make([]snapshot.Disk, 0, len(cached.rows))
		for _, r := range cached.rows {
			disks = append(disks, snapshot.Disk{
				Device:     r.Labels[1],
				Model:      r.Labels[2],
				Serial:     r.Labels[3],
				WWID:       r.Labels[4],
				UUID:       r.Labels[5],
				Transport:  r.Labels[6],
				SubSystem:  r.Labels[7],
				BusPath:    r.Labels[8],
				Attachment: r.Labels[9],
				SizeBytes:  r.Size,
				SectorSize: r.SectorSize,
				Rotational: r.Rotational == 1,
				Readonly:   r.Readonly == 1,
				CDROM:      r.CDROM == 1,
			})
		}
		sort.Slice(disks, func(i, j int) bool { return disks[i].Device < disks[j].Device })
		c.snap.Update(name, func(n *snapshot.Node) { n.Disks = disks })
	}
	if len(cached.rows) > 0 {
		g := diskGauge()
		m := newDiskMeasures()
		reg.MustRegister(append(m.collectors(), g)...)
		for _, r := range cached.rows {
			g.WithLabelValues(r.Labels...).Set(1)
			m.set(r.Node(), r.Device(), r)
		}
	}
	return nil
}

// diskGauge builds the disk identity metric. Identity strings only: the
// measurements it used to carry as labels are their own gauges below, so
// PromQL can do arithmetic on them and a resize moves a series instead of
// abandoning it.
func diskGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_block_disk_info",
		Help: "Disk identity (value 1), one series per disk.",
	}, []string{"node", "device", "model", "serial", "wwid", "uuid", "transport", "subsystem",
		"bus_path", "attachment"})
}

// diskMeasures are the per-disk numeric metrics, all keyed by (node, device).
type diskMeasures struct {
	size, sectorSize          *prometheus.GaugeVec
	rotational, readonly, cdr *prometheus.GaugeVec
}

func newDiskMeasures() diskMeasures {
	g := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help},
			[]string{"node", "device"})
	}
	return diskMeasures{
		size:       g("talos_block_disk_size_bytes", "Total capacity of the disk in bytes, as reported by the kernel."),
		sectorSize: g("talos_block_disk_sector_size_bytes", "Logical sector size in bytes (512 or 4096 on current hardware)."),
		rotational: g("talos_block_disk_rotational", "1 when the disk is rotational (spinning), 0 for solid state."),
		readonly:   g("talos_block_disk_readonly", "1 when the disk is read-only."),
		cdr:        g("talos_block_disk_cdrom", "1 when the device is an optical drive."),
	}
}

func (d diskMeasures) collectors() []prometheus.Collector {
	return []prometheus.Collector{d.size, d.sectorSize, d.rotational, d.readonly, d.cdr}
}

// set writes one disk's measurements.
func (d diskMeasures) set(node, device string, r diskRow) {
	d.size.WithLabelValues(node, device).Set(r.Size)
	d.sectorSize.WithLabelValues(node, device).Set(r.SectorSize)
	d.rotational.WithLabelValues(node, device).Set(r.Rotational)
	d.readonly.WithLabelValues(node, device).Set(r.Readonly)
	d.cdr.WithLabelValues(node, device).Set(r.CDROM)
}

// diskRow is one disk: the identity labels for _info plus the measurements
// that now have their own series.
type diskRow struct {
	Labels     []string // in diskGauge's label order
	Size       float64
	SectorSize float64
	Rotational float64
	Readonly   float64
	CDROM      float64
}

// Node returns the row's node label, and Device its device label.
func (r diskRow) Node() string   { return r.Labels[0] }
func (r diskRow) Device() string { return r.Labels[1] }

// listDisks lists the node's block.Disk resources.
func (c *Collector) listDisks(ctx context.Context, node *collector.NodeClient) ([]diskRow, error) {
	ctx = talosclient.WithNode(ctx, node.Node.IP)

	// Empty ID lists the whole kind from the block namespace.
	md := resource.NewMetadata(talosblock.NamespaceName, talosblock.DiskType, "", resource.VersionUndefined)
	list, err := c.state(node).List(ctx, md)
	if err != nil {
		return nil, fmt.Errorf("list Disk: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil // a node with no disks is not a failure
	}

	out := make([]diskRow, 0, len(list.Items))
	for _, r := range list.Items {
		d, ok := r.(*talosblock.Disk)
		if !ok {
			return nil, fmt.Errorf("unexpected resource %T in Disk list", r)
		}
		spec := d.TypedSpec()
		out = append(out, diskRow{
			Labels: []string{
				node.Node.Name,
				spec.DevPath,
				spec.Model,
				spec.Serial,
				spec.WWID,
				spec.UUID,
				spec.Transport,
				spec.SubSystem,
				spec.BusPath,
				Attachment(spec.Transport),
			},
			Size:       float64(spec.Size),
			SectorSize: float64(spec.SectorSize),
			Rotational: collector.BoolValue(spec.Rotational),
			Readonly:   collector.BoolValue(spec.Readonly),
			CDROM:      collector.BoolValue(spec.CDROM),
		})
	}
	return out, nil
}

// Prune implements collector.Pruner: drop the cached disk list for departed
// nodes.
func (c *Collector) Prune(live map[string]struct{}) {
	// An empty set means discovery has not synced yet, not that the cluster
	// is empty — pruning here would drop every node's state on a transient
	// watch blip. app.prune() guards this too; the Pruner contract says
	// implementations must, so they do.
	if len(live) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for node := range c.cache {
		if _, ok := live[node]; !ok {
			delete(c.cache, node)
		}
	}
}
