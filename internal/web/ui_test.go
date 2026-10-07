package web

// Internal-package tests for the dashboard's arithmetic helpers. They live in
// package web rather than web_test because the helpers take and return
// unexported types (netLink, sensorGroup, coreStat), which an external test
// package could not construct. They assert numbers, never rendered copy.

import (
	"math"
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

func TestPctOfF(t *testing.T) {
	for _, tc := range []struct {
		part, whole float64
		want        int
	}{
		// The regression this closes: a sub-zero reading (cold intake, idle
		// board in a cold room) used to be converted to uint64 at the call
		// site, which is implementation-defined — 0 on arm64, ~2^64 on amd64,
		// where pctOf's clamp rendered it as 100% of the critical limit. The
		// overflow itself is arch-dependent, so what is pinned here is the
		// helper's contract: no float is ever converted unsafely on the way in.
		{part: -5, whole: 90, want: 0},
		{part: -0.5, whole: 90, want: 0},
		{part: 0, whole: 90, want: 0},
		{part: math.NaN(), whole: 90, want: 0},
		{part: 5, whole: 0, want: 0},
		{part: 0.4, whole: 90, want: 0},
		{part: 45, whole: 90, want: 50},
		{part: 89.9, whole: 90, want: 99},
		{part: 95, whole: 90, want: 100},
	} {
		if got := pctOfF(tc.part, tc.whole); got != tc.want {
			t.Errorf("pctOfF(%v, %v) = %d, want %d", tc.part, tc.whole, got, tc.want)
		}
	}
}

func TestSensorPercentOfLimitIgnoresNegativeReadings(t *testing.T) {
	n := &snapshot.Node{Sensors: []snapshot.Sensor{
		{Chip: "/sys/devices/platform/coretemp.0/hwmon/hwmon3", ChipName: "coretemp",
			Sensor: "temp1", Label: "Tctl", Kind: "temperature", Value: -5,
			Limits: map[string]float64{"critical": 90}},
		{Chip: "/sys/devices/platform/coretemp.0/hwmon/hwmon3", ChipName: "coretemp",
			Sensor: "temp2", Label: "Package", Kind: "temperature", Value: 81,
			Limits: map[string]float64{"critical": 90}},
	}}

	groups := sensorsFrom(n)
	if len(groups) != 1 {
		t.Fatalf("sensorsFrom returned %d groups, want 1", len(groups))
	}
	for _, r := range groups[0].Rows {
		switch r.Label {
		case "Tctl":
			if r.Pct != 0 {
				t.Errorf("a -5°C reading against a 90°C limit gave Pct = %d, want 0", r.Pct)
			}
		case "Package":
			if r.Pct != 90 {
				t.Errorf("an 81°C reading against a 90°C limit gave Pct = %d, want 90", r.Pct)
			}
		}
	}

	// The health strip's "closest to limit" figure must be the real reading, not
	// the overflow.
	s := sensorSummaryFrom(groups)
	if s.Closest != "90%" || s.ClosestAt != "coretemp/Package" {
		t.Errorf("closest to limit = %s at %q, want 90%% at coretemp/Package", s.Closest, s.ClosestAt)
	}
}

func TestSensorSummaryCountsEachChipOnce(t *testing.T) {
	k10 := "/sys/devices/pci0000:00/0000:00:18.3/hwmon/hwmon0"
	n := &snapshot.Node{Sensors: []snapshot.Sensor{
		{Chip: k10, ChipName: "k10temp", Sensor: "temp1", Kind: "temperature", Value: 44},
		{Chip: k10, ChipName: "k10temp", Sensor: "freq1", Kind: "frequency", Value: 3200},
		{Chip: "/sys/devices/pci0000:01/0000:01:00.0/nvme/nvme0/hwmon1", ChipName: "nvme",
			Sensor: "temp1", Kind: "temperature", Value: 37},
	}}

	groups := sensorsFrom(n)
	if len(groups) != 2 {
		t.Fatalf("sensorsFrom returned %d groups, want 2", len(groups))
	}
	// Each card still reports its own chip count.
	wantChips := map[string]int{"Temperature": 2, "Frequency": 1}
	for _, g := range groups {
		if g.Chips != wantChips[g.Title] {
			t.Errorf("%s card reports %d chips, want %d", g.Title, g.Chips, wantChips[g.Title])
		}
	}
	// The strip counts chips on the node: k10temp reporting temperature and
	// frequency is one chip, so two cards' worth of "1" must not add up to 2.
	if s := sensorSummaryFrom(groups); s.Chips != 2 {
		t.Errorf("summary chips = %d, want 2 (k10temp, nvme)", s.Chips)
	}
}

func TestNetSummaryCountsBondedTrafficOnce(t *testing.T) {
	links := []netLink{
		{Name: "bond0", Up: true, HasRates: true, rxPerSecond: 1e8, txPerSecond: 2e7},
		{Name: "eno1", Up: true, Physical: true, master: "bond0", HasRates: true,
			rxPerSecond: 5e7, txPerSecond: 1e7},
		{Name: "eno2", Up: true, Physical: true, master: "bond0", HasRates: true,
			rxPerSecond: 5e7, txPerSecond: 1e7},
	}

	s := netSummaryFrom(links)
	if s.RxRate != formatRate(1e8) || s.TxRate != formatRate(2e7) {
		t.Errorf("summary rates = %s rx / %s tx, want the bond's %s / %s (slaves carry the same traffic)",
			s.RxRate, s.TxRate, formatRate(1e8), formatRate(2e7))
	}
	// Interface counts are not traffic: a slave is still an interface.
	if s.Total != 3 || s.Up != 3 || s.Physical != 2 {
		t.Errorf("interface counts = total %d, up %d, physical %d, want 3, 3, 2",
			s.Total, s.Up, s.Physical)
	}
}

func TestMostCommonBreaksTiesOnTheString(t *testing.T) {
	// A 2-2 split is a rollout in progress — the situation the version card
	// exists for. Map order used to decide which version was printed, so the
	// card flipped on every poll.
	if got := mostCommon(map[string]int{"v1.13.4": 2, "v1.13.5": 2}); got != "v1.13.4" {
		t.Errorf("tie = %q, want the lower string v1.13.4", got)
	}
	if got := mostCommon(map[string]int{"v1.13.4": 1, "v1.13.5": 3}); got != "v1.13.5" {
		t.Errorf("majority = %q, want v1.13.5", got)
	}
	if got := mostCommon(nil); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
}

func TestCollapsePolicyMixedReportsNoSinglePolicy(t *testing.T) {
	uniform := []coreStat{
		{Governor: "performance", MinMHz: "800", MaxMHz: "4700"},
		{Governor: "performance", MinMHz: "800", MaxMHz: "4700"},
	}
	if p := collapsePolicy(uniform); p.Mixed || p.Governor != "performance" ||
		p.MinMHz != "800" || p.MaxMHz != "4700" {
		t.Errorf("uniform cores collapsed to %+v", p)
	}

	disagreeing := []coreStat{
		{Governor: "performance", MinMHz: "800", MaxMHz: "4700"},
		{Governor: "powersave", MinMHz: "800", MaxMHz: "4700"},
	}
	p := collapsePolicy(disagreeing)
	if !p.Mixed {
		t.Fatal("two governors across cores did not set Mixed")
	}
	if p.Governor != "" || p.MinMHz != "" || p.MaxMHz != "" {
		t.Errorf("Mixed must leave the fields empty, got %+v", p)
	}
}
