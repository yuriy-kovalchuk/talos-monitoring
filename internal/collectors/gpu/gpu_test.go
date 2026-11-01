package gpu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
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

// fakeAPI is an in-memory file API keyed by path; LS is reader-safe, so it ignores readErr as Talos does.
type fakeAPI struct {
	entries  []string          // names under /sys/class/drm
	links    map[string]string // /sys/class/drm name -> symlink target
	drivers  map[string]string // BDF -> bound driver ("" = listable, nothing bound); a missing BDF is unlistable
	files    map[string]string // path -> content
	fileErrs map[string]error  // path -> read failure (file exists but the read fails)
	lsErr    error
	readErr  error
}

func (a *fakeAPI) LS(_ context.Context, req *machine.ListRequest) (machine.MachineService_ListClient, error) {
	if bdf, ok := strings.CutPrefix(req.GetRoot(), pciDevicesRoot); ok {
		driver, ok := a.drivers[bdf]
		if !ok {
			return nil, status.Error(codes.NotFound, "no such file or directory")
		}
		items := []*machine.FileInfo{{Name: req.GetRoot() + "/vendor"}}
		if driver != "" {
			items = append(items, &machine.FileInfo{Name: req.GetRoot() + "/driver", Link: "../../../bus/pci/drivers/" + driver})
		}
		return &fakeStream{items: items}, nil
	}
	if a.lsErr != nil {
		return nil, a.lsErr
	}
	items := make([]*machine.FileInfo, 0, len(a.entries))
	for _, e := range a.entries {
		items = append(items, &machine.FileInfo{Name: drmRoot + "/" + e, Link: a.links[e]})
	}
	return &fakeStream{items: items}, nil
}

// drmLink is the symlink target sysfs gives a card on a PCI device.
func drmLink(bdf, name string) string {
	return "../../devices/pci0000:00/0000:00:01.1/" + bdf + "/drm/" + name
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

// fakeState serves a fixed PCI inventory.
type fakeState struct {
	state.CoreState
	items []resource.Resource
	err   error
}

// List answers only the hardware namespace, as COSI does: a list in the wrong namespace is silently empty.
func (f fakeState) List(_ context.Context, k resource.Kind, _ ...state.ListOption) (resource.List, error) {
	if f.err != nil {
		return resource.List{}, f.err
	}
	if k.Namespace() != hardware.NamespaceName || k.Type() != hardware.PCIDeviceType {
		return resource.List{}, nil
	}
	return resource.List{Items: f.items}, nil
}

// pciDev builds a COSI PCI device the way Talos formats it: 0x-prefixed lowercase IDs.
func pciDev(bdf, classID, vendorID, productID, driver string) resource.Resource {
	p := hardware.NewPCIDeviceInfo(bdf)
	spec := p.TypedSpec()
	spec.ClassID, spec.VendorID, spec.ProductID, spec.Driver = classID, vendorID, productID, driver
	return p
}

// amdPCI is the inventory behind amdCard: the GPU plus an NVMe drive that must not count.
func amdPCI() []resource.Resource {
	return []resource.Resource{
		pciDev("0000:03:00.0", "0x03", "0x1002", "0x7551", "amdgpu"),
		pciDev("0000:01:00.0", "0x01", "0x144d", "0xa808", "nvme"),
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func collectWith(t *testing.T, api *fakeAPI, pci ...resource.Resource) (map[string]*dto.MetricFamily, *snapshot.Node, error) {
	t.Helper()
	families, snap, _, err := collectOnce(t, New(discardLog()), api, fakeState{items: pci})
	return families, snap, err
}

func collectOnce(t *testing.T, c *Collector, api *fakeAPI, st fakeState) (map[string]*dto.MetricFamily, *snapshot.Node, *Collector, error) {
	t.Helper()
	snap := snapshot.New()
	c.WithSnapshot(snap)
	c.api = func(*collector.NodeClient) fileAPI { return api }
	c.state = func(*collector.NodeClient) state.CoreState { return st }
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
	return byName, snap.Node("node-1"), c, err
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

func amdCard() *fakeAPI {
	return &fakeAPI{
		entries: []string{".", "card0", "card0-DP-1", "renderD128", "version"},
		links: map[string]string{
			"card0":      drmLink("0000:03:00.0", "card0"),
			"card0-DP-1": drmLink("0000:03:00.0", "card0/card0-DP-1"),
			"renderD128": drmLink("0000:03:00.0", "renderD128"),
		},
		drivers: map[string]string{"0000:03:00.0": "amdgpu"},
		files: map[string]string{
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
	families, snap, err := collectWith(t, amdCard(), amdPCI()...)
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

// Utilisation is a ratio 0-1, not the percent sysfs reports; identity carries the BDF so it joins the PCI inventory.
func TestReadingsAndIdentity(t *testing.T) {
	families, snap, err := collectWith(t, amdCard(), amdPCI()...)
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
	families, snap, err := collectWith(t, api, amdPCI()...)
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

// An Intel iGPU publishes none of the amdgpu counters. Reporting
// 0% busy and 0 bytes of VRAM for it would be a lie: those series must be
// absent, and the Has* flags must say so.
func TestDriverWithoutCountersReportsAbsentNotZero(t *testing.T) {
	api := &fakeAPI{
		entries: []string{"card0"},
		links:   map[string]string{"card0": drmLink("0000:00:02.0", "card0")},
		drivers: map[string]string{"0000:00:02.0": "i915"},
		files: map[string]string{
			// power/runtime_status is generic PCI sysfs, so i915 reports it
			// even though it publishes none of the amdgpu counters.
			drmRoot + "/card0/device/power/runtime_status": "active\n",
		},
	}
	families, snap, err := collectWith(t, api, pciDev("0000:00:02.0", "0x03", "0x8086", "0x1912", ""))
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

// os:reader refuses every Read. Identity and count still come from COSI; the card and counters are unknown.
func TestReaderRoleStillReportsIdentityAndCount(t *testing.T) {
	api := amdCard()
	api.readErr = status.Error(codes.PermissionDenied, "denied")
	pci := append(amdPCI(), pciDev("0000:c7:00.0", "0x03", "0x1002", "0x1900", "amdgpu"))
	families, snap, c, err := collectOnce(t, New(discardLog()), api, fakeState{items: pci})
	if err != nil {
		t.Fatalf("permission error surfaced as a scrape failure: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpus", map[string]string{"node": "node-1"}); got != 2 {
		t.Errorf("gpu count = %v, want 2 display devices (the NVMe drive is not one)", got)
	}
	// LS link targets are reader-safe, so the card is known even though its counters are not.
	if got := gaugeFor(t, families, "talos_gpu_info", map[string]string{
		"node": "node-1", "card": "card0", "driver": "amdgpu", "pci_id": "1002:7551", "pci": "0000:03:00.0",
	}); got != 1 {
		t.Error("reader-tier identity missing")
	}
	for _, name := range []string{"talos_gpu_utilization_ratio", "talos_gpu_memory_total_bytes", "talos_gpu_runtime_suspended"} {
		if _, ok := families[name]; ok && len(families[name].GetMetric()) > 0 {
			t.Errorf("%s reported without file access", name)
		}
	}
	if len(snap.GPUs) != 2 || !snap.GPUs[0].CountersDenied || snap.GPUs[0].Card != "card0" {
		t.Errorf("snapshot GPUs = %+v, want card0 first with its counters denied", snap.GPUs)
	}
	if snap.GPUs[1].CountersDenied {
		t.Error("a device with no DRM card has no counters to deny")
	}
	if d := c.Degraded("node-1"); len(d) != 1 || d[0].Reason != collector.ReasonPermission || d[0].Stopped {
		t.Errorf("Degraded = %+v, want one non-stopped permission degradation: COSI still reached the node", d)
	}
}

// Granting os:admin later brings the counters back at the next probe, with no restart.
func TestCountersReturnAfterTheRoleIsGranted(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(discardLog())
	c.gate.WithClock(func() time.Time { return now })
	denied := amdCard()
	denied.readErr = status.Error(codes.PermissionDenied, "denied")
	if _, _, _, err := collectOnce(t, c, denied, fakeState{items: amdPCI()}); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Inside the retry window the refused read is not retried.
	denied.readErr = errors.New("must not be called")
	if _, _, _, err := collectOnce(t, c, denied, fakeState{items: amdPCI()}); err != nil {
		t.Fatalf("paused collector retried the refused read: %v", err)
	}

	now = now.Add(collector.PermissionRetry)
	families, snap, _, err := collectOnce(t, c, amdCard(), fakeState{items: amdPCI()})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpu_utilization_ratio", map[string]string{"card": "card0"}); got != 0.37 {
		t.Errorf("utilization = %v after the role was granted, want 0.37", got)
	}
	if snap.GPUs[0].CountersDenied {
		t.Error("snapshot still marks the counters denied")
	}
	if d := c.Degraded("node-1"); len(d) != 0 {
		t.Errorf("Degraded = %+v after recovery, want none", d)
	}
}

// With file access each card lands on its PCI row by BDF; a display device with no card keeps its own row.
func TestCardsJoinTheirPCIRowByBDF(t *testing.T) {
	pci := append(amdPCI(), pciDev("0000:00:02.0", "0x03", "0x8086", "0x1912", ""))
	families, snap, err := collectWith(t, amdCard(), pci...)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpus", map[string]string{"node": "node-1"}); got != 2 {
		t.Errorf("gpu count = %v, want 2", got)
	}
	if got := gaugeFor(t, families, "talos_gpu_info", map[string]string{"card": "card0", "pci": "0000:03:00.0"}); got != 1 {
		t.Error("card0 not joined to its BDF")
	}
	if got := gaugeFor(t, families, "talos_gpu_info", map[string]string{"card": "", "pci": "0000:00:02.0", "pci_id": "8086:1912"}); got != 1 {
		t.Error("driverless display device lost its row")
	}
	if len(snap.GPUs) != 2 || snap.GPUs[0].CountersDenied || snap.GPUs[1].CountersDenied {
		t.Errorf("snapshot GPUs = %+v, want two rows with readable counters", snap.GPUs)
	}
}

// A DRM card with no PCI device behind it is a boot framebuffer (simpledrm), not a GPU.
func TestSlotlessCardIsNotAGPU(t *testing.T) {
	api := &fakeAPI{
		entries: []string{"card0"},
		links:   map[string]string{"card0": "../../devices/pci0000:00/0000:00:02.0/simple-framebuffer.0/drm/card0"},
	}
	families, _, err := collectWith(t, api, pciDev("0000:00:02.0", "0x03", "0x8086", "0x1912", ""))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpus", map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("gpu count = %v, want 1: the framebuffer is not a second GPU", got)
	}
	if has(families, "talos_gpu_info", "card", "card0") {
		t.Error("simpledrm's card was reported as a GPU")
	}
}

// The driver symlink is the bound driver now; COSI's is boot-time and misses a driver loaded later.
func TestBoundDriverOverridesBootTimeInventory(t *testing.T) {
	api := &fakeAPI{
		entries: []string{"card1"},
		links:   map[string]string{"card1": drmLink("0000:01:00.0", "card1")},
		drivers: map[string]string{"0000:01:00.0": "nvidia", "0000:00:02.0": ""},
		readErr: status.Error(codes.PermissionDenied, "denied"),
	}
	families, _, err := collectWith(t, api,
		pciDev("0000:01:00.0", "0x03", "0x10de", "0x1b81", ""),
		pciDev("0000:00:02.0", "0x03", "0x8086", "0x1912", "i915"))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpu_info", map[string]string{"pci": "0000:01:00.0", "driver": "nvidia", "card": "card1"}); got != 1 {
		t.Error("the bound driver did not replace COSI's empty boot-time driver")
	}
	if got := gaugeFor(t, families, "talos_gpu_info", map[string]string{"pci": "0000:00:02.0", "driver": ""}); got != 1 {
		t.Error("a listable device with no driver symlink kept COSI's stale driver")
	}
}

func TestSlotFromLink(t *testing.T) {
	for link, want := range map[string]string{
		"../../devices/pci0000:00/0000:00:08.1/0000:c7:00.0/drm/card1":                      "0000:c7:00.0",
		"../../devices/pci0000:00/0000:00:01.0/0000:01:00.0/simple-framebuffer.0/drm/card0": "",
		"../../devices/platform/soc/fd4c0000.gpu/drm/card0":                                 "",
		"": "",
	} {
		if got := slotFromLink(link); got != want {
			t.Errorf("slotFromLink(%q) = %q, want %q", link, got, want)
		}
	}
}

// A non-display PCI device a DRM driver binds to (an accelerator) still counts once its card is readable.
func TestDRMCardOnNonDisplayDeviceGetsARow(t *testing.T) {
	families, _, err := collectWith(t, amdCard(), pciDev("0000:03:00.0", "0x12", "0x1002", "0x7551", "amdgpu"))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := gaugeFor(t, families, "talos_gpus", map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("gpu count = %v, want 1", got)
	}
	if !has(families, "talos_gpu_info", "card", "card0") {
		t.Error("card on a processing-accelerator device was dropped")
	}
}

func TestPCIListErrorFailsTheCollection(t *testing.T) {
	wantErr := errors.New("cosi down")
	_, _, _, err := collectOnce(t, New(discardLog()), amdCard(), fakeState{err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
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
	denied := amdCard()
	denied.readErr = status.Error(codes.PermissionDenied, "denied")
	for name, api := range map[string]*fakeAPI{"admin": amdCard(), "reader": denied} {
		t.Run(name, func(t *testing.T) {
			families, snap, err := collectWith(t, api, amdPCI()...)
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
		})
	}
}
