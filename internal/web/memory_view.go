package web

import (
	"strconv"

	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// memUsage splits memory the way `free` does, so the three bar segments add up to total.
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

	Committed   string
	CommitLimit string
	CommitPct   int
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
	m.FreePct = max(100-m.UsedPct-m.BuffCachePct, 0)
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
	}
	return m
}

// memoryModule is one DIMM row, from talos_hw_memory_module_info labels.
type memoryModule struct {
	Slot          string
	SizeMiB       int
	SizeDisplay   string
	SpeedMTs      int // rated transfer rate in MT/s; SMBIOS reports MT/s, not the clock
	Manufacturer  string
	Product       string
	SerialNumber  string
	AssetTag      string
	DeviceLocator string
	BankLocator   string
}

// populatedModules counts installed DIMMs; SMBIOS also lists empty slots, with size 0.
func populatedModules(n *snapshot.Node) int {
	if n == nil {
		return 0
	}
	count := 0
	for _, m := range n.Modules {
		if m.SizeBytes > 0 {
			count++
		}
	}
	return count
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
			SpeedMTs:      int(mod.SpeedTransfer / 1e6),
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
