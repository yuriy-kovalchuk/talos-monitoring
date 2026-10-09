package gpu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// fakeStream satisfies machine.MachineService_ListClient (the embedded nil
// grpc.ClientStream covers the unused methods).
type fakeStream struct {
	grpc.ClientStream
	items []*machine.FileInfo
	i     int
}

func (s *fakeStream) Recv() (*machine.FileInfo, error) {
	if s.i >= len(s.items) {
		return nil, io.EOF
	}
	fi := s.items[s.i]
	s.i++
	return fi, nil
}

// fakeAPI is an in-memory file API keyed by path.
type fakeAPI struct {
	entries  []string          // names under /sys/class/drm
	files    map[string]string // path -> content
	fileErrs map[string]error  // path -> read failure (file exists but the read fails)
	lsErr    error
	readErr  error
}

func (a *fakeAPI) LS(_ context.Context, _ *machine.ListRequest) (machine.MachineService_ListClient, error) {
	if a.lsErr != nil {
		return nil, a.lsErr
	}
	items := make([]*machine.FileInfo, 0, len(a.entries))
	for _, e := range a.entries {
		items = append(items, &machine.FileInfo{Name: drmRoot + "/" + e})
	}
	return &fakeStream{items: items}, nil
}

func (a *fakeAPI) Read(_ context.Context, path string) (io.ReadCloser, error) {
	if a.readErr != nil {
		return nil, a.readErr
	}
	if err := a.fileErrs[path]; err != nil {
		return nil, err
	}
	content, ok := a.files[path]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such file or directory")
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func collectWith(t *testing.T, api *fakeAPI) (map[string]*dto.MetricFamily, *snapshot.Node, error) {
	t.Helper()
	snap := snapshot.New()
	c := New(discardLog()).WithSnapshot(snap)
	c.api = func(*collector.NodeClient) fileAPI { return api }
	reg := prometheus.NewRegistry()
	err := c.Collect(context.Background(),
		&collector.NodeClient{Node: nodes.Node{Name: "node-1", IP: "10.0.0.1"}}, reg)
	families, gErr := reg.Gather()
	if gErr != nil {
		t.Fatalf("Gather: %v", gErr)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}
	return byName, snap.Node("node-1"), err
}

func gaugeFor(t *testing.T, families map[string]*dto.MetricFamily, name string, want map[string]string) float64 {
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
		return m.GetGauge().GetValue()
	}
	t.Fatalf("family %q has no sample matching %v", name, want)
	return 0
}

func has(families map[string]*dto.MetricFamily, name, label, value string) bool {
	f, ok := families[name]
	if !ok {
		return false
	}
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == label && l.GetValue() == value {
				return true
			}
		}
	}
	return false
}

const amdUevent = "DRIVER=amdgpu\nPCI_CLASS=30000\nPCI_ID=1002:7551\nPCI_SLOT_NAME=0000:03:00.0\n"

func amdCard() *fakeAPI {
	return &fakeAPI{
		entries: []string{".", "card0", "card0-DP-1", "renderD128", "version"},
		files: map[string]string{
			drmRoot + "/card0/device/uevent":               amdUevent,
			drmRoot + "/card0/device/gpu_busy_percent":     "37\n",
			drmRoot + "/card0/device/power/runtime_status": "active\n",
			drmRoot + "/card0/device/mem_info_vram_total":  "34208743424\n",
			drmRoot + "/card0/device/mem_info_vram_used":   "32394584064\n",
			drmRoot + "/card0/device/mem_info_gtt_total":   "32529408\n",
			drmRoot + "/card0/device/mem_info_gtt_used":    "461893632\n",
		},
	}
}

// Only cardN directories are GPUs. Connectors (card0-DP-1) and render nodes
// (renderD128) live in the same directory and are not cards.
func TestOnlyCardDirectoriesAreCollected(t *testing.T) {
	families, snap, err := collectWith(t, amdCard())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpus", map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("gpu count = %v, want 1", got)
	}
	if len(snap.GPUs) != 1 || snap.GPUs[0].Card != "card0" {
		t.Fatalf("snapshot GPUs = %+v, want one card0", snap.GPUs)
	}
	if has(families, "talos_gpu_info", "card", "card0-DP-1") {
		t.Error("a DRM connector was collected as a GPU")
	}
	if has(families, "talos_gpu_info", "card", "renderD128") {
		t.Error("a render node was collected as a GPU")
	}
}

// Utilisation is a ratio 0-1, not the percent sysfs reports; identity comes
// from uevent and carries the PCI BDF so it joins the PCI inventory.
func TestReadingsAndIdentity(t *testing.T) {
	families, snap, err := collectWith(t, amdCard())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	card := map[string]string{"node": "node-1", "card": "card0"}

	if got := gaugeFor(t, families, "talos_gpu_utilization_ratio", card); got != 0.37 {
		t.Errorf("utilization = %v, want 0.37 (37%% as a ratio)", got)
	}
	if got := gaugeFor(t, families, "talos_gpu_memory_total_bytes", card); got != 34208743424 {
		t.Errorf("vram total = %v", got)
	}
	if got := gaugeFor(t, families, "talos_gpu_memory_used_bytes", card); got != 32394584064 {
		t.Errorf("vram used = %v", got)
	}
	if got := gaugeFor(t, families, "talos_gpu_info", map[string]string{
		"node": "node-1", "card": "card0", "driver": "amdgpu",
		"pci_id": "1002:7551", "pci": "0000:03:00.0",
	}); got != 1 {
		t.Errorf("gpu_info identity labels missing")
	}
	if snap.GPUs[0].BusyPercent != 37 || snap.GPUs[0].Slot != "0000:03:00.0" {
		t.Errorf("snapshot GPU = %+v", snap.GPUs[0])
	}
	if got := gaugeFor(t, families, "talos_gpu_runtime_suspended", card); got != 0 {
		t.Errorf("runtime_suspended = %v, want 0 (device awake)", got)
	}
}

// A runtime-suspended card cannot answer the busy read. The collector reports
// the derived 0 (an asleep GPU does no work) and the state, instead of
// dropping the series or blaming the driver.
func TestSuspendedCardReportsDerivedZero(t *testing.T) {
	api := amdCard()
	api.files[drmRoot+"/card0/device/power/runtime_status"] = "suspended\n"
	api.fileErrs = map[string]error{
		drmRoot + "/card0/device/gpu_busy_percent": status.Error(codes.Unknown, "read failed: device is suspended"),
	}
	families, snap, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	card := map[string]string{"node": "node-1", "card": "card0"}
	if got := gaugeFor(t, families, "talos_gpu_utilization_ratio", card); got != 0 {
		t.Errorf("utilization = %v, want the derived 0 for a suspended card", got)
	}
	if got := gaugeFor(t, families, "talos_gpu_runtime_suspended", card); got != 1 {
		t.Errorf("runtime_suspended = %v, want 1", got)
	}
	// Files that do not touch the hardware keep reporting.
	if got := gaugeFor(t, families, "talos_gpu_memory_total_bytes", card); got != 34208743424 {
		t.Errorf("vram total = %v", got)
	}
	if !snap.GPUs[0].HasBusy || snap.GPUs[0].BusyPercent != 0 || !snap.GPUs[0].Suspended {
		t.Errorf("snapshot GPU = %+v, want a suspended card with derived-busy 0", snap.GPUs[0])
	}
}

// A failed busy read with unknown power state is not a sleep: the series
// stays absent and the state stays unreported. The value is not guessed.
func TestUnexplainedBusyReadFailureStaysAbsent(t *testing.T) {
	api := amdCard()
	delete(api.files, drmRoot+"/card0/device/power/runtime_status") // state unknown
	api.fileErrs = map[string]error{
		drmRoot + "/card0/device/gpu_busy_percent": status.Error(codes.Unknown, "transient failure"),
	}
	families, snap, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if has(families, "talos_gpu_utilization_ratio", "card", "card0") {
		t.Error("utilization reported for a card whose busy read failed with unknown power state")
	}
	if has(families, "talos_gpu_runtime_suspended", "card", "card0") {
		t.Error("power state reported for a card whose runtime_status read failed")
	}
	if snap.GPUs[0].HasBusy || snap.GPUs[0].Suspended {
		t.Errorf("snapshot GPU = %+v, want neither busy nor suspended", snap.GPUs[0])
	}
}

// An Intel iGPU publishes uevent but none of the amdgpu counters. Reporting
// 0% busy and 0 bytes of VRAM for it would be a lie: those series must be
// absent, and the Has* flags must say so.
func TestDriverWithoutCountersReportsAbsentNotZero(t *testing.T) {
	api := &fakeAPI{
		entries: []string{"card0"},
		files: map[string]string{
			drmRoot + "/card0/device/uevent": "DRIVER=i915\nPCI_ID=8086:1912\nPCI_SLOT_NAME=0000:00:02.0\n",
			// power/runtime_status is generic PCI sysfs, so i915 reports it
			// even though it publishes none of the amdgpu counters.
			drmRoot + "/card0/device/power/runtime_status": "active\n",
		},
	}
	families, snap, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpu_info",
		map[string]string{"node": "node-1", "card": "card0", "driver": "i915"}); got != 1 {
		t.Errorf("i915 identity missing")
	}
	for _, name := range []string{
		"talos_gpu_utilization_ratio", "talos_gpu_memory_total_bytes",
		"talos_gpu_memory_used_bytes", "talos_gpu_gtt_total_bytes",
	} {
		if has(families, name, "card", "card0") {
			t.Errorf("%s reported a value for a driver that does not publish it", name)
		}
	}
	if snap.GPUs[0].HasBusy || snap.GPUs[0].HasVRAM || snap.GPUs[0].HasGTT {
		t.Errorf("snapshot claims readings it does not have: %+v", snap.GPUs[0])
	}
	if got := gaugeFor(t, families, "talos_gpu_runtime_suspended",
		map[string]string{"node": "node-1", "card": "card0"}); got != 0 {
		t.Errorf("runtime_suspended = %v, want 0 (i915 publishes the state file)", got)
	}
}

// A node with no GPU driver has no /sys/class/drm tree at all. That is absent
// hardware, not a scrape failure.
func TestNoDRMTreeIsNotAnError(t *testing.T) {
	api := &fakeAPI{lsErr: status.Error(codes.NotFound, "no such file or directory")}
	families, snap, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect returned an error for a GPU-less node: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpus", map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("gpu count = %v, want 0", got)
	}
	if len(snap.GPUs) != 0 {
		t.Errorf("snapshot GPUs = %+v, want none", snap.GPUs)
	}
}

// File reads need os:admin. A permission error must self-disable the collector
// rather than fail every scrape forever.
func TestPermissionErrorSelfDisables(t *testing.T) {
	snap := snapshot.New()
	c := New(discardLog()).WithSnapshot(snap)
	api := &fakeAPI{lsErr: status.Error(codes.PermissionDenied, "denied")}
	c.api = func(*collector.NodeClient) fileAPI { return api }
	node := &collector.NodeClient{Node: nodes.Node{Name: "node-1"}}

	if err := c.Collect(context.Background(), node, prometheus.NewRegistry()); err != nil {
		t.Fatalf("permission error surfaced as a scrape failure: %v", err)
	}
	if !c.disabled.Load() {
		t.Fatal("collector did not self-disable after a permission error")
	}
	// Second round is a no-op: it must not retry the same failing call.
	api.lsErr = errors.New("must not be called")
	if err := c.Collect(context.Background(), node, prometheus.NewRegistry()); err != nil {
		t.Fatalf("disabled collector returned an error: %v", err)
	}
}

func TestListErrorFailsTheCollection(t *testing.T) {
	wantErr := errors.New("boom")
	_, _, err := collectWith(t, &fakeAPI{lsErr: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestCollectorIdentity(t *testing.T) {
	c := New(discardLog())
	if c.Name() != "gpu" {
		t.Errorf("Name = %q, want gpu", c.Name())
	}
	if c.Class() != collector.Live {
		t.Errorf("Class = %v, want Live", c.Class())
	}
}

// The structural guard: every string this collector writes to the snapshot
// must be reachable from /metrics, and its families must obey the contract.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	families, snap, err := collectWith(t, amdCard())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams := make([]*dto.MetricFamily, 0, len(families))
	for _, f := range families {
		fams = append(fams, f)
	}
	opts := metricguard.Options{}
	for _, v := range metricguard.Validate(fams, opts) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(snap.GPUs, fams, opts) {
		t.Error(v)
	}
}

// TestDegradedReflectsSelfDisable: like sensors, gpu returns nil before making
// a single RPC once self-disabled, which is not evidence of reachability.
func TestDegradedReflectsSelfDisable(t *testing.T) {
	c := New(discardLog())
	if d := c.Degraded("n1"); len(d) != 0 {
		t.Fatalf("a live collector reported %v", d)
	}
	if err := c.disable("n1"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	d := c.Degraded("n1")
	if len(d) != 1 || d[0].Reason != collector.ReasonPermission || !d[0].Stopped {
		t.Errorf("Degraded = %+v, want one stopped %q degradation", d, collector.ReasonPermission)
	}
}
