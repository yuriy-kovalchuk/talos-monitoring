package cpu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
)

// cpufreqFiles is one core's frequency data from the cpufreq files (kHz).
type cpufreqFiles struct {
	cur, min, max uint64
	governor      string
}

// freqSlowTTL bounds the per-core sysfs pass: min/max/governor change only with a policy change.
const freqSlowTTL = 5 * time.Minute

// freqSource picks the live frequency source: /proc/cpuinfo is not per-core on intel_pstate.
type freqSource int

const (
	freqSourceUnknown freqSource = iota
	freqSourceCPUInfo            // one /proc/cpuinfo read per pass
	freqSourcePerCore            // one scaling_cur_freq read per core, concurrent
)

// driverPath is the file probed once per node to pick the source.
const driverPath = "/sys/devices/system/cpu/cpu0/cpufreq/scaling_driver"

// perCoreDrivers have non-per-core /proc/cpuinfo values; the probed driver is logged.
var perCoreDrivers = map[string]bool{"intel_pstate": true, "intel_cpufreq": true}

// freqReadConcurrency keeps the per-core pass as cheap as the /proc/cpuinfo read it replaces.
const freqReadConcurrency = 8

// freqCacheData is the cached cpufreq data for one node.
type freqCacheData struct {
	ts      time.Time
	perCore []cpufreqFiles
	// source and driver are probed once per node and then reused.
	source freqSource
	driver string
	// curTS is when the per-core current pass last ran, so it can lag the collector TTL.
	curTS time.Time
	cur   []uint64
}

// freqFromFiles supplies frequency from node files when the RPC was empty; read errors never fail the scrape.
func (c *Collector) freqFromFiles(ctx context.Context, api machineAPI, nodeName string, nCores int) []cpufreqFiles {
	c.mu.Lock()
	cached := c.freqCache[nodeName]
	c.mu.Unlock()
	if c.fallbackGate.Off() {
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
	// One write-back covers both refreshes: the scraper never overlaps collects for a node.
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

// refreshFreqSlow probes the driver once and re-reads the slow sysfs half at freqSlowTTL.
func (c *Collector) refreshFreqSlow(ctx context.Context, api machineAPI, nodeName string, nCores int, now time.Time, cached freqCacheData) freqCacheData {
	// Probe the cpufreq driver once per node to choose the live-value source.
	if cached.source == freqSourceUnknown {
		cached.driver, cached.source = c.probeFreqSource(ctx, api, nodeName)
	}
	if cached.ts.IsZero() || now.Sub(cached.ts) >= freqSlowTTL {
		if slow := c.readCpufreqSlow(ctx, api, nodeName, nCores); slow != nil {
			// Assign fields rather than replacing the struct: the probed source lives here too.
			cached.ts, cached.perCore = now, slow
		}
	}
	return cached
}

// refreshFreqCurrent re-reads the current frequency from the source this driver needs.
func (c *Collector) refreshFreqCurrent(ctx context.Context, api machineAPI, nodeName string, nCores int, now time.Time, cached freqCacheData) freqCacheData {
	// freqInterval (0 = every pass) lets a many-core machine read less often without slowing usage.
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

// probeFreqSource reads the driver once to pick the source; unreadable keeps the cheap path.
func (c *Collector) probeFreqSource(ctx context.Context, api machineAPI, nodeName string) (string, freqSource) {
	driver, err := collector.ReadFileTrimmed(ctx, api, driverPath)
	if err != nil {
		if collector.IsPermissionError(err) {
			c.fallbackGate.Trip(nodeName, err)
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

// readPerCoreCurrent reads scaling_cur_freq per core; ok is false only if none was readable.
func (c *Collector) readPerCoreCurrent(ctx context.Context, api machineAPI, nodeName string, nCores int) ([]uint64, bool) {
	out := make([]uint64, nCores)
	var anyReadable atomic.Bool
	var wg sync.WaitGroup
	sem := make(chan struct{}, freqReadConcurrency)
	for core := range nCores {
		wg.Add(1)
		go func(core int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", core)
			raw, err := collector.ReadFileTrimmed(ctx, api, path)
			if err != nil {
				if collector.IsPermissionError(err) {
					c.fallbackGate.Trip(nodeName, err)
				}
				return
			}
			if v, err := parseUint(raw); err == nil && v > 0 {
				out[core] = v
				anyReadable.Store(true)
			}
		}(core)
	}
	wg.Wait()
	return out, anyReadable.Load()
}

// readCpufreqSlow reads each core's current/min/max/governor files (kHz).
func (c *Collector) readCpufreqSlow(ctx context.Context, api machineAPI, nodeName string, nCores int) []cpufreqFiles {
	out := make([]cpufreqFiles, nCores)
	anyReadable := false
	for core := range nCores {
		dir := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq", core)
		f := cpufreqFiles{}
		readable := false
		for _, file := range []string{"scaling_cur_freq", "scaling_min_freq", "scaling_max_freq", "scaling_governor"} {
			raw, err := collector.ReadFileTrimmed(ctx, api, dir+"/"+file)
			if err != nil {
				if collector.IsPermissionError(err) {
					c.fallbackGate.Trip(nodeName, err)
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
			anyReadable = true
		}
	}
	if !anyReadable {
		return nil
	}
	return out
}

// readCpuinfoCurrent reads /proc/cpuinfo once and returns per-core kHz.
func (c *Collector) readCpuinfoCurrent(ctx context.Context, api machineAPI, nodeName string, nCores int) ([]uint64, bool) {
	raw, err := collector.ReadFileTrimmed(ctx, api, "/proc/cpuinfo")
	if err != nil {
		if collector.IsPermissionError(err) {
			c.fallbackGate.Trip(nodeName, err)
		}
		return nil, false
	}
	return parseCPUMHz(raw, nCores), true
}

// parseCPUMHz pulls per-core "cpu MHz" values out of /proc/cpuinfo as kHz.
func parseCPUMHz(content string, nCores int) []uint64 {
	out := make([]uint64, nCores)
	core := -1
	for line := range strings.SplitSeq(content, "\n") {
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
