package hwinfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

// fakeState implements only List of state.CoreState; the rest panic if used.
// byType is keyed by resource type; a missing key yields an empty list.
type fakeState struct {
	state.CoreState
	byType map[resource.Type]resource.List
	err    error
}

func (f fakeState) List(_ context.Context, k resource.Kind, _ ...state.ListOption) (resource.List, error) {
	if f.err != nil {
		return resource.List{}, f.err
	}
	return f.byType[k.Type()], nil
}

func fixtureSystemInfo() *hardware.SystemInformation {
	si := hardware.NewSystemInformation("systeminformation")
	spec := si.TypedSpec()
	spec.Manufacturer = "Acme Corp"
	spec.ProductName = "AcmeServer X1"
	spec.Version = "00000001"
	spec.SerialNumber = "SN-12345"
	spec.UUID = "12345678-1234-1234-1234-123456789abc"
	spec.SKUNumber = "SKU-777"
	spec.WakeUpType = "High Green Wake"
	return si
}

func fixtureProcessors() []resource.Resource {
	out := make([]resource.Resource, 0, 2)
	for i, id := range []string{"CPU-0", "CPU-1"} {
		p := hardware.NewProcessorInfo(id)
		spec := p.TypedSpec()
		spec.Socket = id
		spec.ProductName = fmt.Sprintf("Intel Core Processor %d", i)
		spec.CoreCount = 8
		spec.CoreEnabled = 8
		spec.ThreadCount = 16
		spec.MaxSpeed = 3500
		spec.BootSpeed = 800
		spec.PartNumber = "PART-1"
		spec.SerialNumber = fmt.Sprintf("CPU-SN-%d", i)
		spec.AssetTag = "AT-1"
		out = append(out, p)
	}
	return out
}

func fixtureMemoryModules() []resource.Resource {
	out := make([]resource.Resource, 0, 2)
	for i, id := range []string{"A1", "A2"} {
		m := hardware.NewMemoryModuleInfo(id)
		spec := m.TypedSpec()
		spec.Size = 16384
		spec.Speed = 4800
		spec.Manufacturer = "Samsung"
		spec.ProductName = fmt.Sprintf("M321R2GA0PB0-CP %d", i)
		spec.DeviceLocator = fmt.Sprintf("DIMM_A%d", i+1)
		spec.BankLocator = fmt.Sprintf("a%d", i+1)
		spec.SerialNumber = fmt.Sprintf("MEM-SN-%d", i)
		spec.AssetTag = "MAT-1"
		out = append(out, m)
	}
	return out
}

func fixturePCIDevices() []resource.Resource {
	defs := []struct {
		id      string
		class   string
		subcls  string
		vendor  string
		product string
		classID string
		subID   string
		venID   string
		prodID  string
		driver  string
	}{
		{"0000:00:02.0", "Display controller", "VGA compatible controller", "Advanced Micro Devices, Inc. [AMD/ATI]", "Radeon Graphics",
			"0x03", "0x00", "0x1002", "0x1681", "amdgpu"},
		{"0000:01:00.0", "Non-Volatile memory controller", "", "Samsung Electronics Co Ltd", "NVMe SSD Controller PM9A1",
			"0x01", "0x08", "0x144d", "0xa808", "nvme"},
		{"0000:02:00.0", "Network controller", "Ethernet controller", "Intel Corporation", "Ethernet I210",
			"0x02", "0x00", "0x8086", "0x1533", "igb"},
	}
	out := make([]resource.Resource, 0, len(defs))
	for _, d := range defs {
		p := hardware.NewPCIDeviceInfo(d.id)
		spec := p.TypedSpec()
		spec.Class = d.class
		spec.Subclass = d.subcls
		spec.Vendor = d.vendor
		spec.Product = d.product
		spec.ClassID = d.classID
		spec.SubclassID = d.subID
		spec.VendorID = d.venID
		spec.ProductID = d.prodID
		spec.Driver = d.driver
		out = append(out, p)
	}
	return out
}

// fixtures is a full fake state with one SystemInformation, two sockets,
// two DIMMs and three PCI devices.
func fixtures() fakeState {
	return fakeState{byType: map[resource.Type]resource.List{
		hardware.SystemInformationType: {Items: []resource.Resource{fixtureSystemInfo()}},
		hardware.ProcessorType:         {Items: fixtureProcessors()},
		hardware.MemoryModuleType:      {Items: fixtureMemoryModules()},
		hardware.PCIDeviceType:         {Items: fixturePCIDevices()},
	}}
}

func testCollector(c *Collector, st state.CoreState) *Collector {
	c.state = func(*collector.NodeClient) state.CoreState { return st }
	return c
}

// stubMemAPI is a canned machine-API Memory response.
type stubMemAPI struct {
	mi  *machinepb.MemInfo
	err error
}

func (s stubMemAPI) Memory(context.Context) (*machinepb.MemoryResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &machinepb.MemoryResponse{Messages: []*machinepb.Memory{{Meminfo: s.mi}}}, nil
}

// fixtureMemInfo is a plausible /proc/meminfo, in kibibytes as the node reports.
func fixtureMemInfo() *machinepb.MemInfo {
	return &machinepb.MemInfo{
		Memtotal: 32099664, Memfree: 1271228, Memavailable: 8795764,
		Buffers: 110812, Cached: 7628988, Shmem: 288608,
		Anonpages: 21080120, Mapped: 2803960,
		Slab: 1035072, Sreclaimable: 537860, Pagetables: 111252, Kernelstack: 51616,
		Dirty: 2220, Writeback: 0,
		Committedas: 47441732, Commitlimit: 16049832,
		Swaptotal: 0, Swapfree: 0, Swapcached: 0,
		Hugepagestotal: 0, Hugepagesfree: 0, Hugepagesize: 2048,
	}
}

// stubPCIFiles serves canned PCI sysfs files; a missing path is NotFound, which
// is normal for bridges and root ports.
type stubPCIFiles struct {
	files map[string]string
	err   error
}

func (s stubPCIFiles) Read(_ context.Context, path string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	if v, ok := s.files[path]; ok {
		return io.NopCloser(strings.NewReader(v)), nil
	}
	return nil, os.ErrNotExist
}

// testPCICollector wires the COSI state and the sysfs reader.
func testPCICollector(st state.CoreState, files stubPCIFiles) *PCICollector {
	c := NewPCI(discardLog())
	c.state = func(*collector.NodeClient) state.CoreState { return st }
	c.newAPI = func(*collector.NodeClient) pciFileAPI { return files }
	return c
}

// testMemoryCollector wires both of the memory collector's sources.
func testMemoryCollector(st state.CoreState, api memAPI) *MemoryCollector {
	c := NewMemory()
	c.state = func(*collector.NodeClient) state.CoreState { return st }
	c.newAPI = func(*collector.NodeClient) memAPI { return api }
	return c
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testNode() *collector.NodeClient {
	return &collector.NodeClient{Node: nodes.Node{Name: "node-a", IP: "10.0.0.1"}}
}

func gatherSystemInfo(t *testing.T, reg *prometheus.Registry) []*dto.Metric {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() == "talos_hw_system_info" {
			return f.GetMetric()
		}
	}
	t.Fatal("talos_hw_system_info metric family not found")
	return nil
}

func labelMap(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		out[lp.GetName()] = lp.GetValue()
	}
	return out
}

func TestCollectSystemInformation(t *testing.T) {
	c := testCollector(NewSystem(), fixtures())
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}

	metrics := gatherSystemInfo(t, reg)
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}
	if metrics[0].GetGauge().GetValue() != 1 {
		t.Errorf("value = %v, want 1", metrics[0].GetGauge().GetValue())
	}
	got := labelMap(metrics[0])
	want := map[string]string{
		"node":          "node-a",
		"manufacturer":  "Acme Corp",
		"product":       "AcmeServer X1",
		"version":       "00000001",
		"serial_number": "SN-12345",
		"uuid":          "12345678-1234-1234-1234-123456789abc",
		"sku_number":    "SKU-777",
		"wake_up_type":  "High Green Wake",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestCollectProcessors(t *testing.T) {
	c := testCollector(NewProcessor(), fixtures())
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
		if f.GetName() == "talos_hw_processor_info" {
			metrics = f.GetMetric()
		}
	}
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics (one per socket), got %d", len(metrics))
	}
	got := make(map[string]map[string]string, len(metrics))
	for _, m := range metrics {
		l := labelMap(m)
		got[l["socket"]] = l
	}
	for i, socket := range []string{"CPU-0", "CPU-1"} {
		l, ok := got[socket]
		if !ok {
			t.Fatalf("socket %s not in metrics: %v", socket, got)
		}
		want := map[string]string{
			"node":          "node-a",
			"product":       fmt.Sprintf("Intel Core Processor %d", i),
			"part_number":   "PART-1",
			"serial_number": fmt.Sprintf("CPU-SN-%d", i),
			"asset_tag":     "AT-1",
		}
		for k, v := range want {
			if l[k] != v {
				t.Errorf("%s label %s = %q, want %q", socket, k, l[k], v)
			}
		}
		// Counts and speeds are gauges keyed by (node, socket). SMBIOS reports
		// MHz; the metric is hertz per the base-unit convention.
		for metric, wantV := range map[string]float64{
			"talos_hw_processor_cores":         8,
			"talos_hw_processor_cores_enabled": 8,
			"talos_hw_processor_threads":       16,
			"talos_hw_processor_max_hertz":     3500e6,
			"talos_hw_processor_boot_hertz":    800e6,
			"talos_hw_processor_status":        0,
		} {
			if v := gaugeFor(t, fams, metric, "socket", socket); v != wantV {
				t.Errorf("%s{socket=%q} = %v, want %v", metric, socket, v, wantV)
			}
		}
		for _, gone := range []string{"cores", "cores_enabled", "threads", "max_mhz", "boot_mhz", "status"} {
			if _, ok := l[gone]; ok {
				t.Errorf("label %q is still on talos_hw_processor_info", gone)
			}
		}
	}
}

func TestCollectMemoryModules(t *testing.T) {
	c := testMemoryCollector(fixtures(), stubMemAPI{mi: fixtureMemInfo()})
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
		if f.GetName() == "talos_hw_memory_module_info" {
			metrics = f.GetMetric()
		}
	}
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics (one per DIMM), got %d", len(metrics))
	}
	got := make(map[string]map[string]string, len(metrics))
	for _, m := range metrics {
		l := labelMap(m)
		got[l["slot"]] = l
	}
	for i, slot := range []string{"A1", "A2"} {
		l, ok := got[slot]
		if !ok {
			t.Fatalf("slot %s not in metrics: %v", slot, got)
		}
		want := map[string]string{
			"node":           "node-a",
			"manufacturer":   "Samsung",
			"product":        fmt.Sprintf("M321R2GA0PB0-CP %d", i),
			"device_locator": fmt.Sprintf("DIMM_A%d", i+1),
			"bank_locator":   fmt.Sprintf("a%d", i+1),
			"serial_number":  fmt.Sprintf("MEM-SN-%d", i),
			"asset_tag":      "MAT-1",
		}
		for k, v := range want {
			if l[k] != v {
				t.Errorf("%s label %s = %q, want %q", slot, k, l[k], v)
			}
		}
		// 16384 MiB as bytes, and DDR5-4800 as transfers per second — not
		// hertz: 4800 MT/s runs on a 2400 MHz clock, so "_hertz" would be
		// wrong by a factor of two.
		if v := gaugeFor(t, fams, "talos_hw_memory_module_size_bytes", "slot", slot); v != 16384*1024*1024 {
			t.Errorf("size_bytes{slot=%q} = %v, want %v", slot, v, 16384*1024*1024)
		}
		if v := gaugeFor(t, fams, "talos_hw_memory_module_speed_transfers_per_second", "slot", slot); v != 4800e6 {
			t.Errorf("speed{slot=%q} = %v, want %v", slot, v, 4800e6)
		}
		for _, gone := range []string{"size_mib", "speed_mhz"} {
			if _, ok := l[gone]; ok {
				t.Errorf("label %q is still on talos_hw_memory_module_info", gone)
			}
		}
	}
}

func TestCollectPCIDevices(t *testing.T) {
	c := testPCICollector(fixtures(), stubPCIFiles{})
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
		if f.GetName() == "talos_hw_pcidevice_info" {
			metrics = f.GetMetric()
		}
	}
	if len(metrics) != 3 {
		t.Fatalf("expected 3 metrics (one per BDF), got %d", len(metrics))
	}
	got := make(map[string]map[string]string, len(metrics))
	for _, m := range metrics {
		l := labelMap(m)
		got[l["bdf"]] = l
	}
	l, ok := got["0000:00:02.0"]
	if !ok {
		t.Fatalf("BDF 0000:00:02.0 not in metrics: %v", got)
	}
	want := map[string]string{
		"node":        "node-a",
		"class":       "Display controller",
		"subclass":    "VGA compatible controller",
		"vendor":      "Advanced Micro Devices, Inc. [AMD/ATI]",
		"product":     "Radeon Graphics",
		"class_id":    "0x03",
		"subclass_id": "0x00",
		"vendor_id":   "0x1002",
		"product_id":  "0x1681",
		"driver":      "amdgpu",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	if got["0000:01:00.0"]["driver"] != "nvme" {
		t.Errorf("nvme driver = %q, want nvme", got["0000:01:00.0"]["driver"])
	}
}

func TestCollectMissingPCIDevices(t *testing.T) {
	c := testPCICollector(fakeState{byType: map[resource.Type]resource.List{
		hardware.SystemInformationType: {Items: []resource.Resource{fixtureSystemInfo()}},
		hardware.ProcessorType:         {Items: fixtureProcessors()},
		hardware.MemoryModuleType:      {Items: fixtureMemoryModules()},
	}}, stubPCIFiles{})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("a node reporting no PCI devices is not a failure, got %v", err)
	}
	assertNoFamily(t, reg, "talos_hw_pcidevice_info")
}

func TestCollectMissingMemoryModules(t *testing.T) {
	c := testMemoryCollector(fakeState{byType: map[resource.Type]resource.List{
		hardware.SystemInformationType: {Items: []resource.Resource{fixtureSystemInfo()}},
		hardware.ProcessorType:         {Items: fixtureProcessors()},
	}}, stubMemAPI{mi: fixtureMemInfo()})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("a VM with no DIMM inventory is not a failure, got %v", err)
	}
	assertNoFamily(t, reg, "talos_hw_memory_module_info")
	// /proc/meminfo usage is independent of the DIMM inventory and must
	// still be reported.
	assertHasFamily(t, reg, "talos_node_memory_total_bytes")
}

func TestCollectMissingProcessors(t *testing.T) {
	c := testCollector(NewProcessor(), fakeState{byType: map[resource.Type]resource.List{
		hardware.SystemInformationType: {Items: []resource.Resource{fixtureSystemInfo()}},
	}})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("a node reporting no processors is not a failure, got %v", err)
	}
	assertNoFamily(t, reg, "talos_hw_processor_info")
}

func TestCollectListError(t *testing.T) {
	c := testCollector(NewSystem(), fakeState{err: errors.New("boom")})
	reg := prometheus.NewRegistry()
	err := c.Collect(context.Background(), testNode(), reg)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want wrapped 'boom'", err)
	}
}

// TestCollectEmptyList pins finding 1.5: a VM with no DMI tables reports no
// SystemInformation. The RPC succeeded, so that is absent hardware, not a
// scrape failure — it used to pull talos_node_up to 0 for a healthy machine.
func TestCollectEmptyList(t *testing.T) {
	c := testCollector(NewSystem(), fakeState{byType: map[resource.Type]resource.List{}})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("absent hardware should not be an error, got %v", err)
	}
	assertNoFamily(t, reg, "talos_hw_system_info")
}

// assertNoFamily fails when the registry exposes the named metric family.
func assertNoFamily(t *testing.T, reg *prometheus.Registry, name string) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			t.Errorf("expected no %s when the resource is absent", name)
		}
	}
}

// assertHasFamily fails when the registry does not expose the named family.
func assertHasFamily(t *testing.T, reg *prometheus.Registry, name string) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return
		}
	}
	t.Errorf("expected %s to still be reported", name)
}

func TestMemoryCollectorExportsMemInfo(t *testing.T) {
	c := testMemoryCollector(fixtures(), stubMemAPI{mi: fixtureMemInfo()})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	get := func(name string) (float64, bool) {
		for _, f := range fams {
			if f.GetName() != name {
				continue
			}
			for _, m := range f.GetMetric() {
				return m.GetGauge().GetValue(), true
			}
		}
		return 0, false
	}
	// meminfo reports kibibytes; the export is in bytes.
	for _, tc := range []struct {
		metric string
		want   float64
	}{
		{"talos_node_memory_total_bytes", 32099664 * 1024},
		{"talos_node_memory_available_bytes", 8795764 * 1024},
		{"talos_node_memory_cached_bytes", 7628988 * 1024},
		{"talos_node_memory_committed_bytes", 47441732 * 1024},
		{"talos_node_swap_total_bytes", 0},
		{"talos_node_memory_hugepage_size_bytes", 2048 * 1024},
	} {
		got, ok := get(tc.metric)
		if !ok {
			t.Errorf("%s not exported", tc.metric)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %v, want %v", tc.metric, got, tc.want)
		}
	}
	// The DIMM inventory is still registered alongside the usage gauges.
	if _, ok := get("talos_hw_memory_module_info"); !ok {
		t.Error("DIMM inventory missing: the memory collector must report both")
	}
}

func TestMemoryCollectorServesCachedInventory(t *testing.T) {
	// The COSI list is throttled to memInventoryTTL, but the DIMM metric must
	// still be registered on every tick — the scraper hands out a fresh
	// registry each time, so a skipped list must not mean a missing metric.
	st := &countingState{CoreState: fixtures()}
	c := testMemoryCollector(st, stubMemAPI{mi: fixtureMemInfo()})
	for i := 0; i < 3; i++ {
		reg := prometheus.NewRegistry()
		if err := c.Collect(context.Background(), testNode(), reg); err != nil {
			t.Fatalf("collect %d: %v", i, err)
		}
		fams, _ := reg.Gather()
		found := false
		for _, f := range fams {
			if f.GetName() == "talos_hw_memory_module_info" {
				found = true
			}
		}
		if !found {
			t.Fatalf("tick %d: DIMM inventory missing from a cached tick", i)
		}
	}
	if st.lists != 1 {
		t.Errorf("COSI listed %d times across 3 ticks, want 1 (inventory is throttled)", st.lists)
	}
}

// countingState counts List calls so the inventory throttle can be asserted.
// The embedded interface supplies everything else CoreState requires.
type countingState struct {
	state.CoreState
	lists int
}

func (c *countingState) List(ctx context.Context, md resource.Kind, opts ...state.ListOption) (resource.List, error) {
	c.lists++
	return c.CoreState.List(ctx, md, opts...)
}

func TestPCISysfsEnrichment(t *testing.T) {
	base := "/sys/bus/pci/devices/0000:00:02.0/"
	files := stubPCIFiles{files: map[string]string{
		// A device negotiating a slower link than it supports: the whole point
		// of reading these files.
		base + "current_link_speed": "2.5 GT/s PCIe",
		base + "current_link_width": "4",
		base + "max_link_speed":     "16.0 GT/s PCIe",
		base + "max_link_width":     "16",
		base + "numa_node":          "-1",
		base + "irq":                "36",
		base + "power_state":        "D0",
		base + "enable":             "1",
		base + "revision":           "0xc3",
		base + "subsystem_vendor":   "0x1002",
		base + "subsystem_device":   "0x1681",
		// AER files are "Name Count" lines; only the total is exported.
		base + "aer_dev_correctable": "RxErr 3\nBadTLP 0\nBadDLLP 1\nRollover 0",
		base + "aer_dev_fatal":       "Undefined 0\nDLP 0",
		base + "aer_dev_nonfatal":    "Undefined 0\nDLP 2",
	}}
	c := testPCICollector(fixtures(), files)
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	val := func(name string, want map[string]string) (float64, bool) {
		for _, f := range fams {
			if f.GetName() != name {
				continue
			}
		metric:
			for _, m := range f.GetMetric() {
				got := map[string]string{}
				for _, l := range m.GetLabel() {
					got[l.GetName()] = l.GetValue()
				}
				for k, v := range want {
					if got[k] != v {
						continue metric
					}
				}
				if m.GetGauge() != nil {
					return m.GetGauge().GetValue(), true
				}
				return m.GetCounter().GetValue(), true
			}
		}
		return 0, false
	}

	bdf := map[string]string{"bdf": "0000:00:02.0"}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		// Negotiated state and device maximum are separate families, in
		// transfers per second rather than GT/s.
		{"talos_hw_pcidevice_link_speed_transfers_per_second", bdf, 2.5e9},
		{"talos_hw_pcidevice_link_max_speed_transfers_per_second", bdf, 16e9},
		{"talos_hw_pcidevice_link_width_lanes", bdf, 4},
		{"talos_hw_pcidevice_link_max_width_lanes", bdf, 16},
		// IRQ is a label on an info metric: it is an identifier, so grouping
		// by it must be possible and arithmetic on it must not be.
		{"talos_hw_pcidevice_irq_info", map[string]string{"bdf": "0000:00:02.0", "irq": "36"}, 1},
		{"talos_hw_pcidevice_enabled", bdf, 1},
		// AER totals are summed over causes: 3+0+1+0 = 4.
		{"talos_hw_pcidevice_aer_errors_total", map[string]string{"bdf": "0000:00:02.0", "severity": "correctable"}, 4},
		{"talos_hw_pcidevice_aer_errors_total", map[string]string{"bdf": "0000:00:02.0", "severity": "fatal"}, 0},
		{"talos_hw_pcidevice_aer_errors_total", map[string]string{"bdf": "0000:00:02.0", "severity": "nonfatal"}, 2},
		{"talos_hw_pcidevice_power_state_info", map[string]string{"bdf": "0000:00:02.0", "state": "D0"}, 1},
	} {
		got, ok := val(tc.name, tc.labels)
		if !ok {
			t.Errorf("%s%v not exported", tc.name, tc.labels)
			continue
		}
		if got != tc.want {
			t.Errorf("%s%v = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}

	// numa_node is -1 in the fixture, the kernel's "no NUMA node". That must
	// be an absent series, not a magic number that pollutes min() and avg().
	for _, f := range fams {
		if f.GetName() == "talos_hw_pcidevice_numa_node_info" {
			t.Error("numa_node exported despite the platform reporting none")
		}
		if f.GetName() == "talos_hw_pcidevice_numa_node" || f.GetName() == "talos_hw_pcidevice_irq" {
			t.Errorf("%s still exports an identifier as a value", f.GetName())
		}
	}

	// The sysfs identity is folded into the existing info metric.
	if _, ok := val("talos_hw_pcidevice_info", map[string]string{
		"bdf": "0000:00:02.0", "revision": "0xc3", "subsystem_vendor_id": "0x1002",
	}); !ok {
		t.Error("revision and subsystem ids missing from talos_hw_pcidevice_info")
	}
}

func TestPCIDegradesWithoutFileAccess(t *testing.T) {
	// os:reader cannot read files. The COSI inventory must still be reported.
	var logBuf bytes.Buffer
	c := NewPCI(slog.New(slog.NewTextHandler(&logBuf, nil)))
	c.state = func(*collector.NodeClient) state.CoreState { return fixtures() }
	c.newAPI = func(*collector.NodeClient) pciFileAPI {
		return stubPCIFiles{err: errors.New("rpc error: permission denied")}
	}
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect must not fail when only the sysfs enrichment is denied: %v", err)
	}
	fams, _ := reg.Gather()
	found := false
	for _, f := range fams {
		if f.GetName() == "talos_hw_pcidevice_info" && len(f.GetMetric()) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("PCI inventory lost when file reads were denied")
	}
	if !c.disabled.Load() {
		t.Error("expected the sysfs enrichment to disable itself after a permission error")
	}

	// The degradation must be visible in the logs: an operator with os:reader
	// has to be told why link speed and NUMA data vanished, and how to fix it.
	logged := logBuf.String()
	if !strings.Contains(logged, "pci sysfs enrichment disabled") {
		t.Errorf("expected a disable warning in the logs, got %q", logged)
	}
	if !strings.Contains(logged, "talos.roles") {
		t.Errorf("expected the warning to name the Helm value, got %q", logged)
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

// TestMemoryPruneEmptySetKeepsEverything pins the Pruner contract: an empty
// live set means discovery has not synced yet, never "the cluster is empty".
func TestMemoryPruneEmptySetKeepsEverything(t *testing.T) {
	m := NewMemory()
	m.cache = map[string]memInventory{"gone": {}, "live": {}}

	m.Prune(map[string]struct{}{})
	if len(m.cache) != 2 {
		t.Fatalf("Prune(empty) dropped cached state")
	}

	m.Prune(map[string]struct{}{"live": {}})
	if _, ok := m.cache["gone"]; ok {
		t.Error("Prune kept a departed node")
	}
	if _, ok := m.cache["live"]; !ok {
		t.Error("Prune dropped a live node")
	}
}
