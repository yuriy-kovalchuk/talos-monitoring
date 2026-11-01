package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// nodeRow is a discovered node enriched with Talos status; dashboard fields are filled on the overview only.
type nodeRow struct {
	nodes.Node
	TalosVersion string
	Up           bool
	Uptime       string
	CPUPercent   float64
	HasCPU       bool
	RAM          string
	Disks        int

	// Overview table columns (enrichDashboard).
	CPUModel    string  // processor product name, first socket
	Cores       int     // physical cores, summed over sockets
	Threads     int     // hardware threads, summed over sockets
	LoadThreads float64 // CPUPercent applied to Threads: busy threads
	LoadPct     int     // CPUPercent clamped to 0..100, for the inline bar
	MemDIMMs    int     // populated memory slots
	Storage     string  // capacity of the node's local disks
	RebootedAgo string  // set when uptime is under rebootWindow

	// Each Has* guards a collector that may be disabled.
	HasHealth       bool
	Stage           string
	MachineReady    bool
	ServicesHealthy int
	ServicesTotal   int
	ServicesBad     int // failing or stopped; "no health check" is not a fault
	Diagnostics     int
	HasClock        bool
	ClockSynced     bool
	HasLoad         bool
	Load1           float64
	VolumesNotReady int
	GPUs            int
	Schematic       string // short form, for the image-consistency tile

	// Raw carriers for the fleet totals; not rendered directly.
	memMiB       int
	storageBytes uint64
}

// rebootWindow flags a recently rebooted node; the rules may become user-configurable later.
const rebootWindow = time.Hour

// fleet is the capacity summary behind the overview tiles, holding only facts the table cannot show.
type fleet struct {
	Cores       int
	Threads     int
	Memory      string
	DIMMs       int
	Storage     string
	Disks       int
	BusiestNode string
	BusiestPct  float64
	HasBusiest  bool

	// Fleet consistency and capacity the node table cannot show.
	Images        int    // distinct Image Factory schematics across the fleet
	ImageNote     string // the single schematic when there is only one
	GPUs          int
	GPUNodes      int
	ServicesBad   int // summed over nodes
	ServicesTotal int
	// BusiestLoad is the busiest node's own load: the fleet maximum mixed two measures in one tile.
	HasBusiestLoad bool
	BusiestLoad    float64
}

// attentionItem is one triggered rule on the overview attention card.
type attentionItem struct {
	Severity string // "down" | "warn"
	Node     string
	Text     string
}

// attentionRule evaluates one condition fleet-wide, so a rule can later be added as data.
type attentionRule func(rows []nodeRow) []attentionItem

// attentionRules is the enabled set; drift, temperature and scrape-health rules are planned.
var attentionRules = []attentionRule{
	ruleNodeDown, ruleServiceUnhealthy, ruleMachineNotReady, ruleClockUnsynced,
	ruleVolumeNotReady, ruleDiagnostics, ruleRecentReboot,
}

// Services with no health check are excluded: they read unhealthy forever by construction.
func ruleServiceUnhealthy(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if r.Up && r.ServicesBad > 0 {
			out = append(out, attentionItem{Severity: "down", Node: r.Name,
				Text: fmt.Sprintf("%d %s not healthy", r.ServicesBad,
					plural(r.ServicesBad, "service", "services"))})
		}
	}
	return out
}

func ruleMachineNotReady(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if r.Up && r.HasHealth && !r.MachineReady {
			out = append(out, attentionItem{Severity: "down", Node: r.Name,
				Text: "machine not ready at stage " + r.Stage})
		}
	}
	return out
}

// ruleClockUnsynced fires on skew, which breaks etcd, TLS and storage auth silently.
func ruleClockUnsynced(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if r.Up && r.HasClock && !r.ClockSynced {
			out = append(out, attentionItem{Severity: "warn", Node: r.Name, Text: "clock not synced"})
		}
	}
	return out
}

// ruleVolumeNotReady fires when a volume misses its ready phase, e.g. an unlock that failed.
func ruleVolumeNotReady(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if r.Up && r.VolumesNotReady > 0 {
			out = append(out, attentionItem{Severity: "down", Node: r.Name,
				Text: fmt.Sprintf("%d %s not ready", r.VolumesNotReady,
					plural(r.VolumesNotReady, "volume", "volumes"))})
		}
	}
	return out
}

func ruleDiagnostics(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if r.Up && r.Diagnostics > 0 {
			out = append(out, attentionItem{Severity: "warn", Node: r.Name,
				Text: fmt.Sprintf("%d diagnostic %s raised", r.Diagnostics,
					plural(r.Diagnostics, "warning", "warnings"))})
		}
	}
	return out
}

func ruleNodeDown(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if !r.Up {
			out = append(out, attentionItem{Severity: "down", Node: r.Name, Text: "Talos API unreachable"})
		}
	}
	return out
}

func ruleRecentReboot(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if r.Up && r.RebootedAgo != "" {
			out = append(out, attentionItem{Severity: "warn", Node: r.Name, Text: "rebooted " + r.RebootedAgo + " ago"})
		}
	}
	return out
}

func evaluateAttention(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, rule := range attentionRules {
		out = append(out, rule(rows)...)
	}
	return out
}

// stats is the cluster summary for the overview stat cards.
type stats struct {
	Nodes   int
	Up      int
	Talos   string
	Kubelet string
}

// systemInfo is one node's SystemInformation, read from talos_hw_system_info.
type systemInfo struct {
	Available    bool
	Manufacturer string
	Product      string
	Version      string
	SerialNumber string
	UUID         string
	SKUNumber    string
	WakeUpType   string
}

func computeStats(rows []nodeRow) stats {
	s := stats{Nodes: len(rows)}
	talos, kube := make(map[string]int), make(map[string]int)
	for _, r := range rows {
		// Up counts reachability alone, so a version gap cannot hide a node from it.
		if r.Up {
			s.Up++
		}
		if r.Up && r.TalosVersion != "" {
			talos[r.TalosVersion]++
		}
		if r.KubeVersion != "" {
			kube[r.KubeVersion]++
		}
	}
	s.Talos = mostCommon(talos)
	s.Kubelet = mostCommon(kube)
	return s
}

func mostCommon(counts map[string]int) string {
	best, bestN := "", 0
	for v, n := range counts {
		// Ties break on the string, not map order, so the headline version cannot flip on every poll.
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

// enrichDashboard fills the summary columns from the snapshot; reachability stays with the scraper.
func (d *dashboard) enrichDashboard(rows []nodeRow) []nodeRow {
	for i := range rows {
		if snap := d.snap.Node(rows[i].Name); snap != nil {
			enrichRow(&rows[i], snap)
		}
	}
	return rows
}

// enrichRow fills one row, split by snapshot section to keep each part simple.
func enrichRow(row *nodeRow, snap *snapshot.Node) {
	enrichRuntime(row, snap)
	enrichCPU(row, snap)
	enrichMemory(row, snap)
	enrichHealth(row, snap)
	enrichClockLoadVolumesIdentity(row, snap)
	enrichDisks(row, snap)
}

func enrichRuntime(row *nodeRow, snap *snapshot.Node) {
	rt := snap.Runtime
	if rt == nil {
		return
	}
	if rt.HasUptime {
		up := time.Duration(rt.Uptime) * time.Second
		row.Uptime = humanDuration(up)
		if up < rebootWindow {
			row.RebootedAgo = humanDuration(up)
		}
	}
	if rt.Version != "" {
		row.TalosVersion = rt.Version
	}
}

func enrichCPU(row *nodeRow, snap *snapshot.Node) {
	row.CPUModel, row.Cores, row.Threads = cpuTotalsFrom(snap)
	if c := snap.CPU; c != nil && c.HasUsage {
		row.CPUPercent = c.UsagePct
		row.HasCPU = true
		row.LoadPct = clampPct(c.UsagePct)
		row.LoadThreads = c.UsagePct / 100 * float64(row.Threads)
	}
}

func enrichMemory(row *nodeRow, snap *snapshot.Node) {
	mods, total := memoryModulesFrom(snap)
	row.RAM, row.MemDIMMs = total, populatedModules(snap)
	for _, m := range mods {
		row.memMiB += m.SizeMiB
	}
}

func enrichHealth(row *nodeRow, snap *snapshot.Node) {
	h := snap.Health
	if h == nil {
		return
	}
	row.HasHealth = true
	row.Stage, row.MachineReady = h.Stage, h.Ready
	row.ServicesTotal = len(h.Services)
	row.Diagnostics = len(h.Diagnostics)
	for _, svc := range h.Services {
		switch {
		case !svc.Running:
			row.ServicesBad++
		case svc.Unknown:
			// No health check published — not a fault.
		case svc.Healthy:
			row.ServicesHealthy++
		default:
			row.ServicesBad++
		}
	}
}

func enrichClockLoadVolumesIdentity(row *nodeRow, snap *snapshot.Node) {
	if t := snap.TimeSync; t != nil && t.HasStatus {
		row.HasClock, row.ClockSynced = true, t.Synced
	}
	if rt := snap.Runtime; rt != nil && rt.HasLoad {
		row.HasLoad, row.Load1 = true, rt.Load1
	}
	if v := snap.Volumes; v != nil {
		for _, vol := range v.Volumes {
			if !vol.Ready {
				row.VolumesNotReady++
			}
		}
	}
	row.GPUs = len(snap.GPUs)
	if id := snap.Identity; id != nil {
		row.Schematic = shortHash(id.Schematic)
	}
}

func enrichDisks(row *nodeRow, snap *snapshot.Node) {
	count, bytes := localDisksFrom(snap)
	row.Disks, row.storageBytes = count, bytes
	if count > 0 {
		row.Storage = humanize.Bytes(bytes)
	}
}

// dmiPlaceholders render as nothing: "Serial: Unknown" reads like a value. /metrics keeps the raw.
var dmiPlaceholders = map[string]bool{
	"unknown":                true,
	"not specified":          true,
	"to be filled by o.e.m.": true,
	"to be filled by o.e.m":  true,
	"default string":         true,
	"none":                   true,
	"n/a":                    true,
	"system serial number":   true,
	"chassis serial number":  true,
	"0123456789":             true,
}

// cpuModelNoise is SMBIOS marketing boilerplate; on a fleet table it costs a wrapped row.
var cpuModelNoise = strings.NewReplacer(
	"(R)", "", "(r)", "", "(TM)", "", "(tm)", "", "®", "", "™", "",
)

// Vendors are stripped after the noise pass: in the raw name they are followed by "(R)".
var cpuModelVendors = []string{"Intel ", "AMD ", "Advanced Micro Devices "}

// cpuModelCut marks where the useful part of a processor name ends.
var cpuModelCut = []string{" CPU @", " with ", " w/ ", " Processor", " APU "}

// cpuTotalsFrom sums cores and threads over sockets and takes the first product name.
func cpuTotalsFrom(snap *snapshot.Node) (model string, cores, threads int) {
	for _, sock := range cpuSocketsFrom(snap) {
		if model == "" {
			model = shortCPUModel(sock.Product)
		}
		cores += sock.Cores
		threads += sock.Threads
	}
	return model, cores, threads
}

// localDisksFrom counts physically attached disks; loops and network storage are not hardware.
func localDisksFrom(n *snapshot.Node) (count int, total uint64) {
	if n == nil {
		return 0, 0
	}
	for _, d := range n.Disks {
		if d.Attachment != "local" {
			continue
		}
		count++
		total += uint64(d.SizeBytes)
	}
	return count, total
}

// computeFleet sums capacity facts the node table cannot show and picks the busiest node.
func computeFleet(rows []nodeRow) fleet {
	var f fleet
	memMiB, storage := aggregateNodeTotals(&f, rows)
	if memMiB > 0 {
		f.Memory = displaySize(memMiB)
	}
	if storage > 0 {
		f.Storage = humanize.Bytes(storage)
	}

	schematics := aggregateImagesAndServices(&f, rows)
	resolveBusiestLoad(&f, rows)
	f.Images = len(schematics)
	if f.Images == 1 {
		for k := range schematics {
			f.ImageNote = k
		}
	}
	return f
}

// aggregateNodeTotals sums the per-row counters and finds the busiest node by CPU.
func aggregateNodeTotals(f *fleet, rows []nodeRow) (memMiB int, storage uint64) {
	for _, r := range rows {
		f.Cores += r.Cores
		f.Threads += r.Threads
		f.DIMMs += r.MemDIMMs
		f.Disks += r.Disks
		memMiB += r.memMiB
		storage += r.storageBytes
		if r.HasCPU && (!f.HasBusiest || r.CPUPercent > f.BusiestPct) {
			f.BusiestNode, f.BusiestPct, f.HasBusiest = r.Name, r.CPUPercent, true
		}
	}
	return memMiB, storage
}

// aggregateImagesAndServices counts GPUs, service health and distinct schematics.
func aggregateImagesAndServices(f *fleet, rows []nodeRow) map[string]struct{} {
	schematics := make(map[string]struct{})
	for _, r := range rows {
		if r.Schematic != "" {
			schematics[r.Schematic] = struct{}{}
		}
		f.GPUs += r.GPUs
		if r.GPUs > 0 {
			f.GPUNodes++
		}
		f.ServicesBad += r.ServicesBad
		f.ServicesTotal += r.ServicesTotal
	}
	return schematics
}

// resolveBusiestLoad runs last: the busiest node is only known once every row was compared.
func resolveBusiestLoad(f *fleet, rows []nodeRow) {
	for _, r := range rows {
		if r.Name == f.BusiestNode && r.HasLoad {
			f.HasBusiestLoad, f.BusiestLoad = true, r.Load1
		}
	}
}

// systemInfoFrom renders the node's DMI identity from the snapshot.
func systemInfoFrom(n *snapshot.Node) systemInfo {
	if n == nil || n.System == nil {
		return systemInfo{}
	}
	return systemInfo{
		Available:    true,
		Manufacturer: dmiValue(n.System.Manufacturer),
		Product:      dmiValue(n.System.Product),
		Version:      dmiValue(n.System.Version),
		SerialNumber: dmiValue(n.System.SerialNumber),
		UUID:         dmiValue(n.System.UUID),
		SKUNumber:    dmiValue(n.System.SKUNumber),
		WakeUpType:   dmiValue(n.System.WakeUpType),
	}
}
