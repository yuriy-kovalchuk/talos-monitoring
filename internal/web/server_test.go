package web_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/web"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

var testNodes = []nodes.Node{{
	Name:        "node-1",
	IP:          "10.0.0.10",
	Hostname:    "node-1",
	KubeVersion: "v1.35.0",
	OS:          "linux",
	Arch:        "arm64",
	Roles:       []string{"control-plane"},
}}

// newHandler builds a router; status nil means no node is verified.
func newHandler(t *testing.T, ready, withNodes bool, status func(string) (string, bool)) http.Handler {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	return newHandlerWithReg(t, reg, ready, withNodes, status)
}

// testSnap is the snapshot store the current test is building. Fixtures fill
// it through snapFor(t); newHandlerWithReg hands it to the router.
//
// The dashboard reads typed structs now, so a fixture describes a node the way
// a collector does rather than by hand-assembling the metric labels the UI used
// to re-parse. The assertions on rendered HTML are unchanged, which is what
// makes them a safety net for the inversion.
var testSnap *snapshot.Store

// snapFor returns the store for this test, resetting it on first use.
func snapFor(t *testing.T) *snapshot.Store {
	t.Helper()
	if testSnap == nil {
		testSnap = snapshot.New()
		t.Cleanup(func() { testSnap = nil })
	}
	return testSnap
}

// newHandlerWithReg builds a router with a caller-provided registry.
func newHandlerWithReg(t *testing.T, reg *prometheus.Registry, ready, withNodes bool, status func(string) (string, bool)) http.Handler {
	t.Helper()
	list := func() []nodes.Node { return nil }
	if withNodes {
		list = func() []nodes.Node { return testNodes }
	}
	if status == nil {
		status = func(string) (string, bool) { return "", false }
	}
	return web.NewRouter(discardLog, reg, func() bool { return ready }, list, status,
		history.New(10*time.Minute), snapFor(t), testVolumes)
}

// testVolumes is the PersistentVolume snapshot the join uses. Two of the
// fixtures' PVs are known to Kubernetes; anything else renders unnamed.
func testVolumes() map[string]k8svolumes.Volume {
	return map[string]k8svolumes.Volume{
		"pvc-1111": {Name: "pvc-1111", Namespace: "forgejo", Claim: "forgejo-data",
			StorageClass: "truenas-iscsi", Driver: "org.democratic-csi.iscsi", Capacity: 50 << 30},
	}
}

func do(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec, string(body)
}

func checkBody(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestEndpoints(t *testing.T) {
	h := newHandler(t, true, false, nil)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/health", http.StatusOK},
		{"/ready", http.StatusOK},
		{"/metrics", http.StatusOK},
	} {
		rec, _ := do(t, h, tc.path)
		if rec.Code != tc.want {
			t.Errorf("%s: got status %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestReadyNotReady(t *testing.T) {
	h := newHandler(t, false, false, nil)
	rec, _ := do(t, h, "/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/ready: got status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestOverviewNoNodes(t *testing.T) {
	h := newHandler(t, true, false, nil)
	rec, body := do(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("/: got status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("/: Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	checkBody(t, body, "talos-monitoring", "commit", "/metrics", "No nodes discovered yet")
}

func TestOverviewWithNodes(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("/: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "node-1", "control-plane", "badge-cp")
	// Sidebar hardware links fall back to the first discovered node.
	checkBody(t, body, `href="/nodes/node-1/cpu"`, `href="/nodes/node-1/sensors"`)
}

func TestDashboardFacts(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_node_uptime_seconds",
		[]string{"node"},
		map[string]string{"node": "node-1"}, 90061.0))
	reg.MustRegister(mustGauge("talos_node_cpu_usage_percent",
		[]string{"node", "cpu"},
		map[string]string{"node": "node-1", "cpu": "all"}, 42.5))
	reg.MustRegister(mustGauge("talos_hw_memory_module_info",
		[]string{"node", "slot", "manufacturer", "product", "device_locator", "bank_locator",
			"serial_number", "asset_tag"},
		map[string]string{"node": "node-1", "slot": "A1", "manufacturer": "Samsung", "product": "M321", "device_locator": "DIMM_A1", "bank_locator": "a1", "serial_number": "M1", "asset_tag": ""}, 1,
		map[string]string{"node": "node-1", "slot": "A2", "manufacturer": "Samsung", "product": "M321", "device_locator": "DIMM_A2", "bank_locator": "a2", "serial_number": "M2", "asset_tag": ""}, 1))
	moduleMeasures(t, reg, "node-1", []string{"A1", "A2"}, 16384, 4800)
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Modules = []snapshot.Module{
			{Slot: "A1", Manufacturer: "Samsung", Product: "M321", DeviceLocator: "DIMM_A1",
				BankLocator: "a1", SerialNumber: "M1", SizeBytes: 16384 * 1024 * 1024, SpeedTransfer: 4800e6},
			{Slot: "A2", Manufacturer: "Samsung", Product: "M321", DeviceLocator: "DIMM_A2",
				BankLocator: "a2", SerialNumber: "M2", SizeBytes: 16384 * 1024 * 1024, SpeedTransfer: 4800e6},
		}
		n.Runtime = &snapshot.Runtime{Version: "v1.13.4", Uptime: 90061, HasUptime: true}
		n.CPU = &snapshot.CPU{HasUsage: true, UsagePct: 42.5}
		n.Sockets = []snapshot.Socket{{ID: "CPU0", Product: "AMD Ryzen 7 6800H",
			Cores: 8, CoresEnabled: 8, Threads: 16, MaxHertz: 4700e6, BootHertz: 3200e6, Status: 1}}
		n.Disks = []snapshot.Disk{
			{Device: "/dev/sda", Model: "WD10", Serial: "S1", WWID: "w1", Transport: "sata",
				SubSystem: "scsi", BusPath: "0000:02:00.0", Attachment: "local",
				SizeBytes: 1000204886016, Rotational: true},
			{Device: "/dev/sdb", Model: "WD10", Serial: "S2", WWID: "w2", Transport: "iscsi",
				SubSystem: "scsi", BusPath: "0000:03:00.0", Attachment: "network",
				SizeBytes: 1000204886016, Rotational: true},
			// A loop device: no transport. Must NOT be counted as a disk, and
			// must not contribute capacity — a prior regression counted it twice.
			{Device: "/dev/loop0", SubSystem: "loop", Attachment: "virtual", SizeBytes: 134217728},
			// An iSCSI-attached Kubernetes volume: storage reached over the
			// network, not hardware in the machine.
			{Device: "/dev/sdc", Model: "VIRTUAL-DISK", WWID: "w3", Transport: "iscsi",
				SubSystem: "/sys/class/block", Attachment: "network",
				SizeBytes: 5368709120, Rotational: true},
		}
	})
	reg.MustRegister(mustGauge("talos_sensor_value",
		[]string{"node", "chip", "chip_name", "sensor", "kind", "label"},
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon0", "chip_name": "acpitz", "sensor": "temp1", "kind": "temperature", "label": ""}, 20,
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon1", "chip_name": "k10temp", "sensor": "temp1", "kind": "temperature", "label": "Tctl"}, 61))
	reg.MustRegister(mustGauge("talos_block_disk_info",
		[]string{"node", "device", "model", "serial", "wwid", "uuid", "transport", "subsystem",
			"bus_path", "attachment"},
		map[string]string{"node": "node-1", "device": "/dev/sda", "model": "WD10", "serial": "S1", "wwid": "w1", "uuid": "", "transport": "sata", "subsystem": "scsi", "bus_path": "0000:02:00.0", "attachment": "local"}, 1,
		map[string]string{"node": "node-1", "device": "/dev/sdb", "model": "WD10", "serial": "S2", "wwid": "w2", "uuid": "", "transport": "iscsi", "subsystem": "scsi", "bus_path": "0000:03:00.0", "attachment": "network"}, 1,
		// A loop device: no transport. Must NOT be counted as a disk, and must
		// not contribute capacity — a prior regression counted it twice.
		map[string]string{"node": "node-1", "device": "/dev/loop0", "model": "", "serial": "", "wwid": "", "uuid": "", "transport": "", "subsystem": "loop", "bus_path": "", "attachment": "virtual"}, 1,
		// An iSCSI-attached Kubernetes volume: storage the node reaches over the
		// network, not hardware in the machine. 19 of 23 transport-carrying
		// series on the reference cluster look like this.
		map[string]string{"node": "node-1", "device": "/dev/sdc", "model": "VIRTUAL-DISK", "serial": "", "wwid": "w3", "uuid": "", "transport": "iscsi", "subsystem": "/sys/class/block", "bus_path": "", "attachment": "network"}, 1))
	diskMeasures(t, reg, "node-1", map[string]float64{
		"/dev/sda":   1000204886016,
		"/dev/sdb":   1000204886016,
		"/dev/loop0": 134217728,
		"/dev/sdc":   5368709120,
	}, "/dev/sda", "/dev/sdb", "/dev/sdc")
	reg.MustRegister(mustGauge("talos_hw_processor_info",
		[]string{"node", "socket", "product", "part_number", "serial_number", "asset_tag"},
		map[string]string{"node": "node-1", "socket": "CPU0", "product": "AMD Ryzen 7 6800H", "part_number": "", "serial_number": "", "asset_tag": ""}, 1))
	processorMeasures(t, reg, "node-1", []string{"CPU0"}, 8, 8, 16, 4700, 3200)

	// A verified node, so the fleet is healthy and the attention card is empty.
	up := func(string) (string, bool) { return "v1.13.4", true }
	h := newHandlerWithReg(t, reg, true, true, up)
	rec, body := do(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("/: got status %d, want 200", rec.Code)
	}
	// Node table: uptime, load, memory, kubelet version, and the CPU model with
	// its core/thread count.
	checkBody(t, body,
		"1d 1h1m", "42.5%", "32 GiB",
		"Ryzen 7 6800H", "8c / 16t", "6.8 / 16 threads",
		"Uptime", "Kubernetes", "Load", "v1.35.0")
	// Only the locally attached disk counts: the loop device has no transport
	// and the iSCSI volume is storage reached over the network, not hardware.
	checkBody(t, body, "1 disk", "1.0 TB")
	if strings.Contains(body, "3 disk") || strings.Contains(body, "2 disk") {
		t.Error("loop or network-attached storage counted as a local disk")
	}
	// Temperature moved off the dashboard; it lives on the sensors page.
	if strings.Contains(body, "k10temp/Tctl") {
		t.Error("dashboard still renders the temperature column")
	}
	// Fleet tiles carry facts the table cannot show.
	checkBody(t, body, "Cores", "16 threads", "2 DIMMs", "Busiest node")
	// The removed stat cards must not come back: a single modal version is
	// wrong on a heterogeneous cluster.
	if strings.Contains(body, ">Kubelet<") {
		t.Error("overview still renders the removed Kubelet stat card")
	}
	// Attention card: healthy fleet renders the all-clear state.

	// The SMBIOS marketing suffix must not reach the table: it is what pushed
	// the row over the 1100px content column and wrapped it.
	if strings.Contains(body, "with Radeon Graphics") {
		t.Error("CPU model rendered unshortened")
	}
	// Responsive hooks the stylesheet needs to shed columns.
	checkBody(t, body, `class="table-fleet"`, "col-storage", "col-temp", "col-memory", "col-uptime")
}

func TestOverviewAttentionFlagsUnreachableNode(t *testing.T) {
	// status nil => the node's Talos API was never verified.
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/")
	checkBody(t, body, "Talos API unreachable", "1 to review")
	if strings.Contains(body, "all clear") {
		t.Error("attention card claims all clear while a node is unreachable")
	}
}

func TestOverviewAttentionFlagsRecentReboot(t *testing.T) {
	reg := prometheus.NewRegistry()
	// 12 minutes of uptime: inside the reboot window.
	reg.MustRegister(mustGauge("talos_node_uptime_seconds",
		[]string{"node"}, map[string]string{"node": "node-1"}, 720.0))
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{Uptime: 720, HasUptime: true}
	})
	up := func(string) (string, bool) { return "v1.13.4", true }
	h := newHandlerWithReg(t, reg, true, true, up)
	_, body := do(t, h, "/")
	checkBody(t, body, "rebooted", "ago")
	if strings.Contains(body, "all clear") {
		t.Error("attention card claims all clear right after a reboot")
	}
}

func TestOverviewPartial(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/partials/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("/partials/overview: got status %d, want 200", rec.Code)
	}
	// The partial must be swappable with outerHTML: it carries its own id and
	// re-declares the poll, and must not drag the page layout in with it.
	checkBody(t, body, `id="overview-body"`, `hx-get="/partials/overview"`, `hx-swap="outerHTML"`)
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("overview partial returned a full page, not a fragment")
	}
	// Guard against the hx-select-into-itself nesting bug (finding 1.1).
	if strings.Count(body, `id="overview-body"`) != 1 {
		t.Error("overview partial must contain exactly one #overview-body")
	}
}

func TestNodeSwitcherAndCookie(t *testing.T) {
	h := newHandler(t, true, true, nil)

	// Opening a node page offers the remember-node cookie...
	rec, body := do(t, h, "/nodes/node-1/cpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/cpu: got status %d, want 200", rec.Code)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "tm-node=node-1") {
		t.Errorf("expected a tm-node cookie, got %q", setCookie)
	}
	// ...the section page carries the node switcher with section-aware options...
	checkBody(t, body, `id="node-select"`, `value="/nodes/node-1/cpu" selected`)

	// ...and the dashboard sidebar links to the last viewed node's sections.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "tm-node", Value: "node-1"}) // #nosec G124 -- test request cookie
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	page, _ := io.ReadAll(out.Result().Body)
	checkBody(t, string(page), `href="/nodes/node-1"`, `href="/nodes/node-1/cpu"`, `href="/nodes/node-1/memory"`)
}

func TestNodesPage(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "node-1", "10.0.0.10", "linux / arm64", "v1.35.0")
}

func TestNodeDetail(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1: got status %d, want 200", rec.Code)
	}

	// Node switcher present; no collector metrics registered, so the merged
	// Machine card falls back to a single explanatory message.
	checkBody(t, body, "node-switch", "node-select", "No machine data yet")

	rec, _ = do(t, h, "/nodes/missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("/nodes/missing: got status %d, want 404", rec.Code)
	}
	rec, _ = do(t, h, "/nodes/node-1/bogus")
	if rec.Code != http.StatusNotFound {
		t.Errorf("/nodes/node-1/bogus: got status %d, want 404", rec.Code)
	}
}

func TestNodeDetailSystemInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_system_info",
		Help: "test fixture",
	}, []string{"node", "manufacturer", "product", "version", "serial_number", "uuid", "sku_number", "wake_up_type"})
	reg.MustRegister(g)
	g.WithLabelValues("node-1", "Acme Corp", "AcmeServer X1", "V1.0", "SN-12345", "uuid-1", "SKU-777", "Power Switch").Set(1)
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.System = &snapshot.System{Manufacturer: "Acme Corp", Product: "AcmeServer X1",
			Version: "V1.0", SerialNumber: "SN-12345", UUID: "uuid-1",
			SKUNumber: "SKU-777", WakeUpType: "Power Switch"}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1: got status %d, want 200", rec.Code)
	}
	// The Machine card merges k8s identity with the DMI facts worth keeping.
	checkBody(t, body, "Acme Corp", "AcmeServer X1", "SN-12345", "uuid-1")
	// SKU number and wake-up type were dropped: DMI defaults, not information.
	for _, noise := range []string{"SKU-777", "Power Switch", "Wake-up type"} {
		if strings.Contains(body, noise) {
			t.Errorf("node overview still renders dropped DMI field %q", noise)
		}
	}
}

func TestNodeDetailCPU(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_processor_info",
		Help: "test fixture",
	}, []string{"node", "socket", "product", "part_number", "serial_number", "asset_tag"})
	reg.MustRegister(g)
	g.WithLabelValues("node-1", "CPU-1", "Intel Xeon 9995", "P2", "SN-C1", "AT").Set(1)
	g.WithLabelValues("node-1", "CPU-0", "Intel Xeon 9995", "P1", "SN-C0", "AT").Set(1)
	processorMeasures(t, reg, "node-1", []string{"CPU-0", "CPU-1"}, 24, 24, 48, 2600, 1200)
	// Collectors hand the store sockets already sorted by designation.
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Sockets = []snapshot.Socket{
			{ID: "CPU-0", Product: "Intel Xeon 9995", PartNumber: "P1", SerialNumber: "SN-C0",
				AssetTag: "AT", Cores: 24, CoresEnabled: 24, Threads: 48, MaxHertz: 2600e6, BootHertz: 1200e6},
			{ID: "CPU-1", Product: "Intel Xeon 9995", PartNumber: "P2", SerialNumber: "SN-C1",
				AssetTag: "AT", Cores: 24, CoresEnabled: 24, Threads: 48, MaxHertz: 2600e6, BootHertz: 1200e6},
		}
		// Per-core usage: what the removed usage chart used to carry, and
		// what the core grid renders in its place.
		n.CPU = &snapshot.CPU{HasUsage: true, UsagePct: 37.5, Cores: []snapshot.Core{
			{Index: 0, HasUsage: true, UsagePct: 12, CurrentHz: 2400e6, Governor: "performance"},
			{Index: 1, HasUsage: true, UsagePct: 63, CurrentHz: 2600e6, Governor: "performance"},
		}}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/cpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/cpu: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "CPU-0", "Intel Xeon 9995", "24", "48", "2600", "SN-C0", "node-switch")
	// The charts are gone; the page carries a per-core usage grid and a
	// frequency table instead.
	checkBody(t, body, `id="cpu-live"`, "core-grid", "core-bar", "CPU usage", "Per core",
		"37.5%", "Load average", "Processes")
	if strings.Contains(body, "uplot") || strings.Contains(body, "cpu-graph") {
		t.Error("chart markup survived the uPlot removal")
	}
	// Sockets render sorted: CPU-0 row before CPU-1 row.
	if strings.Index(body, "SN-C0") > strings.Index(body, "SN-C1") {
		t.Error("expected CPU-0 row to render before CPU-1 row")
	}
}

func TestNodeDetailMemory(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_memory_module_info",
		Help: "test fixture",
	}, []string{"node", "slot", "manufacturer", "product", "device_locator", "bank_locator",
		"serial_number", "asset_tag"})
	reg.MustRegister(g)
	g.WithLabelValues("node-1", "A2", "Samsung", "M321R2GA0PB0-CP 1", "DIMM_A2", "a2", "MEM-SN-1", "MAT").Set(1)
	g.WithLabelValues("node-1", "A1", "Samsung", "M321R2GA0PB0-CP 0", "DIMM_A1", "a1", "MEM-SN-0", "MAT").Set(1)
	moduleMeasures(t, reg, "node-1", []string{"A1", "A2"}, 16384, 4800)
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Modules = []snapshot.Module{
			{Slot: "A1", Manufacturer: "Samsung", Product: "M321R2GA0PB0-CP 0", DeviceLocator: "DIMM_A1",
				BankLocator: "a1", SerialNumber: "MEM-SN-0", AssetTag: "MAT",
				SizeBytes: 16384 * 1024 * 1024, SpeedTransfer: 4800e6},
			{Slot: "A2", Manufacturer: "Samsung", Product: "M321R2GA0PB0-CP 1", DeviceLocator: "DIMM_A2",
				BankLocator: "a2", SerialNumber: "MEM-SN-1", AssetTag: "MAT",
				SizeBytes: 16384 * 1024 * 1024, SpeedTransfer: 4800e6},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/memory")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/memory: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "16 GiB", "4800", "Samsung", "MEM-SN-0", "32 GiB installed across 2 modules")
	// Slots render sorted: A1 row before A2 row.
	if strings.Index(body, "MEM-SN-0") > strings.Index(body, "MEM-SN-1") {
		t.Error("expected A1 row to render before A2 row")
	}
}

func TestNodeDetailPCI(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_info",
		Help: "test fixture",
	}, []string{"node", "bdf", "class", "subclass", "vendor", "product", "class_id", "subclass_id",
		"vendor_id", "product_id", "driver"})
	reg.MustRegister(g)
	g.WithLabelValues("node-1", "0000:01:00.0", "Non-Volatile memory controller", "", "Samsung", "PM9A1",
		"0x01", "0x08", "0x144d", "0xa808", "nvme").Set(1)
	g.WithLabelValues("node-1", "0000:00:02.0", "Display controller", "VGA compatible controller",
		"AMD/ATI", "Radeon Graphics", "0x03", "0x00", "0x1002", "0x1681", "amdgpu").Set(1)
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.PCI = []snapshot.PCIDevice{
			{BDF: "0000:00:02.0", Class: "Display controller", Subclass: "VGA compatible controller",
				Vendor: "AMD/ATI", Product: "Radeon Graphics", ClassID: "0x03", SubclassID: "0x00",
				VendorID: "0x1002", ProductID: "0x1681", Driver: "amdgpu"},
			{BDF: "0000:01:00.0", Class: "Non-Volatile memory controller",
				Vendor: "Samsung", Product: "PM9A1", ClassID: "0x01", SubclassID: "0x08",
				VendorID: "0x144d", ProductID: "0xa808", Driver: "nvme"},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/pci")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/pci: got status %d, want 200", rec.Code)
	}
	// Devices are grouped by class: one card per class, not a flat 35-row dump.
	checkBody(t, body, "0000:00:02.0", "Display controller / VGA compatible controller", "Radeon Graphics", "0x1002:0x1681", "amdgpu", "1 device")
	if n := strings.Count(body, "<details class=\"card\""); n != 2 {
		t.Errorf("got %d class groups, want 2 (one per distinct class)", n)
	}
	// BDFs render sorted: 0000:00:02.0 row before 0000:01:00.0 row.
	if strings.Index(body, "0000:00:02.0") > strings.Index(body, "0000:01:00.0") {
		t.Error("expected 0000:00:02.0 row to render before 0000:01:00.0 row")
	}
}

func TestNodeDetailDisks(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_block_disk_info",
		Help: "test fixture",
	}, []string{"node", "device", "model", "serial", "wwid", "uuid", "transport", "subsystem",
		"bus_path", "attachment"})
	reg.MustRegister(g)
	g.WithLabelValues("node-1", "/dev/sda", "WDC WD10EZEX", "SATA-SN-1", "wwid-1", "", "sata", "scsi",
		"0000:02:00.0", "local").Set(1)
	g.WithLabelValues("node-1", "/dev/nvme0n1", "SM2263EN NVMe", "NVME-SN-1", "wwid-2", "", "nvme", "nvme",
		"0000:01:00.0", "local").Set(1)
	diskMeasures(t, reg, "node-1", map[string]float64{
		"/dev/sda":     1000204886016,
		"/dev/nvme0n1": 931510603776,
	}, "/dev/sda")
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Disks = []snapshot.Disk{
			{Device: "/dev/nvme0n1", Model: "SM2263EN NVMe", Serial: "NVME-SN-1", WWID: "wwid-2",
				Transport: "nvme", SubSystem: "nvme", BusPath: "0000:01:00.0", Attachment: "local",
				SizeBytes: 931510603776},
			{Device: "/dev/sda", Model: "WDC WD10EZEX", Serial: "SATA-SN-1", WWID: "wwid-1",
				Transport: "sata", SubSystem: "scsi", BusPath: "0000:02:00.0", Attachment: "local",
				SizeBytes: 1000204886016, Rotational: true},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/storage: got status %d, want 200", rec.Code)
	}
	// Sizes are formatted from talos_block_disk_size_bytes now that the disk's
	// pretty-printed size is no longer carried as a label, so the column reads
	// like every other size in the UI ("932 GB", not Talos's "931.51 GB").
	checkBody(t, body, "/dev/nvme0n1", "932 GB", "SM2263EN NVMe", "HDD", "2 local · 1.9 TB", "Attachment", "local")
	// Devices render sorted: /dev/nvme0n1 row before /dev/sda row.
	if strings.Index(body, "/dev/nvme0n1") > strings.Index(body, "/dev/sda") {
		t.Error("expected /dev/nvme0n1 row to render before /dev/sda row")
	}
}

func TestNodeDetailSensors(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_sensor_value",
		[]string{"node", "chip", "chip_name", "sensor", "kind", "label"},
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon1", "chip_name": "k10temp", "sensor": "temp1", "kind": "temperature", "label": "Tctl"}, 61,
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon2", "chip_name": "nvme", "sensor": "temp1", "kind": "temperature", "label": "Composite"}, 44.85,
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon3", "chip_name": "amdgpu", "sensor": "in0", "kind": "voltage", "label": "vddgfx"}, 1.175))
	// Only the nvme publishes a threshold; k10temp does not, which is normal.
	reg.MustRegister(mustGauge("talos_sensor_limit",
		[]string{"node", "chip", "chip_name", "sensor", "kind", "label", "limit"},
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon2", "chip_name": "nvme", "sensor": "temp1", "kind": "temperature", "label": "Composite", "limit": "critical"}, 85))
	reg.MustRegister(mustGauge("talos_sensor_alarm",
		[]string{"node", "chip", "chip_name", "sensor", "kind", "label"},
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon2", "chip_name": "nvme", "sensor": "temp1", "kind": "temperature", "label": "Composite"}, 0))

	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Sensors = []snapshot.Sensor{
			{Chip: "/sys/class/hwmon/hwmon1", ChipName: "k10temp", Sensor: "temp1",
				Kind: "temperature", Label: "Tctl", Value: 61},
			{Chip: "/sys/class/hwmon/hwmon2", ChipName: "nvme", Sensor: "temp1",
				Kind: "temperature", Label: "Composite", Value: 44.85,
				Limits: map[string]float64{"critical": 85}, HasAlarm: true, Alarm: 0},
			{Chip: "/sys/class/hwmon/hwmon3", ChipName: "amdgpu", Sensor: "in0",
				Kind: "voltage", Label: "vddgfx", Value: 1.175},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/sensors")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/sensors: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, `id="sensors-live"`, `hx-get="/partials/nodes/node-1/sensors"`, `hx-trigger="every 30s"`)
	// Values render at a per-kind precision: %g turned a low voltage into
	// scientific notation.
	checkBody(t, body, "61.0", "44.9", "1.175")
	// A reading with a threshold shows how close it is; one without says so
	// rather than inventing a limit.
	checkBody(t, body, "52%", "85.0", "critical", "not reported")
	// Closest to its limit sorts first, even though it is the cooler sensor:
	// 44.9C of 85C beats 61C with no stated ceiling. Compare inside the table,
	// since the health strip names sensors above it.
	tbl := body[strings.Index(body, "<tbody>"):]
	if strings.Index(tbl, "Composite") > strings.Index(tbl, "Tctl") {
		t.Error("a sensor with a threshold must sort above one without")
	}
	// The health strip leads with the two numbers worth knowing.
	checkBody(t, body, "61.0 °C", "k10temp/Tctl", "52%", "nvme/Composite")
}

func mustGauge(name string, labelNames []string, pairs ...any) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: "test fixture"}, labelNames)
	for i := 0; i+1 < len(pairs); i += 2 {
		labels := pairs[i].(map[string]string)
		var val float64
		switch v := pairs[i+1].(type) {
		case float64:
			val = v
		case int:
			val = float64(v)
		default:
			panic(fmt.Sprintf("mustGauge: value must be float64 or int, got %T", pairs[i+1]))
		}
		lvs := make([]string, 0, len(labelNames))
		for _, ln := range labelNames {
			lvs = append(lvs, labels[ln])
		}
		g.WithLabelValues(lvs...).Set(val)
	}
	return g
}

func TestNodeDetailStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_node_version_info",
		[]string{"node", "version", "sha", "arch", "built"},
		map[string]string{"node": "node-1", "version": "v1.13.4", "sha": "abc123", "arch": "amd64", "built": "2026-07-01T00:00:00Z"}, 1))
	reg.MustRegister(mustGauge("talos_node_uptime_seconds",
		[]string{"node"},
		map[string]string{"node": "node-1"}, 90061.0))
	reg.MustRegister(mustGauge("talos_node_cpu_usage_percent",
		[]string{"node", "cpu"},
		map[string]string{"node": "node-1", "cpu": "all"}, 42.5,
		map[string]string{"node": "node-1", "cpu": "0"}, 10,
		map[string]string{"node": "node-1", "cpu": "1"}, 75))
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_freq_hertz",
		Help: "test fixture",
	}, []string{"node", "cpu", "freq"})
	gov := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_governor_info",
		Help: "test fixture",
	}, []string{"node", "cpu", "governor"})
	reg.MustRegister(g, gov)
	for cpu, cur := range map[string]float64{"0": 3500000e3, "1": 2250500e3} {
		for freq, val := range map[string]float64{"current": cur, "minimum": 400000e3, "maximum": 5000000e3} {
			g.WithLabelValues("node-1", cpu, freq).Set(val)
		}
		gov.WithLabelValues("node-1", cpu, "performance").Set(1)
	}
	// Core 2: the node reports no cpufreq data at all (zeros, empty
	// governor) — the card must render an n/a row, not "0 MHz".
	for _, freq := range []string{"current", "minimum", "maximum"} {
		g.WithLabelValues("node-1", "2", freq).Set(0)
	}
	reg.MustRegister(mustGauge("talos_node_processes",
		[]string{"node", "state"},
		map[string]string{"node": "node-1", "state": "running"}, 7,
		map[string]string{"node": "node-1", "state": "blocked"}, 3))
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{Version: "v1.13.4", SHA: "abc123", Arch: "amd64",
			Built: "2026-07-01T00:00:00Z", Uptime: 90061, HasUptime: true, Running: 7, Blocked: 3}
		n.CPU = &snapshot.CPU{
			HasUsage: true, UsagePct: 42.5,
			Cores: []snapshot.Core{
				{Index: 0, HasUsage: true, UsagePct: 10, CurrentHz: 3500000e3,
					MinimumHz: 400000e3, MaximumHz: 5000000e3, Governor: "performance"},
				{Index: 1, HasUsage: true, UsagePct: 75, CurrentHz: 2250500e3,
					MinimumHz: 400000e3, MaximumHz: 5000000e3, Governor: "performance"},
				// Core 2: the node reports no cpufreq data at all (zeros, empty
				// governor) — the card must render an n/a row, not "0 MHz".
				{Index: 2},
			},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)

	// Overview page: the status kv summary (per-core table lives on /cpu).
	rec, body := do(t, h, "/nodes/node-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1: got status %d, want 200", rec.Code)
	}
	// Machine card: versions in one place, boot time, processes.
	checkBody(t, body, "v1.13.4", "abc123", "1d 1h1m", "7 running · 3 blocked")
	// Health strip: live CPU.

	if strings.Contains(body, "Per-core usage") {
		t.Error("per-core table should not be on the overview page")
	}
	// The Talos version used to be rendered twice (Identity and Status).
	if n := strings.Count(body, "v1.13.4"); n != 1 {
		t.Errorf("Talos version rendered %d times on the node overview, want 1", n)
	}
	// Category cards double as navigation into the per-section pages. Match
	// the card markup, not the href alone: the sidebar nav links to every
	// section on every page, so a bare href check passes even when the card
	// is missing entirely.
	//
	// gpu is absent here on purpose — its card is conditional on the node
	// actually having a GPU, and this fixture has none.
	for _, section := range []string{"cpu", "memory", "storage", "pci", "sensors", "health", "identity", "kernel"} {
		if !strings.Contains(body, `class="stat-card card-link" href="/nodes/node-1/`+section+`"`) {
			t.Errorf("node overview missing the %s category card", section)
		}
	}
	if strings.Contains(body, `class="stat-card card-link" href="/nodes/node-1/gpu"`) {
		t.Error("GPU card rendered on a node with no GPU")
	}

	// Every category card must render its own summary, not the empty state,
	// when the data is present. A card whose dataset the overview forgets to
	// load silently reads "no data yet" forever, which no other test catches.
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Kernel = &snapshot.Kernel{
			Params:  []snapshot.KernelParam{{Name: "proc.sys.fs.aio-max-nr"}},
			Modules: []snapshot.KernelModule{{Name: "amdgpu"}},
		}
		n.Health = &snapshot.Health{HasMachineStatus: true, Stage: "running", Ready: true,
			Services: []snapshot.Service{{ID: "etcd", Running: true, Healthy: true}}}
		n.TimeSync = &snapshot.TimeSync{HasStatus: true, Synced: true}
	})
	_, body = do(t, h, "/nodes/node-1")
	checkBody(t, body, "1 tunables", "1 module", "running", "healthy of 1", "clock synced")

	// CPU page: the per-core table is gone. On every machine seen so far the
	// governor and min/max are identical across cores, so the table repeated the
	// same three values once per core; the graphs carry usage and frequency.
	rec, body = do(t, h, "/nodes/node-1/cpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/cpu: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "governor performance", "400 – 5000 MHz", "cores reporting")
	if strings.Contains(body, "Per-core usage") {
		t.Error("per-core usage table should be gone; the graphs carry that data")
	}
	// One statement, not one per core.
	if n := strings.Count(body, "performance"); n != 1 {
		t.Errorf("governor rendered %d times, want 1 (collapsed policy line)", n)
	}
}

func TestCPUPolicyReportsMixedCores(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_freq_hertz",
		Help: "test fixture",
	}, []string{"node", "cpu", "freq"})
	gov := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_governor_info",
		Help: "test fixture",
	}, []string{"node", "cpu", "governor"})
	reg.MustRegister(g, gov)
	// Two cores that genuinely disagree: collapsing must not silently pick one.
	for _, freq := range []string{"current", "minimum", "maximum"} {
		g.WithLabelValues("node-1", "0", freq).Set(3000000e3)
		g.WithLabelValues("node-1", "1", freq).Set(1000000e3)
	}
	gov.WithLabelValues("node-1", "0", "performance").Set(1)
	gov.WithLabelValues("node-1", "1", "powersave").Set(1)
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.CPU = &snapshot.CPU{Cores: []snapshot.Core{
			{Index: 0, CurrentHz: 3000000e3, MinimumHz: 3000000e3, MaximumHz: 3000000e3, Governor: "performance"},
			{Index: 1, CurrentHz: 1000000e3, MinimumHz: 1000000e3, MaximumHz: 1000000e3, Governor: "powersave"},
		}}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/cpu")
	// The page must say the cores disagree rather than pick one governor.
	if !strings.Contains(body, "varies") {
		t.Error("mixed cpufreq policies must be reported, not collapsed to one governor")
	}
	for _, gov := range []string{"governor performance", "governor powersave"} {
		if strings.Contains(body, gov) {
			t.Errorf("page claims %q while the cores disagree", gov)
		}
	}
}

func TestAboutPage(t *testing.T) {
	h := newHandler(t, true, false, nil)
	rec, body := do(t, h, "/about")
	if rec.Code != http.StatusOK {
		t.Fatalf("/about: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "commit", "/metrics", "/health", "/ready")
}

func TestStaticAssets(t *testing.T) {
	h := newHandler(t, true, false, nil)

	rec, body := do(t, h, "/static/css/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/css/style.css: got status %d, want 200", rec.Code)
	}
	// A 200 proves routing; this proves the embedded bytes are the real file
	// and not an empty or truncated one.
	if !strings.Contains(body, ".card") {
		t.Error("style.css does not look like the dashboard stylesheet")
	}

	rec, _ = do(t, h, "/static/js/htmx.min.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/js/htmx.min.js: got status %d, want 200", rec.Code)
	}

	rec, body = do(t, h, "/static/js/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/js/app.js: got status %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "node-select") {
		t.Error("app.js does not carry the node switcher")
	}

	// uPlot is gone. Its bytes must not still be shipped in the binary.
	for _, gone := range []string{"/static/js/uplot.min.js", "/static/css/uplot.min.css", "/static/js/graph.js"} {
		if rec, _ := do(t, h, gone); rec.Code != http.StatusNotFound {
			t.Errorf("%s still served (status %d) after the uPlot removal", gone, rec.Code)
		}
	}
}

// A partial that fails to render must return 5xx: htmx keeps the last good
// content on a 5xx, but an empty 200 would swap in nothing and leave the
// polled region dead until a manual reload.
func TestPartialRenderErrorReturns500(t *testing.T) {
	rec := httptest.NewRecorder()
	web.WritePartialErrorForTest(rec)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed partial render: got status %d, want 500", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Result().Body); len(body) == 0 {
		t.Error("failed partial render: empty body, want an error message")
	}
}

// A full page that fails to render must return 5xx, not an empty 200: an
// operator reading a blank dashboard cannot tell a dead render from a
// network problem. The partial path pins the same contract in its own test.
func TestRenderErrorReturns500(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	web.RenderErrorForTest(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed page render: got status %d, want 500", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Result().Body); len(body) == 0 {
		t.Error("failed page render: empty body, want an error message")
	}
}

// Pages are personal (the sidebar follows the last-viewed-node cookie) and
// partials are time-varying, so neither may survive in a proxy cache.
func TestNoStoreHeaders(t *testing.T) {
	h := newHandler(t, true, true, nil)
	for _, path := range []string{"/", "/nodes", "/nodes/node-1/cpu", "/partials/status", "/partials/overview"} {
		rec, _ := do(t, h, path)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestStatusPartial(t *testing.T) {
	// No verified nodes → down pill.
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/partials/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("/partials/status: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "pill", "0/1 nodes")
	if !strings.Contains(body, "pill-down") {
		t.Error("expected pill-down when no node is verified")
	}

	// All verified → ok pill with version on the pages.
	up := func(string) (string, bool) { return "v1.13.4", true }
	h2 := newHandler(t, true, true, up)
	_, body2 := do(t, h2, "/partials/status")
	if !strings.Contains(body2, "pill-ok") {
		t.Error("expected pill-ok when all nodes are verified")
	}
	_, body3 := do(t, h2, "/")
	checkBody(t, body3, "v1.13.4", "dot-ok")
}

func TestMetricsContent(t *testing.T) {
	h := newHandler(t, true, false, nil)
	rec, body := do(t, h, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: got status %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Error("/metrics: missing go_goroutines")
	}
}

func TestShortCPUModel(t *testing.T) {
	// Names observed on the reference cluster, plus pass-through cases.
	for _, tc := range []struct{ in, want string }{
		{"AMD Ryzen 7 6800H with Radeon Graphics", "Ryzen 7 6800H"},
		{"AMD Ryzen 7 255 w/ Radeon 780M Graphics", "Ryzen 7 255"},
		{"Intel(R) Core(TM) i5-6400T CPU @ 2.20GHz", "Core i5-6400T"},
		{"Intel(R) Core(TM) i5-6500T CPU @ 2.50GHz", "Core i5-6500T"},
		{"Intel(R) Xeon(R) Silver 4310 CPU @ 2.10GHz", "Xeon Silver 4310"},
		{"Neoverse-N1", "Neoverse-N1"},
		{"", ""},
	} {
		if got := web.ShortCPUModelForTest(tc.in); got != tc.want {
			t.Errorf("shortCPUModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNodeDataForLoadsOnlyWhatThePageRenders(t *testing.T) {
	// Index order: system, cpu, memory, pci, disks, sensors, status, summary,
	// links, health, identity, gpu, timesync, kernel, volumeLayer, diskIO.
	//
	// The last three ride along with an existing page rather than owning one:
	// health also loads timesync, identity also loads kernel, disks also
	// loads the volume layer.
	for _, tc := range []struct {
		section string
		want    [16]bool
	}{
		{"", [16]bool{true, true, true, false, false, false, true, true, true, true, true, true, true, true, false, false}},
		{"cpu", [16]bool{false, true, false, false, false, false, true, false, false, false, false, false, false, false, false, false}},
		{"memory", [16]bool{false, false, true, false, false, false, false, false, false, false, false, false, false, false, false, false}},
		{"storage", [16]bool{false, false, false, false, true, false, false, false, false, false, false, false, false, false, true, true}},
		{"pci", [16]bool{false, false, false, true, false, false, false, false, false, false, false, false, false, false, false, false}},
		{"sensors", [16]bool{false, false, false, false, false, true, false, false, false, false, false, false, false, false, false, false}},
		{"links", [16]bool{false, false, false, false, false, false, false, false, true, false, false, false, false, false, false, false}},
		{"health", [16]bool{false, false, false, false, false, false, false, false, false, true, false, false, true, false, false, false}},
		{"identity", [16]bool{false, false, false, false, false, false, false, false, false, false, true, false, false, false, false, false}},
		// kernel keeps identity too: the cmdline, kernel version and boot entry
		// are the context its tables need.
		{"kernel", [16]bool{false, false, false, false, false, false, false, false, false, false, true, false, false, true, false, false}},
		// gpu loads sensors so the card's own thermals sit on its page.
		{"gpu", [16]bool{false, false, false, false, false, true, false, false, false, false, false, true, false, false, false, false}},
	} {
		if got := web.NodeDataForTest(tc.section); got != tc.want {
			t.Errorf("nodeDataFor(%q) = %v, want %v", tc.section, got, tc.want)
		}
	}
}

func TestCategoryPagesDoNotRenderOtherSections(t *testing.T) {
	// The PCI page must not build sensor groups or the memory table, and the
	// sensors page must not build the PCI table.
	h := newHandler(t, true, true, nil)
	for _, tc := range []struct{ path, mustNotContain string }{
		{"/nodes/node-1/pci", "Hardware · Temperature"},
		{"/nodes/node-1/pci", "Hardware · Memory"},
		{"/nodes/node-1/sensors", "Hardware — PCI"},
		{"/nodes/node-1/memory", "Hardware · Disks"},
	} {
		_, body := do(t, h, tc.path)
		if strings.Contains(body, tc.mustNotContain) {
			t.Errorf("%s rendered %q from another section", tc.path, tc.mustNotContain)
		}
	}
}

func TestNodeLiveCardPartials(t *testing.T) {
	h := newHandler(t, true, true, nil)
	// Every node page now has a live region (finding 5.14): only the overview
	// and sensors refreshed themselves, and the rest needed a manual reload.
	for _, tc := range []struct{ card, id string }{
		{"overview", "node-live"},
		{"cpu", "cpu-live"},
		{"memory", "memory-live"},
		{"storage", "storage-live"},
		{"pci", "pci-live"},
		{"links", "links-live"},
		{"sensors", "sensors-live"},
		{"health", "health-live"},
		{"identity", "identity-live"},
		{"gpu", "gpu-live"},
	} {
		rec, body := do(t, h, "/partials/nodes/node-1/"+tc.card)
		if rec.Code != http.StatusOK {
			t.Fatalf("/partials/nodes/node-1/%s: got status %d, want 200", tc.card, rec.Code)
		}
		// A fragment, not a page: polling the page cost a full render plus a
		// cluster-wide gather on every tick.
		if strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("%s partial returned a full page", tc.card)
		}
		// Exactly one root, carrying its own poll, swapped with outerHTML.
		// hx-select-into-itself nested a copy per tick, and every copy then
		// started its own poller (finding 1.1).
		if n := strings.Count(body, `id="`+tc.id+`"`); n != 1 {
			t.Errorf("%s partial has %d #%s roots, want 1", tc.card, n, tc.id)
		}
		checkBody(t, body, `hx-swap="outerHTML"`, `hx-get="/partials/nodes/node-1/`+tc.card+`"`)
		if strings.Contains(body, "hx-select") {
			t.Errorf("%s partial reintroduced hx-select (the nesting bug)", tc.card)
		}
	}

	if rec, _ := do(t, h, "/partials/nodes/node-1/bogus"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown partial: got status %d, want 404", rec.Code)
	}
	if rec, _ := do(t, h, "/partials/nodes/missing/cpu"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown node: got status %d, want 404", rec.Code)
	}
}

// The charts are gone, and with them the reason a live region could not swap
// its whole card. What remains is the rule that outlived them: no page may
// reference chart markup or the removed graph endpoint.
func TestNoChartMarkupRemains(t *testing.T) {
	h := newHandler(t, true, true, nil)
	for _, path := range []string{
		"/", "/nodes/node-1", "/nodes/node-1/cpu", "/nodes/node-1/links",
		"/partials/nodes/node-1/cpu", "/partials/nodes/node-1/links",
	} {
		_, body := do(t, h, path)
		for _, gone := range []string{"cpu-graph", "cpu-freq-graph", "net-graph", "uplot", "/api/graph"} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still references %q", path, gone)
			}
		}
	}
}

func TestPageTemplatesHaveNoSelfNestingPollers(t *testing.T) {
	// Any element that polls itself must swap outerHTML; hx-select-into-itself
	// with innerHTML nests a copy on every tick (finding 1.1).
	h := newHandler(t, true, true, nil)
	for _, path := range []string{"/", "/nodes/node-1", "/nodes/node-1/cpu", "/nodes/node-1/sensors"} {
		_, body := do(t, h, path)
		if strings.Contains(body, "hx-select") {
			t.Errorf("%s uses hx-select; poll a dedicated partial with outerHTML instead", path)
		}
	}
}

func TestDMIValueBlanksPlaceholders(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Unknown", ""},
		{"unknown", ""},
		{"Not Specified", ""},
		{"To Be Filled By O.E.M.", ""},
		{"Default string", ""},
		{"None", ""},
		{"Unknown - [0x9B05]", ""}, // unresolved JEDEC vendor id, not a name
		{"Unknown - [0x0000]", ""},
		{"  Unknown  ", ""},
		{"", ""},
		// Real values must survive untouched.
		{"Samsung", "Samsung"},
		{"B68005KF91685", "B68005KF91685"},
		{"AMD Ryzen 7 6800H", "AMD Ryzen 7 6800H"},
		{"Unknown Vendor Ltd", "Unknown Vendor Ltd"},
	} {
		if got := web.DMIValueForTest(tc.in); got != tc.want {
			t.Errorf("dmiValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMemoryPageShowsLiveUsage(t *testing.T) {
	reg := prometheus.NewRegistry()
	// 32 GiB total, 2 GiB free, 6 GiB cache+buffers, 10 GiB available.
	const gib = 1024 * 1024 * 1024
	for name, v := range map[string]float64{
		"talos_node_memory_total_bytes":        32 * gib,
		"talos_node_memory_free_bytes":         2 * gib,
		"talos_node_memory_buffers_bytes":      1 * gib,
		"talos_node_memory_cached_bytes":       5 * gib,
		"talos_node_memory_available_bytes":    10 * gib,
		"talos_node_memory_committed_bytes":    48 * gib,
		"talos_node_memory_commit_limit_bytes": 16 * gib,
		"talos_node_swap_total_bytes":          0,
	} {
		reg.MustRegister(mustGauge(name, []string{"node"}, map[string]string{"node": "node-1"}, v))
	}
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Memory = &snapshot.Memory{
			Present: true, Total: 32 * gib, Free: 2 * gib, Buffers: 1 * gib,
			Cached: 5 * gib, Available: 10 * gib,
			Committed: 48 * gib, CommitLimit: 16 * gib,
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/memory")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/memory: got status %d, want 200", rec.Code)
	}
	// used = total - free - buffers - cached = 24 GiB = 75%.
	checkBody(t, body, "75%", "24 GiB", "10 GiB", "6.0 GiB")
	// Available is not free, and the page must not conflate them.

	// No swap configured must read as such, not as "0 B used".

	// Overcommit is surfaced with the reason, not as a bare scary number.
	checkBody(t, body, "committed", "overcommitted")
	// The three bar segments must add up to the whole width.
	widths := regexp.MustCompile(`mem-seg mem-\w+" style="width:(\d+)%`).FindAllStringSubmatch(body, -1)
	if len(widths) != 3 {
		t.Fatalf("got %d bar segments, want 3", len(widths))
	}
	sum := 0
	for _, w := range widths {
		n, _ := strconv.Atoi(w[1])
		sum += n
	}
	if sum != 100 {
		t.Errorf("bar segments sum to %d%%, want 100%%", sum)
	}
}

func TestPCIPageSurfacesDegradedLinks(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_hw_pcidevice_info",
		[]string{"node", "bdf", "class", "subclass", "vendor", "product", "class_id", "subclass_id",
			"vendor_id", "product_id", "driver", "revision", "subsystem_vendor_id", "subsystem_product_id"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "class": "Display controller",
			"subclass": "", "vendor": "AMD", "product": "Radeon 680M", "class_id": "0x03",
			"subclass_id": "0x00", "vendor_id": "0x1002", "product_id": "0x1681", "driver": "amdgpu",
			"revision": "0xc3", "subsystem_vendor_id": "0x1002", "subsystem_product_id": "0x1681"}, 1,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "class": "Mass storage controller",
			"subclass": "", "vendor": "Samsung", "product": "PM9A1", "class_id": "0x01",
			"subclass_id": "0x08", "vendor_id": "0x144d", "product_id": "0xa808", "driver": "nvme",
			"revision": "0x03", "subsystem_vendor_id": "0x126f", "subsystem_product_id": "0x2263"}, 1))
	// The GPU negotiated x4 at 2.5 GT/s on a x16 16 GT/s slot; the NVMe is fine.
	reg.MustRegister(mustGauge("talos_hw_pcidevice_link_gtps",
		[]string{"node", "bdf", "link"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "current"}, 2.5,
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "maximum"}, 16,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "current"}, 8,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "maximum"}, 8))
	reg.MustRegister(mustGauge("talos_hw_pcidevice_link_width",
		[]string{"node", "bdf", "link"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "current"}, 4,
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "maximum"}, 16,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "current"}, 4,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "maximum"}, 4))
	reg.MustRegister(mustGauge("talos_hw_pcidevice_aer_errors_total",
		[]string{"node", "bdf", "severity"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "severity": "correctable"}, 7))
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.PCI = []snapshot.PCIDevice{
			// Sorted by BDF, as the collector hands them over. The NVMe link is
			// healthy; the GPU negotiated x4 at 2.5 GT/s on a x16 16 GT/s slot.
			{BDF: "0000:01:00.0", Class: "Mass storage controller", Vendor: "Samsung",
				Product: "PM9A1", ClassID: "0x01", SubclassID: "0x08", VendorID: "0x144d",
				ProductID: "0xa808", Driver: "nvme", Revision: "0x03",
				SubsystemVendorID: "0x126f", SubsystemProductID: "0x2263",
				HasLink: true, LinkSpeedGTps: 8, MaxSpeedGTps: 8, LinkWidth: 4, MaxWidth: 4},
			{BDF: "0000:04:00.0", Class: "Display controller", Vendor: "AMD",
				Product: "Radeon 680M", ClassID: "0x03", SubclassID: "0x00", VendorID: "0x1002",
				ProductID: "0x1681", Driver: "amdgpu", Revision: "0xc3",
				SubsystemVendorID: "0x1002", SubsystemProductID: "0x1681",
				HasLink: true, LinkSpeedGTps: 2.5, MaxSpeedGTps: 16, LinkWidth: 4, MaxWidth: 16,
				HasAER: true, AERCorrectable: 7},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/pci")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/pci: got status %d, want 200", rec.Code)
	}
	// The degraded link is stated with what it should have been.
	checkBody(t, body, "2.5 GT/s x4", "of 16 GT/s x16 · degraded", "link degraded", "7 AER errors")
	// The healthy device shows its link without a degraded note.

	// Subsystem identity from sysfs reaches the page.
	checkBody(t, body, "sub 0x1002:0x1681", "rev 0xc3")
	// The class holding the fault sorts first and opens by default.
	if strings.Index(body, "Display controller") > strings.Index(body, "Mass storage controller") {
		t.Error("the class with a degraded link must sort before healthy ones")
	}
	if !strings.Contains(body, `<details class="card" open>`) {
		t.Error("the class needing attention must render open")
	}
}

func TestLinksPage(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_net_link_info",
		[]string{"node", "link", "type", "kind", "hwaddr", "driver", "driver_version",
			"firmware_version", "bus_path", "pci_id", "vendor", "product", "port", "duplex"},
		map[string]string{"node": "node-1", "link": "enp1s0", "type": "ether", "kind": "",
			"hwaddr": "00:e0:4c:68:10:09", "driver": "r8169", "driver_version": "6.18.34-talos",
			"firmware_version": "", "bus_path": "0000:01:00.0", "pci_id": "10EC:8125",
			"vendor": "Realtek", "product": "RTL8125 2.5GbE", "port": "TwistedPair", "duplex": "Full"}, 1,
		map[string]string{"node": "node-1", "link": "eno1", "type": "ether", "kind": "",
			"hwaddr": "ac:e2:d3:04:3c:fb", "driver": "e1000e", "driver_version": "6.18.34-talos",
			"firmware_version": "0.1-4", "bus_path": "0000:00:1f.6", "pci_id": "8086:15E3",
			"vendor": "Intel", "product": "I219-LM", "port": "TwistedPair", "duplex": "Unknown"}, 1,
		map[string]string{"node": "node-1", "link": "bond0", "type": "ether", "kind": "bond",
			"hwaddr": "3a:ab:59:15:d3:1a", "driver": "bonding", "driver_version": "",
			"firmware_version": "", "bus_path": "", "pci_id": "", "vendor": "", "product": "",
			"port": "Other", "duplex": "Unknown"}, 1))
	reg.MustRegister(mustGauge("talos_net_link_up", []string{"node", "link"},
		map[string]string{"node": "node-1", "link": "enp1s0"}, 1,
		map[string]string{"node": "node-1", "link": "eno1"}, 0,
		map[string]string{"node": "node-1", "link": "bond0"}, 0))
	reg.MustRegister(mustGauge("talos_net_link_speed_mbit", []string{"node", "link"},
		map[string]string{"node": "node-1", "link": "enp1s0"}, 2500))
	reg.MustRegister(mustGauge("talos_net_link_mtu_bytes", []string{"node", "link"},
		map[string]string{"node": "node-1", "link": "enp1s0"}, 1500))
	reg.MustRegister(mustGauge("talos_net_address_info",
		[]string{"node", "link", "address", "family", "scope"},
		map[string]string{"node": "node-1", "link": "enp1s0", "address": "10.0.4.6/24",
			"family": "inet4", "scope": "global"}, 1))
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Links = []snapshot.Link{
			{Name: "bond0", Type: "ether", Kind: "bond", HWAddr: "3a:ab:59:15:d3:1a",
				Driver: "bonding", Port: "Other", Duplex: "Unknown"},
			{Name: "eno1", Type: "ether", HWAddr: "ac:e2:d3:04:3c:fb", Driver: "e1000e",
				DriverVersion: "6.18.34-talos", FirmwareVersion: "0.1-4", BusPath: "0000:00:1f.6",
				PCIID: "8086:15E3", Vendor: "Intel", Product: "I219-LM",
				Port: "TwistedPair", Duplex: "Unknown"},
			{Name: "enp1s0", Type: "ether", HWAddr: "00:e0:4c:68:10:09", Driver: "r8169",
				DriverVersion: "6.18.34-talos", BusPath: "0000:01:00.0", PCIID: "10EC:8125",
				Vendor: "Realtek", Product: "RTL8125 2.5GbE", Port: "TwistedPair", Duplex: "Full",
				Up: true, Carrier: true, SpeedMbit: 2500, MTUBytes: 1500,
				Addresses: []snapshot.Address{{Address: "10.0.4.6/24", Family: "inet4", Scope: "global"}}},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/links")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/links: got status %d, want 200", rec.Code)
	}
	// Speed is rendered in Gbit once it reaches 1000 Mbit.
	checkBody(t, body, "2.5 Gbit/s", "Full", "MTU 1500", "10.0.4.6/24", "r8169", "0000:01:00.0")
	// A link with no carrier says so instead of showing a bogus speed.

	// Firmware version appears alongside the driver version when reported.

	// Physical NICs sort before the logical interfaces built on them.
	if strings.Index(body, "bond0") < strings.Index(body, "enp1s0") {
		t.Error("physical NICs must sort before logical interfaces")
	}
}

func TestCPUPageShowsObservedFrequencyRange(t *testing.T) {
	// A single "current frequency" is false precision: the value swings, and the
	// scrape that reads it raises it. The page must show the range.
	hist := history.New(10 * time.Minute)
	base := time.Now().Add(-time.Minute)
	// History and the metric are both in hertz.
	for i, v := range []float64{900e6, 2700e6, 1400e6, 2400e6, 1000e6} {
		hist.Append("node-1", "freq.current.0", base.Add(time.Duration(i)*time.Second), v)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_node_cpu_freq_hertz",
		[]string{"node", "cpu", "freq"},
		map[string]string{"node": "node-1", "cpu": "0", "freq": "current"}, 2700e6,
		map[string]string{"node": "node-1", "cpu": "0", "freq": "minimum"}, 800e6,
		map[string]string{"node": "node-1", "cpu": "0", "freq": "maximum"}, 3100e6))
	reg.MustRegister(mustGauge("talos_node_cpu_governor_info",
		[]string{"node", "cpu", "governor"},
		map[string]string{"node": "node-1", "cpu": "0", "governor": "powersave"}, 1))

	list := func() []nodes.Node { return testNodes }
	h := web.NewRouter(discardLog, reg, func() bool { return true }, list,
		func(string) (string, bool) { return "v1.13.4", true }, hist, snapFor(t), testVolumes)
	rec, body := do(t, h, "/nodes/node-1/cpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/cpu: got status %d, want 200", rec.Code)
	}
	// min 900, max 2700, median 1400 of the five samples.
	checkBody(t, body, "900 – 2700 MHz", "1400", "Observed range", "Median")
	// The caveat must be stated, not implied.

	// The policy bounds are labelled as policy, not confused with observation.
}

// The hardware _info metrics carry identity strings only; every measurement
// they used to encode as a label is its own gauge (finding 3.1). These helpers
// emit both halves the way the collectors do, so fixtures stay readable.

// keyedGauge registers a {node,<key>} gauge from a map of key -> value.
func keyedGauge(t *testing.T, reg *prometheus.Registry, name, key, node string, values map[string]float64) {
	t.Helper()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: "test fixture"},
		[]string{"node", key})
	reg.MustRegister(g)
	for k, v := range values {
		g.WithLabelValues(node, k).Set(v)
	}
}

// snapNode fills one node's snapshot section. Fixtures describe a node the way
// a collector does now, instead of hand-assembling the labels the UI used to
// re-parse.
func snapNode(t *testing.T, node string, fn func(*snapshot.Node)) {
	t.Helper()
	snapFor(t).Update(node, fn)
}

// processorMeasures emits the per-socket gauges for one uniform CPU model.
func processorMeasures(t *testing.T, reg *prometheus.Registry, node string, sockets []string,
	cores, coresEnabled, threads, maxMHz, bootMHz float64) {
	t.Helper()
	for name, v := range map[string]float64{
		"talos_hw_processor_cores":         cores,
		"talos_hw_processor_cores_enabled": coresEnabled,
		"talos_hw_processor_threads":       threads,
		"talos_hw_processor_max_hertz":     maxMHz * 1e6,
		"talos_hw_processor_boot_hertz":    bootMHz * 1e6,
		"talos_hw_processor_status":        0,
	} {
		per := make(map[string]float64, len(sockets))
		for _, sock := range sockets {
			per[sock] = v
		}
		keyedGauge(t, reg, name, "socket", node, per)
	}
}

// moduleMeasures emits the per-DIMM gauges for uniform modules.
func moduleMeasures(t *testing.T, reg *prometheus.Registry, node string, slots []string, sizeMiB, speedMTs float64) {
	t.Helper()
	size, speed := make(map[string]float64, len(slots)), make(map[string]float64, len(slots))
	for _, sl := range slots {
		size[sl] = sizeMiB * 1024 * 1024
		speed[sl] = speedMTs * 1e6
	}
	keyedGauge(t, reg, "talos_hw_memory_module_size_bytes", "slot", node, size)
	keyedGauge(t, reg, "talos_hw_memory_module_speed_transfers_per_second", "slot", node, speed)
}

// diskMeasures emits the per-disk gauges. rotational lists the spinning disks.
func diskMeasures(t *testing.T, reg *prometheus.Registry, node string, sizes map[string]float64, rotational ...string) {
	t.Helper()
	keyedGauge(t, reg, "talos_block_disk_size_bytes", "device", node, sizes)
	rot := make(map[string]float64, len(sizes))
	for dev := range sizes {
		rot[dev] = 0
	}
	for _, dev := range rotational {
		rot[dev] = 1
	}
	keyedGauge(t, reg, "talos_block_disk_rotational", "device", node, rot)
}

// TestStaticAssetsAreCacheableAndSelfHosted covers findings 5.4 and 5.5. Every
// page load used to re-fetch the htmx and uPlot bundles (~120 KB), and the
// fonts came from Google's CDN, which a cluster with no egress cannot reach.
func TestStaticAssetsAreCacheableAndSelfHosted(t *testing.T) {
	h := newHandler(t, true, true, nil)

	for _, path := range []string{
		"/static/css/style.css",
		"/static/css/fonts.css",
		"/static/js/htmx.min.js",
		"/static/fonts/manrope-latin-var.woff2",
		"/static/fonts/jetbrains-mono-latin-var.woff2",
	} {
		rec, _ := do(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: got status %d, want 200", path, rec.Code)
			continue
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s: no ETag", path)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
			t.Errorf("%s: Cache-Control = %q, want a max-age", path, cc)
		}

		// A conditional request must be answered 304, not with the body again.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("If-None-Match", etag)
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusNotModified {
			t.Errorf("%s: revalidation got %d, want 304", path, rec2.Code)
		}
		if rec2.Body.Len() != 0 {
			t.Errorf("%s: 304 carried a %d-byte body", path, rec2.Body.Len())
		}
	}

	// No page may reach out to a font CDN: an air-gapped cluster would render
	// with the fallback stack instead of the intended typography.
	_, body := do(t, h, "/")
	for _, host := range []string{"fonts.googleapis.com", "fonts.gstatic.com"} {
		if strings.Contains(body, host) {
			t.Errorf("layout still references %s", host)
		}
	}
}

// TestSecurityHeaders: the dashboard and /metrics are unauthenticated and
// carry chassis serials, system UUIDs and disk WWIDs. Network isolation by
// the operator is the real control, but headers are free and close the easy
// holes.
func TestSecurityHeaders(t *testing.T) {
	h := newHandler(t, true, true, nil)
	for _, path := range []string{"/", "/nodes/node-1", "/metrics", "/static/js/app.js"} {
		rec, _ := do(t, h, path)
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Errorf("%s: no Content-Security-Policy", path)
			continue
		}
		// Everything is served from this binary, so nothing may load
		// cross-origin — that is what makes a strict policy affordable here.
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP missing %q", path, want)
			}
		}
		if strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows unsafe-eval", path)
		}
	}
}

// TestIfNoneMatchParsesTheList: the header is a comma-separated list and
// entries may be weak. A substring test would also match an unrelated tag that
// happens to contain ours.
func TestIfNoneMatchParsesTheList(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, _ := do(t, h, "/static/js/app.js")
	etag := rec.Header().Get("ETag")

	for _, tc := range []struct {
		header string
		want   int
	}{
		{etag, http.StatusNotModified},
		{`"other", ` + etag, http.StatusNotModified},
		{"W/" + etag, http.StatusNotModified},
		{"*", http.StatusNotModified},
		{`"unrelated"`, http.StatusOK},
		{`"x` + strings.Trim(etag, `"`) + `x"`, http.StatusOK}, // superstring must NOT match
	} {
		req := httptest.NewRequest(http.MethodGet, "/static/js/app.js", nil)
		req.Header.Set("If-None-Match", tc.header)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		if r.Code != tc.want {
			t.Errorf("If-None-Match %s: got %d, want %d", tc.header, r.Code, tc.want)
		}
	}
}

// TestNoInlineScriptOrHandlers: the CSP is script-src 'self', so an inline
// <script> or an on*= attribute is silently blocked at runtime. The node
// switcher was exactly that — an onchange= that would have stopped navigating
// the moment the CSP shipped.
func TestNoInlineScriptOrHandlers(t *testing.T) {
	h := newHandler(t, true, true, nil)
	inlineScript := regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`)
	handler := regexp.MustCompile(`\son[a-z]+\s*=`)

	for _, path := range []string{"/", "/nodes", "/nodes/node-1", "/nodes/node-1/cpu",
		"/nodes/node-1/links", "/nodes/node-1/sensors", "/about"} {
		_, body := do(t, h, path)
		if m := inlineScript.FindString(body); m != "" {
			t.Errorf("%s: inline <script> would be blocked by the CSP: %q", path, m)
		}
		if m := handler.FindString(body); m != "" {
			t.Errorf("%s: inline event handler would be blocked by the CSP: %q", path, m)
		}
	}
	// And the switcher still exists, wired from the external bundle.
	_, body := do(t, h, "/nodes/node-1")
	checkBody(t, body, `id="node-select"`, `/static/js/app.js`)
}

// TestAssetURLsAreVersioned covers the failure that broke the node switcher:
// the ETag was the git commit, so editing a static asset without committing
// left it unchanged while Cache-Control said the browser could reuse the old
// file for a day. A stale graph.js then had no switcher handler, while the
// markup no longer had the inline one.
//
// Two defences: the ETag is now the asset content, and asset URLs carry that
// hash so a new build cannot be served from an old cache at all.
func TestAssetURLsAreVersioned(t *testing.T) {
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1")

	v := strings.Trim(web.AssetVersion(), `"`)
	if v == "" {
		t.Fatal("empty asset version")
	}
	for _, asset := range []string{
		"/static/js/app.js", "/static/js/htmx.min.js", "/static/css/style.css",
	} {
		want := asset + "?v=" + v
		if !strings.Contains(body, want) {
			t.Errorf("layout references %s without the version query (want %s)", asset, want)
		}
	}

	// The ETag must be the content hash, not the build identity: a dev build
	// keeps the same version and commit across asset edits.
	rec, _ := do(t, h, "/static/js/app.js")
	if got := rec.Header().Get("ETag"); got != web.AssetVersion() {
		t.Errorf("ETag = %q, want the asset hash %q", got, web.AssetVersion())
	}
	if strings.Contains(rec.Header().Get("ETag"), "dev") {
		t.Error("ETag still derives from the build identity")
	}
}

// A service that publishes no health check must not read as broken. Talos
// reports Healthy=false with Unknown=true for `dashboard` and the extension
// services, so on the reference cluster two of twelve services per node look
// unhealthy to any renderer that reads Healthy alone.
func TestHealthPageSeparatesNoCheckFromFailing(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true,
			Stage:            "running",
			Ready:            true,
			Services: []snapshot.Service{
				{ID: "etcd", Running: true, Healthy: true},
				{ID: "dashboard", Running: true, Unknown: true},
				{ID: "kubelet", Running: true},
				{ID: "trustd"},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/health")

	// One of each state, and the counts that follow from them.
	for _, want := range []string{"healthy", "no check", "failing", "stopped"} {
		if !strings.Contains(body, ">"+want+"</span>") {
			t.Errorf("health page missing the %q service state", want)
		}
	}
	// Unhealthy is failing+stopped = 2. dashboard's missing health check is
	// NOT a fault and must stay out of that count.
	checkBody(t, body, "1 failing · 1 stopped", "of 4 services",
		"1 without a health check")
}

// Diagnostics and unmet conditions are absent on a healthy node, so their
// cards must not render at all rather than render empty.
func TestHealthPageHidesEmptyDiagnosticsAndConditions(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true, Stage: "running", Ready: true,
			Services: []snapshot.Service{{ID: "etcd", Running: true, Healthy: true}},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/health")

	checkBody(t, body, "none raised", "running")
	if strings.Contains(body, "Unmet conditions") {
		t.Error("unmet-conditions card rendered on a ready node")
	}
	if strings.Contains(body, "<th>Warning</th>") {
		t.Error("diagnostics table rendered with no diagnostics")
	}
}

// A node the health collector has not reached yet renders the empty state,
// not a page full of zeroes that reads as "everything is stopped".
func TestHealthPageWithoutDataRendersEmptyState(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/health: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "No health data yet")
	if strings.Contains(body, "of 0 services") {
		t.Error("empty node rendered service counts instead of the empty state")
	}
}

// The schematic id is 64 hex characters and differs only somewhere in the
// middle. The page shows the git-length short form an operator can actually
// compare, and keeps the full value on the page for copying.
func TestIdentityPageShortensTheSchematic(t *testing.T) {
	const id = "65cf8364cd0de4cf7b851dc7067a2db83d0ba04f11d8635c6cd3334be6ffb825"
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{
			Schematic:     id,
			KernelVersion: "6.18.34-talos",
			BootedEntry:   "talos-v1.13.4~1.efi",
			Extensions: []snapshot.Extension{
				{Name: "amd-ucode", Version: "20260519"},
				{Name: "iscsi-tools", Version: "v0.2.0"},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/identity")

	checkBody(t, body, "65cf8364cd0d", id, "talos-v1.13.4~1.efi",
		"kernel 6.18.34-talos", "amd-ucode", "iscsi-tools", "2 extensions")
}

// SecureBoot=false and "no SecurityState resource" render differently: the
// first is a real answer, the second is an absence.
func TestIdentityPageDistinguishesAbsentSecurityState(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{Schematic: "65cf8364cd0de4cf"}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/identity")

	checkBody(t, body, "not reported")
	if strings.Contains(body, "Security posture") {
		t.Error("security posture card rendered with no SecurityState")
	}
	// No extensions is a fact worth stating, not an empty table.
	checkBody(t, body, "No system extensions on this node")
}

func TestIdentityPageRendersSecurityPosture(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{
			HasSecurityState:        true,
			SecureBoot:              false,
			BootedWithUKI:           true,
			ModuleSignatureEnforced: true,
			SELinuxState:            "enabled, permissive",
			FIPSState:               "disabled",
			Cmdline:                 "talos.platform=metal selinux=1 module.sig_enforce=1",
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/identity")

	checkBody(t, body, "Security posture", "enabled, permissive", "enforced")
	// The kernel command line moved to its own page.
	if strings.Contains(body, "module.sig_enforce=1") {
		t.Error("kernel cmdline still rendered on the Image page")
	}
}

func TestIdentityPageWithoutDataRendersEmptyState(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/identity")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/identity: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "No image data yet")
}

// Clock offsets are microseconds. Raw seconds render as 4.4597e-05, which is
// unreadable, so the page scales them to the unit an operator thinks in.
func TestHealthPageRendersTheClockStrip(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{HasMachineStatus: true, Stage: "running", Ready: true}
		n.TimeSync = &snapshot.TimeSync{
			HasStatus: true, Synced: true, Epoch: 2,
			HasAdjtime: true, KernelSynced: true, State: "TIME_OK",
			OffsetSeconds: -44.597e-6, MaxErrorSeconds: 0.46,
			FrequencyRatio: 1.0000331520996093,
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/health")

	// "33.2 ppm", not "+33.2": html/template escapes the leading plus to
	// &#43;, which the browser renders as "+" but a substring match will not
	// find. The sign is deliberate — drift has a direction.
	checkBody(t, body, "-44.6 µs", "460.0 ms", "33.2 ppm", "TIME_OK")
	if strings.Contains(body, "4.4597e-05") {
		t.Error("raw seconds leaked into the clock strip")
	}
}

// A driver that publishes no counters must say so rather than render 0%.
func TestGPUPageDistinguishesAbsentFromZero(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.GPUs = []snapshot.GPU{
			{Card: "card0", Driver: "amdgpu", Slot: "0000:03:00.0",
				HasBusy: true, BusyPercent: 0,
				HasVRAM: true, VRAMTotalBytes: 34208743424, VRAMUsedBytes: 32394584064},
			{Card: "card1", Driver: "i915", Slot: "0000:00:02.0"},
			{Card: "card2", Driver: "amdgpu", Slot: "0000:41:00.0",
				HasBusy: true, BusyPercent: 0, Suspended: true},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/gpu")

	// card0 is genuinely idle at 0%, and 94% of its VRAM is gone.
	checkBody(t, body, "amdgpu", "94%", "0%")
	// card1 reports nothing, which must not read as an idle, empty GPU.
	checkBody(t, body, "not reported by i915")
	// card2 is asleep: its 0 is derived, and the state must say so rather
	// than the driver being blamed for not reporting.
	checkBody(t, body, "asleep")
}

func TestGPUPageWithoutCardsExplainsWhy(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/gpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/gpu: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "No GPU with a driver bound")
}

// The 19 permanently-ready pseudo-volumes are folded away so the three real
// partitions are visible — but a pseudo-volume that FAILED must still show.
func TestStoragePageFoldsHealthyPseudoVolumesOnly(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Volumes = &snapshot.Volumes{
			SystemDisk: "nvme0n1",
			Volumes: []snapshot.Volume{
				{ID: "EPHEMERAL", Type: "partition", Phase: "ready", Ready: true,
					Filesystem: "xfs", Location: "/dev/nvme0n1p4", PrettySize: "126 GB"},
				{ID: "/var/log", Type: "directory", Phase: "ready", Ready: true},
				{ID: "/var/mnt", Type: "directory", Phase: "failed", Ready: false,
					ErrorMessage: "mount target busy"},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")

	checkBody(t, body, "EPHEMERAL", "126 GB", "system disk nvme0n1")
	// The failed directory volume is listed despite being a pseudo-volume.
	checkBody(t, body, "/var/mnt", "mount target busy", "1 NOT READY")
	// The healthy one is folded into the footnote instead.
	checkBody(t, body, "1 healthy directory, overlay and symlink")
	if strings.Contains(body, ">/var/log\n") {
		t.Error("healthy pseudo-volume rendered as a row")
	}
}

// An encrypted volume with stale key slots is a future unlock failure.
func TestStoragePageSurfacesFailedEncryptionSyncs(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Volumes = &snapshot.Volumes{Volumes: []snapshot.Volume{
			{ID: "STATE", Type: "partition", Phase: "ready", Ready: true,
				EncryptionProvider: "luks2", EncryptionFailedSyncs: 2},
		}}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")
	checkBody(t, body, "luks2", "2 encryption keys failed to sync")
}

// An unsupported kernel parameter is silently ignored by the kernel, so the
// page has to say so explicitly.
func TestKernelPageFlagsUnsupportedParams(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{
			Schematic:     "65cf8364cd0de4cf",
			KernelVersion: "6.18.34-talos",
			Cmdline:       "talos.platform=metal selinux=1 module.sig_enforce=1",
		}
		n.Kernel = &snapshot.Kernel{
			Params: []snapshot.KernelParam{
				{Name: "proc.sys.fs.aio-max-nr", Current: "1048576", Default: "65536"},
				{Name: "proc.sys.net.core.typo", Unsupported: true},
			},
			Modules: []snapshot.KernelModule{
				{Name: "amdgpu", State: "Live", SizeBytes: 16965632, ReferenceCount: 2},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/kernel")

	checkBody(t, body, "unsupported: the running kernel ignores this",
		"1 UNSUPPORTED", "1 differ from default", "amdgpu", "1 module",
		"6.18.34-talos", "module.sig_enforce=1")
}

// The disks page shows cumulative counters, not rates. Busy is io_time as a
// share of uptime, which is meaningless without an uptime to divide by — it
// must stay blank then rather than render a misleading 0%.
func TestStoragePageRendersBlockIO(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{HasUptime: true, Uptime: 1000}
		n.DiskIO = []snapshot.DiskIO{
			{Device: "nvme0n1", Kind: "disk", ReadBytes: 1 << 30, WrittenBytes: 2 << 30,
				ReadsCompleted: 12345, WritesCompleted: 6789, IOTimeSeconds: 250, IOInProgress: 3},
			{Device: "nvme0n1p4", Kind: "partition", ReadBytes: 1 << 20},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")

	checkBody(t, body, "Block I/O", "nvme0n1", "1.0 GiB", "2.0 GiB", "12,345", "25.0%", "partition")
	// The caveat has to be on the page, not just in the docs.
	checkBody(t, body, "do not sum")
}

func TestStoragePageBlankBusyWithoutUptime(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.DiskIO = []snapshot.DiskIO{{Device: "sda", Kind: "disk", IOTimeSeconds: 250}}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")
	if strings.Contains(body, "0.0%") {
		t.Error("busy rendered as 0.0% with no uptime to divide by")
	}
}

func TestNodeOverviewShowsLoadAverage(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{
			Version: "v1.13.4", HasUptime: true, Uptime: 3600,
			HasLoad: true, Load1: 1.92, Load5: 1.53, Load15: 1.47,
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1")
	checkBody(t, body, "Load average", "1.92 · 1.53 · 1.47")
}

// A node whose LoadAvg RPC failed has no load to show; the row must be absent
// rather than rendering zeroes that read as an idle machine.
func TestNodeOverviewHidesLoadWhenAbsent(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{Version: "v1.13.4", HasUptime: true, Uptime: 3600}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1")
	if strings.Contains(body, "Load average") {
		t.Error("load row rendered for a node with no load reading")
	}
}

// The overview refreshes itself every 5s by re-rendering overview-body. Any
// dataset the full page sets but the partial forgets vanishes on the first
// refresh — the page looks right until you leave it open for five seconds.
func TestOverviewPartialRendersEverythingTheFullPageDoes(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true, Stage: "running", Ready: true,
			Services: []snapshot.Service{{ID: "etcd", Running: true, Healthy: true}},
		}
		n.TimeSync = &snapshot.TimeSync{HasStatus: true, Synced: true}
		n.Identity = &snapshot.Identity{Schematic: "65cf8364cd0de4cf7b85"}
	})
	h := newHandler(t, true, true, nil)

	_, full := do(t, h, "/")
	_, partial := do(t, h, "/partials/overview")

	for _, want := range []string{"System status", "running", "1 / 1", "synced", "Images"} {
		if !strings.Contains(full, want) {
			t.Errorf("overview page missing %q", want)
		}
		if !strings.Contains(partial, want) {
			t.Errorf("overview PARTIAL missing %q — it will vanish on the first refresh", want)
		}
	}
}

// attentionCard returns just the attention card's markup, so an assertion
// about what the card says cannot be satisfied by the tables below it.
func attentionCard(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "Needs attention")
	if i < 0 {
		t.Fatal("no attention card on the overview")
	}
	// The fleet tiles follow the card; anything past that is another section
	// and must not satisfy an assertion about what the card says.
	j := strings.Index(body[i:], `<div class="stats">`)
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}

// The attention card is the first thing on the dashboard, so a rule that fires
// on a healthy cluster makes the whole card worthless. `dashboard` and every
// `ext-*` service report healthy=false with unknown=true permanently, and the
// service rule must not count them.
func TestAttentionIgnoresServicesWithNoHealthCheck(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true, Stage: "running", Ready: true,
			Services: []snapshot.Service{
				{ID: "etcd", Running: true, Healthy: true},
				{ID: "dashboard", Running: true, Unknown: true},
				{ID: "ext-iscsid", Running: true, Unknown: true},
			},
		}
		n.TimeSync = &snapshot.TimeSync{HasStatus: true, Synced: true}
	})
	up := func(string) (string, bool) { return "v1.13.4", true }
	h := newHandler(t, true, true, up)
	_, body := do(t, h, "/")

	// Scope the assertion to the attention card: the system-status matrix
	// below it legitimately shows a "not healthy" count per node.
	if strings.Contains(attentionCard(t, body), "not healthy") {
		t.Error("attention fired on services that publish no health check")
	}
	checkBody(t, body, "all clear")
}

func TestAttentionFiresOnRealFaults(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true, Stage: "booting", Ready: false,
			Services: []snapshot.Service{
				{ID: "etcd", Running: true, Healthy: false},     // failing
				{ID: "kubelet", Running: false},                 // stopped
				{ID: "dashboard", Running: true, Unknown: true}, // not a fault
			},
			Diagnostics: []snapshot.Diagnostic{{ID: "address-overlap", Message: "overlap"}},
		}
		n.TimeSync = &snapshot.TimeSync{HasStatus: true, Synced: false}
		n.Volumes = &snapshot.Volumes{Volumes: []snapshot.Volume{
			{ID: "EPHEMERAL", Type: "partition", Phase: "failed", Ready: false},
		}}
	})
	up := func(string) (string, bool) { return "v1.13.4", true }
	h := newHandler(t, true, true, up)
	_, body := do(t, h, "/")

	checkBody(t, body,
		"2 services not healthy",
		"machine not ready at stage booting",
		"clock not synced",
		"1 volume not ready",
		"1 diagnostic warning raised")
}

// "Where is the CPU time going" is the first question a busy node raises, and
// it was invisible while only the opt-in per-mode counter carried it.
func TestCPUPageShowsModeBreakdown(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.CPU = &snapshot.CPU{HasUsage: true, UsagePct: 42, Cores: []snapshot.Core{
			{Index: 0, HasUsage: true, UsagePct: 42},
		}, Modes: []snapshot.ModeShare{
			{Mode: "user", Ratio: 0.30},
			{Mode: "system", Ratio: 0.10},
			{Mode: "iowait", Ratio: 0.02},
			{Mode: "idle", Ratio: 0.58},
			{Mode: "steal", Ratio: 0.0001}, // below the render floor
		}}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/cpu")

	checkBody(t, body, "Where the time goes", "user", "30.0%", "iowait", "2.0%", "idle")
	// iowait/steal/irq are "waiting on something else" and must not read as
	// the node's own work.
	if !strings.Contains(body, "mode-wait") {
		t.Error("iowait not toned as wait time")
	}
	// A mode under 0.1% is noise on a bar and must be dropped, not rendered
	// as a zero-width segment. Match the segment markup, not the word: the
	// card's own footnote names steal as an example.
	if strings.Contains(body, `title="steal`) {
		t.Error("a 0.01% mode was rendered; it should be below the floor")
	}
}

// On the first scrape after a restart there is no interval to divide by, so
// the breakdown must be absent rather than showing a boot-to-now average as
// if it were current.
func TestCPUPageHidesModeBreakdownWithoutAnInterval(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.CPU = &snapshot.CPU{HasUsage: false, Cores: []snapshot.Core{{Index: 0}}}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/cpu")
	if strings.Contains(body, "Where the time goes") {
		t.Error("mode breakdown rendered with no interval to derive it from")
	}
}

// The links page collapsed error and drop counters into a word ("Issues") and
// never showed a number, and never rendered MTU or MAC at all.
func TestLinksPageShowsCountersAndSummary(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Links = []snapshot.Link{
			{Name: "eth0", Up: true, Carrier: true, SpeedMbit: 1000, MTUBytes: 1500,
				HWAddr: "aa:bb:cc:dd:ee:ff", Driver: "igb", BusPath: "0000:01:00.0",
				HasCounters: true, HasRates: true,
				RxBytes: 5 << 30, TxBytes: 2 << 30, RxPerSecond: 1 << 20, TxPerSecond: 1 << 19,
				RxErrors: 7, TxDropped: 3},
			{Name: "lo", Up: true, HasCounters: true, RxBytes: 1 << 20, TxBytes: 1 << 20},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/links")

	// Summary strip.
	checkBody(t, body, "Interfaces", "2", "Errors &amp; drops", "1")
	// Counters table with real numbers, not just a word.
	checkBody(t, body, "Counters", "5.0 GiB", "2.0 GiB", "aa:bb:cc:dd:ee:ff", "1500")
	// The empty chart card left by the uPlot removal must be gone.
	if strings.Contains(body, "refreshes every 2.5 s") {
		t.Error("the orphaned throughput card survived")
	}
}

// The Busiest node tile reported one node's CPU% next to a DIFFERENT node's
// load average — two measures, two nodes, one tile. The load shown must belong
// to the node the tile names.
func TestBusiestTileReportsOneNode(t *testing.T) {
	up := func(string) (string, bool) { return "v1.13.4", true }
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.CPU = &snapshot.CPU{HasUsage: true, UsagePct: 55}
		n.Runtime = &snapshot.Runtime{HasUptime: true, Uptime: 60, HasLoad: true, Load1: 1.25}
	})
	h := newHandler(t, true, true, up)
	_, body := do(t, h, "/")

	i := strings.Index(body, "Busiest node")
	if i < 0 {
		t.Fatal("no busiest-node tile")
	}
	tile := body[i:min(i+400, len(body))]
	checkBody(t, tile, "55.0%", "node-1", "load 1.25")
	// The fleet-maximum load of some other node must not appear here.
	if strings.Contains(tile, " on node-") {
		t.Error("the tile still names a second node")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
