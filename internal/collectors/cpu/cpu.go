// Package cpu collects fast-changing CPU data from the Talos machine service at the fastest TTL (5s).
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

// seriesFreqCurrent is the only history series left: the CPU page reports an observed range, not one reading.
const seriesFreqCurrent = "freq.current." // + core index

// defaultTTL is the natural refresh period of CPU usage and live frequency.
const defaultTTL = 5 * time.Second

// machineAPI is the per-node Talos client subset the collector uses (test seam).
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

// cpuSample is the previous scrape's per-core CPU time, for usage deltas.
type cpuSample struct {
	totals []float64 // all modes summed, per core
	idles  []float64 // idle+iowait, per core

	// modes sums each /proc/stat mode across cores: the question is node-wide, not per-thread.
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

	// freqInterval throttles the per-core frequency pass (0 = every tick), on per-core-source nodes only.
	freqInterval time.Duration

	// hist feeds the dashboard graph (nil = history disabled).
	hist *history.Store

	// snap receives the typed live state alongside the metrics.
	snap *snapshot.Store

	// sysstat shares the SystemStat response with nodeapi, which wants boot time and process counts.
	sysstat *sysstat.Cache

	// modeSeconds turns on talos_node_cpu_seconds_total: 10 series per thread, which dominates cardinality.
	modeSeconds bool

	// fallbackGate trips when the frequency file fallback hits a permission error (reads need os:admin).
	fallbackGate *collector.PermissionGate
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

// New returns the cpu collector; hist may be nil and freqInterval 0 means every tick.
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
		fallbackGate: collector.NewPermissionGate(log, "per-core CPU frequency fallback", "; the rest of the cpu data is unaffected"),
	}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Live }

// DefaultTTL implements collector.DefaultTTLer.
func (c *Collector) DefaultTTL() time.Duration { return defaultTTL }

// Collect registers the usage ratio, the per-core frequency families, governor info and opt-in mode seconds.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	api := c.newAPI(node)
	name := node.Node.Name

	// Frequency is sampled first: the scrape is load, so a late reading reads high (+2.9 % to +121 % measured).
	freqs, err := freqStats(ctx, api)
	if err != nil {
		return err
	}

	// CPUFreqStats returns zeros without the legacy cpuinfo_cur_freq file; node files then supply it.
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

	// A ratio, computed in the exporter because the counter it would derive from is opt-in (§1.1).
	usage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_usage_ratio",
		Help: "CPU usage as a ratio 0-1 since the previous scrape (cpu=\"all\" is the node total). " +
			"Derived over the exporter's own scrape interval; for querying prefer " +
			"rate(talos_node_cpu_seconds_total{mode=\"idle\"}[window]), which uses your window.",
	}, []string{"node", "cpu"})
	reg.MustRegister(usage)

	// Node-level mode shares: the always-on companion to the opt-in seconds family (§1.1); 10 series per node.
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

// appendHistory records per-core frequency samples; a zero frequency is a gap, not a sample.
func (c *Collector) appendHistory(node string, freqCur []float64) {
	now := c.now()
	for i, v := range freqCur {
		if v > 0 {
			c.hist.Append(node, seriesFreqCurrent+strconv.Itoa(i), now, v)
		}
	}
}

// freqStats calls CPUFreqStats, failing when the response carries no stats at all.
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

// nonEmptyFreqs counts cores the RPC reported data for; all-zero is the kernel signature.
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

// systemCores fetches per-core CPU time through the shared SystemStat cache.
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
	// Opt-in (10 modes x thread x node is ~64 000 series at 100 nodes) and already seconds, not jiffies.
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

// usageResult is one scrape's usage: per-core percent (-1 = no sample), node total, mode shares.
type usageResult struct {
	byCore []float64
	allPct float64
	hasAll bool
	shares []snapshot.ModeShare
}

// computeUsage derives per-core and node usage from the delta since the previous scrape.
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
			// byCore stays in percent for the UI and history ring; the metric is a ratio.
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

		// prev.modes is nil on the first scrape; the series stay absent rather than reporting a boot-to-now average.
		if hadPrev && prev.modes != nil {
			res.shares = modeShares(name, modeRatio, modeNow, prev.modes)
		}
	}
	return res
}

// modeShares sets each mode's share of CPU time over the interval; nil when the interval is empty.
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

// registerFreqFamilies registers the four frequency series; RPC values win over the file fallback.
func registerFreqFamilies(name string, freqs []*machinepb.CPUFreqStats, files []cpufreqFiles, usageByCore []float64, reg prometheus.Registerer) ([]float64, []snapshot.Core) {
	// Three quantities, three families: one family keyed by a freq label would make avg() meaningless.
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

	// The governor is a mutable attribute: as a label, a powersave-to-performance switch would end the series.
	gov := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_cpu_governor_info",
		Help: "Active cpufreq governor for a core (value 1).",
	}, []string{"node", "cpu", "governor"})

	freqCur := make([]float64, len(freqs))
	coreStates := make([]snapshot.Core, len(freqs))
	for i, fm := range freqs {
		core := strconv.Itoa(i)
		cur, minFreq, maxFreq, governor := fm.GetCurrentFrequency(), fm.GetMinimumFrequency(), fm.GetMaximumFrequency(), fm.GetGovernor()
		if cur == 0 && minFreq == 0 && maxFreq == 0 && governor == "" && i < len(files) {
			cur, minFreq, maxFreq, governor = files[i].cur, files[i].min, files[i].max, files[i].governor
		}
		// cpufreq reports kHz; these are hertz per the base-unit convention.
		freqCur[i] = float64(cur) * 1e3
		// No core runs at 0 Hz: a zero is an unmeasured value (os:reader, no cpufreq), so the series stays absent.
		setPositive(f, name, core, freqCur[i])
		setPositive(fMin, name, core, float64(minFreq)*1e3)
		setPositive(fMax, name, core, float64(maxFreq)*1e3)
		if governor != "" {
			gov.WithLabelValues(name, core, governor).Set(1)
		}
		coreStates[i] = snapshot.Core{
			Index:     i,
			CurrentHz: float64(cur) * 1e3,
			MinimumHz: float64(minFreq) * 1e3,
			MaximumHz: float64(maxFreq) * 1e3,
			Governor:  governor,
		}
		if i < len(usageByCore) && usageByCore[i] >= 0 {
			coreStates[i].HasUsage, coreStates[i].UsagePct = true, usageByCore[i]
		}
	}
	reg.MustRegister(f, fMin, fMax, gov)
	return freqCur, coreStates
}

// setPositive sets the series only for a measured, non-zero value.
func setPositive(g *prometheus.GaugeVec, node, core string, v float64) {
	if v > 0 {
		g.WithLabelValues(node, core).Set(v)
	}
}

// Degraded reports the file fallback off, not Stopped: the CPUStats RPC still reaches the node.
func (c *Collector) Degraded(string) []collector.Degradation {
	if c.fallbackGate.Denied() {
		return []collector.Degradation{{Reason: collector.ReasonPermission}}
	}
	return nil
}

func parseUint(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 10, 64)
}

// takePrev swaps in the new sample and returns the previous; a core-count change drops it.
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

// cpuStatTotal sums every mode of a CPUStat: all CPU time belongs to one of them.
func cpuStatTotal(cs *machinepb.CPUStat) float64 {
	return cs.GetUser() + cs.GetNice() + cs.GetSystem() + cs.GetIdle() + cs.GetIowait() +
		cs.GetIrq() + cs.GetSoftIrq() + cs.GetSteal() + cs.GetGuest() + cs.GetGuestNice()
}

// modeOrder is every /proc/stat mode, busiest-first; map iteration would reorder the breakdown.
var modeOrder = [...]string{
	"user", "system", "nice", "iowait", "irq", "softirq", "steal", "guest", "guest_nice", "idle",
}

// nodeModeTotals sums each mode across cores, so the node view is one delta pair per mode.
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
	// An empty live set means discovery has not synced; the Pruner contract says that prunes nothing.
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
