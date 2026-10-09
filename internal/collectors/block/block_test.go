package block

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosblock "github.com/siderolabs/talos/pkg/machinery/resources/block"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

// fakeState implements only List of state.CoreState; the rest panic if used.
type fakeState struct {
	state.CoreState
	list resource.List
	err  error
}

func (f fakeState) List(_ context.Context, _ resource.Kind, _ ...state.ListOption) (resource.List, error) {
	return f.list, f.err
}

// stubMountAPI is a canned Mounts response.
type stubMountAPI struct {
	stats []*machinepb.MountStat
	err   error
}

func (s stubMountAPI) Mounts(context.Context) (*machinepb.MountsResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &machinepb.MountsResponse{Messages: []*machinepb.Mounts{{Stats: s.stats}}}, nil
}

func testCollector(st state.CoreState, mounts ...*machinepb.MountStat) *Collector {
	c := New()
	c.state = func(*collector.NodeClient) state.CoreState { return st }
	c.newAPI = func(*collector.NodeClient) mountAPI { return stubMountAPI{stats: mounts} }
	return c
}

// mount is a MountStat literal.
func mount(fs string, size, avail uint64, on string) *machinepb.MountStat {
	return &machinepb.MountStat{Filesystem: fs, Size: size, Available: avail, MountedOn: on}
}

func testNode() *collector.NodeClient {
	return &collector.NodeClient{Node: nodes.Node{Name: "node-a", IP: "10.0.0.1"}}
}

func fixtures() fakeState {
	nvme := talosblock.NewDisk("block", "nvme0n1")
	nvmeSpec := nvme.TypedSpec()
	nvmeSpec.DevPath = "/dev/nvme0n1"
	nvmeSpec.SetSize(931510603776)
	nvmeSpec.SectorSize = 512
	nvmeSpec.Model = "SM2263EN NVMe"
	nvmeSpec.Serial = "NVME-SN-1"
	nvmeSpec.WWID = "3126f8192263abc"
	nvmeSpec.UUID = "uuid-nvme-1"
	nvmeSpec.Transport = "nvme"
	nvmeSpec.SubSystem = "nvme"
	nvmeSpec.BusPath = "0000:01:00.0"

	sata := talosblock.NewDisk("block", "sda")
	sataSpec := sata.TypedSpec()
	sataSpec.DevPath = "/dev/sda"
	sataSpec.SetSize(1000204886016) // 931 GiB
	sataSpec.SectorSize = 512
	sataSpec.Rotational = true
	sataSpec.Model = "WDC WD10EZEX"
	sataSpec.Serial = "SATA-SN-1"
	sataSpec.WWID = "wd-wwid-1"
	sataSpec.Transport = "sata"
	sataSpec.SubSystem = "scsi"
	sataSpec.BusPath = "0000:02:00.0"

	return fakeState{list: resource.List{Items: []resource.Resource{nvme, sata}}}
}

func TestCollectDisks(t *testing.T) {
	c := testCollector(fixtures())
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}

	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var metrics []*dto.Metric
	for _, f := range fams {
		if f.GetName() == "talos_block_disk_info" {
			metrics = f.GetMetric()
		}
	}
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics (one per disk), got %d", len(metrics))
	}
	got := make(map[string]map[string]string, len(metrics))
	for _, m := range metrics {
		labels := make(map[string]string, len(m.GetLabel()))
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		got[labels["device"]] = labels
	}
	nvme, ok := got["/dev/nvme0n1"]
	if !ok {
		t.Fatalf("/dev/nvme0n1 not in metrics: %v", got)
	}
	wantNVMe := map[string]string{
		"node":      "node-a",
		"model":     "SM2263EN NVMe",
		"serial":    "NVME-SN-1",
		"wwid":      "3126f8192263abc",
		"uuid":      "uuid-nvme-1",
		"transport": "nvme",
		"subsystem": "nvme",
		"bus_path":  "0000:01:00.0",
	}
	for k, v := range wantNVMe {
		if nvme[k] != v {
			t.Errorf("nvme label %s = %q, want %q", k, nvme[k], v)
		}
	}
	// The measurements are gauges now, not label strings, so PromQL can sum
	// and compare them (finding 3.1).
	for _, tc := range []struct {
		metric, device string
		want           float64
	}{
		{"talos_block_disk_size_bytes", "/dev/nvme0n1", 931510603776},
		{"talos_block_disk_sector_size_bytes", "/dev/nvme0n1", 512},
		{"talos_block_disk_rotational", "/dev/nvme0n1", 0},
		{"talos_block_disk_readonly", "/dev/nvme0n1", 0},
		{"talos_block_disk_cdrom", "/dev/nvme0n1", 0},
		{"talos_block_disk_rotational", "/dev/sda", 1},
	} {
		if v := gaugeFor(t, fams, tc.metric, "device", tc.device); v != tc.want {
			t.Errorf("%s{device=%q} = %v, want %v", tc.metric, tc.device, v, tc.want)
		}
	}
	// The measurements must no longer be labels on _info: a resize has to move
	// the series, not abandon it and start a new one.
	for _, gone := range []string{"size_bytes", "size", "sector_size", "readonly", "cdrom", "rotational"} {
		if _, ok := nvme[gone]; ok {
			t.Errorf("label %q is still on talos_block_disk_info", gone)
		}
	}
	if got["/dev/sda"]["transport"] != "sata" {
		t.Errorf("sda transport = %q, want sata", got["/dev/sda"]["transport"])
	}
}

func TestCollectListError(t *testing.T) {
	c := testCollector(fakeState{err: errors.New("boom")})
	reg := prometheus.NewRegistry()
	err := c.Collect(context.Background(), testNode(), reg)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want wrapped 'boom'", err)
	}
}

// TestCollectEmptyList pins finding 1.5: a successful list that comes back
// empty means the node has no disks, not that the scrape failed. Reporting it
// as an error made the error counter climb forever and logged at Warn every
// interval, because there is never cached data to fall back to.
func TestCollectEmptyList(t *testing.T) {
	c := testCollector(fakeState{})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("empty disk list should not be an error, got %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() == "talos_block_disk_info" {
			t.Error("expected no disk metric when the node reports no disks")
		}
	}
}

func TestRegisterMountsClassifiesAndFilters(t *testing.T) {
	const gb = 1 << 30
	c := testCollector(fixtures(),
		// A CSI volume, seen twice on the node: the driver's globalmount and
		// the pod-level mount. Only one series may result.
		mount("/dev/sda", 10*gb, 9*gb, "/var/lib/kubelet/plugins/kubernetes.io/csi/org.democratic-csi.iscsi/abc/globalmount"),
		mount("/dev/sda", 10*gb, 9*gb, "/var/lib/kubelet/pods/pod-uid-1/volumes/kubernetes.io~csi/pvc-1111/mount"),
		// An NFS-backed volume: the device is not /dev/.
		mount("10.0.3.3:/mnt/tank/pvc-2222", 100*gb, 95*gb, "/var/lib/kubelet/pods/pod-uid-2/volumes/kubernetes.io~csi/pvc-2222/mount"),
		// Workload mounts that are not volumes.
		mount("tmpfs", 32*gb, 32*gb, "/var/lib/kubelet/pods/pod-uid-3/volumes/kubernetes.io~secret/certs"),
		// The node's own filesystem, plus a bind mount of the same device.
		mount("/dev/nvme0n1p4", 125*gb, 93*gb, "/var"),
		mount("none", 125*gb, 93*gb, "/opt"),
		// The read-only squashfs root reports zero size and would read 100% full.
		mount("rootfs", 0, 0, "/"),
	)
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	series := func(name string) map[string]float64 {
		out := map[string]float64{}
		for _, f := range fams {
			if f.GetName() != name {
				continue
			}
			for _, m := range f.GetMetric() {
				key := ""
				for _, l := range m.GetLabel() {
					if l.GetName() != "node" {
						key += l.GetValue() + " "
					}
				}
				out[key] = m.GetGauge().GetValue()
			}
		}
		return out
	}

	// Volumes: one series per PV, not one per mount.
	vs := series("talos_volume_size_bytes")
	if len(vs) != 2 {
		t.Errorf("got %d volume series %v, want 2 (globalmount must be deduped)", len(vs), vs)
	}
	if vs["pvc-1111 "] != 10*gb {
		t.Errorf("pvc-1111 size = %v, want %v", vs["pvc-1111 "], float64(10*gb))
	}
	// used = size - available.
	if u := series("talos_volume_used_bytes")["pvc-1111 "]; u != 1*gb {
		t.Errorf("pvc-1111 used = %v, want %v (size - available)", u, float64(1*gb))
	}
	// The NFS volume is present and labelled as network-reached.
	if _, ok := vs["pvc-2222 "]; !ok {
		t.Error("NFS-backed volume missing: the filter must key on the mountpoint, not the device")
	}
	info := series("talos_volume_info")
	foundNetwork := false
	for k := range info {
		if strings.Contains(k, "network") {
			foundNetwork = true
		}
	}
	if !foundNetwork {
		t.Errorf("no volume labelled as network-reached, got %v", info)
	}

	// Node filesystems: /var only. The /opt bind mount of the same device is
	// deduped, the secret tmpfs is a workload mount, and the 0-size root is
	// skipped so it does not render as permanently full.
	fs := series("talos_filesystem_size_bytes")
	if len(fs) != 1 {
		t.Errorf("got %d filesystem series %v, want 1 (/var)", len(fs), fs)
	}
	if _, ok := fs["/var "]; !ok {
		t.Errorf("expected /var, got %v", fs)
	}
}

func TestDiskInventoryThrottledButAlwaysRegistered(t *testing.T) {
	c := testCollector(fixtures(), mount("/dev/nvme0n1p4", 1<<30, 1<<29, "/var"))
	for i := 0; i < 3; i++ {
		reg := prometheus.NewRegistry()
		if err := c.Collect(context.Background(), testNode(), reg); err != nil {
			t.Fatalf("collect %d: %v", i, err)
		}
		fams, _ := reg.Gather()
		found := false
		for _, f := range fams {
			if f.GetName() == "talos_block_disk_info" {
				found = true
			}
		}
		if !found {
			t.Fatalf("tick %d: disk inventory missing from a cached tick", i)
		}
	}
}

// gaugeFor returns the value of the named metric whose key label matches.
func gaugeFor(t *testing.T, fams []*dto.MetricFamily, name, key, value string) float64 {
	t.Helper()
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == key && lp.GetValue() == value {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("metric %s{%s=%q} not found", name, key, value)
	return 0
}

// TestPruneEmptySetKeepsEverything pins the Pruner contract: an empty live
// set means discovery has not synced yet, never "the cluster is empty" —
// dropping state there would wipe the cache on a transient watch blip.
func TestPruneEmptySetKeepsEverything(t *testing.T) {
	c := New()
	c.cache = map[string]diskInventory{"gone": {}, "live": {}}

	c.Prune(map[string]struct{}{})
	if len(c.cache) != 2 {
		t.Fatalf("Prune(empty) dropped cached state")
	}

	c.Prune(map[string]struct{}{"live": {}})
	if _, ok := c.cache["gone"]; ok {
		t.Error("Prune kept a departed node")
	}
	if _, ok := c.cache["live"]; !ok {
		t.Error("Prune dropped a live node")
	}
}

// TestDegradedReportsStaleDiskInventory pins the shape that used to be
// invisible: the refresh fails, a cached list is served, Collect returns nil.
// The scrape must not fail and the cached disks must still be exported - but
// the failure has to be reported rather than dropped.
func TestDegradedReportsStaleDiskInventory(t *testing.T) {
	c := testCollector(fixtures(), mount("/dev/nvme0n1p4", 1<<30, 1<<29, "/var"))
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if d := c.Degraded("node-a"); len(d) != 0 {
		t.Fatalf("a clean collection reported %v", d)
	}

	// Force the inventory due, then make the list fail.
	c.mu.Lock()
	cached := c.cache["node-a"]
	cached.ts = cached.ts.Add(-2 * time.Hour)
	c.cache["node-a"] = cached
	c.mu.Unlock()
	c.state = func(*collector.NodeClient) state.CoreState {
		return fakeState{err: errors.New("cosi unavailable")}
	}

	reg = prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("a failed refresh with a cache must not fail the scrape: %v", err)
	}
	fams, _ := reg.Gather()
	found := false
	for _, f := range fams {
		if f.GetName() == "talos_block_disk_info" && len(f.GetMetric()) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("the cached disk list was not re-registered on a failed refresh")
	}
	d := c.Degraded("node-a")
	if len(d) != 1 || d[0].Reason != collector.ReasonInventory {
		t.Errorf("Degraded = %+v, want one %q degradation", d, collector.ReasonInventory)
	}

	// A successful refresh clears it: ts stayed at the last success, so the
	// next tick retries rather than waiting out another hour.
	c.state = func(*collector.NodeClient) state.CoreState { return fixtures() }
	reg = prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if d := c.Degraded("node-a"); len(d) != 0 {
		t.Errorf("after a successful refresh Degraded = %+v, want empty", d)
	}
}
