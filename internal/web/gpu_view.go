package web

import (
	"fmt"

	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// gpuView is one GPU on the gpu page.
type gpuView struct {
	Card      string
	Driver    string
	PCI       string
	PCIID     string
	Model     string // product name from the PCI inventory, joined on the BDF
	Denied    bool   // counters need os:admin, so none were read
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
	models := make(map[string]string, len(n.PCI))
	for _, d := range n.PCI {
		models[d.BDF] = d.Product
	}
	out := make([]gpuView, 0, len(n.GPUs))
	for _, g := range n.GPUs {
		row := gpuView{
			Card: g.Card, Driver: g.Driver, PCI: g.Slot, PCIID: g.PCIID, Model: models[g.Slot], Denied: g.CountersDenied,
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

// gpuDrivers are the hwmon chip names a GPU registers under.
var gpuDrivers = map[string]bool{"amdgpu": true, "nvidia": true, "i915": true, "xe": true, "radeon": true}

// gpuSensorsFrom re-shows the sensors page's GPU readings on the GPU page: one dataset, two views.
func gpuSensorsFrom(groups []sensorGroup) []sensorReading {
	var out []sensorReading
	for _, g := range groups {
		for _, r := range g.Rows {
			if !gpuDrivers[r.ChipName] {
				continue
			}
			// Carry the unit: rows were grouped by kind upstream and this flattens them.
			r.Value += " " + g.Unit
			if r.HasLimit {
				r.Limit += " " + g.Unit
			}
			out = append(out, r)
		}
	}
	return out
}
