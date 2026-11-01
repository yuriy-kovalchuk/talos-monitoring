package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

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

func TestNodesPage(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "node-1", "10.0.0.10", "linux / arm64", "v1.35.0")
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
	checkBody(t, tile, ">55.0<", "node-1", "load 1.25")
	// The fleet-maximum load of some other node must not appear here.
	if strings.Contains(tile, " on node-") {
		t.Error("the tile still names a second node")
	}
}
