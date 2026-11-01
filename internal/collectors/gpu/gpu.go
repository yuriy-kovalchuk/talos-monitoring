// Package gpu reports GPUs from the COSI PCI inventory and, with os:admin, their DRM sysfs counters.
package gpu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"google.golang.org/grpc/codes"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "gpu"

const drmRoot = "/sys/class/drm"

// pciDevicesRoot holds one directory per PCI device; its driver entry is a symlink LS resolves.
const pciDevicesRoot = "/sys/bus/pci/devices/"

// displayClassID is the PCI base class of every display controller (VGA, 3D, other).
const displayClassID = "0x03"

// cardRe matches a DRM card directory but not its connector children.
var cardRe = regexp.MustCompile(`^card[0-9]+$`)

// fileAPI is the part of the Talos client the collector uses (test seam).
type fileAPI interface {
	LS(ctx context.Context, req *machine.ListRequest) (machine.MachineService_ListClient, error)
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// Collector reports every GPU on one node: identity from COSI, counters from DRM sysfs.
type Collector struct {
	// api returns the file API to use for a node; overridable in tests.
	api func(node *collector.NodeClient) fileAPI

	// state returns the COSI state holding the PCI inventory; overridable in tests.
	state func(node *collector.NodeClient) state.CoreState

	// gate trips on the first permission error: sysfs reads need os:admin, the COSI inventory does not.
	gate *collector.PermissionGate

	// snap receives the typed view alongside the metrics.
	snap *snapshot.Store
}

// New returns the gpu collector.
func New(log *slog.Logger) *Collector {
	return &Collector{
		api:   defaultAPI,
		state: collector.DefaultState,
		gate:  collector.NewPermissionGate(log, "gpu counters", "; GPU identity from the PCI inventory keeps being reported"),
	}
}

func defaultAPI(node *collector.NodeClient) fileAPI { return node.Client }

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Live }

// card is one GPU's readings; Has* flags separate an unpublished file from a real zero.
type card struct {
	Name   string // card0, card1, ...; empty when no DRM card resolves to this device
	Driver string // bound now, from the device's driver symlink; COSI's boot-time value when unlistable
	PCIID  string // vendor:device, e.g. 1002:7551
	Slot   string // PCI BDF, joins onto the pci collector's data

	// Denied means the counter reads were refused (os:reader), so every counter below is unknown, not absent.
	Denied bool

	HasBusy bool
	Busy    float64 // percent, 0-100

	// Suspended means the card is asleep and cannot answer sysfs reads, so busy is a derived 0.
	Suspended     bool
	hasPowerState bool

	HasVRAM   bool
	VRAMTotal float64
	VRAMUsed  float64

	HasGTT   bool
	GTTTotal float64
	GTTUsed  float64
}

// Collect builds rows from COSI and LS link targets (os:reader), then reads counters when file reads are allowed.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	name := node.Node.Name
	api := c.api(node)

	gpus, inventory, err := displayDevices(ctx, c.state(node))
	if err != nil {
		return err
	}
	cards, err := listCards(ctx, api)
	if err != nil {
		return err
	}
	gpus = attachCards(gpus, inventory, cards)
	for i := range gpus {
		if driver, ok := boundDriver(ctx, api, gpus[i].Slot); ok {
			gpus[i].Driver = driver
		}
	}
	c.readAllCounters(ctx, api, name, gpus)

	sort.Slice(gpus, func(i, j int) bool {
		if gpus[i].Slot != gpus[j].Slot {
			return gpus[i].Slot < gpus[j].Slot
		}
		return gpus[i].Name < gpus[j].Name
	})

	register(name, gpus, reg)
	if c.snap != nil {
		view := make([]snapshot.GPU, 0, len(gpus))
		for _, cd := range gpus {
			view = append(view, snapshot.GPU{
				Card: cd.Name, Driver: cd.Driver, PCIID: cd.PCIID, Slot: cd.Slot, CountersDenied: cd.Denied,
				HasBusy: cd.HasBusy, BusyPercent: cd.Busy, Suspended: cd.Suspended,
				HasVRAM: cd.HasVRAM, VRAMTotalBytes: cd.VRAMTotal, VRAMUsedBytes: cd.VRAMUsed,
				HasGTT: cd.HasGTT, GTTTotalBytes: cd.GTTTotal, GTTUsedBytes: cd.GTTUsed,
			})
		}
		c.snap.Update(name, func(n *snapshot.Node) { n.GPUs = view })
	}
	return nil
}

// readAllCounters is the os:admin tier: a refused read flags every carded row denied, a granted one clears the gate.
func (c *Collector) readAllCounters(ctx context.Context, api fileAPI, node string, gpus []card) {
	if c.gate.Skip() {
		markDenied(gpus)
		return
	}
	read := false
	for i := range gpus {
		if gpus[i].Name == "" {
			continue // no DRM card, so no counter files
		}
		cd := gpus[i]
		if err := readCounters(ctx, api, &cd); err != nil {
			c.gate.Trip(node, err)
			markDenied(gpus)
			return
		}
		gpus[i], read = cd, true
	}
	if read {
		c.gate.Clear(node)
	}
}

// displayDevices returns a row per COSI display controller, plus the whole inventory by BDF for other cards.
func displayDevices(ctx context.Context, st state.CoreState) ([]card, map[string]card, error) {
	list, err := collector.ListIn(ctx, st, hardware.NamespaceName, "pci devices", hardware.PCIDeviceType)
	if err != nil {
		return nil, nil, err
	}
	var rows []card
	inventory := make(map[string]card, len(list.Items))
	for _, r := range list.Items {
		p, ok := r.(*hardware.PCIDevice)
		if !ok {
			continue
		}
		spec := p.TypedSpec()
		// COSI's driver is boot-time inventory; the live driver symlink overrides it when listable.
		row := card{
			Slot:   p.Metadata().ID(),
			Driver: spec.Driver,
			PCIID:  strings.ToLower(strings.TrimPrefix(spec.VendorID, "0x") + ":" + strings.TrimPrefix(spec.ProductID, "0x")),
		}
		inventory[row.Slot] = row
		if spec.ClassID == displayClassID {
			rows = append(rows, row)
		}
	}
	return rows, inventory, nil
}

// markDenied flags the counters of every row with a card as refused rather than absent.
func markDenied(gpus []card) {
	for i, g := range gpus {
		if g.Name != "" {
			gpus[i] = card{Name: g.Name, Driver: g.Driver, PCIID: g.PCIID, Slot: g.Slot, Denied: true}
		}
	}
}

// drmCard is a DRM card and the PCI device its sysfs symlink resolves to.
type drmCard struct{ name, slot string }

// attachCards names each row's card by BDF; a card on a non-display PCI device gets its own row.
func attachCards(gpus []card, inventory map[string]card, cards []drmCard) []card {
	bySlot := make(map[string]int, len(gpus))
	for i, g := range gpus {
		bySlot[g.Slot] = i
	}
	for _, dc := range cards {
		if i, ok := bySlot[dc.slot]; ok {
			gpus[i].Name = dc.name
			continue
		}
		row, ok := inventory[dc.slot]
		if !ok {
			row = card{Slot: dc.slot}
		}
		row.Name = dc.name
		bySlot[dc.slot] = len(gpus)
		gpus = append(gpus, row)
	}
	return gpus
}

// bdfRe matches a PCI address as sysfs spells it.
var bdfRe = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// slotFromLink returns the BDF a /sys/class/drm symlink resolves under, or "" when the card's parent is not a PCI device.
func slotFromLink(link string) string {
	parts := strings.Split(link, "/")
	for i := len(parts) - 1; i > 0; i-- {
		if parts[i] == "drm" {
			// simpledrm sits under .../<bdf>/simple-framebuffer.0/drm: its parent is the framebuffer, not a GPU.
			if bdfRe.MatchString(parts[i-1]) {
				return parts[i-1]
			}
			return ""
		}
	}
	return ""
}

// listCards returns every PCI-backed card under /sys/class/drm; LS carries link targets, so os:reader suffices.
func listCards(ctx context.Context, api fileAPI) ([]drmCard, error) {
	entries, err := list(ctx, api, drmRoot)
	if err != nil {
		if talosclient.StatusCode(err) == codes.NotFound {
			return nil, nil // no GPU driver bound means no /sys/class/drm tree: absent hardware, not a failure
		}
		return nil, err
	}
	var out []drmCard
	for _, info := range entries {
		base := path.Base(info.GetName())
		if !cardRe.MatchString(base) {
			continue
		}
		if slot := slotFromLink(info.GetLink()); slot != "" {
			out = append(out, drmCard{name: base, slot: slot})
		}
	}
	return out, nil
}

// boundDriver names the driver bound to a PCI device now; ok is false when the device directory is unlistable.
func boundDriver(ctx context.Context, api fileAPI, slot string) (string, bool) {
	entries, err := list(ctx, api, pciDevicesRoot+slot)
	if err != nil {
		return "", false
	}
	for _, info := range entries {
		if path.Base(info.GetName()) == "driver" && info.GetLink() != "" {
			return path.Base(info.GetLink()), true
		}
	}
	return "", true // listed and no driver symlink: nothing is bound
}

// list returns one directory level, skipping entries the node could not stat.
func list(ctx context.Context, api fileAPI, root string) ([]*machine.FileInfo, error) {
	stream, err := api.LS(ctx, &machine.ListRequest{Root: root})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", root, err)
	}
	var out []*machine.FileInfo
	for {
		info, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", root, err)
		}
		if info.GetError() == "" {
			out = append(out, info)
		}
	}
}

// readCounters reads one card's sysfs counters; it returns only a permission error, since every file is optional.
func readCounters(ctx context.Context, api fileAPI, cd *card) error {
	base := drmRoot + "/" + cd.Name + "/device"

	// power/runtime_status is generic PCI sysfs and decides how a failed busy read reads.
	s, err := collector.ReadFile(ctx, api, base+"/power/runtime_status")
	if collector.IsPermissionError(err) {
		return err
	}
	if err == nil {
		cd.hasPowerState = true
		cd.Suspended = strings.TrimSpace(s) == "suspended"
	}

	// The rest is driver-specific: a missing file means an absent series.
	busy, busyErr := readNumber(ctx, api, base+"/gpu_busy_percent")
	switch {
	case collector.IsPermissionError(busyErr):
		return busyErr
	case busyErr == nil:
		cd.HasBusy, cd.Busy = true, busy
	case talosclient.StatusCode(busyErr) == codes.NotFound:
		// The driver does not publish the file (Intel i915): absent, asleep or awake.
	case cd.Suspended:
		// An asleep GPU does no work, so 0 is honest; the flag marks it derived, not measured.
		cd.HasBusy, cd.Busy = true, 0
	}
	// Any other error with unknown power state falls through to absent: the value is not guessed.
	total, totalErr := readNumber(ctx, api, base+"/mem_info_vram_total")
	used, usedErr := readNumber(ctx, api, base+"/mem_info_vram_used")
	if totalErr == nil && usedErr == nil {
		cd.HasVRAM, cd.VRAMTotal, cd.VRAMUsed = true, total, used
	}
	gttTotal, gttTotalErr := readNumber(ctx, api, base+"/mem_info_gtt_total")
	gttUsed, gttUsedErr := readNumber(ctx, api, base+"/mem_info_gtt_used")
	if gttTotalErr == nil && gttUsedErr == nil {
		cd.HasGTT, cd.GTTTotal, cd.GTTUsed = true, gttTotal, gttUsed
	}
	return nil
}

// readNumber reads a single-value sysfs file, surfacing the error to tell absent from failed.
func readNumber(ctx context.Context, api fileAPI, path string) (float64, error) {
	s, err := collector.ReadFileTrimmed(ctx, api, path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %q is not a number", path, s)
	}
	return v, nil
}

// register exports utilisation as a 0-1 ratio, VRAM and GTT as separate pools, suspension as 0/1.
func register(nodeName string, cards []card, reg prometheus.Registerer) {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_gpu_info",
		Help: "GPU identity (value 1), one series per display-class PCI device. pci is the BDF, which joins onto talos_hw_pcidevice_info for vendor and model. driver is the one bound now (empty: none). card is empty when no DRM card resolves to the device.",
	}, []string{"node", "card", "driver", "pci_id", "pci"})
	g := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, []string{"node", "card"})
	}
	util := g("talos_gpu_utilization_ratio", "Fraction of time the GPU was busy, 0-1. Absent on drivers that do not report it (Intel i915); a derived 0 while the card is runtime-suspended (see talos_gpu_runtime_suspended).")
	suspended := g("talos_gpu_runtime_suspended", "1 while the card's PCI device is runtime-suspended (asleep), 0 while awake. A suspended card cannot report its counters: utilisation is a derived 0 and memory is absent. Absent where the driver publishes no power/runtime_status.")
	vramTotal := g("talos_gpu_memory_total_bytes", "Total video memory on the card, in bytes.")
	vramUsed := g("talos_gpu_memory_used_bytes", "Video memory in use, in bytes.")
	gttTotal := g("talos_gpu_gtt_total_bytes", "Total GTT (system memory addressable by the GPU), in bytes.")
	gttUsed := g("talos_gpu_gtt_used_bytes", "GTT in use, in bytes.")
	count := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_gpus",
		Help: "Number of GPUs on the node: display-class PCI devices, plus any other PCI device a DRM driver is bound to. Counts hardware, driver or not.",
	}, []string{"node"})
	reg.MustRegister(info, util, suspended, vramTotal, vramUsed, gttTotal, gttUsed, count)

	count.WithLabelValues(nodeName).Set(float64(len(cards)))
	for _, cd := range cards {
		info.WithLabelValues(nodeName, cd.Name, cd.Driver, cd.PCIID, cd.Slot).Set(1)
		if cd.HasBusy {
			util.WithLabelValues(nodeName, cd.Name).Set(cd.Busy / 100)
		}
		if cd.hasPowerState {
			v := 0.0
			if cd.Suspended {
				v = 1
			}
			suspended.WithLabelValues(nodeName, cd.Name).Set(v)
		}
		if cd.HasVRAM {
			vramTotal.WithLabelValues(nodeName, cd.Name).Set(cd.VRAMTotal)
			vramUsed.WithLabelValues(nodeName, cd.Name).Set(cd.VRAMUsed)
		}
		if cd.HasGTT {
			gttTotal.WithLabelValues(nodeName, cd.Name).Set(cd.GTTTotal)
			gttUsed.WithLabelValues(nodeName, cd.Name).Set(cd.GTTUsed)
		}
	}
}

// Degraded flags the counters off, not Stopped: GPU identity still comes from COSI.
func (c *Collector) Degraded(string) []collector.Degradation {
	if c.gate.Denied() {
		return []collector.Degradation{{Reason: collector.ReasonPermission}}
	}
	return nil
}

var _ collector.Collector = (*Collector)(nil)
