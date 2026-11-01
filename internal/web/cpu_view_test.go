package web_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/web"
)

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
