package web

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/version"
	"github.com/yuriy-kovalchuk/talos-monitoring/static"
)

// nodeDetail is the node-page part of pageData, embedded so templates write {{.CPU}}.
type nodeDetail struct {
	Node           *nodeRow        // the node this page is about
	Section        string          // a nodeSections key; "" is the node overview
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
	Etcd           *etcdView       // health page: etcd member, control planes only
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
	// Page is the nav identity and the content block name ("<Page>-content"); Template overrides it.
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
		// Named fallback, not "or": shadowing the template builtin breaks every {{if or ...}} at render time.
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

// row builds one node's row without materialising the whole fleet.
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

// bufPool recycles the two-phase render buffers, which are tens of kilobytes.
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
	// hist is optional (the router allows nil); freqObservedFrom already guarded it.
	if d.hist != nil {
		page.HistWindow = humanDuration(d.hist.Window())
	}

	// Two-phase render because {{template}} takes no dynamic name; the buffers are pooled.
	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufPool.Put(buf)
	}()
	if err := pagesTmpl.ExecuteTemplate(buf, page.contentTemplate(), page); err != nil {
		d.log.Error("render content failed", "page", page.Page, "err", err)
		// Nothing is written yet, so a clean 5xx beats an empty 200 that reads as a network fault.
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Pages are personal (sidebar cookie), so no proxy in front may cache them.
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
		page := pageData{Page: "node", Title: name, Node: &row}
		d.fillNodePage(&page, name, nodeDataFor(""))
		setNodeCookie(w, name)
		d.render(w, r, page)
		return
	}
	http.Error(w, "node not found", http.StatusNotFound)
}

// nodeSections maps /nodes/{name}/{section} to its content template and menu label.
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
			Node: &row, Section: section}
		d.fillNodePage(&page, name, nodeDataFor(section))
		setNodeCookie(w, name)
		d.render(w, r, page)
		return
	}
	http.Error(w, "node not found", http.StatusNotFound)
}

// nodeData picks the datasets a page needs, so pages never build tables they would drop.
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

	// These ride with an existing page rather than owning one.
	timesync    bool
	kernel      bool
	volumeLayer bool
	diskIO      bool
}

// nodeDataFor maps a section to the datasets it renders; the empty section is the node overview.
func nodeDataFor(section string) nodeData {
	switch section {
	case "cpu":
		return nodeData{cpu: true, status: true}
	case "memory":
		return nodeData{memory: true}
	case "storage":
		// The whole storage stack on one page: devices, traffic, volumes, filesystems, PVs.
		return nodeData{disks: true, volumeLayer: true, diskIO: true}
	case "pci":
		return nodeData{pci: true}
	case "sensors":
		return nodeData{sensors: true}
	case "links":
		return nodeData{links: true}
	case "health":
		// Clock discipline is a health signal, so the health page renders it too.
		return nodeData{health: true, timesync: true}
	case "identity":
		return nodeData{identity: true}
	case "kernel":
		return nodeData{kernel: true, identity: true}
	case "gpu":
		// The GPU's temperature, fan and power come from its hwmon chip, so they share the page.
		return nodeData{gpu: true, sensors: true}
	default: // node overview
		// The overview strip reports stage, services, load and clock.
		return nodeData{system: true, cpu: true, memory: true, status: true, summary: true,
			links: true, health: true, identity: true, gpu: true, timesync: true,
			kernel: true}
	}
}

// fillNodePage fills only what want asks for, from the typed snapshot: one map lookup.
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
		page.Etcd = etcdFrom(snap)
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

// nodeapi's version wins: the pool learns it once at verify and never re-verifies.
func preferNodeapiVersion(page *pageData) {
	if page.Node != nil && page.Status != nil && page.Status.Version != "" {
		page.Node.TalosVersion = page.Status.Version
	}
}

// nodeCookie remembers the last viewed node for the sidebar hardware links.
const nodeCookie = "tm-node"

func setNodeCookie(w http.ResponseWriter, name string) {
	// No Secure flag: local dev serves plain http and Secure would drop the cookie.
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- UI-preference cookie, not a security boundary
		Name:     nodeCookie,
		Value:    name,
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
	})
}

// hwBase is the sidebar link base: current node, else cookie, else first discovered; "" if none.
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

func (d *dashboard) about(w http.ResponseWriter, r *http.Request) {
	d.render(w, r, pageData{Page: "about", Title: "About"})
}

// writePartial renders into the pooled buffer so a template error can still answer 500, not an empty 200.
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

// overviewPartial re-renders just the overview body so the poll skips the rest of the page.
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

// nodePartials maps each live card to its template block and the datasets that block needs.
var nodePartials = map[string]struct {
	tmpl string
	want nodeData
}{
	// The section name is the endpoint, so a live block and its full page ask for the same data.
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
	page := pageData{Node: &row}
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
