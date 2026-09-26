// Package diskio collects per-device block I/O counters from the DiskStats
// machine API RPC (the node's /proc/diskstats).
//
// This is the half of storage monitoring the block collector does not cover.
// block knows every disk's model, WWID, transport, capacity and how full it
// is; none of that says whether the disk is doing any work, how long it is
// taking, or how deep its queue is. Throughput, IOPS and latency are where a
// storage investigation starts, and they were the largest remaining gap
// against node_exporter.
//
// The RPC is os:reader-safe — unlike the sysfs collectors, this needs no
// os:admin and has no self-disable path.
package diskio

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "diskio"

// sectorBytes converts /proc/diskstats sector counts to bytes.
//
// The kernel always reports these in 512-byte units regardless of the device's
// real logical or physical sector size — see Documentation/admin-guide/iostats.rst.
// Multiplying by the disk's own sector size (4096 on most NVMe) would overstate
// throughput eightfold.
const sectorBytes = 512

// virtualRe matches devices with no hardware behind them: Talos mounts each
// system extension as a loop device (8 of them on the reference nodes), and
// ram/fd devices are equally meaningless here. zram is deliberately NOT in
// this set — compressed swap is real traffic worth seeing.
var virtualRe = regexp.MustCompile(`^(loop|ram|fd)[0-9]+$`)

// partitionRe splits a partition name into its parent disk and index:
// "nvme0n1p3" -> "nvme0n1", "sda1" -> "sda".
var partitionRe = regexp.MustCompile(`^(.*?)p?([0-9]+)$`)

// diskAPI is the subset of the Talos client the collector uses (test seam).
type diskAPI interface {
	DiskStats(ctx context.Context) (*machinepb.DiskStatsResponse, error)
}

type talosAPI struct{ client *talosclient.Client }

func (t talosAPI) DiskStats(ctx context.Context) (*machinepb.DiskStatsResponse, error) {
	return t.client.MachineClient.DiskStats(ctx, &emptypb.Empty{})
}

// Collector reads per-device I/O counters for one node.
type Collector struct {
	newAPI func(node *collector.NodeClient) diskAPI

	// snap receives the typed view alongside the metrics. nil disables it.
	snap *snapshot.Store
}

// New returns the diskio collector.
func New() *Collector {
	return &Collector{newAPI: func(node *collector.NodeClient) diskAPI {
		return talosAPI{client: node.Client}
	}}
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
//
// Live: these are rate sources. Sampling them on the inventory cadence would
// make every rate() a five-minute average and hide exactly the bursts worth
// looking at.
func (c *Collector) Class() collector.Class { return collector.Live }

// Collect reads the counters and registers them.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	resp, err := c.newAPI(node).DiskStats(ctx)
	if err != nil {
		return fmt.Errorf("diskstats: %w", err)
	}

	var devices []*machinepb.DiskStat
	for _, msg := range resp.GetMessages() {
		// Total is deliberately ignored: it is a pre-summed row that counts
		// partitions on top of their parent disk, so it overstates a machine
		// with a partitioned system disk. Sum the type="disk" devices instead.
		devices = append(devices, msg.GetDevices()...)
	}

	rows := classify(devices)
	register(node.Node.Name, rows, reg)
	if c.snap != nil {
		view := make([]snapshot.DiskIO, 0, len(rows))
		for _, r := range rows {
			view = append(view, snapshot.DiskIO{
				Device: r.Device, Kind: r.Kind,
				ReadBytes: r.ReadBytes, WrittenBytes: r.WrittenBytes,
				ReadsCompleted: r.ReadsCompleted, WritesCompleted: r.WritesCompleted,
				IOInProgress: r.IOInProgress, IOTimeSeconds: r.IOTimeSeconds,
			})
		}
		c.snap.Update(node.Node.Name, func(n *snapshot.Node) { n.DiskIO = view })
	}
	return nil
}

// row is one block device's counters, in base units.
type row struct {
	Device string
	Kind   string // disk | partition | virtual

	ReadsCompleted  float64
	ReadsMerged     float64
	ReadBytes       float64
	ReadTimeSeconds float64

	WritesCompleted  float64
	WritesMerged     float64
	WrittenBytes     float64
	WriteTimeSeconds float64

	IOInProgress          float64
	IOTimeSeconds         float64
	IOTimeWeightedSeconds float64

	DiscardsCompleted  float64
	DiscardsMerged     float64
	DiscardedBytes     float64
	DiscardTimeSeconds float64
}

// classify converts the RPC rows to base units and labels each device as a
// whole disk, a partition of one, or a virtual device.
//
// Partition detection is name-based against the same response: nvme0n1p3 is a
// partition because nvme0n1 is also present. That matters because a partition's
// counters are also counted in its parent, so summing every device double-counts
// every partitioned disk. The `kind` label on talos_block_io_info is what lets a
// query pick one level:
//
//	sum(rate(talos_block_io_read_bytes_total[5m]))
//	  * on(node,device) group_left talos_block_io_info{kind="disk"}
func classify(devices []*machinepb.DiskStat) []row {
	names := make(map[string]bool, len(devices))
	for _, d := range devices {
		names[d.GetName()] = true
	}

	out := make([]row, 0, len(devices))
	for _, d := range devices {
		name := d.GetName()
		if name == "" {
			continue
		}
		kind := "disk"
		switch {
		case virtualRe.MatchString(name):
			// Extension squashfs mounts and friends: no hardware, no value.
			continue
		case isPartitionOf(name, names):
			kind = "partition"
		case strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "dm-"):
			kind = "virtual"
		}
		ms := func(v uint64) float64 { return float64(v) / 1000 }
		sec := func(v uint64) float64 { return float64(v) * sectorBytes }
		out = append(out, row{
			Device: name, Kind: kind,

			ReadsCompleted:  float64(d.GetReadCompleted()),
			ReadsMerged:     float64(d.GetReadMerged()),
			ReadBytes:       sec(d.GetReadSectors()),
			ReadTimeSeconds: ms(d.GetReadTimeMs()),

			WritesCompleted:  float64(d.GetWriteCompleted()),
			WritesMerged:     float64(d.GetWriteMerged()),
			WrittenBytes:     sec(d.GetWriteSectors()),
			WriteTimeSeconds: ms(d.GetWriteTimeMs()),

			IOInProgress:          float64(d.GetIoInProgress()),
			IOTimeSeconds:         ms(d.GetIoTimeMs()),
			IOTimeWeightedSeconds: ms(d.GetIoTimeWeightedMs()),

			DiscardsCompleted:  float64(d.GetDiscardCompleted()),
			DiscardsMerged:     float64(d.GetDiscardMerged()),
			DiscardedBytes:     sec(d.GetDiscardSectors()),
			DiscardTimeSeconds: ms(d.GetDiscardTimeMs()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// isPartitionOf reports whether name is a partition of another device in the
// same set. Linux names partitions parent+N, with an interposed "p" when the
// parent's name already ends in a digit (nvme0n1p3, mmcblk0p1, sda1).
func isPartitionOf(name string, names map[string]bool) bool {
	m := partitionRe.FindStringSubmatch(name)
	if m == nil || m[1] == "" {
		return false
	}
	return names[m[1]]
}

// register exports the counters.
//
// Every _total is a counter: these are monotonic since boot, so rate() works
// from the first scrape and a reboot reads as a counter reset, which is
// exactly right. Time fields are seconds (the RPC carries milliseconds) and
// sector counts are bytes, per the base-unit convention.
func register(nodeName string, rows []row, reg prometheus.Registerer) {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_block_io_info",
		Help: "Block device identity for the I/O counters (value 1). kind is disk, partition or virtual; a partition's counters are also included in its parent, so join on kind=\"disk\" before summing across devices.",
	}, []string{"node", "device", "kind"})
	c := func(name, help string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help},
			[]string{"node", "device"})
	}
	g := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help},
			[]string{"node", "device"})
	}

	readsCompleted := c("talos_block_io_reads_completed_total", "Reads completed successfully.")
	readsMerged := c("talos_block_io_reads_merged_total", "Reads merged with an adjacent request before being issued.")
	readBytes := c("talos_block_io_read_bytes_total", "Bytes read, derived from 512-byte kernel sectors.")
	readTime := c("talos_block_io_read_time_seconds_total", "Time spent reading, summed over all requests; divide by reads_completed for mean read latency.")

	writesCompleted := c("talos_block_io_writes_completed_total", "Writes completed successfully.")
	writesMerged := c("talos_block_io_writes_merged_total", "Writes merged with an adjacent request before being issued.")
	writtenBytes := c("talos_block_io_written_bytes_total", "Bytes written, derived from 512-byte kernel sectors.")
	writeTime := c("talos_block_io_write_time_seconds_total", "Time spent writing, summed over all requests.")

	ioNow := g("talos_block_io_now", "Requests currently in flight on the device.")
	ioTime := c("talos_block_io_time_seconds_total", "Time the device spent with any I/O in flight; rate() gives utilisation as a fraction 0-1.")
	ioTimeWeighted := c("talos_block_io_time_weighted_seconds_total", "Time in flight weighted by queue depth; rate() gives the mean queue length.")

	discardsCompleted := c("talos_block_io_discards_completed_total", "Discard (TRIM) requests completed.")
	discardsMerged := c("talos_block_io_discards_merged_total", "Discard requests merged before being issued.")
	discardedBytes := c("talos_block_io_discarded_bytes_total", "Bytes discarded, derived from 512-byte kernel sectors.")
	discardTime := c("talos_block_io_discard_time_seconds_total", "Time spent on discard requests.")

	reg.MustRegister(info, readsCompleted, readsMerged, readBytes, readTime,
		writesCompleted, writesMerged, writtenBytes, writeTime,
		ioNow, ioTime, ioTimeWeighted,
		discardsCompleted, discardsMerged, discardedBytes, discardTime)

	for _, r := range rows {
		info.WithLabelValues(nodeName, r.Device, r.Kind).Set(1)
		set := func(cv *prometheus.CounterVec, v float64) { cv.WithLabelValues(nodeName, r.Device).Add(v) }
		set(readsCompleted, r.ReadsCompleted)
		set(readsMerged, r.ReadsMerged)
		set(readBytes, r.ReadBytes)
		set(readTime, r.ReadTimeSeconds)
		set(writesCompleted, r.WritesCompleted)
		set(writesMerged, r.WritesMerged)
		set(writtenBytes, r.WrittenBytes)
		set(writeTime, r.WriteTimeSeconds)
		set(ioTime, r.IOTimeSeconds)
		set(ioTimeWeighted, r.IOTimeWeightedSeconds)
		set(discardsCompleted, r.DiscardsCompleted)
		set(discardsMerged, r.DiscardsMerged)
		set(discardedBytes, r.DiscardedBytes)
		set(discardTime, r.DiscardTimeSeconds)
		ioNow.WithLabelValues(nodeName, r.Device).Set(r.IOInProgress)
	}
}

var _ collector.Collector = (*Collector)(nil)
