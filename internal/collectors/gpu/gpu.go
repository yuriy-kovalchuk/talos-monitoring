// Package gpu collects GPU utilisation and memory from the DRM sysfs tree.
//
// This is the half of GPU monitoring the COSI resources do not cover, and the
// half that matters for a compute GPU. Temperature, fan, power cap and clocks
// already arrive through the sensors collector, which matches hwmon chips
// generically and so picks up amdgpu without knowing what it is. What hwmon
// does not expose is how busy the GPU is and how much of its VRAM is gone —
// those live in /sys/class/drm/cardN/device/, and a card sitting at 95% VRAM
// with an OOM-ing workload looks perfectly healthy on every other panel.
//
// Like sensors, this reads files through the Talos API, which needs the
// os:admin role. On a permission error the collector self-disables for the
// process lifetime and logs a hint, rather than failing every scrape.
//
// Absent hardware is not an error: a node with no GPU driver loaded has no
// /sys/class/drm/cardN at all, and reports nothing.
//
// A card whose PCI device is runtime-suspended (asleep) cannot answer sysfs
// reads. For those, utilisation is exported as a derived 0 — an asleep GPU
// does no work — and the state itself as talos_gpu_runtime_suspended, rather
// than the series vanishing or the driver being blamed for not reporting.
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
	"sync/atomic"

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

// cardRe matches a DRM card directory and not its connector children:
// "card0" yes, "card0-DP-1" and "renderD128" no.
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
	log *slog.Logger

	// disabled is set on the first permission error: file reads need
	// os:admin, so retrying every interval would only repeat the failure.
	disabled atomic.Bool

	// snap receives the typed view alongside the metrics.
	snap *snapshot.Store
}

// New returns the gpu collector.
func New(log *slog.Logger) *Collector {
	return &Collector{api: defaultAPI, log: log}
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

// card is one GPU's readings. The Has* flags separate "this driver does not
// publish the file" from a genuine zero: an Intel iGPU has no
// gpu_busy_percent, and reporting 0% busy for it would be a lie.
type card struct {
	Name   string // card0, card1, ...
	Driver string
	PCIID  string // vendor:device, e.g. 1002:7551
	Slot   string // PCI BDF, joins onto the pci collector's data

	HasBusy bool
	Busy    float64 // percent, 0-100

	// Suspended is set when power/runtime_status read "suspended": the card's
	// own counters are unreadable then, busy is a derived 0, and the UI
	// renders the state instead of a value. hasPowerState marks that the
	// state read itself succeeded (it is generic PCI sysfs, present on every
	// real card) so the metric is exported as 0 rather than guessed.
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
	if c.disabled.Load() {
		return nil // self-disabled (os:reader); the disable warning has the hint
	}
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	api := c.api(node)
	name := node.Node.Name

	names, err := listCards(ctx, api)
	if err != nil {
		if collector.IsPermissionError(err) {
			return c.disable(name)
		}
		return err
	}

	cards := make([]card, 0, len(names))
	for _, cardName := range names {
		cd, err := readCard(ctx, api, cardName)
		if err != nil {
			if collector.IsPermissionError(err) {
				return c.disable(name)
			}
			// A card that disappeared mid-scrape (driver unbind, hot
			// unplug) must not fail the whole node.
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
		// A node with no GPU driver bound has no /sys/class/drm tree at all.
		// Depending on where the RPC fails that NotFound surfaces here or
		// from Recv below; both are absent hardware, not a failure.
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

// readCard reads one card's identity and counters. Identity comes from uevent,
// which is the only file guaranteed present for any DRM driver.
func readCard(ctx context.Context, api fileAPI, name string) (card, error) {
	base := drmRoot + "/" + name + "/device"
	cd := card{Name: name}

	uevent, err := collector.ReadFile(ctx, api, base+"/uevent")
	if err != nil {
		return card{}, err
	}
	for _, line := range strings.Split(uevent, "\n") {
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

	// power/runtime_status is generic PCI sysfs, present on every real card.
	// It is read first because it decides how a failed busy read is
	// interpreted: a runtime-suspended device cannot answer sysfs reads, and
	// the state file itself still answers (it reports "suspended").
	if s, err := collector.ReadFile(ctx, api, base+"/power/runtime_status"); err == nil {
		cd.hasPowerState = true
		cd.Suspended = strings.TrimSpace(s) == "suspended"
	}

	// Everything below is driver-specific and optional. A missing file means
	// this driver does not report the quantity, which is an absent series.
	busy, busyErr := readNumber(ctx, api, base+"/gpu_busy_percent")
	switch {
	case busyErr == nil:
		cd.HasBusy, cd.Busy = true, busy
	case talosclient.StatusCode(busyErr) == codes.NotFound:
		// The driver does not publish the file (Intel i915): absent, asleep
		// or awake.
	case cd.Suspended:
		// The read failed because the device is runtime-suspended. An asleep
		// GPU does no work, so 0 is the honest value; the suspended flag (and
		// its metric) marks it as derived, not measured.
		cd.HasBusy, cd.Busy = true, 0
	}
	// Any other read error with unknown power state falls through to absent:
	// the value is not guessed.
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

// readNumber reads a single-value sysfs file. The read error is surfaced so a
// caller can tell a missing file (the driver does not report it) from a
// failed read; an unparseable file is a parse error, not a zero.
func readNumber(ctx context.Context, api fileAPI, path string) (float64, error) {
	s, err := collector.ReadFile(ctx, api, path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %q is not a number", path, s)
	}
	return v, nil
}

// register exports the cards.
//
// Utilisation is a ratio 0-1, not the percent sysfs reports, per the base-unit
// convention. VRAM and GTT are separate families because they are different
// pools: VRAM is on the card, GTT is system memory the GPU can address, and
// summing them would be meaningless. The runtime-suspended flag is exported
// as a 0/1 gauge, not a labelled enum: dashboards want to match on it
// (== 1), and the transient "suspending"/"resuming" states are not states a
// user needs to act on.
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

// disable marks the collector self-disabled and logs the hint once.
func (c *Collector) disable(nodeName string) error {
	if c.disabled.CompareAndSwap(false, true) {
		c.log.Warn("gpu collector disabled: file reads need the os:admin role (got a permission error)",
			"node", nodeName, "value", "talos.roles")
	}
	return nil
}

var _ collector.Collector = (*Collector)(nil)
