package web

import (
	"sort"
	"strconv"
	"strings"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// sensorReading is one row of a sensor card.
type sensorReading struct {
	Chip     string  // hwmon directory base name (hwmon0, thermal_zone0)
	ChipName string  // driver name (acpitz, k10temp, ...)
	Sensor   string  // sysfs sensor (temp1, fan0, in0, ...)
	Label    string  // optional human label (Tctl, edge, ...)
	value    float64 // raw sensor value; Value is its display format
	Value    string  // formatted sensor value

	// HasLimit is false for drivers that publish no threshold (k10temp, amdgpu).
	HasLimit bool
	Limit    string
	LimitOf  string // "critical" or "max"
	Pct      int
	Alarm    bool
}

// Name is the human identifier: the driver's label when there is one.
func (r sensorReading) Name() string {
	if r.Label != "" {
		return r.Label
	}
	return r.Sensor
}

// sensorGroup is one card: all readings of a single kind.
type sensorGroup struct {
	Title string // card title (Temperature, Fan, ...)
	Unit  string // unit of the displayed values (°C, RPM, ...)
	Rows  []sensorReading
	Chips int // distinct chips in this group

	// chipNames lets the summary dedupe chips across kinds instead of summing the cards.
	chipNames []string
}

// sensorSummary is the sensors page health strip.
type sensorSummary struct {
	Available bool
	Hottest   string
	HottestAt string
	Closest   string // percent of limit, as text
	ClosestAt string
	Alarms    int
	Sensors   int
	Chips     int
}

// sensorKindOrder is the sensors page card order (most interesting first).
var sensorKindOrder = []string{"temperature", "fan", "voltage", "power", "frequency"}

// sensorKindTitles maps the kind label to the card title and display unit.
var sensorKindTitles = map[string]string{
	"temperature": "Temperature",
	"fan":         "Fan",
	"voltage":     "Voltage",
	"power":       "Power",
	"frequency":   "Frequency",
}

var sensorUnits = map[string]string{
	"temperature": "°C",
	"fan":         "RPM",
	"voltage":     "V",
	"power":       "W",
	"frequency":   "MHz",
}

// sensorPrecision is the decimals per kind; %g rendered a voltage as 1.23e-05.
var sensorPrecision = map[string]int{
	"temperature": 1, "fan": 0, "voltage": 3, "power": 1, "frequency": 0,
}

// sensorsFrom groups a node's readings by kind, from the snapshot.
func sensorsFrom(n *snapshot.Node) []sensorGroup {
	if n == nil {
		return nil
	}
	byKind := map[string][]sensorReading{}
	chips := map[string]map[string]struct{}{}
	for _, sn := range n.Sensors {
		kind := sn.Kind
		if kind == "" || kind == "unknown" {
			continue
		}
		r := sensorReading{
			Chip:     sn.Chip[strings.LastIndex(sn.Chip, "/")+1:],
			ChipName: sn.ChipName,
			Sensor:   sn.Sensor,
			Label:    sn.Label,
			value:    sn.Value,
			Value:    strconv.FormatFloat(sn.Value, 'f', sensorPrecision[kind], 64),
		}
		// Critical wins over max: it is the threshold that matters.
		for _, of := range []string{"critical", "max"} {
			if limit, ok := sn.Limits[of]; ok && limit > 0 {
				r.HasLimit, r.LimitOf = true, of
				r.Limit = strconv.FormatFloat(limit, 'f', sensorPrecision[kind], 64)
				r.Pct = pctOfF(sn.Value, limit)
				break
			}
		}
		r.Alarm = sn.HasAlarm && sn.Alarm > 0
		byKind[kind] = append(byKind[kind], r)
		if chips[kind] == nil {
			chips[kind] = map[string]struct{}{}
		}
		chips[kind][sn.Chip] = struct{}{}
	}
	var groups []sensorGroup
	for _, kind := range sensorKindOrder {
		rows := byKind[kind]
		if len(rows) == 0 {
			continue
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Alarm != rows[j].Alarm {
				return rows[i].Alarm
			}
			if rows[i].HasLimit != rows[j].HasLimit {
				return rows[i].HasLimit
			}
			if rows[i].Pct != rows[j].Pct {
				return rows[i].Pct > rows[j].Pct
			}
			return rows[i].Name() < rows[j].Name()
		})
		names := make([]string, 0, len(chips[kind]))
		for c := range chips[kind] {
			names = append(names, c)
		}
		groups = append(groups, sensorGroup{
			Title:     sensorKindTitles[kind],
			Unit:      sensorUnits[kind],
			Rows:      rows,
			Chips:     len(names),
			chipNames: names,
		})
	}
	return groups
}

// sensorSummaryFrom builds the sensors health strip.
func sensorSummaryFrom(groups []sensorGroup) sensorSummary {
	var s sensorSummary
	var hottest, closest float64
	// Distinct chips, not the sum of cards: one chip can report several kinds.
	chips := map[string]struct{}{}
	for _, g := range groups {
		s.Sensors += len(g.Rows)
		for _, c := range g.chipNames {
			chips[c] = struct{}{}
		}
		for _, r := range g.Rows {
			if r.Alarm {
				s.Alarms++
			}
			if g.Title != "Temperature" {
				continue
			}
			s.Available = true
			// Compare the raw value, never re-parse the display format.
			if r.value > hottest {
				hottest, s.Hottest, s.HottestAt = r.value, r.Value+" °C", r.ChipName+"/"+r.Name()
			}
			if r.HasLimit && float64(r.Pct) > closest {
				closest = float64(r.Pct)
				s.Closest = strconv.Itoa(r.Pct) + "%"
				s.ClosestAt = r.ChipName + "/" + r.Name()
			}
		}
	}
	s.Chips = len(chips)
	return s
}
