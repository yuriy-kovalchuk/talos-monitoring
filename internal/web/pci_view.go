package web

import (
	"sort"
	"strings"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// pciDevice is one PCI row: the COSI inventory plus the readable sysfs detail.
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

// pciGroup is one device class; a flat BDF dump is not what this page is for.
type pciGroup struct {
	Class    string
	Devices  []pciDevice
	Degraded int // devices in this class negotiating below their maximum
	Errors   int // devices in this class reporting AER errors
}

// NeedsAttention is a bool because html/template refuses an int pipeline in an attribute.
func (g pciGroup) NeedsAttention() bool { return g.Degraded > 0 || g.Errors > 0 }

// pciGroupsFrom groups a node's devices by class, worst first.
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
			// Bridges and idle links sit below maximum by design, so only endpoints are flagged.
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
