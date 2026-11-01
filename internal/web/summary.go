package web

import (
	"strconv"

	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// nodeSummary is the node overview's at-a-glance data, built from counters, not the full tables.
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
