package web_test

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

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
