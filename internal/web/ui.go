package web

import (
	"bytes"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/version"
	"github.com/yuriy-kovalchuk/talos-monitoring/static"
)

// nodeRow is a discovered node enriched with Talos API status. The Uptime/
// CPUPercent/RAM/MaxTemp/Disks fields are dashboard facts, filled only on
// the overview page (enrichDashboard).
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

	// System state, for the cluster dashboard's status matrix and the
	// attention rules. Each Has* guards a collector that may be disabled.
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

// rebootWindow flags a node as recently rebooted on the overview. Fixed for
// now; the attention rules are expected to become user-configurable later.
const rebootWindow = time.Hour

// fleet is the capacity summary behind the overview tiles. It deliberately
// carries only facts the node table cannot show.
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
	// BusiestLoad is the busiest node's own load average. It used to be the
	// fleet maximum, which meant the tile showed one node's CPU% above another
	// node's load — two measures, two nodes, one tile, and it read as a bug.
	HasBusiestLoad bool
	BusiestLoad    float64
}

// attentionItem is one triggered rule on the overview attention card.
type attentionItem struct {
	Severity string // "down" | "warn"
	Node     string
	Text     string
}

// attentionRule evaluates one condition across the fleet. Keeping the rules
// as a list means a configurable rule can later be added as data rather than
// as new rendering code.
type attentionRule func(rows []nodeRow) []attentionItem

// attentionRules is the enabled rule set. Only node-down and recent-reboot
// ship today; version drift, temperature thresholds and scrape health are
// planned as configurable rules.
var attentionRules = []attentionRule{
	ruleNodeDown, ruleServiceUnhealthy, ruleMachineNotReady, ruleClockUnsynced,
	ruleVolumeNotReady, ruleDiagnostics, ruleRecentReboot,
}

// ruleServiceUnhealthy fires on a service that is stopped or failing its health
// check. A service that publishes NO health check is excluded — `dashboard` and
// every `ext-*` service report unhealthy forever by construction, so counting
// them would make this rule permanently noisy.
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

// ruleClockUnsynced fires on clock skew, which breaks etcd, TLS and storage
// auth silently and shows up on no other panel.
func ruleClockUnsynced(rows []nodeRow) []attentionItem {
	var out []attentionItem
	for _, r := range rows {
		if r.Up && r.HasClock && !r.ClockSynced {
			out = append(out, attentionItem{Severity: "warn", Node: r.Name, Text: "clock not synced"})
		}
	}
	return out
}

// ruleVolumeNotReady fires on a volume that did not reach its ready phase — on
// an encrypted volume that means it did not unlock, and there is no filesystem
// for a usage metric to go wrong on.
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

// plural picks the singular or plural form for a count (the template helper's
// Go-side twin).
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

// nodeDetail is the part of pageData only a node page fills. Kept separate so
// pageData is not 30 fields of which a given page sets four; embedded rather
// than nested so templates still write {{.CPU}}, not {{.Detail.CPU}}.
//
// Which of these a page loads is decided by nodeDataFor: the disks page does
// not pay for the PCI list.
type nodeDetail struct {
	Node           *nodeRow        // the node this page is about
	Section        string          // "" (overview) | health | identity | kernel | cpu | memory | storage | links | gpu | pci | sensors
	System         systemInfo      // node overview: DMI identity
	Summary        nodeSummary     // node overview: at-a-glance counts
	Status         *nodeStatus     // live status card
	CPU            []cpuSocket     // cpu page: one row per socket
	FreqObserved   freqObserved    // cpu page: frequency range over the history window
	Memory         []memoryModule  // memory page: one row per DIMM
	MemoryTotal    string          // memory page: installed total
	MemUsage       memUsage        // memory page: live usage bar
	PCI            []pciDevice     // pci page: flat device list
	PCIGroups      []pciGroup      // pci page: devices grouped by class
	Links          []netLink       // links page: network interfaces
	Disks          []diskDevice    // disks page: one row per block device
	DisksTotal     string          // disks page: local-disk capacity
	DisksLocal     int             // physically attached disks
	DisksNetwork   int             // storage attached over the network (iSCSI, ...)
	DisksVirtual   int             // loop / device-mapper devices
	Volumes        []volumeRow     // disks page: mounted PersistentVolumes
	Filesystems    []filesystemRow // disks page: the node's own filesystems
	SensorGroups   []sensorGroup   // sensors page: readings by kind
	SensorSummary  sensorSummary   // sensors page: health strip
	Health         healthView      // health page: services, stage, diagnostics
	Identity       identityView    // identity page: image, extensions, security
	TimeSync       timeSyncView    // health page: clock discipline
	Kernel         kernelView      // identity page: tunables and modules
	VolumeLayer    volumeLayerView // disks page: Talos volume layer
	GPUs           []gpuView       // gpu page
	DiskIO         []diskIORow     // storage page: per-device I/O counters
	StorageSummary storageSummary  // storage page: the triage strip
	GPUSensors     []sensorReading // gpu page: readings from GPU hwmon chips
	NetSummary     netSummary      // links page: the triage strip
}

// pageData is the shared template data for every dashboard page.
type pageData struct {
	// Page is the navigation identity ("overview" | "nodes" | "node" |
	// "about"), which is also the template block name: the content block is
	// "<Page>-content". Template is set only when the two differ.
	Page       string
	Template   string
	Title      string
	Version    string
	Commit     string
	BuildDate  string
	Nodes      []nodeRow
	Stats      stats
	HWBase     string          // "/nodes/<name>" for the sidebar hardware links ("" = no nodes)
	AssetV     string          // embedded-asset hash, appended to asset URLs (cache busting)
	HistWindow string          // humanized in-memory graph history window (cpu page)
	Fleet      fleet           // overview capacity tiles
	Attention  []attentionItem // overview attention card (empty = all clear)
	SystemRows []nodeRow       // overview system-status matrix, one row per node

	nodeDetail
}

// contentTemplate is the block render executes for this page.
func (p pageData) contentTemplate() string {
	if p.Template != "" {
		return p.Template
	}
	return p.Page + "-content"
}

// layoutData wraps pageData with the rendered page content.
type layoutData struct {
	pageData
	Inner template.HTML // rendered "<page>-content" block
}

// systemInfo is one node's SystemInformation for the dashboard, parsed from
// the talos_hw_system_info metric (Available=false until the hwinfo
// collector has produced data for the node).
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

// dashboard renders the HTML pages from the embedded static assets.
type dashboard struct {
	log     *slog.Logger
	list    func() []nodes.Node
	status  func(name string) (string, bool) // node name → (talos version, up)
	gather  prometheus.Gatherer
	hist    *history.Store  // in-memory graph history (cpu collector data)
	snap    *snapshot.Store // typed inventory, the page data source
	volumes func() map[string]k8svolumes.Volume
}

var (
	pagesTmpl  = template.Must(template.New("").Funcs(tmplFuncs()).ParseFS(static.FS, "html/*.html"))
	statusTmpl = template.Must(template.ParseFS(static.FS, "html/partials/*.html"))
)

func tmplFuncs() template.FuncMap {
	return template.FuncMap{
		// fallback picks the first non-empty string. It is deliberately NOT
		// called "or": that name shadows the template builtin, so every
		// boolean `{{if or .A .B}}` in every template silently fails to
		// execute with "wrong type for value; expected string; got bool" —
		// and the page's content block fails (render answers 500,
		// TestRenderErrorReturns500).
		"fallback": func(a, b string) string {
			if a == "" {
				return b
			}
			return a
		},
		// plural picks the singular or plural form for a count.
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"badgeClass": func(role string) string {
			switch role {
			case "control-plane":
				return "badge-cp"
			case "worker":
				return "badge-w"
			default:
				return "badge"
			}
		},
	}
}

// row builds a single node's row without materialising the whole fleet. The
// node pages used to call rows() to find their node and then render() called
// it again, so every node page built the fleet list twice (finding 2.2.5).
func (d *dashboard) row(name string) (nodeRow, bool) {
	for _, n := range d.list() {
		if n.Name != name {
			continue
		}
		ver, up := d.status(n.Name)
		return nodeRow{Node: n, TalosVersion: ver, Up: up}, true
	}
	return nodeRow{}, false
}

func (d *dashboard) rows() []nodeRow {
	ns := d.list()
	rows := make([]nodeRow, 0, len(ns))
	for _, n := range ns {
		ver, up := d.status(n.Name)
		rows = append(rows, nodeRow{Node: n, TalosVersion: ver, Up: up})
	}
	return rows
}

func computeStats(rows []nodeRow) stats {
	s := stats{Nodes: len(rows)}
	talos, kube := make(map[string]int), make(map[string]int)
	for _, r := range rows {
		// Up counts reachability alone: a node with a version gap must not
		// vanish from the up counter while the status pill still shows it up.
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
		if n > bestN {
			best, bestN = v, n
		}
	}
	return best
}

// bufPool recycles the two-phase render buffers. Pages are tens of kilobytes,
// and every request grew one from nothing.
var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (d *dashboard) render(w http.ResponseWriter, r *http.Request, page pageData) {
	rows := d.rows()
	if page.Page == "overview" {
		rows = d.enrichDashboard(rows)
		page.Fleet = computeFleet(rows)
		page.Attention = evaluateAttention(rows)
		page.SystemRows = rows
	}
	page.Nodes = rows
	// Only the overview renders fleet stats; other pages paid for them anyway.
	if page.Page == "overview" {
		page.Stats = computeStats(rows)
	}
	page.Version = version.Version
	page.Commit = version.Commit
	page.BuildDate = version.BuildDate
	page.HWBase = d.hwBase(r, rows, page.Node)
	page.AssetV = strings.Trim(AssetVersion(), `"`)
	// hist is optional (the router's signature allows nil, and benchmarks pass
	// it); freqObservedFrom already guarded this field and render did not.
	if d.hist != nil {
		page.HistWindow = humanDuration(d.hist.Window())
	}

	// Two-phase render: the page content block first (into a buffer), then
	// the shared layout around it. Go templates cannot pass dynamic names to
	// the {{template}} action, so the dispatch happens here.
	//
	// The buffer is pooled: growing one from zero per request was 29 % of all
	// allocations in this path, and a dashboard page is tens of kilobytes.
	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufPool.Put(buf)
	}()
	if err := pagesTmpl.ExecuteTemplate(buf, page.contentTemplate(), page); err != nil {
		d.log.Error("render content failed", "page", page.Page, "err", err)
		// Headers are not written yet, so a clean 5xx is possible. An empty
		// 200 would render as a blank page that looks like a network problem.
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Pages are personal (the sidebar follows the last-viewed-node cookie),
	// so they must not survive in a proxy cache in front of the binary.
	w.Header().Set("Cache-Control", "no-store")
	// The buffer holds output of html/template (auto-escaped), not raw input.
	if err := pagesTmpl.ExecuteTemplate(w, "layout", layoutData{pageData: page, Inner: template.HTML(buf.String())}); err != nil { // #nosec G203 -- trusted template output
		d.log.Error("render layout failed", "page", page.Page, "err", err)
	}
}

func (d *dashboard) overview(w http.ResponseWriter, r *http.Request) {
	d.render(w, r, pageData{Page: "overview", Title: "Dashboard"})
}

func (d *dashboard) nodesPage(w http.ResponseWriter, r *http.Request) {
	d.render(w, r, pageData{Page: "nodes", Title: "Nodes"})
}

func (d *dashboard) nodePage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	row, ok := d.row(name)
	if ok {
		page := pageData{Page: "node", Title: name, nodeDetail: nodeDetail{Node: &row}}
		d.fillNodePage(&page, name, nodeDataFor(""))
		setNodeCookie(w, name)
		d.render(w, r, page)
		return
	}
	http.Error(w, "node not found", http.StatusNotFound)
}

// nodeSections maps the /nodes/{name}/{section} path value to its content
// template and menu label.
var nodeSections = map[string]struct {
	page  string
	label string
}{
	// System — what Talos is running.
	"health":   {page: "node_health", label: "Health"},
	"identity": {page: "node_identity", label: "Image"},
	"kernel":   {page: "node_kernel", label: "Kernel"},
	// Hardware — what is in the box.
	"cpu":     {page: "node_cpu", label: "CPU"},
	"memory":  {page: "node_memory", label: "Memory"},
	"storage": {page: "node_storage", label: "Storage"},
	"links":   {page: "node_links", label: "Network"},
	"gpu":     {page: "node_gpu", label: "GPU"},
	"pci":     {page: "node_pci", label: "PCI"},
	"sensors": {page: "node_sensors", label: "Sensors"},
}

func (d *dashboard) nodeSection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	section := r.PathValue("section")
	meta, ok := nodeSections[section]
	if !ok {
		http.Error(w, "unknown section", http.StatusNotFound)
		return
	}
	row, found := d.row(name)
	if found {
		page := pageData{Page: meta.page, Title: name + " · " + meta.label,
			nodeDetail: nodeDetail{Node: &row, Section: section}}
		d.fillNodePage(&page, name, nodeDataFor(section))
		setNodeCookie(w, name)
		d.render(w, r, page)
		return
	}
	http.Error(w, "node not found", http.StatusNotFound)
}

// nodeData selects which per-node datasets a page needs. Building all of them
// for every page is wasteful: the PCI page has no use for sensor groups, and
// the overview only wants counts, not the 35-row PCI table.
type nodeData struct {
	system   bool
	cpu      bool // processor sockets
	memory   bool // DIMM rows
	pci      bool // full PCI table
	disks    bool // full disk table
	sensors  bool // full sensor groups
	links    bool // network interfaces and their addresses
	status   bool // nodeapi status + per-core table
	summary  bool // cheap counts for the overview cards
	health   bool // service states, machine stage, diagnostics
	identity bool // schematic, extensions, boot entry, security posture
	gpu      bool // GPU utilisation and memory

	// These three ride along with an existing page rather than owning one:
	// clock discipline is a health signal, tunables and modules belong with
	// the image, and the volume layer sits above the disks.
	timesync    bool
	kernel      bool
	volumeLayer bool
	diskIO      bool
}

// nodeDataFor maps a node page section to the datasets it actually renders.
// The empty section is the node overview.
func nodeDataFor(section string) nodeData {
	switch section {
	case "cpu":
		return nodeData{cpu: true, status: true}
	case "memory":
		return nodeData{memory: true}
	case "storage":
		// The whole storage stack on one page: devices, their traffic, the
		// Talos volume layer above them, filesystems, and the PVs on top.
		return nodeData{disks: true, volumeLayer: true, diskIO: true}
	case "pci":
		return nodeData{pci: true}
	case "sensors":
		return nodeData{sensors: true}
	case "links":
		return nodeData{links: true}
	case "health":
		// Clock discipline is a health signal, so the health page renders
		// it too even though it is a separate collector.
		return nodeData{health: true, timesync: true}
	case "identity":
		return nodeData{identity: true}
	case "kernel":
		return nodeData{kernel: true, identity: true}
	case "gpu":
		// sensors too: the GPU's temperature, fan and power come from its own
		// hwmon chip, and "how is my GPU" should not need two pages.
		return nodeData{gpu: true, sensors: true}
	default: // node overview
		// The overview strip reports stage, services, load and clock, so it
		// needs health and timesync alongside the per-section summaries.
		return nodeData{system: true, cpu: true, memory: true, status: true, summary: true,
			links: true, health: true, identity: true, gpu: true, timesync: true,
			kernel: true}
	}
}

// fillNodePage fills only the datasets want asks for, from the typed snapshot
// the collectors wrote. No gather, no label parsing: one map lookup.
//
// Split into per-section helpers purely to keep each one's branch count down:
// the sections are independent and their fill order doesn't matter, except
// where a helper's own comment says otherwise.
func (d *dashboard) fillNodePage(page *pageData, name string, want nodeData) {
	snap := d.snap.Node(name)
	fillSystemCPUMemory(page, snap, want)
	fillPCISensorsLinks(page, snap, want)
	fillStorage(d, page, snap, name, want)
	fillHealthIdentityKernel(page, snap, want)
	fillGPU(page, snap, want)
	fillStatusAndSummary(d, page, snap, name, want)
	preferNodeapiVersion(page)
}

func fillSystemCPUMemory(page *pageData, snap *snapshot.Node, want nodeData) {
	if want.system {
		page.System = systemInfoFrom(snap)
	}
	if want.cpu {
		page.CPU = cpuSocketsFrom(snap)
	}
	if want.memory {
		page.Memory, page.MemoryTotal = memoryModulesFrom(snap)
		page.MemUsage = memUsageFrom(snap)
	}
}

func fillPCISensorsLinks(page *pageData, snap *snapshot.Node, want nodeData) {
	if want.pci {
		page.PCIGroups = pciGroupsFrom(snap)
	}
	if want.sensors {
		page.SensorGroups = sensorsFrom(snap)
		page.SensorSummary = sensorSummaryFrom(page.SensorGroups)
	}
	if want.links {
		page.Links = netLinksFrom(snap)
		page.NetSummary = netSummaryFrom(page.Links)
	}
}

func fillStorage(d *dashboard, page *pageData, snap *snapshot.Node, name string, want nodeData) {
	if want.disks {
		page.Volumes = d.volumesFrom(snap, name)
		page.Filesystems = filesystemsFrom(snap)
		page.Disks, page.DisksTotal = diskDevicesFrom(snap)
		countDisksByAttachment(page)
	}
	if want.volumeLayer {
		page.VolumeLayer = volumeLayerFrom(snap)
	}
	if want.diskIO {
		page.DiskIO = diskIOFrom(snap)
	}
	if want.disks && want.volumeLayer {
		page.StorageSummary = storageSummaryFrom(page)
	}
}

func countDisksByAttachment(page *pageData) {
	for _, disk := range page.Disks {
		switch disk.Attachment {
		case "local":
			page.DisksLocal++
		case "network":
			page.DisksNetwork++
		default:
			page.DisksVirtual++
		}
	}
}

func fillHealthIdentityKernel(page *pageData, snap *snapshot.Node, want nodeData) {
	if want.health {
		page.Health = healthFrom(snap)
	}
	if want.identity {
		page.Identity = identityFrom(snap)
	}
	if want.timesync {
		page.TimeSync = timeSyncFrom(snap)
	}
	if want.kernel {
		page.Kernel = kernelFrom(snap)
	}
}

func fillGPU(page *pageData, snap *snapshot.Node, want nodeData) {
	if want.gpu {
		page.GPUSensors = gpuSensorsFrom(page.SensorGroups)
		page.GPUs = gpusFrom(snap)
	}
}

func fillStatusAndSummary(d *dashboard, page *pageData, snap *snapshot.Node, name string, want nodeData) {
	if want.status {
		page.Status = nodeStatusFrom(snap)
	}
	if want.cpu {
		page.FreqObserved = freqObservedFrom(d.hist, name)
	}
	if want.summary {
		page.Summary = nodeSummaryFrom(snap)
	}
}

// preferNodeapiVersion prefers the nodeapi collector's Talos version over the
// client pool's: the pool only learns a version when it verifies a node, and
// it never re-verifies (finding 1.2), so nodeapi's is the current one wherever
// both exist.
func preferNodeapiVersion(page *pageData) {
	if page.Node != nil && page.Status != nil && page.Status.Version != "" {
		page.Node.TalosVersion = page.Status.Version
	}
}

// nodeSummary is the node overview's at-a-glance data: the counts behind the
// category cards, plus the live figures for the health strip. It is built from
// counters rather than from the full tables those pages render.
type nodeSummary struct {
	Model       string // shortened processor product
	Socket      string
	Cores       int
	Threads     int
	MaxMHz      int
	LoadPct     int // clamped, for the inline bar
	MemoryTotal string
	MemoryDIMMs int
	MemorySpeed int // MHz, first populated module
	DiskLocal   int
	DiskNetwork int
	DiskVirtual int
	Storage     string
	PCIDevices  int
	SensorKinds []sensorKindCount
	MaxTemp     string
	TempSensor  string
	Links       int // physical NICs
	LinksUp     int
}

// sensorKindCount is one "5 temperature" line on the sensors summary card.
type sensorKindCount struct {
	Kind  string
	Count int
}

func nodeSummaryFrom(snap *snapshot.Node) nodeSummary {
	var s nodeSummary
	summaryCPU(&s, snap)
	summaryMemory(&s, snap)
	summaryDisks(&s, snap)
	if snap != nil {
		s.PCIDevices = len(snap.PCI)
	}
	summarySensorKinds(&s, snap)
	summaryLinks(&s, snap)
	if v, sensor, ok := maxTempFrom(snap); ok {
		s.MaxTemp = strconv.FormatFloat(v, 'f', 1, 64) + " °C"
		s.TempSensor = sensor
	}
	return s
}

func summaryCPU(s *nodeSummary, snap *snapshot.Node) {
	for _, sock := range cpuSocketsFrom(snap) {
		if s.Model == "" {
			s.Model, s.Socket, s.MaxMHz = shortCPUModel(sock.Product), sock.Socket, sock.MaxMHz
		}
		s.Cores += sock.Cores
		s.Threads += sock.Threads
	}
	if snap != nil && snap.CPU != nil && snap.CPU.HasUsage {
		s.LoadPct = clampPct(snap.CPU.UsagePct)
	}
}

func summaryMemory(s *nodeSummary, snap *snapshot.Node) {
	mods, total := memoryModulesFrom(snap)
	s.MemoryTotal, s.MemoryDIMMs = total, len(mods)
	for _, m := range mods {
		if m.SpeedMHz > 0 {
			s.MemorySpeed = m.SpeedMHz
			break
		}
	}
}

// summaryDisks counts disks by attachment without building the full table.
func summaryDisks(s *nodeSummary, snap *snapshot.Node) {
	var localBytes uint64
	if snap != nil {
		for _, d := range snap.Disks {
			switch d.Attachment {
			case "local":
				s.DiskLocal++
				localBytes += uint64(d.SizeBytes)
			case "network":
				s.DiskNetwork++
			default:
				s.DiskVirtual++
			}
		}
	}
	if s.DiskLocal > 0 {
		s.Storage = humanize.Bytes(localBytes)
	}
}

func summarySensorKinds(s *nodeSummary, snap *snapshot.Node) {
	byKind := map[string]int{}
	if snap != nil {
		for _, sn := range snap.Sensors {
			if sn.Kind != "" && sn.Kind != "unknown" {
				byKind[sn.Kind]++
			}
		}
	}
	for _, kind := range sensorKindOrder {
		if n := byKind[kind]; n > 0 {
			s.SensorKinds = append(s.SensorKinds, sensorKindCount{Kind: kind, Count: n})
		}
	}
}

func summaryLinks(s *nodeSummary, snap *snapshot.Node) {
	for _, l := range netLinksFrom(snap) {
		if !l.Physical {
			continue
		}
		s.Links++
		if l.Up {
			s.LinksUp++
		}
	}
}

// maxTempFrom returns the node's hottest temperature reading.
func maxTempFrom(n *snapshot.Node) (float64, string, bool) {
	best, sensor, found := 0.0, "", false
	if n == nil {
		return 0, "", false
	}
	for _, sn := range n.Sensors {
		if sn.Kind != "temperature" || (found && sn.Value <= best) {
			continue
		}
		name := sn.Label
		if name == "" {
			name = sn.Sensor
		}
		best, sensor, found = sn.Value, sn.ChipName+"/"+name, true
	}
	return best, sensor, found
}

// nodeCookie remembers the last viewed node so the sidebar hardware links
// work from any page.
const nodeCookie = "tm-node"

func setNodeCookie(w http.ResponseWriter, name string) {
	// No Secure flag: the dashboard is served over plain http in local dev
	// (Secure would make the browser drop the cookie).
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- UI-preference cookie, not a security boundary
		Name:     nodeCookie,
		Value:    name,
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
	})
}

// hwBase is the sidebar hardware link base: the current node on node pages,
// else the last-viewed node (cookie), else the first discovered node; "" when
// there are no nodes at all.
func (d *dashboard) hwBase(r *http.Request, rows []nodeRow, node *nodeRow) string {
	if node != nil {
		return "/nodes/" + node.Name
	}
	byName := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		byName[row.Name] = struct{}{}
	}
	if c, err := r.Cookie(nodeCookie); err == nil {
		if _, ok := byName[c.Value]; ok {
			return "/nodes/" + c.Value
		}
	}
	if len(rows) > 0 {
		return "/nodes/" + rows[0].Name
	}
	return ""
}

// enrichDashboard fills the per-node summary columns (uptime, CPU usage, RAM,
// max temperature, disk count) from the snapshot.
//
// Reachability stays with rows[i].Up, which the caller took from the scraper's
// last-round result; the client pool's verified flag is only ever set, never
// cleared.
func (d *dashboard) enrichDashboard(rows []nodeRow) []nodeRow {
	for i := range rows {
		if snap := d.snap.Node(rows[i].Name); snap != nil {
			enrichRow(&rows[i], snap)
		}
	}
	return rows
}

// enrichRow fills one row's summary columns from its snapshot. Split along the
// snapshot sections it reads, purely to keep each part's complexity down —
// the sections are independent and order between them doesn't matter.
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
	row.RAM, row.MemDIMMs = total, len(mods)
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

// clampPct bounds a percentage to 0..100 for the inline load bar.
func clampPct(v float64) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return int(v + 0.5)
	}
}

// dmiPlaceholders are the strings firmware writes when a DMI field was never
// populated. Rendering them is worse than rendering nothing: "Serial: Unknown"
// reads like a value. The raw label stays in /metrics for anyone who wants it —
// only the UI cleans.
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

// dmiValue blanks a DMI placeholder so the template's "–" fallback takes over.
// It also drops the "Unknown - [0x9B05]" form, which is an unresolved JEDEC
// vendor id rather than a manufacturer name.
func dmiValue(v string) string {
	t := strings.TrimSpace(v)
	if dmiPlaceholders[strings.ToLower(t)] || strings.HasPrefix(strings.ToLower(t), "unknown - [") {
		return ""
	}
	return t
}

// cpuModelNoise is the marketing boilerplate SMBIOS puts in processor names.
// "Intel(R) Core(TM) i5-6400T CPU @ 2.20GHz" is 40 characters of which 14 carry
// information, and on a fleet table that difference is a wrapped row.
var cpuModelNoise = strings.NewReplacer(
	"(R)", "", "(r)", "", "(TM)", "", "(tm)", "", "®", "", "™", "",
)

// cpuModelVendors are stripped only after the noise pass: in the raw string the
// vendor is followed by "(R)", not a space, so a single replacer never sees it.
// The family name (Ryzen, Core, Xeon, EPYC) already identifies the vendor.
var cpuModelVendors = []string{"Intel ", "AMD ", "Advanced Micro Devices "}

// cpuModelCut marks where the useful part of a processor name ends.
var cpuModelCut = []string{" CPU @", " with ", " w/ ", " Processor", " APU "}

// shortCPUModel trims a SMBIOS processor name to the part that identifies the
// part: "AMD Ryzen 7 6800H with Radeon Graphics" becomes "Ryzen 7 6800H".
// An unrecognised name is returned collapsed but otherwise intact.
func shortCPUModel(name string) string {
	out := strings.Join(strings.Fields(cpuModelNoise.Replace(name)), " ")
	for _, cut := range cpuModelCut {
		if i := strings.Index(out, cut); i > 0 {
			out = out[:i]
		}
	}
	for _, v := range cpuModelVendors {
		if rest, ok := strings.CutPrefix(out, v); ok {
			out = rest
			break
		}
	}
	return strings.TrimSpace(out)
}

// cpuTotalsFrom sums cores and threads over a node's sockets and returns the
// first socket's product name.
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

// localDisksFrom counts the physically attached disks and their capacity.
// Loop devices and network-attached storage are not this machine's hardware.
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

// computeFleet sums the capacity facts the node table cannot show and picks
// the busiest node.
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

// aggregateNodeTotals sums the per-row counters shared across the fleet and
// finds the busiest node by CPU. Returns the total RAM (MiB) and storage
// (bytes) for the caller to format.
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

// aggregateImagesAndServices counts GPUs, service health, and distinct
// schematics: image consistency, i.e. how many distinct schematics the fleet
// is running. More than one is not automatically wrong — a GPU node
// legitimately runs a different image — but it is the first thing to check
// after an upgrade.
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

// resolveBusiestLoad runs after aggregateNodeTotals: the busiest node is only
// known once every row has been compared.
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

// cpuSocket is one socket row for the dashboard, parsed from
// talos_hw_processor_info labels.
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

// cpuSocketsFrom renders the socket rows from the snapshot, which the
// collector already sorted by designation.
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

// cpuMode is one /proc/stat mode's share of the interval, ready to render.
// Tone drives the bar colour: iowait, steal and irq are the modes that mean
// "this node is waiting on something else", and they read differently from
// user/system time.
type cpuMode struct {
	Mode  string
	Pct   string
	Width float64
	Tone  string // busy | wait | idle
}

// freqObserved is the frequency actually seen over the history window.
//
// A single "current frequency" is false precision: this hardware legitimately
// swings between its minimum and maximum many times a second, and the scrape
// that reads it briefly raises it. A range over the window is the honest form.
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

// cpuPolicy is the node's cpufreq policy, collapsed. On every machine seen so
// far the governor and the min/max bounds are identical across all cores, so
// the old per-core table repeated the same three values once per core.
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

// nodeStatusFrom builds the node's live status card from the snapshot.
// Returns nil when nothing has been collected, which the page renders as
// "no data yet" rather than as a machine reporting zeros.
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
		// Modes below a tenth of a percent are noise on a bar chart and push
		// the readable ones off the row.
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
		// A node with no cpufreq data reports 0 (amd_pstate kernels lack the
		// legacy cpuinfo_cur_freq file the Talos RPC reads); the card renders
		// that as n/a, not as "0 MHz".
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

// collapsePolicy reduces the per-core governor/min/max to one statement,
// flagging Mixed when the cores genuinely disagree.
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

// humanDuration renders an uptime as "2d 4h3m" / "5h 12m3s" / "3m 7s".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh%dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm%ds", hours, mins, secs)
	case mins > 0 && secs == 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
}

// freqMHz renders a kilohertz frequency as megahertz (no decimals when whole).
func freqMHz(hz float64) string {
	mhz := hz / 1e6
	if mhz == math.Trunc(mhz) {
		return strconv.FormatInt(int64(mhz), 10)
	}
	return strconv.FormatFloat(mhz, 'f', 1, 64)
}

// memUsage is a node's live memory picture for the memory page.
//
// The split follows `free`: used = total - free - buffers - cached, so the
// three bar segments add up to total. "Available" is reported separately
// because it is the number that actually answers "can more be scheduled here",
// and it is not the same as free — on the reference cluster a node showed
// 1.2 GiB free but 8.6 GiB available.
type memUsage struct {
	Available bool

	Total     string
	Used      string
	BuffCache string
	Free      string
	Avail     string

	UsedPct      int // bar segments, in percent of total
	BuffCachePct int
	FreePct      int
	UsedPctLabel string

	HasSwap   bool
	SwapUsed  string
	SwapTotal string

	Committed        string
	CommitLimit      string
	CommitPct        int
	CommitOvercommit bool
}

// memUsageFrom renders the memory bar and swap/commit context.
func memUsageFrom(n *snapshot.Node) memUsage {
	var m memUsage
	if n == nil || n.Memory == nil || !n.Memory.Present || n.Memory.Total == 0 {
		return m
	}
	mem := n.Memory
	total := uint64(mem.Total)
	free, avail := uint64(mem.Free), uint64(mem.Available)
	buffCache := uint64(mem.Buffers) + uint64(mem.Cached)
	used := total
	if free+buffCache < total {
		used = total - free - buffCache
	}

	m.Available = true
	m.Total = humanize.IBytes(total)
	m.Used = humanize.IBytes(used)
	m.BuffCache = humanize.IBytes(buffCache)
	m.Free = humanize.IBytes(free)
	m.Avail = humanize.IBytes(avail)
	m.UsedPct = pctOf(used, total)
	m.BuffCachePct = pctOf(buffCache, total)
	m.FreePct = 100 - m.UsedPct - m.BuffCachePct
	if m.FreePct < 0 {
		m.FreePct = 0
	}
	m.UsedPctLabel = strconv.Itoa(m.UsedPct)

	if swapTotal := uint64(mem.SwapTotal); swapTotal > 0 {
		m.HasSwap = true
		m.SwapTotal = humanize.IBytes(swapTotal)
		m.SwapUsed = humanize.IBytes(swapTotal - uint64(mem.SwapFree))
	}

	if limit := uint64(mem.CommitLimit); limit > 0 {
		committed := uint64(mem.Committed)
		m.Committed = humanize.IBytes(committed)
		m.CommitLimit = humanize.IBytes(limit)
		m.CommitPct = pctOf(committed, limit)
		m.CommitOvercommit = committed > limit
	}
	return m
}

// pctOf is a whole-percent share, clamped to 0..100.
func pctOf(part, whole uint64) int {
	if whole == 0 {
		return 0
	}
	p := int(float64(part) / float64(whole) * 100)
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	default:
		return p
	}
}

// memoryModule is one DIMM row for the dashboard, parsed from
// talos_hw_memory_module_info labels.
type memoryModule struct {
	Slot          string
	SizeMiB       int
	SizeDisplay   string
	SpeedMHz      int
	Manufacturer  string
	Product       string
	SerialNumber  string
	AssetTag      string
	DeviceLocator string
	BankLocator   string
}

// memoryModulesFrom renders the DIMM rows and the installed total.
func memoryModulesFrom(n *snapshot.Node) ([]memoryModule, string) {
	if n == nil || len(n.Modules) == 0 {
		return nil, ""
	}
	out := make([]memoryModule, 0, len(n.Modules))
	totalMiB := 0
	for _, mod := range n.Modules {
		mib := int(mod.SizeBytes / (1024 * 1024))
		totalMiB += mib
		out = append(out, memoryModule{
			Slot:          mod.Slot,
			SizeMiB:       mib,
			SizeDisplay:   displaySize(mib),
			SpeedMHz:      int(mod.SpeedTransfer / 1e6),
			Manufacturer:  dmiValue(mod.Manufacturer),
			Product:       dmiValue(mod.Product),
			SerialNumber:  dmiValue(mod.SerialNumber),
			AssetTag:      dmiValue(mod.AssetTag),
			DeviceLocator: mod.DeviceLocator,
			BankLocator:   mod.BankLocator,
		})
	}
	return out, displaySize(totalMiB)
}

// displaySize renders a MiB count as GiB when it divides evenly.
func displaySize(mib int) string {
	if mib%1024 == 0 && mib != 0 {
		return fmt.Sprintf("%d GiB", mib/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

// pciDevice is one row for the PCI card, parsed from talos_hw_pcidevice_info
// labels and enriched with the sysfs detail.
type pciDevice struct {
	BDF       string
	Class     string // class, with subclass appended when non-empty
	Vendor    string
	Product   string
	VendorID  string
	ProductID string
	Driver    string
	Revision  string
	Subsystem string // subsystem vendor:device, the OEM card identity

	// Link state; HasLink is false on bridges and root ports, which have none.
	HasLink    bool
	LinkSpeed  string
	LinkWidth  string
	MaxSpeed   string
	MaxWidth   string
	Degraded   bool // negotiated below what the device supports
	PowerState string
	NUMANode   string
	AERErrors  float64
	HasAER     bool
}

// pciGroup is one class of devices on the PCI page. A flat list of every BDF is
// a dump; you come to this page looking for one kind of thing.
type pciGroup struct {
	Class    string
	Devices  []pciDevice
	Degraded int // devices in this class negotiating below their maximum
	Errors   int // devices in this class reporting AER errors
}

// NeedsAttention reports whether this class holds a degraded link or a device
// reporting errors. A bool, because html/template refuses an int-valued
// pipeline in an attribute position.
func (g pciGroup) NeedsAttention() bool { return g.Degraded > 0 || g.Errors > 0 }

// pciGroupsFrom groups a node's devices by class, worst first: classes holding
// a degraded link or an AER error come before healthy ones.
func pciGroupsFrom(n *snapshot.Node) []pciGroup {
	devices := pciDevicesFrom(n)
	if len(devices) == 0 {
		return nil
	}
	byClass := map[string][]pciDevice{}
	for _, d := range devices {
		byClass[d.Class] = append(byClass[d.Class], d)
	}
	out := make([]pciGroup, 0, len(byClass))
	for class, devs := range byClass {
		g := pciGroup{Class: class, Devices: devs}
		for _, d := range devs {
			if d.Degraded {
				g.Degraded++
			}
			if d.AERErrors > 0 {
				g.Errors++
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].Degraded+out[i].Errors, out[j].Degraded+out[j].Errors
		if li != lj {
			return li > lj
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// trimFloat renders a float without a trailing ".0".
func trimFloat(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// pciDevicesFrom renders the PCI rows from the snapshot, already sorted by BDF.
func pciDevicesFrom(n *snapshot.Node) []pciDevice {
	if n == nil {
		return nil
	}
	out := make([]pciDevice, 0, len(n.PCI))
	for _, d := range n.PCI {
		class := d.Class
		if d.Subclass != "" && d.Subclass != class {
			class += " / " + d.Subclass
		}
		dev := pciDevice{
			BDF:       d.BDF,
			Class:     class,
			Vendor:    dmiValue(d.Vendor),
			Product:   dmiValue(d.Product),
			VendorID:  d.VendorID,
			ProductID: d.ProductID,
			Driver:    d.Driver,
			Revision:  d.Revision,
		}
		if d.SubsystemVendorID != "" || d.SubsystemProductID != "" {
			dev.Subsystem = d.SubsystemVendorID + ":" + d.SubsystemProductID
		}
		if d.HasLink {
			dev.HasLink = true
			dev.LinkSpeed = trimFloat(d.LinkSpeedGTps) + " GT/s"
			dev.MaxSpeed = trimFloat(d.MaxSpeedGTps) + " GT/s"
			dev.LinkWidth = "x" + trimFloat(d.LinkWidth)
			dev.MaxWidth = "x" + trimFloat(d.MaxWidth)
			// A bridge or root port negotiates whatever the device behind it
			// needs, and PCIe links also downshift when idle (ASPM), so "below
			// maximum" is normal there. Only flag endpoints, or the page cries
			// wolf: on the reference node 3 of 5 PCI bridges sit below their
			// maximum with nothing wrong.
			dev.Degraded = (d.LinkSpeedGTps < d.MaxSpeedGTps || d.LinkWidth < d.MaxWidth) &&
				!strings.HasPrefix(dev.Class, "Bridge")
		}
		if d.HasNUMA && d.NUMANode >= 0 {
			dev.NUMANode = trimFloat(d.NUMANode)
		}
		dev.PowerState = d.PowerState
		if d.HasAER {
			dev.HasAER = true
			dev.AERErrors = d.AERCorrectable + d.AERFatal + d.AERNonFatal
		}
		out = append(out, dev)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// volumeRow is one mounted PersistentVolume: node-side usage from Talos joined
// with Kubernetes identity on the PV name.
type volumeRow struct {
	PV           string
	Namespace    string
	Claim        string
	StorageClass string
	Driver       string
	Node         string
	Device       string
	Kind         string // "block" | "network"
	Size         string
	Used         string
	Requested    string
	UsedPct      int
	Named        bool // false when Kubernetes has no PV for this mount
}

// Label is the human name of the volume: namespace/claim when Kubernetes knows
// it, otherwise the raw PV name.
func (v volumeRow) Label() string {
	if v.Namespace != "" && v.Claim != "" {
		return v.Namespace + "/" + v.Claim
	}
	return v.PV
}

// filesystemRow is one of the node's own filesystems.
type filesystemRow struct {
	MountPoint string
	Device     string
	Size       string
	Used       string
	Avail      string
	UsedPct    int
}

// volumesFrom renders the mounted PersistentVolumes, joined with what
// Kubernetes knows about each one.
func (d *dashboard) volumesFrom(n *snapshot.Node, node string) []volumeRow {
	if n == nil {
		return nil
	}
	var known map[string]k8svolumes.Volume
	if d.volumes != nil {
		known = d.volumes()
	}

	var out []volumeRow
	for _, fs := range n.Filesystems {
		if fs.PV == "" {
			continue
		}
		// Binary units throughout: Kubernetes states capacity in GiB, so
		// rendering the filesystem size in SI GB next to it made a ~2%
		// metadata overhead look like a ~7% one, and could show a size
		// larger than the request.
		row := volumeRow{
			PV:     fs.PV,
			Node:   node,
			Size:   humanize.IBytes(uint64(fs.SizeBytes)),
			Used:   humanize.IBytes(uint64(fs.UsedBytes)),
			Device: fs.Device,
			Kind:   fs.Kind,
		}
		if fs.SizeBytes > 0 {
			row.UsedPct = pctOf(uint64(fs.UsedBytes), uint64(fs.SizeBytes))
		}
		if v, ok := known[fs.PV]; ok {
			row.Named = true
			row.Namespace, row.Claim = v.Namespace, v.Claim
			row.StorageClass, row.Driver = v.StorageClass, v.Driver
			if v.Capacity > 0 {
				row.Requested = humanize.IBytes(uint64(v.Capacity))
			}
		}
		out = append(out, row)
	}
	// Fullest first: the reason to look at this table is to find what is filling up.
	sort.Slice(out, func(i, j int) bool {
		if out[i].UsedPct != out[j].UsedPct {
			return out[i].UsedPct > out[j].UsedPct
		}
		return out[i].Label() < out[j].Label()
	})
	return out
}

// filesystemsFrom renders the node's own filesystems (PersistentVolumes are
// listed separately, by volumesFrom).
func filesystemsFrom(n *snapshot.Node) []filesystemRow {
	if n == nil {
		return nil
	}
	var out []filesystemRow
	for _, fs := range n.Filesystems {
		if fs.PV != "" || fs.SizeBytes <= 0 {
			continue
		}
		out = append(out, filesystemRow{
			MountPoint: fs.Mountpoint,
			Device:     fs.Device,
			Size:       humanize.IBytes(uint64(fs.SizeBytes)),
			Used:       humanize.IBytes(uint64(fs.UsedBytes)),
			Avail:      humanize.IBytes(uint64(fs.AvailBytes)),
			UsedPct:    pctOf(uint64(fs.UsedBytes), uint64(fs.SizeBytes)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MountPoint < out[j].MountPoint })
	return out
}

// diskDevice is one row for the disks card, parsed from
// talos_block_disk_info labels.
type diskDevice struct {
	Device     string
	Size       string // pretty size from the metric
	Model      string
	Serial     string
	Transport  string
	Media      string // "CD-ROM" | "HDD" | "SSD"
	WWID       string
	Attachment string // "local" | "network" | "virtual"
}

// diskDevicesFrom renders the disk rows and the locally attached total.
func diskDevicesFrom(n *snapshot.Node) ([]diskDevice, string) {
	if n == nil || len(n.Disks) == 0 {
		return nil, ""
	}
	out := make([]diskDevice, 0, len(n.Disks))
	totalBytes := uint64(0)
	for _, d := range n.Disks {
		media := "SSD"
		switch {
		case d.CDROM:
			media = "CD-ROM"
		case d.Rotational:
			media = "HDD"
		case d.Transport == "":
			media = "–" // loop/virtual devices have no media type
		}
		if d.Attachment == "local" {
			totalBytes += uint64(d.SizeBytes)
		}
		out = append(out, diskDevice{
			Device:     d.Device,
			Size:       humanize.Bytes(uint64(d.SizeBytes)),
			Model:      dmiValue(d.Model),
			Serial:     dmiValue(d.Serial),
			Transport:  d.Transport,
			Media:      media,
			WWID:       d.WWID,
			Attachment: d.Attachment,
		})
	}
	return out, humanize.Bytes(totalBytes)
}

// netLink is one network interface for the links page.
type netLink struct {
	Name      string
	Type      string
	Kind      string // "" for a physical NIC
	Physical  bool
	HWAddr    string
	Driver    string
	Versions  string // driver / firmware, when reported
	BusPath   string
	PCIID     string
	Vendor    string
	Product   string
	Port      string
	Duplex    string
	Up        bool
	Carrier   bool
	Speed     string // "" when the link has no carrier
	MTU       string
	Addresses []netAddress

	// Live throughput and error counters.
	HasRates bool
	RxRate   string
	TxRate   string
	// Raw bytes/second behind RxRate/TxRate, so the page summary can sum them
	// without parsing the formatted strings back.
	rxPerSecond float64
	txPerSecond float64
	RxDropped   float64
	TxDropped   float64
	RxErrors    float64
	TxErrors    float64
	HasProblem  bool // any non-zero drop or error counter

	// Cumulative totals since boot. The page collapsed the error and drop
	// counters into the Issues string alone, so a link with errors showed a
	// word and never a number; MTU and HWAddr were populated here and never
	// rendered at all.
	HasTotals bool
	RxTotal   string
	TxTotal   string

	// Issues is the short form shown in the interfaces table.
	Issues string
}

// netAddress is one address configured on an interface.
type netAddress struct {
	Address string
	Family  string
	Scope   string
}

// netLinksFrom renders the interface rows from the snapshot.
func netLinksFrom(n *snapshot.Node) []netLink {
	if n == nil {
		return nil
	}
	out := make([]netLink, 0, len(n.Links))
	for _, l := range n.Links {
		link := netLink{
			Name:     l.Name,
			Type:     l.Type,
			Kind:     l.Kind,
			Physical: l.Kind == "",
			HWAddr:   l.HWAddr,
			Driver:   l.Driver,
			BusPath:  l.BusPath,
			PCIID:    l.PCIID,
			Vendor:   dmiValue(l.Vendor),
			Product:  dmiValue(l.Product),
			Port:     l.Port,
			Duplex:   l.Duplex,
			Up:       l.Up,
			Carrier:  l.Carrier,
			MTU:      trimFloat(l.MTUBytes),
		}
		if dv, fv := l.DriverVersion, l.FirmwareVersion; dv != "" || fv != "" {
			link.Versions = strings.TrimSpace(strings.Trim(dv+" / "+fv, "/ "))
		}
		if l.SpeedMbit > 0 {
			link.Speed = formatLinkSpeed(l.SpeedMbit)
		}
		if l.HasRates {
			link.HasRates = true
			link.RxRate = formatRate(l.RxPerSecond)
			link.TxRate = formatRate(l.TxPerSecond)
			link.rxPerSecond, link.txPerSecond = l.RxPerSecond, l.TxPerSecond
		}
		if l.HasCounters {
			link.HasTotals = true
			link.RxTotal = humanize.IBytes(uint64(l.RxBytes))
			link.TxTotal = humanize.IBytes(uint64(l.TxBytes))
		}
		link.RxDropped, link.TxDropped = l.RxDropped, l.TxDropped
		link.RxErrors, link.TxErrors = l.RxErrors, l.TxErrors
		link.HasProblem = link.RxDropped+link.TxDropped+link.RxErrors+link.TxErrors > 0
		link.Issues = summariseIssues(link)
		for _, a := range l.Addresses {
			link.Addresses = append(link.Addresses, netAddress{
				Address: a.Address, Family: a.Family, Scope: a.Scope,
			})
		}
		out = append(out, link)
	}
	if len(out) == 0 {
		return nil
	}
	// Carrying traffic first, then the rest of the hardware, then the logical
	// interfaces configured on top of it. A down bond should never sit above a
	// live NIC.
	rank := func(l netLink) int {
		switch {
		case l.Physical && l.Up:
			return 0
		case l.Physical:
			return 1
		default:
			return 2
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// summariseIssues is the one-line form of the drop and error counters for the
// interfaces table; the detail card carries the full breakdown.
func summariseIssues(l netLink) string {
	var parts []string
	if d := l.RxDropped + l.TxDropped; d > 0 {
		parts = append(parts, trimFloat(d)+" dropped")
	}
	if e := l.RxErrors + l.TxErrors; e > 0 {
		parts = append(parts, trimFloat(e)+" errors")
	}
	return strings.Join(parts, " · ")
}

// formatRate renders a byte-per-second throughput.
func formatRate(bps float64) string {
	if bps < 1 {
		return "0 B/s"
	}
	return humanize.Bytes(uint64(bps)) + "/s"
}

// formatLinkSpeed renders megabits as Gbit/s once it reaches 1000.
func formatLinkSpeed(mbit float64) string {
	if mbit >= 1000 {
		return trimFloat(mbit/1000) + " Gbit/s"
	}
	return trimFloat(mbit) + " Mbit/s"
}

// sensorReading is one row of a sensor card.
type sensorReading struct {
	Chip     string  // hwmon directory base name (hwmon0, thermal_zone0)
	ChipName string  // driver name (acpitz, k10temp, ...)
	Sensor   string  // sysfs sensor (temp1, fan0, in0, ...)
	Label    string  // optional human label (Tctl, edge, ...)
	value    float64 // raw sensor value; Value is its display format
	Value    string  // formatted sensor value

	// Threshold context. HasLimit is false for drivers that publish none,
	// which is common: k10temp and amdgpu do not, coretemp and nvme do.
	HasLimit bool
	Limit    string
	LimitOf  string // "critical" or "max"
	Pct      int
	Alarm    bool
}

// Name is the human identifier: the driver's label when there is one.
func (r sensorReading) Name() string {
	if r.Label != "" {
		return r.Label
	}
	return r.Sensor
}

// sensorGroup is one card on the sensors page: all readings of a single
// kind.
type sensorGroup struct {
	Title string // card title (Temperature, Fan, ...)
	Unit  string // unit of the displayed values (°C, RPM, ...)
	Rows  []sensorReading
	Chips int // distinct chips in this group
}

// sensorSummary is the sensors page health strip.
type sensorSummary struct {
	Available bool
	Hottest   string
	HottestAt string
	Closest   string // percent of limit, as text
	ClosestAt string
	Alarms    int
	Sensors   int
	Chips     int
}

// sensorKindOrder is the sensors page card order (most interesting first).
var sensorKindOrder = []string{"temperature", "fan", "voltage", "power", "frequency"}

// sensorKindTitles maps the kind label to the card title and display unit.
var sensorKindTitles = map[string]string{
	"temperature": "Temperature",
	"fan":         "Fan",
	"voltage":     "Voltage",
	"power":       "Power",
	"frequency":   "Frequency",
}

var sensorUnits = map[string]string{
	"temperature": "°C",
	"fan":         "RPM",
	"voltage":     "V",
	"power":       "W",
	"frequency":   "MHz",
}

// sensorPrecision is how many decimals each kind is rendered with. %g used to
// turn a low voltage into 1.23e-05.
var sensorPrecision = map[string]int{
	"temperature": 1, "fan": 0, "voltage": 3, "power": 1, "frequency": 0,
}

// sensorsFrom groups a node's readings by kind, from the snapshot.
func sensorsFrom(n *snapshot.Node) []sensorGroup {
	if n == nil {
		return nil
	}
	byKind := map[string][]sensorReading{}
	chips := map[string]map[string]struct{}{}
	for _, sn := range n.Sensors {
		kind := sn.Kind
		if kind == "" || kind == "unknown" {
			continue
		}
		r := sensorReading{
			Chip:     sn.Chip[strings.LastIndex(sn.Chip, "/")+1:],
			ChipName: sn.ChipName,
			Sensor:   sn.Sensor,
			Label:    sn.Label,
			value:    sn.Value,
			Value:    strconv.FormatFloat(sn.Value, 'f', sensorPrecision[kind], 64),
		}
		// Critical wins over max: it is the threshold that matters, and most
		// drivers publishing both set critical higher.
		for _, of := range []string{"critical", "max"} {
			if limit, ok := sn.Limits[of]; ok && limit > 0 {
				r.HasLimit, r.LimitOf = true, of
				r.Limit = strconv.FormatFloat(limit, 'f', sensorPrecision[kind], 64)
				r.Pct = pctOf(uint64(sn.Value*1000), uint64(limit*1000))
				break
			}
		}
		r.Alarm = sn.HasAlarm && sn.Alarm > 0
		byKind[kind] = append(byKind[kind], r)
		if chips[kind] == nil {
			chips[kind] = map[string]struct{}{}
		}
		chips[kind][sn.Chip] = struct{}{}
	}
	var groups []sensorGroup
	for _, kind := range sensorKindOrder {
		rows := byKind[kind]
		if len(rows) == 0 {
			continue
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Alarm != rows[j].Alarm {
				return rows[i].Alarm
			}
			if rows[i].HasLimit != rows[j].HasLimit {
				return rows[i].HasLimit
			}
			if rows[i].Pct != rows[j].Pct {
				return rows[i].Pct > rows[j].Pct
			}
			return rows[i].Name() < rows[j].Name()
		})
		groups = append(groups, sensorGroup{
			Title: sensorKindTitles[kind],
			Unit:  sensorUnits[kind],
			Rows:  rows,
			Chips: len(chips[kind]),
		})
	}
	return groups
}

// sensorSummaryFrom builds the sensors health strip.
func sensorSummaryFrom(groups []sensorGroup) sensorSummary {
	var s sensorSummary
	var hottest, closest float64
	for _, g := range groups {
		s.Sensors += len(g.Rows)
		s.Chips += g.Chips
		for _, r := range g.Rows {
			if r.Alarm {
				s.Alarms++
			}
			if g.Title != "Temperature" {
				continue
			}
			s.Available = true
			// The raw value, not a re-parse of the display format: parsing back
			// loses precision and the project pattern is to keep raw values
			// alongside their formatting.
			if r.value > hottest {
				hottest, s.Hottest, s.HottestAt = r.value, r.Value+" °C", r.ChipName+"/"+r.Name()
			}
			if r.HasLimit && float64(r.Pct) > closest {
				closest = float64(r.Pct)
				s.Closest = strconv.Itoa(r.Pct) + "%"
				s.ClosestAt = r.ChipName + "/" + r.Name()
			}
		}
	}
	return s
}

func (d *dashboard) about(w http.ResponseWriter, r *http.Request) {
	d.render(w, r, pageData{Page: "about", Title: "About"})
}

// writePartial renders a fragment into the pooled buffer and sends it.
// The buffer (as in render) lets a template error return 500 instead of an
// empty 200: htmx keeps the last good content on a 5xx, but an empty 200
// swaps in nothing and kills the poller with it. no-store keeps the
// time-varying fragments out of any proxy in front of the binary.
func (d *dashboard) writePartial(w http.ResponseWriter, tmpl *template.Template, name string, data any) {
	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufPool.Put(buf)
	}()
	if err := tmpl.ExecuteTemplate(buf, name, data); err != nil {
		d.log.Error("render partial failed", "partial", name, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// overviewPartial re-renders just the overview body (attention card, fleet
// tiles, node table) for the htmx poll. A dedicated partial keeps the poll off
// the full page: rendering the whole page would re-run every card for every
// node on each tick.
func (d *dashboard) overviewPartial(w http.ResponseWriter, r *http.Request) {
	page := pageData{Page: "overview", Title: "Dashboard"}
	rows := d.enrichDashboard(d.rows())
	page.Nodes = rows
	page.Fleet = computeFleet(rows)
	page.Attention = evaluateAttention(rows)
	page.SystemRows = rows
	page.Stats = computeStats(rows)
	page.HWBase = d.hwBase(r, rows, nil)

	d.writePartial(w, pagesTmpl, "overview-body", page)
}

// nodePartials maps a live card to the template block that renders it and the
// datasets that block needs. Each card is polled on its own endpoint so a
// refresh costs one small fragment instead of a whole page render plus a
// cluster-wide gather.
var nodePartials = map[string]struct {
	tmpl string
	want nodeData
}{
	// The section name is the endpoint, so a page's live block and its full
	// render ask for the same data. Each block re-renders only its own region.
	// The cpu and links blocks used to exclude their chart containers, because
	// a swap destroyed the uPlot instance bound to them; with the charts gone
	// every block swaps whole.
	"overview": {tmpl: "node-live", want: nodeDataFor("")},
	"cpu":      {tmpl: "cpu-live", want: nodeDataFor("cpu")},
	"memory":   {tmpl: "memory-live", want: nodeDataFor("memory")},
	"storage":  {tmpl: "storage-live", want: nodeDataFor("storage")},
	"pci":      {tmpl: "pci-live", want: nodeDataFor("pci")},
	"links":    {tmpl: "links-live", want: nodeDataFor("links")},
	"sensors":  {tmpl: "sensors-live", want: nodeDataFor("sensors")},
	"health":   {tmpl: "health-live", want: nodeDataFor("health")},
	"identity": {tmpl: "identity-live", want: nodeDataFor("identity")},
	"gpu":      {tmpl: "gpu-live", want: nodeDataFor("gpu")},
	"kernel":   {tmpl: "kernel-live", want: nodeDataFor("kernel")},
}

// nodePartial renders one live card for a node.
func (d *dashboard) nodePartial(w http.ResponseWriter, r *http.Request) {
	name, card := r.PathValue("name"), r.PathValue("card")
	spec, ok := nodePartials[card]
	if !ok {
		http.Error(w, "unknown partial", http.StatusNotFound)
		return
	}
	row, found := d.row(name)
	if !found {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	page := pageData{nodeDetail: nodeDetail{Node: &row}}
	d.fillNodePage(&page, name, spec.want)
	d.writePartial(w, pagesTmpl, spec.tmpl, page)
}

// statusPartial renders the header status pill (htmx partial).
func (d *dashboard) statusPartial(w http.ResponseWriter, _ *http.Request) {
	rows := d.rows()
	up := 0
	for _, r := range rows {
		if r.Up {
			up++
		}
	}
	d.writePartial(w, statusTmpl, "status", map[string]int{"Up": up, "Total": len(rows)})
}

// serviceRow is one init-system service on the health page.
//
// State collapses Talos's three booleans into the one word an operator reads,
// but only for display — the metrics keep all three, because "no health check"
// and "health check failing" are genuinely different and only the raw booleans
// can tell them apart.
type serviceRow struct {
	Name  string
	State string // healthy | failing | no check | stopped
	Tone  string // pill class suffix: ok | warn | down
}

// unmetRow is one condition keeping the machine from being ready.
type unmetRow struct {
	Name   string
	Reason string
}

// diagnosticRow is one warning Talos is raising, with its doc link.
type diagnosticRow struct {
	ID      string
	Message string
	URL     string
}

// healthView is the health page: what the node says about its own state.
type healthView struct {
	Available bool // the collector has reported at least once
	HasStage  bool
	Stage     string
	Ready     bool

	Services    []serviceRow
	Unmet       []unmetRow
	Diagnostics []diagnosticRow

	Total   int
	Healthy int
	Failing int // running, but the health check does not pass
	NoCheck int // running, publishes no health check
	Stopped int
	// Unhealthy is Failing+Stopped: the count that actually warrants
	// attention. NoCheck is deliberately excluded — a service with no health
	// check is not a fault, and counting it here is exactly the false alarm
	// the three separate metric families exist to avoid.
	Unhealthy int
}

// healthFrom builds the health page from the snapshot.
func healthFrom(n *snapshot.Node) healthView {
	if n == nil || n.Health == nil {
		return healthView{}
	}
	h := n.Health
	view := healthView{
		Available: true,
		HasStage:  h.HasMachineStatus,
		Stage:     h.Stage,
		Ready:     h.Ready,
		Total:     len(h.Services),
		Services:  make([]serviceRow, 0, len(h.Services)),
	}
	for _, svc := range h.Services {
		row := serviceRow{Name: svc.ID}
		switch {
		case !svc.Running:
			row.State, row.Tone = "stopped", "down"
			view.Stopped++
		case svc.Unknown:
			// Not a fault: the dashboard and extension services publish no
			// health check at all, so Healthy=false carries no information.
			row.State, row.Tone = "no check", "warn"
			view.NoCheck++
		case svc.Healthy:
			row.State, row.Tone = "healthy", "ok"
			view.Healthy++
		default:
			row.State, row.Tone = "failing", "down"
			view.Failing++
		}
		view.Services = append(view.Services, row)
	}
	view.Unhealthy = view.Failing + view.Stopped
	for _, c := range h.UnmetConditions {
		view.Unmet = append(view.Unmet, unmetRow{Name: c.Name, Reason: c.Reason})
	}
	for _, d := range h.Diagnostics {
		view.Diagnostics = append(view.Diagnostics, diagnosticRow{
			ID: d.ID, Message: d.Message,
			URL: "https://talos.dev/diagnostic/" + d.ID,
		})
	}
	return view
}

// extensionRow is one installed system extension on the identity page.
type extensionRow struct {
	Name    string
	Version string
}

// identityView is the identity page: what software the node is running.
//
// SchematicShort is the first 12 characters of the schematic id. The full
// 64-character hash is unreadable in a table and identical between nodes for
// its whole length until it is not; the short form is what an operator
// actually compares, and the full value stays in talos_schematic_info.
type identityView struct {
	Available      bool
	Schematic      string
	SchematicShort string
	KernelVersion  string
	BootedEntry    string
	Cmdline        string
	Extensions     []extensionRow

	HasSecurity             bool
	SecureBoot              bool
	BootedWithUKI           bool
	ModuleSignatureEnforced bool
	SELinuxState            string
	FIPSState               string
}

// identityFrom builds the identity page from the snapshot.
func identityFrom(n *snapshot.Node) identityView {
	if n == nil || n.Identity == nil {
		return identityView{}
	}
	id := n.Identity
	view := identityView{
		Available:               true,
		Schematic:               id.Schematic,
		SchematicShort:          shortHash(id.Schematic),
		KernelVersion:           id.KernelVersion,
		BootedEntry:             id.BootedEntry,
		Cmdline:                 id.Cmdline,
		HasSecurity:             id.HasSecurityState,
		SecureBoot:              id.SecureBoot,
		BootedWithUKI:           id.BootedWithUKI,
		ModuleSignatureEnforced: id.ModuleSignatureEnforced,
		SELinuxState:            id.SELinuxState,
		FIPSState:               id.FIPSState,
	}
	for _, e := range id.Extensions {
		view.Extensions = append(view.Extensions, extensionRow{Name: e.Name, Version: e.Version})
	}
	return view
}

// shortHash trims a hex id to its first 12 characters, the length git uses for
// the same job. Anything shorter than that is left alone.
func shortHash(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// timeSyncView is the clock strip on the health page.
//
// Offset and errors are pre-formatted: raw seconds render as 4.4597e-05, which
// is unreadable at a glance, and the useful comparison is against a threshold
// an operator carries in their head ("under a millisecond is fine").
type timeSyncView struct {
	Available    bool
	Synced       bool
	KernelSynced bool
	SyncDisabled bool
	State        string
	Epoch        int
	Offset       string
	MaxError     string
	DriftPPM     string
}

// timeSyncFrom builds the clock strip from the snapshot.
func timeSyncFrom(n *snapshot.Node) timeSyncView {
	if n == nil || n.TimeSync == nil || !n.TimeSync.HasStatus {
		return timeSyncView{}
	}
	t := n.TimeSync
	view := timeSyncView{
		Available: true, Synced: t.Synced, SyncDisabled: t.SyncDisabled,
		Epoch: t.Epoch, KernelSynced: t.KernelSynced, State: t.State,
	}
	if t.HasAdjtime {
		view.Offset = shortDuration(t.OffsetSeconds)
		view.MaxError = shortDuration(t.MaxErrorSeconds)
		// The kernel reports a frequency ratio; ppm is the unit anyone who
		// works with clocks actually uses.
		view.DriftPPM = fmt.Sprintf("%+.1f ppm", (t.FrequencyRatio-1)*1e6)
	}
	return view
}

// shortDuration renders a signed second count in the largest unit that keeps
// it readable, which for clock offsets is almost always µs or ms.
func shortDuration(seconds float64) string {
	abs := math.Abs(seconds)
	switch {
	case abs == 0:
		return "0"
	case abs < 1e-6:
		return fmt.Sprintf("%.0f ns", seconds*1e9)
	case abs < 1e-3:
		return fmt.Sprintf("%.1f µs", seconds*1e6)
	case abs < 1:
		return fmt.Sprintf("%.1f ms", seconds*1e3)
	default:
		return fmt.Sprintf("%.2f s", seconds)
	}
}

// kernelParamRow is one tunable on the identity page. Drifted marks a value
// that differs from the kernel default — normal for a tuned node, but the
// column an operator scans first.
type kernelParamRow struct {
	Name        string
	Current     string
	Default     string
	Drifted     bool
	Unsupported bool
}

// kernelModuleRow is one loaded module.
type kernelModuleRow struct {
	Name  string
	State string
	Size  string
	Refs  int
}

// kernelView is the kernel block on the identity page.
type kernelView struct {
	Available   bool
	Params      []kernelParamRow
	Modules     []kernelModuleRow
	Unsupported int
	Drifted     int
}

// kernelFrom builds the kernel block from the snapshot.
func kernelFrom(n *snapshot.Node) kernelView {
	if n == nil || n.Kernel == nil {
		return kernelView{}
	}
	view := kernelView{Available: true}
	for _, p := range n.Kernel.Params {
		drifted := p.Current != p.Default
		if drifted {
			view.Drifted++
		}
		if p.Unsupported {
			view.Unsupported++
		}
		view.Params = append(view.Params, kernelParamRow{
			Name: p.Name, Current: p.Current, Default: p.Default,
			Drifted: drifted, Unsupported: p.Unsupported,
		})
	}
	for _, m := range n.Kernel.Modules {
		view.Modules = append(view.Modules, kernelModuleRow{
			Name: m.Name, State: m.State, Size: humanize.IBytes(uint64(m.SizeBytes)), Refs: m.ReferenceCount,
		})
	}
	return view
}

// volumeRowLayer is one Talos volume on the disks page.
type volumeRowLayer struct {
	ID         string
	Type       string
	Phase      string
	Ready      bool
	Filesystem string
	Location   string
	Size       string
	Encrypted  bool
	Provider   string
	FailedSync int
	Error      string
}

// volumeLayerView is the Talos volume layer block on the disks page.
//
// Pseudo is the count of directory/overlay/symlink volumes, which are folded
// away by default: on the reference cluster 19 of 22 volumes are those, they
// are permanently ready, and listing them buries the three that matter.
type volumeLayerView struct {
	Available      bool
	Volumes        []volumeRowLayer
	Pseudo         int
	NotReady       int
	SystemDisk     string
	SystemDiskPath string
}

// realVolumeTypes are the volume kinds backed by actual storage.
var realVolumeTypes = map[string]bool{"partition": true, "disk": true, "external": true}

// volumeLayerFrom builds the volume layer block from the snapshot.
func volumeLayerFrom(n *snapshot.Node) volumeLayerView {
	if n == nil || n.Volumes == nil {
		return volumeLayerView{}
	}
	v := n.Volumes
	view := volumeLayerView{
		Available:  true,
		SystemDisk: v.SystemDisk, SystemDiskPath: v.SystemDiskPath,
	}
	for _, vol := range v.Volumes {
		if !vol.Ready {
			view.NotReady++
		}
		// A pseudo-volume is folded away only while it is healthy: one that
		// failed still needs to be visible.
		if !realVolumeTypes[vol.Type] && vol.Ready {
			view.Pseudo++
			continue
		}
		size := vol.PrettySize
		if size == "" && vol.SizeBytes > 0 {
			size = humanize.IBytes(uint64(vol.SizeBytes))
		}
		view.Volumes = append(view.Volumes, volumeRowLayer{
			ID: vol.ID, Type: vol.Type, Phase: vol.Phase, Ready: vol.Ready,
			Filesystem: vol.Filesystem, Location: vol.Location, Size: size,
			Encrypted: vol.EncryptionProvider != "", Provider: vol.EncryptionProvider,
			FailedSync: vol.EncryptionFailedSyncs, Error: vol.ErrorMessage,
		})
	}
	return view
}

// gpuView is one GPU card on the gpu page.
type gpuView struct {
	Card      string
	Driver    string
	PCI       string
	PCIID     string
	HasBusy   bool
	BusyPct   string
	Suspended bool
	HasVRAM   bool
	VRAMUsed  string
	VRAMTotal string
	VRAMPct   int
	HasGTT    bool
	GTTUsed   string
	GTTTotal  string
}

// gpusFrom builds the gpu page from the snapshot.
func gpusFrom(n *snapshot.Node) []gpuView {
	if n == nil {
		return nil
	}
	out := make([]gpuView, 0, len(n.GPUs))
	for _, g := range n.GPUs {
		row := gpuView{
			Card: g.Card, Driver: g.Driver, PCI: g.Slot, PCIID: g.PCIID,
			HasBusy: g.HasBusy, Suspended: g.Suspended,
			HasVRAM: g.HasVRAM, HasGTT: g.HasGTT,
		}
		if g.HasBusy {
			row.BusyPct = fmt.Sprintf("%.0f", g.BusyPercent)
		}
		if g.HasVRAM {
			row.VRAMUsed = humanize.IBytes(uint64(g.VRAMUsedBytes))
			row.VRAMTotal = humanize.IBytes(uint64(g.VRAMTotalBytes))
			if g.VRAMTotalBytes > 0 {
				row.VRAMPct = int(g.VRAMUsedBytes / g.VRAMTotalBytes * 100)
			}
		}
		if g.HasGTT {
			row.GTTUsed = humanize.IBytes(uint64(g.GTTUsedBytes))
			row.GTTTotal = humanize.IBytes(uint64(g.GTTTotalBytes))
		}
		out = append(out, row)
	}
	return out
}

// diskIORow is one block device's I/O on the disks page.
//
// These are counters, and the dashboard has no rate window for them — showing
// a raw "4.99 TB read" since boot is honest and occasionally useful, but it is
// not a throughput reading and the column header says so. Grafana is where
// rate() belongs; this table exists so the metric is discoverable and so the
// device list is visible next to the disk inventory.
type diskIORow struct {
	Device   string
	Kind     string
	Read     string
	Written  string
	Reads    string
	Writes   string
	InFlight float64
	Busy     string // io_time as a share of uptime, when uptime is known
	// busyPct is the number behind Busy. Kept so the page summary can rank
	// devices without parsing its own formatted output back — the same
	// mistake the metrics path carried until Phase 4d.
	busyPct float64
}

// diskIOFrom builds the disk I/O table from the snapshot.
func diskIOFrom(n *snapshot.Node) []diskIORow {
	if n == nil || len(n.DiskIO) == 0 {
		return nil
	}
	uptime := 0.0
	if n.Runtime != nil && n.Runtime.HasUptime {
		uptime = n.Runtime.Uptime
	}
	out := make([]diskIORow, 0, len(n.DiskIO))
	for _, d := range n.DiskIO {
		row := diskIORow{
			Device: d.Device, Kind: d.Kind,
			Read:     humanize.IBytes(uint64(d.ReadBytes)),
			Written:  humanize.IBytes(uint64(d.WrittenBytes)),
			Reads:    humanize.Comma(int64(d.ReadsCompleted)),
			Writes:   humanize.Comma(int64(d.WritesCompleted)),
			InFlight: d.IOInProgress,
		}
		// Share of the node's life the device spent with I/O outstanding.
		// Meaningless without an uptime to divide by, so it stays blank then
		// rather than rendering a misleading 0%.
		if uptime > 0 {
			row.busyPct = d.IOTimeSeconds / uptime * 100
			row.Busy = fmt.Sprintf("%.1f%%", row.busyPct)
		}
		out = append(out, row)
	}
	return out
}

// storageSummary is the triage strip at the top of the storage page: the four
// questions worth answering before scrolling five tables.
type storageSummary struct {
	Capacity        string
	Disks           int
	HasBusiest      bool
	BusiestDevice   string
	BusiestBusy     string
	VolumesTotal    int
	VolumesNotReady int
	PVs             int
	PVFullest       string
	PVFullestPct    int
}

// storageSummaryFrom derives the strip from the page's own tables, so the
// numbers cannot drift from what is rendered below them.
func storageSummaryFrom(p *pageData) storageSummary {
	sum := storageSummary{
		Capacity: p.DisksTotal,
		Disks:    p.DisksLocal,
		PVs:      len(p.Volumes),
	}
	// Busiest is the device with the largest share of uptime spent with I/O
	// in flight. Partitions are skipped: their time is also counted in the
	// parent, so a partition would routinely outrank the disk it sits on.
	best := -1.0
	for _, d := range p.DiskIO {
		if d.Kind != "disk" || d.Busy == "" {
			continue
		}
		if pct := d.busyPct; pct > best {
			best, sum.BusiestDevice, sum.BusiestBusy, sum.HasBusiest = pct, d.Device, d.Busy, true
		}
	}
	if p.VolumeLayer.Available {
		// Total counts the folded-away healthy pseudo-volumes too, so the
		// strip agrees with what Talos actually models.
		sum.VolumesTotal = len(p.VolumeLayer.Volumes) + p.VolumeLayer.Pseudo
		sum.VolumesNotReady = p.VolumeLayer.NotReady
	}
	// Volumes arrive fullest-first, so the head is the one to name.
	if len(p.Volumes) > 0 {
		sum.PVFullest, sum.PVFullestPct = p.Volumes[0].Label(), p.Volumes[0].UsedPct
	}
	return sum
}

// gpuDrivers are the hwmon chip names a GPU registers under. A chip here is
// the card's own thermal sensor, not a mainboard one.
var gpuDrivers = map[string]bool{"amdgpu": true, "nvidia": true, "i915": true, "xe": true, "radeon": true}

// gpuSensorsFrom picks the GPU's own readings out of the full sensor set, so
// "how is my GPU" is answerable on one page. The same readings stay on the
// sensors page: this is a second view of one dataset, not a second source.
//
// Readings are not attributed to a specific card here. The link exists in the
// export — talos_sensor_chip_info carries the owning PCI address, and the
// Grafana GPU dashboard joins it onto talos_gpu_info to name the card — but the
// snapshot does not carry it yet, so this table still shows the chip.
func gpuSensorsFrom(groups []sensorGroup) []sensorReading {
	var out []sensorReading
	for _, g := range groups {
		for _, r := range g.Rows {
			if !gpuDrivers[r.ChipName] {
				continue
			}
			// Carry the unit through: the rows are grouped by kind upstream,
			// and this flattens them back into one table.
			r.Value += " " + g.Unit
			if r.HasLimit {
				r.Limit += " " + g.Unit
			}
			out = append(out, r)
		}
	}
	return out
}

// netSummary is the triage strip at the top of the links page.
type netSummary struct {
	Total    int
	Up       int
	Physical int
	RxRate   string
	TxRate   string
	Problems int // interfaces with a non-zero error or drop counter
}

// netSummaryFrom derives the strip from the rendered rows, so the numbers
// cannot drift from the table beneath them.
//
// The fleet rates are summed from the per-interface rates rather than from a
// node-level counter, because there is no node-level one: /proc/net/dev is
// per-interface. Loopback is included — it is real traffic on this node, and
// excluding it would make the strip disagree with the table.
func netSummaryFrom(links []netLink) netSummary {
	var sum netSummary
	var rx, tx float64
	for _, l := range links {
		sum.Total++
		if l.Up {
			sum.Up++
		}
		if l.Physical {
			sum.Physical++
		}
		if l.HasProblem {
			sum.Problems++
		}
		if l.HasRates && l.Up {
			rx += l.rxPerSecond
			tx += l.txPerSecond
		}
	}
	if rx > 0 || tx > 0 {
		sum.RxRate, sum.TxRate = formatRate(rx), formatRate(tx)
	}
	return sum
}
