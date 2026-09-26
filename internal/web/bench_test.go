package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/web"
)

// synthSnap builds a store shaped like a real cluster: per node one socket,
// six disks, thirty PCI devices, ten sensors and per-thread CPU state.
//
// These benchmarks exist to keep the 4d inversion from regressing. Before it,
// every page re-parsed the whole exposition format per node, so the dashboard
// went from 0.9 ms at 4 nodes to 111 ms at 100 nodes x 64 threads — series grew
// 69x while render time grew 122x. Reading the typed snapshot instead:
//
//	                4 nodes    20 nodes   100 nodes
//	dashboard       0.25 ms    0.63 ms    2.24 ms
//	node page       0.09 ms    0.18 ms    0.64 ms
//
// If a future change reintroduces a per-node scan of something cluster-wide,
// the 100-node numbers are where it will show up first.
func synthSnap(nodeCount, threads int) (*snapshot.Store, []nodes.Node) {
	s := snapshot.New()
	var list []nodes.Node
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("node-%03d", i)
		list = append(list, nodes.Node{Name: name, IP: fmt.Sprintf("10.0.%d.%d", i/250, i%250),
			KubeVersion: "v1.35.0", Roles: []string{"worker"}})
		cores := make([]snapshot.Core, threads)
		for c := range cores {
			cores[c] = snapshot.Core{Index: c, HasUsage: true, UsagePct: 30,
				CurrentHz: 2.4e9, MinimumHz: 8e8, MaximumHz: 4e9, Governor: "powersave"}
		}
		disks := make([]snapshot.Disk, 6)
		for d := range disks {
			disks[d] = snapshot.Disk{Device: fmt.Sprintf("/dev/sd%c", 'a'+d),
				Transport: "sata", Attachment: "local", SizeBytes: 1e12}
		}
		pci := make([]snapshot.PCIDevice, 30)
		for d := range pci {
			pci[d] = snapshot.PCIDevice{BDF: fmt.Sprintf("0000:%02x:00.0", d),
				Class: "Class", Vendor: "V", Product: "P", Driver: "drv"}
		}
		sensors := make([]snapshot.Sensor, 10)
		for d := range sensors {
			sensors[d] = snapshot.Sensor{Chip: "/sys/x", ChipName: "coretemp",
				Sensor: fmt.Sprintf("temp%d", d), Kind: "temperature", Value: 45}
		}
		s.Update(name, func(n *snapshot.Node) {
			n.Runtime = &snapshot.Runtime{Version: "v1.13.4", Uptime: 90061, HasUptime: true}
			n.CPU = &snapshot.CPU{HasUsage: true, UsagePct: 42, Cores: cores}
			n.Sockets = []snapshot.Socket{{ID: "CPU0", Product: "Xeon", Cores: threads / 2, Threads: threads}}
			n.Modules = []snapshot.Module{{Slot: "A1", SizeBytes: 34359738368, SpeedTransfer: 3200e6}}
			n.Disks, n.PCI, n.Sensors = disks, pci, sensors
			n.Memory = &snapshot.Memory{Present: true, Total: 34359738368, Free: 8e9, Available: 1e10}
		})
	}
	return s, list
}

func benchPage(b *testing.B, nodeCount, threads int, path string) {
	snap, list := synthSnap(nodeCount, threads)
	h := web.NewRouter(discardLog, prometheus.NewRegistry(), func() bool { return true },
		func() []nodes.Node { return list },
		func(string) (string, bool) { return "v1.13.4", true },
		history.New(10*time.Minute), snap, testVolumes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("%s: status %d", path, rec.Code)
		}
	}
}

func BenchmarkDashboard4(b *testing.B)   { benchPage(b, 4, 16, "/") }
func BenchmarkDashboard20(b *testing.B)  { benchPage(b, 20, 32, "/") }
func BenchmarkDashboard100(b *testing.B) { benchPage(b, 100, 64, "/") }
func BenchmarkNodePage4(b *testing.B)    { benchPage(b, 4, 16, "/nodes/node-000/cpu") }
func BenchmarkNodePage20(b *testing.B)   { benchPage(b, 20, 32, "/nodes/node-000/cpu") }
func BenchmarkNodePage100(b *testing.B)  { benchPage(b, 100, 64, "/nodes/node-000/cpu") }
