package cpu

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

// fakeAPI is a canned machineAPI.
type fakeAPI struct {
	sys     *machinepb.SystemStatResponse
	freq    *machinepb.CPUFreqStatsResponse
	err     error
	files   map[string]string // path → content
	errRead map[string]error  // path → read error
	reads   map[string]int    // path → read count (nil = don't count)
}

func (f fakeAPI) SystemStat(context.Context) (*machinepb.SystemStatResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sys, nil
}

func (f fakeAPI) CPUFreqStats(context.Context) (*machinepb.CPUFreqStatsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.freq, nil
}

// fakeReadMu guards the reads counter: the per-core frequency pass reads
// concurrently, so an unguarded map would race under -race.
var fakeReadMu sync.Mutex

func (f fakeAPI) Read(_ context.Context, path string) (io.ReadCloser, error) {
	if f.reads != nil {
		fakeReadMu.Lock()
		f.reads[path]++
		fakeReadMu.Unlock()
	}
	if f.err != nil {
		return nil, f.err
	}
	if e := f.errRead[path]; e != nil {
		return nil, e
	}
	if v, ok := f.files[path]; ok {
		return io.NopCloser(strings.NewReader(v)), nil
	}
	return nil, os.ErrNotExist
}

func testNode() *collector.NodeClient {
	return &collector.NodeClient{Node: nodes.Node{Name: "node-a", IP: "10.0.0.1"}}
}

func testCollector(api machineAPI) *Collector {
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 0, false)
	c.newAPI = func(*collector.NodeClient) machineAPI { return api }
	return c
}

// testCollectorWithModeSeconds is testCollector with the opt-in jiffies export on.
func testCollectorWithModeSeconds(api machineAPI) *Collector {
	c := testCollector(api)
	c.modeSeconds = true
	return c
}

func cores(user, idle float64) []*machinepb.CPUStat {
	return []*machinepb.CPUStat{{User: user, Idle: idle}, {User: user, Idle: idle}}
}

func statResponse(cores []*machinepb.CPUStat) *machinepb.SystemStatResponse {
	return &machinepb.SystemStatResponse{Messages: []*machinepb.SystemStat{{
		BootTime: 1000,
		Cpu:      cores,
	}}}
}

// zeroFreqResponse is a CPUFreqStats response where every core came back
// empty — the shape returned on kernels without cpuinfo_cur_freq.
func zeroFreqResponse(n int) *machinepb.CPUFreqStatsResponse {
	cores := make([]*machinepb.CPUFreqStats, n)
	for i := range cores {
		cores[i] = &machinepb.CPUFreqStats{}
	}
	return &machinepb.CPUFreqStatsResponse{Messages: []*machinepb.CPUsFreqStats{{
		CpuFreqStats: cores,
	}}}
}

func freqResponse(n int) *machinepb.CPUFreqStatsResponse {
	cores := make([]*machinepb.CPUFreqStats, n)
	for i := range cores {
		cores[i] = &machinepb.CPUFreqStats{
			CurrentFrequency: 3500000,
			MinimumFrequency: 400000,
			MaximumFrequency: 5000000,
			Governor:         "performance",
		}
	}
	return &machinepb.CPUFreqStatsResponse{Messages: []*machinepb.CPUsFreqStats{{
		CpuFreqStats: cores,
	}}}
}

// findMetric looks up one metric value by family and label match.
func findMetric(fams []*dto.MetricFamily, family string, want map[string]string) (float64, bool) {
	for _, f := range fams {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			ok := true
			for k, v := range want {
				if labels[k] != v {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue(), true
			}
			return 0, true
		}
	}
	return 0, false
}

func countFamily(fams []*dto.MetricFamily, family string) int {
	n := 0
	for _, f := range fams {
		if f.GetName() == family {
			n += len(f.GetMetric())
		}
	}
	return n
}

func collect(t *testing.T, c *Collector, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return fams
}

func TestCollectFirstScrape(t *testing.T) {
	c := testCollectorWithModeSeconds(fakeAPI{sys: statResponse(cores(100, 900)), freq: freqResponse(2)})
	fams := collect(t, c, prometheus.NewRegistry())

	if v, ok := findMetric(fams, "talos_node_cpu_seconds_total", map[string]string{
		"node": "node-a", "cpu": "0", "mode": "user",
	}); !ok || v != 100 {
		t.Errorf("cpu seconds user: got %v %v, want 100", v, ok)
	}
	if n := countFamily(fams, "talos_node_cpu_seconds_total"); n != 2*10 {
		t.Errorf("cpu seconds: got %d series, want 20 (2 cores x 10 modes)", n)
	}
	if v, ok := findMetric(fams, "talos_node_cpu_freq_hertz", map[string]string{
		"node": "node-a", "cpu": "1",
	}); !ok || v != 3500000*1e3 {
		t.Errorf("freq current: got %v %v, want 3.5e9 Hz", v, ok)
	}
	if n := countFamily(fams, "talos_node_cpu_usage_ratio"); n != 0 {
		t.Errorf("usage: got %d series on first scrape, want 0 (no previous sample)", n)
	}
}

func TestCollectUsageRatio(t *testing.T) {
	c := testCollector(fakeAPI{sys: statResponse(cores(100, 900)), freq: freqResponse(2)})

	// First scrape primes the delta; second bumps core0 +1000 busy and
	// core1 +100 idle → per-core 1.0 / 0.0, all 1000/1100. The metric is a
	// ratio 0-1, not a percentage.
	collect(t, c, prometheus.NewRegistry())
	collect(t, c, prometheus.NewRegistry())

	api := fakeAPI{
		sys:  statResponse([]*machinepb.CPUStat{{User: 1100, Idle: 900}, {User: 100, Idle: 1000}}),
		freq: freqResponse(2),
	}
	c.newAPI = func(*collector.NodeClient) machineAPI { return api }
	fams := collect(t, c, prometheus.NewRegistry())

	if v, ok := findMetric(fams, "talos_node_cpu_usage_ratio", map[string]string{"node": "node-a", "cpu": "0"}); !ok || v != 1 {
		t.Errorf("usage core0: got %v %v, want 1.0", v, ok)
	}
	if v, ok := findMetric(fams, "talos_node_cpu_usage_ratio", map[string]string{"node": "node-a", "cpu": "1"}); !ok || v != 0 {
		t.Errorf("usage core1: got %v %v, want 0", v, ok)
	}
	if v, ok := findMetric(fams, "talos_node_cpu_usage_ratio", map[string]string{"node": "node-a", "cpu": "all"}); !ok ||
		v < 0.90909 || v > 0.90910 {
		t.Errorf("usage all: got %v, want ~0.909", v)
	}
}

func TestCollectCoreCountChangeDropsPrev(t *testing.T) {
	c := testCollector(fakeAPI{sys: statResponse(cores(100, 900)), freq: freqResponse(2)})
	collect(t, c, prometheus.NewRegistry())

	api := fakeAPI{
		sys:  statResponse([]*machinepb.CPUStat{{User: 500, Idle: 100}}),
		freq: freqResponse(1),
	}
	c.newAPI = func(*collector.NodeClient) machineAPI { return api }
	fams := collect(t, c, prometheus.NewRegistry())
	if n := countFamily(fams, "talos_node_cpu_usage_ratio"); n != 0 {
		t.Errorf("usage after core count change: got %d series, want 0", n)
	}
}

func TestCollectSystemStatError(t *testing.T) {
	c := testCollector(fakeAPI{err: errors.New("boom")})
	err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want wrapped 'boom'", err)
	}
}

func TestCollectEmptySystemStat(t *testing.T) {
	c := testCollector(fakeAPI{sys: &machinepb.SystemStatResponse{}, freq: freqResponse(2)})
	err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "system stat response is empty") {
		t.Fatalf("err = %v, want 'system stat response is empty'", err)
	}
}

const cpuinfoFixture = "processor\t: 0\nmodel name\t: AMD\ncpu MHz\t\t: 2354.464\n\ncache size\t: 512 KB\n\nprocessor\t: 1\ncpu MHz\t\t: 1450.000\n"

func TestCollectFreqFallbackFromFile(t *testing.T) {
	api := fakeAPI{
		sys:  statResponse(cores(100, 900)),
		freq: zeroFreqResponse(2),
		files: map[string]string{
			"/proc/cpuinfo": cpuinfoFixture,
			// cpu0: min/max/governor from sysfs; current comes from
			// /proc/cpuinfo, so the sysfs cur value (2613116) must be
			// ignored in favor of 2354464.
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq": "2613116",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq": "1095838",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq": "4787082",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor": "powersave",
			// cpu1 has no cpufreq files at all → only the cpuinfo cur.
		},
	}
	c := testCollector(api)
	fams := collect(t, c, prometheus.NewRegistry())

	// Live reading and policy bounds are separate families now.
	want := map[string]float64{
		"talos_node_cpu_freq_hertz":     2354464e3,
		"talos_node_cpu_freq_min_hertz": 1095838e3,
		"talos_node_cpu_freq_max_hertz": 4787082e3,
	}
	for metric, v := range want {
		if got, ok := findMetric(fams, metric, map[string]string{
			"node": "node-a", "cpu": "0",
		}); !ok || got != v {
			t.Errorf("core0 %s: got %v %v, want %v", metric, got, ok, v)
		}
	}
	// cpu1: current from /proc/cpuinfo, no min/max/governor.
	if got, ok := findMetric(fams, "talos_node_cpu_freq_hertz", map[string]string{
		"node": "node-a", "cpu": "1",
	}); !ok || got != 1450000*1e3 {
		t.Errorf("freq core1 current: got %v %v, want 1.45e9 Hz (cpuinfo)", got, ok)
	}
	if got, ok := findMetric(fams, "talos_node_cpu_freq_min_hertz", map[string]string{
		"node": "node-a", "cpu": "1",
	}); !ok || got != 0 {
		t.Errorf("freq core1 minimum: got %v %v, want 0 (no cpufreq files)", got, ok)
	}
}

func TestCollectFreqFallbackSlowCacheThrottle(t *testing.T) {
	api := fakeAPI{
		sys:  statResponse(cores(100, 900)),
		freq: zeroFreqResponse(2),
		files: map[string]string{
			"/proc/cpuinfo": cpuinfoFixture,
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq": "2613116",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq": "1095838",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq": "4787082",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor": "powersave",
		},
		reads: map[string]int{},
	}
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 0, false)
	c.newAPI = func(*collector.NodeClient) machineAPI { return api }

	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) { c.now = func() time.Time { return t0.Add(d) } }

	at(0)
	collect(t, c, prometheus.NewRegistry())
	// cpuinfo: 1 read; sysfs: 4 files for the one readable core (cpu0),
	// cpu1 has none.
	if api.reads["/proc/cpuinfo"] != 1 || api.reads["/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq"] != 1 {
		t.Fatalf("first tick reads: %v, want cpuinfo=1 min=1", api.reads)
	}

	// Within freqSlowTTL: only /proc/cpuinfo is re-read, and the cached
	// min/max/governor must keep being served (regression: the fresh-cache
	// path used to drop them).
	api.files["/proc/cpuinfo"] = strings.Replace(cpuinfoFixture, "2354.464", "2900.000", 1)
	at(time.Minute)
	fams := collect(t, c, prometheus.NewRegistry())
	if api.reads["/proc/cpuinfo"] != 2 || api.reads["/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq"] != 1 {
		t.Fatalf("tick within cache: %v, want cpuinfo=2 min=1 (sysfs cached)", api.reads)
	}
	if got, ok := findMetric(fams, "talos_node_cpu_freq_hertz", map[string]string{
		"node": "node-a", "cpu": "0",
	}); !ok || got != 2900000*1e3 {
		t.Errorf("tick 2 current: got %v %v, want 2.9e9 Hz (fresh cpuinfo)", got, ok)
	}
	if got, ok := findMetric(fams, "talos_node_cpu_freq_min_hertz", map[string]string{
		"node": "node-a", "cpu": "0",
	}); !ok || got != 1095838*1e3 {
		t.Errorf("tick 2 minimum: got %v %v, want 1.095838e9 Hz (cached sysfs)", got, ok)
	}

	at(6 * time.Minute)
	collect(t, c, prometheus.NewRegistry())
	// Past freqSlowTTL: the sysfs pass runs again.
	if api.reads["/proc/cpuinfo"] != 3 || api.reads["/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq"] != 2 {
		t.Fatalf("tick after expiry: %v, want cpuinfo=3 min=2", api.reads)
	}
}

func TestCollectFreqFallbackCpuinfoMissing(t *testing.T) {
	// No /proc/cpuinfo → the current frequency degrades to the slow
	// sysfs value instead of zero.
	api := fakeAPI{
		sys:  statResponse(cores(100, 900)),
		freq: zeroFreqResponse(2),
		files: map[string]string{
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq": "2613116",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq": "1095838",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq": "4787082",
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor": "powersave",
		},
	}
	c := testCollector(api)
	fams := collect(t, c, prometheus.NewRegistry())

	if got, ok := findMetric(fams, "talos_node_cpu_freq_hertz", map[string]string{
		"node": "node-a", "cpu": "0",
	}); !ok || got != 2613116*1e3 {
		t.Errorf("freq core0 current without cpuinfo: got %v %v, want 2.613116e9 Hz (sysfs)", got, ok)
	}
}

func TestCollectFreqFallbackPermissionDenied(t *testing.T) {
	var logBuf bytes.Buffer
	c := New(slog.New(slog.NewTextHandler(&logBuf, nil)), nil, 0, false)
	api := fakeAPI{
		sys:  statResponse(cores(100, 900)),
		freq: zeroFreqResponse(2),
		errRead: map[string]error{
			"/proc/cpuinfo": status.Error(codes.PermissionDenied, "permission denied"),
			"/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq": status.Error(codes.PermissionDenied, "permission denied"),
		},
	}
	c.newAPI = func(*collector.NodeClient) machineAPI { return api }
	reg := prometheus.NewRegistry()

	// Permission error must not fail the scrape; the RPC data still lands
	// and the freq stays empty (the pre-fallback behavior).
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect with permission-denied fallback: got error %v, want nil", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if v, ok := findMetric(fams, "talos_node_cpu_freq_hertz", map[string]string{
		"node": "node-a", "cpu": "0",
	}); !ok || v != 0 {
		t.Errorf("freq after permission error: got %v %v, want 0", v, ok)
	}

	// Second scrape: no error, and the warning was logged exactly once.
	if err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry()); err != nil {
		t.Fatalf("second Collect: got error %v, want nil", err)
	}
	if n := strings.Count(logBuf.String(), "fallback disabled"); n != 1 {
		t.Errorf("expected exactly one fallback warning across two scrapes, got %d\nlog:\n%s", n, logBuf.String())
	}
}

func TestCollectAppendsFrequencyHistory(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hist := history.New(time.Minute)
	c := New(log, hist, 0, false)

	// Pin the collector clock (started at the real now, so samples stay
	// inside the store's 1-minute window).
	var now time.Time
	now = time.Now()
	c.now = func() time.Time { return now }

	c.newAPI = func(*collector.NodeClient) machineAPI {
		return fakeAPI{sys: statResponse(cores(100, 900)), freq: freqResponse(2)}
	}
	// Scrape 1: primes the delta (no usage), one freq sample per core.
	collect(t, c, prometheus.NewRegistry())
	now = now.Add(5 * time.Second)

	// Scrape 2: core0 +1000 busy, core1 +100 idle → 100% / 0%, all 1000/1100.
	c.newAPI = func(*collector.NodeClient) machineAPI {
		return fakeAPI{
			sys:  statResponse([]*machinepb.CPUStat{{User: 1100, Idle: 900}, {User: 100, Idle: 1000}}),
			freq: freqResponse(2),
		}
	}
	collect(t, c, prometheus.NewRegistry())
	now = now.Add(5 * time.Second)

	// Scrape 3: jiffies unchanged → no usage samples, one more freq sample.
	collect(t, c, prometheus.NewRegistry())

	pts, ok := hist.Points("node-a")
	if !ok {
		t.Fatal("history: ok=false, want data")
	}
	// Usage history went with the uPlot panels: nothing reads it now, and the
	// usage percentages are still exported as metrics. Only the frequency
	// samples survive, because the CPU page reports an observed range over
	// the window instead of a single scrape-biased reading.
	for _, gone := range []string{"usage.all", "usage.0", "usage.1"} {
		if len(pts[gone]) != 0 {
			t.Errorf("%s: %d points, want none — usage history was removed with the charts", gone, len(pts[gone]))
		}
	}
	if got := len(pts["freq.current.0"]); got != 3 {
		t.Fatalf("freq.current.0: got %d points, want 3", got)
	}
	// History carries the same unit as the metric: hertz.
	if v := pts["freq.current.0"][0].V; v != 3500000*1e3 {
		t.Errorf("freq.current.0: got %v, want 3.5e9 Hz", v)
	}
	// Freq at all three scrapes, oldest-first.
	if pts["freq.current.0"][2].T <= pts["freq.current.0"][1].T {
		t.Errorf("freq timestamps not increasing: %v <= %v", pts["freq.current.0"][2].T, pts["freq.current.0"][1].T)
	}
}

// freqProbeAPI builds a node whose CPUFreqStats is empty (so the file fallback
// runs) with the given cpufreq driver and two cores.
func freqProbeAPI(driver string, reads map[string]int) fakeAPI {
	files := map[string]string{"/proc/cpuinfo": cpuinfoFixture}
	if driver != "" {
		files["/sys/devices/system/cpu/cpu0/cpufreq/scaling_driver"] = driver
	}
	for _, core := range []string{"0", "1"} {
		base := "/sys/devices/system/cpu/cpu" + core + "/cpufreq/"
		files[base+"scaling_cur_freq"] = "2613116"
		files[base+"scaling_min_freq"] = "1095838"
		files[base+"scaling_max_freq"] = "4787082"
		files[base+"scaling_governor"] = "powersave"
	}
	return fakeAPI{sys: statResponse(cores(100, 900)), freq: zeroFreqResponse(2), files: files, reads: reads}
}

// On intel_pstate /proc/cpuinfo reports a shared P-state rather than per-core
// values, so the live frequency must come from one scaling_cur_freq read per
// core instead.
func TestFreqSourcePerCoreOnIntelPstate(t *testing.T) {
	reads := map[string]int{}
	c := testCollector(freqProbeAPI("intel_pstate", reads))
	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return t0 }
	collect(t, c, prometheus.NewRegistry())
	c.now = func() time.Time { return t0.Add(10 * time.Second) }
	collect(t, c, prometheus.NewRegistry())

	if n := reads["/proc/cpuinfo"]; n != 0 {
		t.Errorf("/proc/cpuinfo read %d times on intel_pstate, want 0 (it is not per-core there)", n)
	}
	// One read in the cached slow pass, then one per tick for the live value.
	for _, core := range []string{"0", "1"} {
		p := "/sys/devices/system/cpu/cpu" + core + "/cpufreq/scaling_cur_freq"
		if reads[p] < 3 {
			t.Errorf("cpu%s scaling_cur_freq read %d times over 2 ticks, want >= 3", core, reads[p])
		}
	}
	// The driver is probed once and cached.
	if n := reads[driverPath]; n != 1 {
		t.Errorf("scaling_driver probed %d times, want 1 (cached per node)", n)
	}
}

// amd-pstate derives per-core values in /proc/cpuinfo, so the single cheap read
// stays: no per-core pass beyond the cached slow one.
func TestFreqSourceCPUInfoOnAMDPstate(t *testing.T) {
	reads := map[string]int{}
	c := testCollector(freqProbeAPI("amd-pstate-epp", reads))
	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return t0 }
	collect(t, c, prometheus.NewRegistry())
	c.now = func() time.Time { return t0.Add(10 * time.Second) }
	collect(t, c, prometheus.NewRegistry())

	if n := reads["/proc/cpuinfo"]; n != 2 {
		t.Errorf("/proc/cpuinfo read %d times over 2 ticks, want 2", n)
	}
	for _, core := range []string{"0", "1"} {
		p := "/sys/devices/system/cpu/cpu" + core + "/cpufreq/scaling_cur_freq"
		if reads[p] != 1 {
			t.Errorf("cpu%s scaling_cur_freq read %d times, want 1 (slow pass only)", core, reads[p])
		}
	}
}

// An unreadable driver file keeps the cheap path: that is the pre-existing
// behaviour and costs one read per pass rather than one per core.
func TestFreqSourceDefaultsToCPUInfoWhenDriverUnknown(t *testing.T) {
	reads := map[string]int{}
	c := testCollector(freqProbeAPI("", reads))
	collect(t, c, prometheus.NewRegistry())
	if n := reads["/proc/cpuinfo"]; n != 1 {
		t.Errorf("/proc/cpuinfo read %d times with no driver file, want 1", n)
	}
}

// freqInterval throttles the per-core pass without slowing usage sampling.
func TestFreqIntervalThrottlesPerCoreReads(t *testing.T) {
	reads := map[string]int{}
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, time.Minute, false)
	api := freqProbeAPI("intel_pstate", reads)
	c.newAPI = func(*collector.NodeClient) machineAPI { return api }

	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	for _, d := range []time.Duration{0, 5 * time.Second, 10 * time.Second} {
		c.now = func() time.Time { return t0.Add(d) }
		collect(t, c, prometheus.NewRegistry())
	}
	p := "/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq"
	// 1 slow pass + 1 live pass; ticks 2 and 3 are inside freqInterval.
	if reads[p] != 2 {
		t.Errorf("scaling_cur_freq read %d times across 3 ticks with a 1m interval, want 2", reads[p])
	}
}

// TestModeSecondsAreOptIn: the per-core, per-mode counters are 400 of 2200
// series on a 4-node cluster and nothing in the UI reads them, so they are off
// unless asked for. The usage ratio derived from the same counters must still
// be exported.
func TestModeSecondsAreOptIn(t *testing.T) {
	api := fakeAPI{sys: statResponse(cores(100, 900)), freq: freqResponse(2)}

	off := testCollector(api)
	collect(t, off, prometheus.NewRegistry()) // first scrape: no previous sample
	// Advance the counters so the second scrape has a usage delta.
	busier := fakeAPI{sys: statResponse(cores(200, 1800)), freq: freqResponse(2)}
	off.newAPI = func(*collector.NodeClient) machineAPI { return busier }
	fams := collect(t, off, prometheus.NewRegistry())
	if n := countFamily(fams, "talos_node_cpu_seconds_total"); n != 0 {
		t.Errorf("mode seconds off: got %d series, want 0", n)
	}
	if n := countFamily(fams, "talos_node_cpu_usage_ratio"); n == 0 {
		t.Error("usage percent must still be exported when jiffies are off")
	}

	on := testCollectorWithModeSeconds(api)
	fams = collect(t, on, prometheus.NewRegistry())
	if n := countFamily(fams, "talos_node_cpu_seconds_total"); n != 2*10 {
		t.Errorf("mode seconds on: got %d series, want 20 (2 cores x 10 modes)", n)
	}
}
