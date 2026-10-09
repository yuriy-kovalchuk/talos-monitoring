// Package cpu collects the fast-changing per-node CPU data from the Talos
// machine service (SystemStat CPU time and CPUFreqStats) and exposes it as
// Prometheus metrics.
//
// It is the fastest collector in the registry (default TTL 5s): CPU usage
// gauges are computed as deltas between consecutive scrapes (no previous
// sample → no usage series) and the per-core frequency comes from the
// CPUFreqStats RPC, with a file fallback for kernels where the upstream
// handler returns zeros (missing legacy cpuinfo_cur_freq file — the case on
// both amd_pstate and intel_pstate).
//
// The fallback picks its source per node from the cpufreq driver: one
// /proc/cpuinfo read where that file is per-core, or one scaling_cur_freq read
// per core (concurrent) on drivers where it is not. See freqSource.
package cpu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/sysstat"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the registry name of the cpu collector.
const Name = "cpu"

// seriesFreqCurrent is the only history series still written. Per-core usage
// used to be recorded here too, for the removed usage chart; nothing reads it
// now, and the frequency samples survive because the CPU page reports an
// observed range over the window rather than a single biased reading.
const seriesFreqCurrent = "freq.current." // + core index

// defaultTTL is the natural refresh period of CPU usage and live frequency.
const defaultTTL = 5 * time.Second

// machineAPI is the subset of the per-node Talos client used by the
// collector (seam for tests).
type machineAPI interface {
	SystemStat(ctx context.Context) (*machinepb.SystemStatResponse, error)
	CPUFreqStats(ctx context.Context) (*machinepb.CPUFreqStatsResponse, error)
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// talosAPI adapts the per-node machinery client to machineAPI.
type talosAPI struct {
	client *talosclient.Client
}

func (t talosAPI) SystemStat(ctx context.Context) (*machinepb.SystemStatResponse, error) {
	return t.client.MachineClient.SystemStat(ctx, &empty.Empty{})
}

func (t talosAPI) CPUFreqStats(ctx context.Context) (*machinepb.CPUFreqStatsResponse, error) {
	return t.client.MachineClient.CPUFreqStats(ctx, &empty.Empty{})
}

func (t talosAPI) Read(ctx context.Context, path string) (io.ReadCloser, error) {
	return t.client.Read(ctx, path)
}

// cpuSample is the previous scrape's per-core CPU time, used to compute
// CPU usage deltas.
type cpuSample struct {
	totals []float64 // all modes summed, per core
	idles  []float64 // idle+iowait, per core

	// modes is each /proc/stat mode summed across every core. Node-level
	// rather than per-core: the breakdown answers "where is this node's CPU
	// time going", and per-core would be 10 values per thread for a question
	// nobody asks per thread.
	modes map[string]float64
}

// Collector implements collector.Collector for fast per-node CPU data.
type Collector struct {
	newAPI func(node *collector.NodeClient) machineAPI
	log    *slog.Logger
	now    func() time.Time // overridable in tests

	mu        sync.Mutex
	prev      map[string]cpuSample     // keyed by node name
	freqCache map[string]freqCacheData // keyed by node name

	// freqInterval throttles the per-core current-frequency pass (0 = every
	// collector tick). Only meaningful on nodes using the per-core source.
	freqInterval time.Duration

	// hist receives one sample per scrape for the dashboard graph
	// (nil = history disabled, e.g. in tests that do not check it).
	hist *history.Store

	// snap receives the typed live state alongside the metrics.
	snap *snapshot.Store

	// sysstat shares the SystemStat response with the nodeapi collector,
	// which wants the boot time and process counts out of the same message.
	sysstat *sysstat.Cache

	// modeSeconds enables talos_node_cpu_seconds_total. Off by default: it is
	// 10 series per thread per node and dominates both the payload and
	// Prometheus cardinality.
	modeSeconds bool

	// fallbackWarned is set once the frequency file fallback hits a
	// permission error (file reads need os:admin); later scrapes stop
	// trying instead of repeating the same failure.
	fallbackWarned atomic.Bool
	// fallbackNoted logs the first fallback use once (debug aid).
	fallbackNoted atomic.Bool
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// WithSysStat shares a SystemStat cache with the other collectors that read it.
func (c *Collector) WithSysStat(s *sysstat.Cache) *Collector {
	c.sysstat = s
	return c
}

// New returns the cpu collector. hist may be nil (history disabled).
// freqInterval throttles the per-core frequency pass (0 = every tick).
func New(log *slog.Logger, hist *history.Store, freqInterval time.Duration, modeSeconds bool) *Collector {
	return &Collector{
		log:         log,
		modeSeconds: modeSeconds,
		newAPI: func(node *collector.NodeClient) machineAPI {
			return talosAPI{client: node.Client}
		},
		now:          time.Now,
		hist:         hist,
		freqInterval: freqInterval,
		prev:         make(map[string]cpuSample),
		freqCache:    make(map[string]freqCacheData),
	}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Live }

// DefaultTTL implements collector.DefaultTTLer.
func (c *Collector) DefaultTTL() time.Duration { return defaultTTL }

// Collect calls the machine service RPCs and registers:
//
//	talos_node_cpu_seconds_total{node,cpu,mode}
//	talos_node_cpu_usage_ratio{node,cpu} (delta since previous scrape)
//	talos_node_cpu_freq_hertz{node,cpu}
//	talos_node_cpu_freq_{min,max}_hertz{node,cpu}
//	talos_node_cpu_governor_info{node,cpu,governor}
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	api := c.newAPI(node)
	name := node.Node.Name

	// Frequency is sampled FIRST, before any other work in this scrape.
	//
	// A scrape is itself load: SystemStat, CPUFreqStats and a sysfs read per
	// core arrive together, and cpufreq governors react within microseconds, so
	// a reading taken at the end of that burst is biased high. Measured against
	// independent concurrent sampling over the same window: +2.9 % on a
	// 16-thread node with headroom, +121 % on a busy 4-thread one (§6.7 0k).
	// Reading first does not eliminate the effect — the RPC that carries the
	// request is already load — but it moves the sample to the quietest point
	// of the scrape instead of the busiest.
	freqs, err := freqStats(ctx, api)
	if err != nil {
		return err
	}

	// Upstream CPUFreqStats returns zeros on kernels without the legacy
	// cpuinfo_cur_freq file (e.g. amd_pstate); when any core is empty, fall
	// back to node files: one /proc/cpuinfo read per tick for the live
	// current frequency, plus a per-core cpufreq sysfs pass cached for
	// freqSlowTTL (min/max/governor change only with a policy change).
	var files []cpufreqFiles
	if nonEmptyFreqs(freqs) < len(freqs) {
		files = c.freqFromFiles(ctx, api, name, len(freqs))
	}

	cores, err := c.systemCores(ctx, name, api)
	if err != nil {
		return err
	}

	if c.modeSeconds {
		registerModeSeconds(name, cores, reg)
	}

	// A ratio, not a percentage: Prometheus convention, and it composes without
	// a stray /100 in every expression.
	//
	// This is a rate computed inside the exporter, over the exporter's own
	// scrape interval rather than a window the querier chooses — normally an
	// antipattern. It stays because the dashboard needs it and the counter it
	// derives from is opt-in, so the UI cannot depend on that counter being
	// there. The HELP says as much so nobody prefers it by accident.
	usage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_usage_ratio",
		Help: "CPU usage as a ratio 0-1 since the previous scrape (cpu=\"all\" is the node total). " +
			"Derived over the exporter's own scrape interval; for querying prefer " +
			"rate(talos_node_cpu_seconds_total{mode=\"idle\"}[window]), which uses your window.",
	}, []string{"node", "cpu"})
	reg.MustRegister(usage)

	// Node-level CPU time by mode, as a share of the interval. This is the
	// always-on companion to the opt-in talos_node_cpu_seconds_total: the UI
	// cannot depend on an opt-in family (§1.1), and "where is the CPU time
	// going" is the first question a busy node raises. Ten series per node.
	modeRatio := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_mode_ratio",
		Help: "Share of the node's CPU time spent in each /proc/stat mode since the previous scrape, 0-1 (the modes sum to 1). " +
			"Derived over the exporter's own scrape interval; for querying prefer " +
			"rate(talos_node_cpu_seconds_total[window]), which uses your window.",
	}, []string{"node", "mode"})
	reg.MustRegister(modeRatio)

	res := c.computeUsage(name, cores, usage, modeRatio)

	freqCur, coreStates := registerFreqFamilies(name, freqs, files, res.byCore, reg)

	if c.snap != nil {
		cpu := snapshot.CPU{HasUsage: res.hasAll, UsagePct: res.allPct, Cores: coreStates, Modes: res.shares}
		c.snap.Update(name, func(n *snapshot.Node) { n.CPU = &cpu })
	}

	if c.hist != nil {
		c.appendHistory(name, freqCur)
	}
	return nil
}

// appendHistory records the scrape's per-core frequency samples. A zero
// frequency (no cpufreq data) is a gap, not a sample.
func (c *Collector) appendHistory(node string, freqCur []float64) {
	now := c.now()
	for i, v := range freqCur {
		if v > 0 {
			c.hist.Append(node, seriesFreqCurrent+strconv.Itoa(i), now, v)
		}
	}
}

// freqStats calls the CPUFreqStats RPC and returns its per-core data, failing
// when the response carries no stats at all.
func freqStats(ctx context.Context, api machineAPI) ([]*machinepb.CPUFreqStats, error) {
	freq, err := api.CPUFreqStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("cpu freq stats: %w", err)
	}
	if len(freq.GetMessages()) == 0 || freq.GetMessages()[0].GetCpuFreqStats() == nil {
		return nil, errors.New("cpu freq stats response is empty")
	}
	return freq.GetMessages()[0].GetCpuFreqStats(), nil
}

// nonEmptyFreqs counts the cores the RPC reported any data for. A core is
// empty when all four of its fields are zero — the signature of kernels
// without the legacy cpuinfo_cur_freq file (see freqFromFiles).
func nonEmptyFreqs(freqs []*machinepb.CPUFreqStats) int {
	n := 0
	for _, fm := range freqs {
		if fm.GetCurrentFrequency() > 0 || fm.GetMinimumFrequency() > 0 ||
			fm.GetMaximumFrequency() > 0 || fm.GetGovernor() != "" {
			n++
		}
	}
	return n
}

// systemCores fetches the per-core CPU time counters through the shared
// SystemStat cache (the nodeapi collector reads boot time and process counts
// from the same message) and returns the response's per-core stats.
func (c *Collector) systemCores(ctx context.Context, name string, api machineAPI) ([]*machinepb.CPUStat, error) {
	sys, err := c.sysstat.Get(ctx, name, api.SystemStat)
	if err != nil {
		return nil, fmt.Errorf("system stat: %w", err)
	}
	if len(sys.GetMessages()) == 0 || sys.GetMessages()[0].GetCpu() == nil {
		return nil, errors.New("system stat response is empty")
	}
	return sys.GetMessages()[0].GetCpu(), nil
}

// registerModeSeconds exposes talos_node_cpu_seconds_total.
func registerModeSeconds(name string, cores []*machinepb.CPUStat, reg prometheus.Registerer) {
	// Per-mode CPU time is opt-in: 10 modes x thread x node is 400 series on a
	// 4-node cluster and ~64 000 at 100 nodes x 64 threads, and nothing in the
	// UI reads it. The usage ratio is derived from the same numbers, so the
	// default export stays useful without it.
	//
	// Talos returns these already divided by USER_HZ, so they are seconds, not
	// jiffies — the metric was misnamed until this was checked against uptime.
	j := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_node_cpu_seconds_total",
		Help: "Cumulative CPU time per core and mode since boot, in seconds. " +
			"Matches node_cpu_seconds_total, so the usual idiom applies: " +
			"100 * (1 - rate(...{mode=\"idle\"}[5m])).",
	}, []string{"node", "cpu", "mode"})
	for i, cs := range cores {
		core := strconv.Itoa(i)
		for mode, val := range cpuStatModes(cs) {
			// Fresh counter per Collect: start at 0, add the cumulative value.
			j.WithLabelValues(name, core, mode).Add(val)
		}
	}
	reg.MustRegister(j)
}

// usageResult is one scrape's usage computation: per-core usage in percent
// (-1 = no usable sample for that core), the node total, and the per-mode
// time shares. hasAll is false when there is no interval to derive usage
// from (first scrape after a restart, or a core-count change).
type usageResult struct {
	byCore []float64
	allPct float64
	hasAll bool
	shares []snapshot.ModeShare
}

// computeUsage derives the per-core and node usage from the delta between
// this scrape and the previous one, setting the usage and mode-ratio gauges
// as it goes.
func (c *Collector) computeUsage(name string, cores []*machinepb.CPUStat, usage, modeRatio *prometheus.GaugeVec) usageResult {
	totals := make([]float64, len(cores))
	idles := make([]float64, len(cores))
	for i, cs := range cores {
		totals[i] = cpuStatTotal(cs)
		idles[i] = cs.GetIdle() + cs.GetIowait()
	}
	byCore := make([]float64, len(cores))
	for i := range byCore {
		byCore[i] = -1 // no sample this scrape
	}
	modeNow := nodeModeTotals(cores)

	var res usageResult
	prev, hadPrev := c.takePrev(name, totals, idles, modeNow)
	var allTotal, allBusy float64
	if hadPrev {
		for i := range totals {
			dTotal := totals[i] - prev.totals[i]
			if dTotal <= 0 {
				continue
			}
			dBusy := dTotal - (idles[i] - prev.idles[i])
			// byCore stays in percent: the dashboard and the history ring
			// both render percent. The metric is a ratio, per convention.
			byCore[i] = dBusy / dTotal * 100
			usage.WithLabelValues(name, strconv.Itoa(i)).Set(dBusy / dTotal)
			allTotal += dTotal
			allBusy += dBusy
		}
	}
	res.byCore = byCore
	if allTotal > 0 {
		res.allPct = allBusy / allTotal * 100
		res.hasAll = true
		usage.WithLabelValues(name, "all").Set(allBusy / allTotal)

		// prev.modes is nil on the first scrape after a restart, when there is
		// no interval to divide by; the series stay absent rather than
		// reporting a boot-to-now average as if it were current.
		if hadPrev && prev.modes != nil {
			res.shares = modeShares(name, modeRatio, modeNow, prev.modes)
		}
	}
	return res
}

// modeShares computes each mode's share of the node's CPU time over the
// interval between the previous and current scrape, setting the registered
// mode-ratio gauge for every mode. Returns nil (and sets nothing) when the
// interval is empty.
func modeShares(name string, modeRatio *prometheus.GaugeVec, now, prev map[string]float64) []snapshot.ModeShare {
	var dTotalAll float64
	deltas := make(map[string]float64, len(modeOrder))
	for _, mode := range modeOrder {
		d := now[mode] - prev[mode]
		if d < 0 {
			d = 0 // counter reset (reboot): treat as no time, not negative
		}
		deltas[mode] = d
		dTotalAll += d
	}
	if dTotalAll == 0 {
		return nil
	}
	shares := make([]snapshot.ModeShare, 0, len(modeOrder))
	for _, mode := range modeOrder {
		r := deltas[mode] / dTotalAll
		modeRatio.WithLabelValues(name, mode).Set(r)
		shares = append(shares, snapshot.ModeShare{Mode: mode, Ratio: r})
	}
	return shares
}

// registerFreqFamilies registers the four per-core frequency series and
// returns the same data the snapshot and history ring consume. RPC values
// win; a core the RPC reported as empty takes its values from the file
// fallback when one was collected.
func registerFreqFamilies(name string, freqs []*machinepb.CPUFreqStats, files []cpufreqFiles, usageByCore []float64, reg prometheus.Registerer) ([]float64, []snapshot.Core) {
	// The live reading and the policy bounds are three different quantities,
	// so they are three families. As one family keyed by a `freq` label,
	// avg(talos_node_cpu_freq_hertz) averaged live clock speeds with policy
	// ceilings and returned a number that meant nothing.
	f := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_freq_hertz",
		Help: "Current per-core CPU frequency in hertz. Sampled at scrape time, and the " +
			"scrape itself briefly raises it: CPU governors react to any work within " +
			"microseconds, so a reading taken over a remote API is biased high on nodes " +
			"with little headroom. Prefer a range or quantile over a window to a single sample.",
	}, []string{"node", "cpu"})
	fMin := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_freq_min_hertz",
		Help: "Lower bound of the core's cpufreq policy, in hertz. Static until the policy changes.",
	}, []string{"node", "cpu"})
	fMax := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_freq_max_hertz",
		Help: "Upper bound of the core's cpufreq policy, in hertz. Static until the policy changes.",
	}, []string{"node", "cpu"})

	// The governor is a mutable attribute, not part of the reading's identity.
	// As a label on the gauge, a routine powersave -> performance transition
	// ended the frequency series and started a fresh one, leaving the old one
	// stale in the head block.
	gov := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_governor_info",
		Help: "Active cpufreq governor for a core (value 1).",
	}, []string{"node", "cpu", "governor"})

	freqCur := make([]float64, len(freqs))
	coreStates := make([]snapshot.Core, len(freqs))
	for i, fm := range freqs {
		core := strconv.Itoa(i)
		cur, min, max, governor := fm.GetCurrentFrequency(), fm.GetMinimumFrequency(), fm.GetMaximumFrequency(), fm.GetGovernor()
		if cur == 0 && min == 0 && max == 0 && governor == "" && i < len(files) {
			cur, min, max, governor = files[i].cur, files[i].min, files[i].max, files[i].governor
		}
		// cpufreq reports kHz; these are hertz per the base-unit convention.
		freqCur[i] = float64(cur) * 1e3
		f.WithLabelValues(name, core).Set(float64(cur) * 1e3)
		fMin.WithLabelValues(name, core).Set(float64(min) * 1e3)
		fMax.WithLabelValues(name, core).Set(float64(max) * 1e3)
		if governor != "" {
			gov.WithLabelValues(name, core, governor).Set(1)
		}
		coreStates[i] = snapshot.Core{
			Index:     i,
			CurrentHz: float64(cur) * 1e3,
			MinimumHz: float64(min) * 1e3,
			MaximumHz: float64(max) * 1e3,
			Governor:  governor,
		}
		if i < len(usageByCore) && usageByCore[i] >= 0 {
			coreStates[i].HasUsage, coreStates[i].UsagePct = true, usageByCore[i]
		}
	}
	reg.MustRegister(f, fMin, fMax, gov)
	return freqCur, coreStates
}

// cpufreqFiles holds one core's frequency data from the cpufreq files
// (kHz, the metric's native unit).
type cpufreqFiles struct {
	cur, min, max uint64
	governor      string
}

// freqSlowTTL bounds how stale the per-core cpufreq sysfs pass may be
// before it is re-run: min/max/governor change only with a cpufreq policy
// change (effectively never), while the live current frequency comes from
// a single /proc/cpuinfo read on every tick.
const freqSlowTTL = 5 * time.Minute

// freqSource selects where a node's live per-core frequency comes from.
//
// /proc/cpuinfo is one read for the whole machine, but its "cpu MHz" lines are
// only per-core on drivers that derive them from aperf/mperf. On intel_pstate
// they report a shared/requested P-state: measured on a 4-core i5-6400T,
// /proc/cpuinfo showed zero spread in 3 of 4 rounds while scaling_cur_freq
// showed the cores 1200-1600 MHz apart in the same rounds. amd-pstate-epp
// tracks sysfs correctly, so it keeps the cheap path.
type freqSource int

const (
	freqSourceUnknown freqSource = iota
	freqSourceCPUInfo            // one /proc/cpuinfo read per pass
	freqSourcePerCore            // one scaling_cur_freq read per core, concurrent
)

// driverPath is the file probed once per node to pick the source.
const driverPath = "/sys/devices/system/cpu/cpu0/cpufreq/scaling_driver"

// perCoreDrivers are the cpufreq drivers whose /proc/cpuinfo values are not
// per-core. Anything else keeps the single-read path; the probed driver is
// logged so an unexpected one is diagnosable.
var perCoreDrivers = map[string]bool{"intel_pstate": true, "intel_cpufreq": true}

// freqReadConcurrency bounds the per-core sysfs reads. Sequentially those are
// ~5 ms each (80 ms for 16 cores); at this limit the whole pass costs about
// the same as the single /proc/cpuinfo read it replaces (13 ms vs 7 ms).
const freqReadConcurrency = 8

// freqCacheData is the cached cpufreq data for one node.
type freqCacheData struct {
	ts      time.Time
	perCore []cpufreqFiles
	// source and driver are probed once per node and then reused.
	source freqSource
	driver string
	// curTS is when the per-core current pass last ran, so it can be given a
	// slower cadence than the collector TTL on machines with many cores.
	curTS time.Time
	cur   []uint64
}

// freqFromFiles returns per-core frequency data from node files when the
// CPUFreqStats RPC came back empty: the current frequency is read once per
// tick from /proc/cpuinfo (per-core "cpu MHz" lines), and min/max/governor
// come from the per-core cpufreq sysfs files, re-read at most every
// freqSlowTTL. A permission error (an os:reader ServiceAccount) warns once
// and stops the file reads for the rest of the process (the cache keeps
// serving); other read errors leave the affected cores without data.
// Neither kind fails the scrape — the RPC data was collected fine.
func (c *Collector) freqFromFiles(ctx context.Context, api machineAPI, nodeName string, nCores int) []cpufreqFiles {
	c.mu.Lock()
	cached := c.freqCache[nodeName]
	c.mu.Unlock()
	if c.fallbackWarned.Load() {
		return cached.perCore
	}
	if c.fallbackNoted.CompareAndSwap(false, true) {
		c.log.Debug("CPUFreqStats returned empty data; using the file-based frequency fallback",
			"node", nodeName, "current", "/proc/cpuinfo per tick", "min/max/governor", "cpufreq sysfs, "+
				freqSlowTTL.String()+" cache")
	}

	now := c.now()
	cached = c.refreshFreqSlow(ctx, api, nodeName, nCores, now, cached)
	cached = c.refreshFreqCurrent(ctx, api, nodeName, nCores, now, cached)
	// Collects for one node never overlap (the scraper's in-flight guard), so
	// one write-back at the end covers both refreshes.
	c.mu.Lock()
	c.freqCache[nodeName] = cached
	c.mu.Unlock()

	out := make([]cpufreqFiles, nCores)
	for i := range out {
		if i < len(cached.perCore) {
			out[i] = cached.perCore[i]
		}
	}
	for i := range out {
		if i < len(cached.cur) && cached.cur[i] > 0 {
			out[i].cur = cached.cur[i]
		}
	}
	return out
}

// refreshFreqSlow probes the cpufreq driver (once per node) and re-reads the
// per-core cpufreq sysfs files when they are older than freqSlowTTL: the
// slow half of the file fallback, for min/max/governor, which change only
// with a cpufreq policy change (effectively never).
func (c *Collector) refreshFreqSlow(ctx context.Context, api machineAPI, nodeName string, nCores int, now time.Time, cached freqCacheData) freqCacheData {
	// Probe the cpufreq driver once per node to choose the live-value source.
	if cached.source == freqSourceUnknown {
		cached.driver, cached.source = c.probeFreqSource(ctx, api, nodeName)
	}
	if cached.ts.IsZero() || now.Sub(cached.ts) >= freqSlowTTL {
		if slow := c.readCpufreqSlow(ctx, api, nodeName, nCores); slow != nil {
			// Assign fields rather than replacing the struct: the probed source
			// and the last per-core values live here too.
			cached.ts, cached.perCore = now, slow
		}
	}
	return cached
}

// refreshFreqCurrent re-reads the per-core current frequency from whichever
// source this node's driver needs when its cadence is due.
func (c *Collector) refreshFreqCurrent(ctx context.Context, api machineAPI, nodeName string, nCores int, now time.Time, cached freqCacheData) freqCacheData {
	// freqInterval (0 = every pass) lets a many-core machine read it less
	// often than the collector TTL without slowing usage sampling.
	due := c.freqInterval <= 0 || cached.curTS.IsZero() || now.Sub(cached.curTS) >= c.freqInterval
	if !due {
		return cached
	}
	var cur []uint64
	var ok bool
	if cached.source == freqSourcePerCore {
		cur, ok = c.readPerCoreCurrent(ctx, api, nodeName, nCores)
	} else {
		cur, ok = c.readCpuinfoCurrent(ctx, api, nodeName, nCores)
	}
	if ok {
		cached.cur, cached.curTS = cur, now
	}
	return cached
}

// probeFreqSource reads the cpufreq driver once and decides where this node's
// live per-core frequency must come from. An unreadable driver file keeps the
// cheap path: it is the pre-existing behaviour and costs one read per pass.
func (c *Collector) probeFreqSource(ctx context.Context, api machineAPI, nodeName string) (string, freqSource) {
	driver, err := readSysfsFile(ctx, api, driverPath)
	if err != nil {
		if collector.IsPermissionError(err) {
			c.warnFallback(nodeName)
		}
		c.log.Debug("cpufreq driver not readable; using the /proc/cpuinfo frequency source",
			"node", nodeName, "err", err)
		return "", freqSourceCPUInfo
	}
	src := freqSourceCPUInfo
	if perCoreDrivers[driver] {
		src = freqSourcePerCore
	}
	c.log.Info("cpufreq source selected", "node", nodeName, "driver", driver,
		"source", map[freqSource]string{freqSourceCPUInfo: "/proc/cpuinfo", freqSourcePerCore: "per-core scaling_cur_freq"}[src])
	return driver, src
}

// readPerCoreCurrent reads scaling_cur_freq for every core concurrently. ok is
// false only when no core was readable at all; a core that fails individually
// is left at zero and rendered as a gap.
func (c *Collector) readPerCoreCurrent(ctx context.Context, api machineAPI, nodeName string, nCores int) ([]uint64, bool) {
	out := make([]uint64, nCores)
	var any atomic.Bool
	var wg sync.WaitGroup
	sem := make(chan struct{}, freqReadConcurrency)
	for core := 0; core < nCores; core++ {
		wg.Add(1)
		go func(core int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", core)
			raw, err := readSysfsFile(ctx, api, path)
			if err != nil {
				if collector.IsPermissionError(err) {
					c.warnFallback(nodeName)
				}
				return
			}
			if v, err := parseUint(raw); err == nil && v > 0 {
				out[core] = v
				any.Store(true)
			}
		}(core)
	}
	wg.Wait()
	return out, any.Load()
}

// readCpufreqSlow reads the per-core cpufreq sysfs files (current, min,
// max, governor — kHz). Returns nil if no core's files were readable.
func (c *Collector) readCpufreqSlow(ctx context.Context, api machineAPI, nodeName string, nCores int) []cpufreqFiles {
	out := make([]cpufreqFiles, nCores)
	any := false
	for core := 0; core < nCores; core++ {
		dir := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq", core)
		f := cpufreqFiles{}
		readable := false
		for _, file := range []string{"scaling_cur_freq", "scaling_min_freq", "scaling_max_freq", "scaling_governor"} {
			raw, err := readSysfsFile(ctx, api, dir+"/"+file)
			if err != nil {
				if collector.IsPermissionError(err) {
					c.warnFallback(nodeName)
					return nil
				}
				c.log.Debug("cpufreq fallback: file not readable",
					"node", nodeName, "file", dir+"/"+file, "error", err)
				break // file absent → the rest are absent too
			}
			readable = true
			switch file {
			case "scaling_cur_freq":
				f.cur, _ = parseUint(raw)
			case "scaling_min_freq":
				f.min, _ = parseUint(raw)
			case "scaling_max_freq":
				f.max, _ = parseUint(raw)
			default:
				f.governor = raw
			}
		}
		if readable {
			out[core] = f
			any = true
		}
	}
	if !any {
		return nil
	}
	return out
}

// readCpuinfoCurrent reads /proc/cpuinfo once and returns the per-core live
// frequency in kHz from the "cpu MHz" lines. ok is false if the file could
// not be read.
func (c *Collector) readCpuinfoCurrent(ctx context.Context, api machineAPI, nodeName string, nCores int) ([]uint64, bool) {
	raw, err := readSysfsFile(ctx, api, "/proc/cpuinfo")
	if err != nil {
		if collector.IsPermissionError(err) {
			c.warnFallback(nodeName)
		}
		return nil, false
	}
	return parseCpuMHz(raw, nCores), true
}

// parseCpuMHz extracts the per-core "cpu MHz" values from /proc/cpuinfo
// content and returns them in kHz. Cores without a value get 0.
func parseCpuMHz(content string, nCores int) []uint64 {
	out := make([]uint64, nCores)
	core := -1
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "processor":
			if i, err := parseUint(val); err == nil && i < 4096 {
				core = int(i)
			}
		case "cpu MHz":
			if f, err := strconv.ParseFloat(val, 64); err == nil && core >= 0 && core < nCores {
				out[core] = uint64(f * 1000)
			}
		}
	}
	return out
}

// warnFallback logs the os:admin hint once per process (the fallback needs
// file reads; the rest of the cpu data is unaffected).
func (c *Collector) warnFallback(nodeName string) {
	if c.fallbackWarned.CompareAndSwap(false, true) {
		c.log.Warn("per-core CPU frequency fallback disabled: file reads need the os:admin role (got a permission error)",
			"node", nodeName,
			"hint", "give the monitor ServiceAccount the os:admin role (Helm value talos.roles); the rest of the cpu data is unaffected")
	}
}

// Degraded implements collector.DegradedReporter. Not Stopped: the CPUStats RPC
// still runs, so the round still reaches the node. What is off is the file
// fallback that supplies per-core min/max/governor when the RPC returns empty -
// and once fallbackWarned is set it is off for every node, permanently.
func (c *Collector) Degraded(string) []collector.Degradation {
	if c.fallbackWarned.Load() {
		return []collector.Degradation{{Reason: collector.ReasonPermission}}
	}
	return nil
}

// readSysfsFile reads a small sysfs file and returns its trimmed content.
func readSysfsFile(ctx context.Context, api machineAPI, path string) (string, error) {
	s, err := collector.ReadFile(ctx, api, path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func parseUint(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 10, 64)
}

// takePrev swaps in the new sample for the node and returns the previous
// one. A core count change (hotplug, or first scrape) drops the previous
// sample so no bogus delta is computed.
func (c *Collector) takePrev(name string, totals, idles []float64, modes map[string]float64) (cpuSample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.prev[name]
	if ok && len(prev.totals) != len(totals) {
		ok = false
	}
	c.prev[name] = cpuSample{totals: totals, idles: idles, modes: modes}
	return prev, ok
}

// cpuStatTotal sums every mode of a CPUStat (all CPU time accounts for one of
// them, per /proc/stat semantics).
func cpuStatTotal(cs *machinepb.CPUStat) float64 {
	return cs.GetUser() + cs.GetNice() + cs.GetSystem() + cs.GetIdle() + cs.GetIowait() +
		cs.GetIrq() + cs.GetSoftIrq() + cs.GetSteal() + cs.GetGuest() + cs.GetGuestNice()
}

// modeOrder is every /proc/stat mode, busiest-first for display. Iterating a
// map would reorder the breakdown on every scrape.
var modeOrder = [...]string{
	"user", "system", "nice", "iowait", "irq", "softirq", "steal", "guest", "guest_nice", "idle",
}

// nodeModeTotals sums each mode across every core, so the node-level breakdown
// is one delta pair per mode instead of one per core.
func nodeModeTotals(cores []*machinepb.CPUStat) map[string]float64 {
	out := make(map[string]float64, len(modeOrder))
	for _, cs := range cores {
		for mode, v := range cpuStatModes(cs) {
			out[mode] += v
		}
	}
	return out
}

func cpuStatModes(cs *machinepb.CPUStat) map[string]float64 {
	return map[string]float64{
		"user":       cs.GetUser(),
		"nice":       cs.GetNice(),
		"system":     cs.GetSystem(),
		"idle":       cs.GetIdle(),
		"iowait":     cs.GetIowait(),
		"irq":        cs.GetIrq(),
		"softirq":    cs.GetSoftIrq(),
		"steal":      cs.GetSteal(),
		"guest":      cs.GetGuest(),
		"guest_nice": cs.GetGuestNice(),
	}
}

// Prune implements collector.Pruner: drop per-node samples for departed nodes.
func (c *Collector) Prune(live map[string]struct{}) {
	// An empty set means discovery has not synced yet, not that the cluster
	// is empty — pruning here would drop every node's state on a transient
	// watch blip. app.prune() guards this too; the Pruner contract says
	// implementations must, so they do.
	if len(live) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for node := range c.prev {
		if _, ok := live[node]; !ok {
			delete(c.prev, node)
		}
	}
	for node := range c.freqCache {
		if _, ok := live[node]; !ok {
			delete(c.freqCache, node)
		}
	}
}
