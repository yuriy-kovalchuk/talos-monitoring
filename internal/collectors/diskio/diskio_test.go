package diskio

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

type fakeAPI struct {
	resp *machinepb.DiskStatsResponse
	err  error
}

func (f fakeAPI) DiskStats(context.Context) (*machinepb.DiskStatsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func resp(total *machinepb.DiskStat, devices ...*machinepb.DiskStat) *machinepb.DiskStatsResponse {
	return &machinepb.DiskStatsResponse{Messages: []*machinepb.DiskStats{
		{Total: total, Devices: devices},
	}}
}

func dev(name string) *machinepb.DiskStat { return &machinepb.DiskStat{Name: name} }

func collectWith(t *testing.T, r *machinepb.DiskStatsResponse) (map[string]*dto.MetricFamily, *snapshot.Node) {
	t.Helper()
	snap := snapshot.New()
	c := New().WithSnapshot(snap)
	c.newAPI = func(*collector.NodeClient) diskAPI { return fakeAPI{resp: r} }
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(),
		&collector.NodeClient{Node: nodes.Node{Name: "node-1", IP: "10.0.0.1"}}, reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}
	return byName, snap.Node("node-1")
}

func valueFor(t *testing.T, families map[string]*dto.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	f, ok := families[name]
	if !ok {
		t.Fatalf("family %q not registered", name)
	}
next:
	for _, m := range f.GetMetric() {
		labels := make(map[string]string, len(m.GetLabel()))
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		for k, v := range want {
			if labels[k] != v {
				continue next
			}
		}
		if m.GetCounter() != nil {
			return m.GetCounter().GetValue()
		}
		return m.GetGauge().GetValue()
	}
	t.Fatalf("family %q has no sample matching %v", name, want)
	return 0
}

func devices(t *testing.T, families map[string]*dto.MetricFamily) map[string]string {
	t.Helper()
	out := map[string]string{}
	f, ok := families["talos_block_io_info"]
	if !ok {
		return out
	}
	for _, m := range f.GetMetric() {
		var d, k string
		for _, l := range m.GetLabel() {
			switch l.GetName() {
			case "device":
				d = l.GetValue()
			case "kind":
				k = l.GetValue()
			}
		}
		out[d] = k
	}
	return out
}

// /proc/diskstats always counts in 512-byte sectors regardless of the device's
// real sector size. Using the disk's own 4096-byte sectors would overstate
// throughput eightfold.
func TestSectorsAreAlwaysFiveTwelveBytes(t *testing.T) {
	d := dev("nvme0n1")
	d.ReadSectors, d.WriteSectors, d.DiscardSectors = 1000, 2000, 8
	families, _ := collectWith(t, resp(nil, d))

	l := map[string]string{"node": "node-1", "device": "nvme0n1"}
	if got := valueFor(t, families, "talos_block_io_read_bytes_total", l); got != 512000 {
		t.Errorf("read bytes = %v, want 512000 (1000 x 512)", got)
	}
	if got := valueFor(t, families, "talos_block_io_written_bytes_total", l); got != 1024000 {
		t.Errorf("written bytes = %v, want 1024000", got)
	}
	if got := valueFor(t, families, "talos_block_io_discarded_bytes_total", l); got != 4096 {
		t.Errorf("discarded bytes = %v, want 4096", got)
	}
}

// The RPC carries milliseconds; the metric contract wants seconds.
func TestTimesAreConvertedToSeconds(t *testing.T) {
	d := dev("sda")
	d.ReadTimeMs, d.WriteTimeMs, d.IoTimeMs, d.IoTimeWeightedMs, d.DiscardTimeMs = 1500, 250, 111610700, 4000, 60
	families, _ := collectWith(t, resp(nil, d))

	l := map[string]string{"node": "node-1", "device": "sda"}
	for name, want := range map[string]float64{
		"talos_block_io_read_time_seconds_total":     1.5,
		"talos_block_io_write_time_seconds_total":    0.25,
		"talos_block_io_time_seconds_total":          111610.7,
		"talos_block_io_time_weighted_seconds_total": 4,
		"talos_block_io_discard_time_seconds_total":  0.06,
	} {
		if got := valueFor(t, families, name, l); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// A partition's counters are also counted in its parent, so summing every
// device double-counts a partitioned disk. The kind label is what lets a query
// pick one level, and it must be derived, not guessed.
func TestPartitionsAreIdentifiedByTheirParent(t *testing.T) {
	families, _ := collectWith(t, resp(nil,
		dev("nvme0n1"), dev("nvme0n1p1"), dev("nvme0n1p4"),
		dev("sda"), dev("sda1"),
		dev("sdb1"), // no parent present: a whole disk that happens to end in a digit
		dev("zram0"),
	))

	want := map[string]string{
		"nvme0n1": "disk", "nvme0n1p1": "partition", "nvme0n1p4": "partition",
		"sda": "disk", "sda1": "partition",
		"sdb1":  "disk",
		"zram0": "virtual",
	}
	got := devices(t, families)
	for d, k := range want {
		if got[d] != k {
			t.Errorf("%s classified as %q, want %q", d, got[d], k)
		}
	}
}

// Talos mounts every system extension as a loop device — eight of them on the
// reference nodes, with no hardware behind them.
func TestVirtualDevicesAreDropped(t *testing.T) {
	families, snap := collectWith(t, resp(nil,
		dev("loop0"), dev("loop7"), dev("ram0"), dev("fd0"),
		dev("nvme0n1"), dev("zram0"),
	))

	got := devices(t, families)
	for _, d := range []string{"loop0", "loop7", "ram0", "fd0"} {
		if _, ok := got[d]; ok {
			t.Errorf("%s was collected; loop/ram/fd have no hardware behind them", d)
		}
	}
	// zram is compressed swap — real traffic, kept and labelled virtual.
	if got["zram0"] != "virtual" {
		t.Errorf("zram0 kind = %q, want virtual", got["zram0"])
	}
	if len(snap.DiskIO) != 2 {
		t.Errorf("snapshot has %d devices, want 2 (nvme0n1, zram0)", len(snap.DiskIO))
	}
}

// The Total row sums partitions on top of their parents, so it overstates any
// machine with a partitioned system disk.
func TestTotalRowIsIgnored(t *testing.T) {
	total := dev("total")
	total.ReadSectors = 999999
	families, _ := collectWith(t, resp(total, dev("nvme0n1")))

	if _, ok := devices(t, families)["total"]; ok {
		t.Error("the pre-summed Total row was exported as a device")
	}
	if n := len(devices(t, families)); n != 1 {
		t.Errorf("%d devices exported, want 1", n)
	}
}

// rate() only works if these are counters. A gauge would break every
// throughput query and silently misreport reboots.
func TestCumulativeSeriesAreCounters(t *testing.T) {
	families, _ := collectWith(t, resp(nil, dev("nvme0n1")))
	for _, name := range []string{
		"talos_block_io_read_bytes_total", "talos_block_io_written_bytes_total",
		"talos_block_io_reads_completed_total", "talos_block_io_writes_completed_total",
		"talos_block_io_time_seconds_total", "talos_block_io_discards_completed_total",
	} {
		f, ok := families[name]
		if !ok {
			t.Fatalf("family %q missing", name)
		}
		if f.GetType() != dto.MetricType_COUNTER {
			t.Errorf("%s is %v, want COUNTER", name, f.GetType())
		}
	}
	// Queue depth is a point-in-time reading, not cumulative.
	if f := families["talos_block_io_now"]; f.GetType() != dto.MetricType_GAUGE {
		t.Errorf("talos_block_io_now is %v, want GAUGE", f.GetType())
	}
}

func TestRPCErrorFailsTheCollection(t *testing.T) {
	c := New()
	wantErr := errors.New("unavailable")
	c.newAPI = func(*collector.NodeClient) diskAPI { return fakeAPI{err: wantErr} }
	err := c.Collect(context.Background(),
		&collector.NodeClient{Node: nodes.Node{Name: "node-1"}}, prometheus.NewRegistry())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestCollectorIdentity(t *testing.T) {
	c := New()
	if c.Name() != "diskio" {
		t.Errorf("Name = %q, want diskio", c.Name())
	}
	// Live: these are rate sources; the inventory cadence would turn every
	// rate() into a five-minute average.
	if c.Class() != collector.Live {
		t.Errorf("Class = %v, want Live", c.Class())
	}
}

// The structural guard: every string this collector writes to the snapshot
// must be reachable from /metrics, and its families must obey the contract.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	d := dev("nvme0n1")
	d.ReadSectors, d.WriteSectors, d.ReadTimeMs = 1000, 2000, 1500
	families, snap := collectWith(t, resp(nil, d, dev("nvme0n1p1"), dev("zram0")))

	fams := make([]*dto.MetricFamily, 0, len(families))
	for _, f := range families {
		fams = append(fams, f)
	}
	opts := metricguard.Options{}
	for _, v := range metricguard.Validate(fams, opts) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(snap.DiskIO, fams, opts) {
		t.Error(v)
	}
}
