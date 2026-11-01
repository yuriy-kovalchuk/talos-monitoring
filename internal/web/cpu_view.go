package web

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// cpuSocket is one socket row, from the snapshot the hwinfo collector wrote.
type cpuSocket struct {
	Socket       string
	Product      string
	Cores        int
	CoresEnabled int
	Threads      int
	MaxMHz       int
	BootMHz      int
	Status       int
	PartNumber   string
	SerialNumber string
	AssetTag     string
}

// cpuSocketsFrom renders the socket rows, already sorted by the collector.
func cpuSocketsFrom(n *snapshot.Node) []cpuSocket {
	if n == nil {
		return nil
	}
	out := make([]cpuSocket, 0, len(n.Sockets))
	for _, sock := range n.Sockets {
		out = append(out, cpuSocket{
			Socket:       sock.ID,
			Product:      sock.Product,
			Cores:        sock.Cores,
			CoresEnabled: sock.CoresEnabled,
			Threads:      sock.Threads,
			MaxMHz:       int(sock.MaxHertz / 1e6),
			BootMHz:      int(sock.BootHertz / 1e6),
			Status:       sock.Status,
			PartNumber:   dmiValue(sock.PartNumber),
			SerialNumber: dmiValue(sock.SerialNumber),
			AssetTag:     dmiValue(sock.AssetTag),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nodeStatus is the "Node — Status" card, parsed from the nodeapi metrics.
type nodeStatus struct {
	Version    string
	SHA        string
	Arch       string
	Built      string
	Uptime     string
	CPUPercent float64
	HasCPU     bool
	Running    uint64
	Blocked    uint64
	HasLoad    bool
	Load       string // "1.92 · 1.53 · 1.47"
	Cores      []coreStat
	Policy     cpuPolicy
	Modes      []cpuMode
}

// Tone drives the bar colour: iowait, steal and irq mean waiting on something else.
type cpuMode struct {
	Mode  string
	Pct   string
	Width float64
	Tone  string // busy | wait | idle
}

// freqObserved is the frequency actually seen over the history window; one reading is false precision.
type freqObserved struct {
	Available bool
	Min       string
	Median    string
	Max       string
	Samples   int
	Window    string
}

// freqObservedFrom summarises the per-core frequency history for a node.
func freqObservedFrom(hist *history.Store, node string) freqObserved {
	var o freqObserved
	if hist == nil {
		return o
	}
	series, ok := hist.Points(node)
	if !ok {
		return o
	}
	var vals []float64
	for name, pts := range series {
		if !strings.HasPrefix(name, "freq.current.") {
			continue
		}
		for _, p := range pts {
			if p.V > 0 {
				vals = append(vals, p.V)
			}
		}
	}
	if len(vals) == 0 {
		return o
	}
	sort.Float64s(vals)
	o.Available = true
	o.Samples = len(vals)
	o.Min = trimFloat(math.Round(vals[0] / 1e6))
	o.Max = trimFloat(math.Round(vals[len(vals)-1] / 1e6))
	o.Median = trimFloat(math.Round(vals[len(vals)/2] / 1e6))
	o.Window = humanDuration(hist.Window())
	return o
}

// cpuPolicy collapses the per-core governor and bounds, which matched on every machine seen so far.
type cpuPolicy struct {
	Governor string
	MinMHz   string
	MaxMHz   string
	Mixed    bool // cores disagree; say so rather than pick one
}

// coreStat is one row of the status card's per-core table.
type coreStat struct {
	Index    int
	Usage    float64
	HasUsage bool
	FreqMHz  string
	MinMHz   string
	MaxMHz   string
	Governor string
}

// nodeStatusFrom builds the status card, or nil when nothing was collected ("no data yet").
func nodeStatusFrom(n *snapshot.Node) *nodeStatus {
	if n == nil || (n.Runtime == nil && n.CPU == nil) {
		return nil
	}
	st := &nodeStatus{}
	fillStatusRuntime(st, n.Runtime)
	fillStatusCPUModes(st, n.CPU)
	fillStatusCores(st, n.CPU)
	sort.Slice(st.Cores, func(i, j int) bool { return st.Cores[i].Index < st.Cores[j].Index })
	st.Policy = collapsePolicy(st.Cores)
	return st
}

func fillStatusRuntime(st *nodeStatus, rt *snapshot.Runtime) {
	if rt == nil {
		return
	}
	st.Version, st.SHA, st.Arch, st.Built = rt.Version, rt.SHA, rt.Arch, rt.Built
	if rt.HasUptime {
		st.Uptime = humanDuration(time.Duration(rt.Uptime * float64(time.Second)))
	}
	st.Running, st.Blocked = rt.Running, rt.Blocked
	if rt.HasLoad {
		st.HasLoad = true
		st.Load = fmt.Sprintf("%.2f · %.2f · %.2f", rt.Load1, rt.Load5, rt.Load15)
	}
}

func fillStatusCPUModes(st *nodeStatus, c *snapshot.CPU) {
	if c == nil {
		return
	}
	for _, m := range c.Modes {
		// Below a tenth of a percent is bar-chart noise that pushes readable modes off.
		if m.Ratio < 0.001 {
			continue
		}
		tone := "busy"
		switch m.Mode {
		case "idle":
			tone = "idle"
		case "iowait", "steal", "irq", "softirq":
			tone = "wait"
		}
		st.Modes = append(st.Modes, cpuMode{
			Mode: m.Mode, Pct: fmt.Sprintf("%.1f", m.Ratio*100),
			Width: m.Ratio * 100, Tone: tone,
		})
	}
}

func fillStatusCores(st *nodeStatus, c *snapshot.CPU) {
	if c == nil {
		return
	}
	st.CPUPercent, st.HasCPU = c.UsagePct, c.HasUsage
	for _, core := range c.Cores {
		cs := coreStat{Index: core.Index, Governor: core.Governor}
		// No cpufreq data reports 0 (amd_pstate lacks the file the RPC reads): render n/a, not 0 MHz.
		if core.CurrentHz > 0 {
			cs.FreqMHz = freqMHz(core.CurrentHz)
		}
		if core.MinimumHz > 0 {
			cs.MinMHz = freqMHz(core.MinimumHz)
		}
		if core.MaximumHz > 0 {
			cs.MaxMHz = freqMHz(core.MaximumHz)
		}
		cs.Usage, cs.HasUsage = core.UsagePct, core.HasUsage
		st.Cores = append(st.Cores, cs)
	}
}

// collapsePolicy folds per-core governor/min/max into one statement, flagging Mixed.
func collapsePolicy(cores []coreStat) cpuPolicy {
	var p cpuPolicy
	govs, mins, maxs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range cores {
		if c.Governor != "" {
			govs[c.Governor] = true
		}
		if c.MinMHz != "" {
			mins[c.MinMHz] = true
		}
		if c.MaxMHz != "" {
			maxs[c.MaxMHz] = true
		}
	}
	p.Mixed = len(govs) > 1 || len(mins) > 1 || len(maxs) > 1
	if p.Mixed {
		// Fields stay empty: another consumer would show a policy only some cores have.
		return p
	}
	for g := range govs {
		p.Governor = g
	}
	for m := range mins {
		p.MinMHz = m
	}
	for m := range maxs {
		p.MaxMHz = m
	}
	return p
}
