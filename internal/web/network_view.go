package web

import (
	"sort"
	"strings"

	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// netLink is one network interface for the links page.
type netLink struct {
	Name     string
	Type     string
	Kind     string // "" for a physical NIC
	Physical bool
	// master ("" = topmost) is not rendered; it stops the summary counting a bond and its slaves.
	master    string
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
	// Raw bytes/second so the summary can sum without re-parsing strings.
	rxPerSecond float64
	txPerSecond float64
	RxDropped   float64
	TxDropped   float64
	RxErrors    float64
	TxErrors    float64
	HasProblem  bool // any non-zero drop or error counter

	// Cumulative since boot: the table carries the numbers, not just an "issues" word.
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
			master:   l.Master,
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
	// Links carrying traffic first, then hardware, then the logical interfaces on top.
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

// summariseIssues is the one-line form of the drop and error counters for the table.
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

// netSummary is the triage strip at the top of the links page.
type netSummary struct {
	Total    int
	Up       int
	Physical int
	RxRate   string
	TxRate   string
	Problems int // interfaces with a non-zero error or drop counter
}

// netSummaryFrom sums topmost interfaces only: a bond's counters already include its slaves'.
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
		if l.HasRates && l.Up && l.master == "" {
			rx += l.rxPerSecond
			tx += l.txPerSecond
		}
	}
	if rx > 0 || tx > 0 {
		sum.RxRate, sum.TxRate = formatRate(rx), formatRate(tx)
	}
	return sum
}
