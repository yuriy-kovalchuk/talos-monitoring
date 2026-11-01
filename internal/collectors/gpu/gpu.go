// Package gpu collects GPU utilisation and VRAM from the DRM sysfs tree, which hwmon does not expose.
package gpu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc/codes"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "gpu"

const drmRoot = "/sys/class/drm"

// cardRe matches a DRM card directory but not its connector children.
var cardRe = regexp.MustCompile(`^card[0-9]+$`)

// fileAPI is the part of the Talos client the collector uses (test seam).
type fileAPI interface {
	LS(ctx context.Context, req *machine.ListRequest) (machine.MachineService_ListClient, error)
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// Collector reads DRM sysfs for every GPU on one node.
type Collector struct {
	// api returns the file API to use for a node; overridable in tests.
	api func(node *collector.NodeClient) fileAPI

	// gate trips on the first permission error (reads need os:admin).
	gate *collector.PermissionGate

	// snap receives the typed view alongside the metrics.
	snap *snapshot.Store
}

// New returns the gpu collector.
func New(log *slog.Logger) *Collector {
	return &Collector{
		api:  defaultAPI,
		gate: collector.NewPermissionGate(log, "gpu collector disabled", " and restart the exporter to re-enable"),
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
	Name   string // card0, card1, ...
	Driver string
	PCIID  string // vendor:device, e.g. 1002:7551
	Slot   string // PCI BDF, joins onto the pci collector's data

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

// Collect discovers every DRM card and reads its counters.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	if c.gate.Off() {
		return nil // self-disabled (os:reader); the disable warning has the hint
	}
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	api := c.api(node)
	name := node.Node.Name

	names, err := listCards(ctx, api)
	if err != nil {
		if collector.IsPermissionError(err) {
			c.gate.Trip(name, err)
			return nil
		}
		return err
	}

	cards := make([]card, 0, len(names))
	for _, cardName := range names {
		cd, err := readCard(ctx, api, cardName)
		if err != nil {
			if collector.IsPermissionError(err) {
				c.gate.Trip(name, err)
				return nil
			}
			// A card unbound or unplugged mid-scrape must not fail the whole node.
			continue
		}
		cards = append(cards, cd)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Name < cards[j].Name })

	register(name, cards, reg)
	if c.snap != nil {
		view := make([]snapshot.GPU, 0, len(cards))
		for _, cd := range cards {
			view = append(view, snapshot.GPU{
				Card: cd.Name, Driver: cd.Driver, PCIID: cd.PCIID, Slot: cd.Slot,
				HasBusy: cd.HasBusy, BusyPercent: cd.Busy, Suspended: cd.Suspended,
				HasVRAM: cd.HasVRAM, VRAMTotalBytes: cd.VRAMTotal, VRAMUsedBytes: cd.VRAMUsed,
				HasGTT: cd.HasGTT, GTTTotalBytes: cd.GTTTotal, GTTUsedBytes: cd.GTTUsed,
			})
		}
		c.snap.Update(name, func(n *snapshot.Node) { n.GPUs = view })
	}
	return nil
}

// listCards returns the card directories under /sys/class/drm.
func listCards(ctx context.Context, api fileAPI) ([]string, error) {
	stream, err := api.LS(ctx, &machine.ListRequest{Root: drmRoot})
	if err != nil {
		// No GPU driver bound means no /sys/class/drm tree: absent hardware, not a failure.
		if talosclient.StatusCode(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", drmRoot, err)
	}
	var out []string
	for {
		info, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if talosclient.StatusCode(err) == codes.NotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("list %s: %w", drmRoot, err)
		}
		if info.GetError() != "" {
			continue
		}
		base := info.GetName()
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			base = base[idx+1:]
		}
		if cardRe.MatchString(base) {
			out = append(out, base)
		}
	}
	return out, nil
}

// readCard reads one card; uevent is the only file any DRM driver guarantees.
func readCard(ctx context.Context, api fileAPI, name string) (card, error) {
	base := drmRoot + "/" + name + "/device"
	cd := card{Name: name}

	uevent, err := collector.ReadFile(ctx, api, base+"/uevent")
	if err != nil {
		return card{}, err
	}
	for line := range strings.SplitSeq(uevent, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "DRIVER":
			cd.Driver = value
		case "PCI_ID":
			cd.PCIID = value
		case "PCI_SLOT_NAME":
			cd.Slot = value
		}
	}

	// power/runtime_status is generic PCI sysfs and decides how a failed busy read reads.
	if s, err := collector.ReadFile(ctx, api, base+"/power/runtime_status"); err == nil {
		cd.hasPowerState = true
		cd.Suspended = strings.TrimSpace(s) == "suspended"
	}

	// The rest is driver-specific: a missing file means an absent series.
	busy, busyErr := readNumber(ctx, api, base+"/gpu_busy_percent")
	switch {
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
	return cd, nil
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
		Help: "GPU identity (value 1). pci is the BDF, which joins onto talos_hw_pci_device_info for vendor and model.",
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
		Help: "Number of GPUs with a driver bound on the node.",
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

// Degraded flags the self-disable: the collector returns nil without contacting the node.
func (c *Collector) Degraded(string) []collector.Degradation {
	if c.gate.Off() {
		return []collector.Degradation{{Reason: collector.ReasonPermission, Stopped: true}}
	}
	return nil
}

var _ collector.Collector = (*Collector)(nil)
