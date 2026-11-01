package sensors

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
)

// layoutTTL bounds layout reuse: chip names, labels and thresholds do not change while a machine runs.
const layoutTTL = time.Hour

// readConcurrency bounds the live value reads.
const readConcurrency = 8

// limitFiles are read once per sensor; availability varies by driver, so having none is normal.
var limitFiles = map[string]string{
	"crit": "critical",
	"max":  "max",
	"min":  "min",
	// amdgpu's board power limit is a cap (power1_cap), not a max.
	"cap":     "cap",
	"cap_max": "cap max",
}

// sensorMeta is what does not change between scrapes, discovered once.
type sensorMeta struct {
	Chip      string // sysfs directory, e.g. /sys/class/hwmon/hwmon0
	ChipName  string // driver name, e.g. coretemp
	Sensor    string // temp1, fan0, ...
	Kind      string // temperature, fan, voltage, power, frequency
	PCI       string // owning PCI device, "" when the chip has no PCI parent
	Label     string // Tctl, Package id 0, ...
	Family    string // temp, fan, in, power, freq — drives value scaling
	InputPath string
	AlarmPath string             // "" when the chip publishes none
	Limits    map[string]float64 // "critical" | "max" | "min" | "cap" | "cap max"
}

// layout is a node's discovered sensor set.
type layout struct {
	ts      time.Time
	sensors []sensorMeta
}

// layoutCache holds one layout per node.
type layoutCache struct {
	mu sync.Mutex
	by map[string]layout
}

func newLayoutCache() *layoutCache { return &layoutCache{by: map[string]layout{}} }

// get returns a node's layout when it is still fresh.
func (c *layoutCache) get(node string, now time.Time) (layout, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.by[node]
	if !ok || now.Sub(l.ts) >= layoutTTL {
		return layout{}, false
	}
	return l, true
}

func (c *layoutCache) put(node string, l layout) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.by[node] = l
}

// discover walks the sysfs roots; the expensive path, at most once per layoutTTL.
func (c *Collector) discover(ctx context.Context, api fileAPI) ([]sensorMeta, error) {
	hwmon, err := discoverHwmonSensors(ctx, api)
	if err != nil {
		return nil, err
	}
	thermal, err := discoverThermalZones(ctx, api)
	if err != nil {
		return nil, err
	}
	out := slices.Concat(hwmon, thermal)

	sort.Slice(out, func(i, j int) bool {
		if out[i].Chip != out[j].Chip {
			return out[i].Chip < out[j].Chip
		}
		return out[i].Sensor < out[j].Sensor
	})
	return out, nil
}

// discoverHwmonSensors walks every hwmon chip and returns its sensors.
func discoverHwmonSensors(ctx context.Context, api fileAPI) ([]sensorMeta, error) {
	chips, links, err := listDirLinks(ctx, api, hwmonRoot)
	if err != nil {
		return nil, err
	}
	var out []sensorMeta
	for _, chip := range chips {
		if !chipRe.MatchString(chip) {
			continue
		}
		metas, err := discoverChipSensors(ctx, api, chip, links[chip])
		if err != nil {
			return nil, err
		}
		out = append(out, metas...)
	}
	return out, nil
}

// discoverChipSensors builds a sensorMeta for each file it recognises on a chip.
func discoverChipSensors(ctx context.Context, api fileAPI, chip, link string) ([]sensorMeta, error) {
	pci := pciBDF(link)
	files, err := listDir(ctx, api, chip)
	if err != nil {
		return nil, err
	}
	byBase := map[string]bool{}
	for _, f := range files {
		byBase[f[strings.LastIndex(f, "/")+1:]] = true
	}

	chipName := chip[strings.LastIndex(chip, "/")+1:]
	if v, err := collector.ReadFileTrimmed(ctx, api, chip+"/name"); err == nil && v != "" {
		chipName = v
	}

	var out []sensorMeta
	for base := range byBase {
		if meta, ok := buildHwmonSensor(ctx, api, chip, chipName, pci, base, byBase); ok {
			out = append(out, meta)
		}
	}
	return out, nil
}

// buildHwmonSensor builds one sensor's description from its _input base name.
func buildHwmonSensor(ctx context.Context, api fileAPI, chip, chipName, pci, base string, byBase map[string]bool) (sensorMeta, bool) {
	sm := inputRe.FindStringSubmatch(base)
	if sm == nil {
		return sensorMeta{}, false
	}
	sensor := sm[1] + sm[2]
	// Where a chip has both, _input is the reading and _average the rolling mean.
	if sm[3] == "average" && byBase[sensor+"_input"] {
		return sensorMeta{}, false
	}
	meta := sensorMeta{
		Chip: chip, ChipName: chipName, PCI: pci, Sensor: sensor,
		Kind: familyKind[sm[1]], Family: sm[1],
		InputPath: chip + "/" + base,
		Limits:    map[string]float64{},
	}
	if label, err := collector.ReadFileTrimmed(ctx, api, chip+"/"+sensor+"_label"); err == nil {
		meta.Label = label
	}
	applySensorLimits(ctx, api, chip, sensor, sm[1], byBase, &meta)
	applySensorAlarm(chip, sensor, byBase, &meta)
	return meta, true
}

// applySensorLimits reads whichever threshold files this sensor publishes.
func applySensorLimits(ctx context.Context, api fileAPI, chip, sensor, family string, byBase map[string]bool, meta *sensorMeta) {
	for suffix, name := range limitFiles {
		f := sensor + "_" + suffix
		if !byBase[f] {
			continue
		}
		if raw, err := collector.ReadFileTrimmed(ctx, api, chip+"/"+f); err == nil {
			if v, ok := scaleValue(raw, family); ok {
				meta.Limits[name] = v
			}
		}
	}
}

// applySensorAlarm records the alarm file, preferring crit_alarm over alarm.
func applySensorAlarm(chip, sensor string, byBase map[string]bool, meta *sensorMeta) {
	for _, a := range []string{sensor + "_crit_alarm", sensor + "_alarm"} {
		if byBase[a] {
			meta.AlarmPath = chip + "/" + a
			return
		}
	}
}

// discoverThermalZones returns a synthetic "temp" sensor per ACPI thermal zone.
func discoverThermalZones(ctx context.Context, api fileAPI) ([]sensorMeta, error) {
	zones, err := listDir(ctx, api, thermalRoot)
	if err != nil {
		return nil, err
	}
	var out []sensorMeta
	for _, zone := range zones {
		if !zoneRe.MatchString(zone) {
			continue
		}
		out = append(out, buildThermalZone(ctx, api, zone))
	}
	return out, nil
}

// buildThermalZone builds a zone's description, including trip-point limits.
func buildThermalZone(ctx context.Context, api fileAPI, zone string) sensorMeta {
	name := zone[strings.LastIndex(zone, "/")+1:]
	if t, err := collector.ReadFileTrimmed(ctx, api, zone+"/type"); err == nil && t != "" {
		name = t
	}
	meta := sensorMeta{
		Chip: zone, ChipName: name, Sensor: "temp", Kind: "temperature", Family: "temp",
		InputPath: zone + "/temp",
		Limits:    map[string]float64{},
	}
	applyThermalTripPoints(ctx, api, zone, &meta)
	return meta
}

// applyThermalTripPoints folds trip points into Limits: critical, else the first non-critical as max.
func applyThermalTripPoints(ctx context.Context, api fileAPI, zone string, meta *sensorMeta) {
	for i := range 8 {
		tt, err := collector.ReadFileTrimmed(ctx, api, zone+"/trip_point_"+strconv.Itoa(i)+"_type")
		if err != nil {
			break
		}
		raw, err := collector.ReadFileTrimmed(ctx, api, zone+"/trip_point_"+strconv.Itoa(i)+"_temp")
		if err != nil {
			continue
		}
		v, ok := scaleValue(raw, "temp")
		if !ok || v <= 0 {
			continue
		}
		if tt == "critical" {
			meta.Limits["critical"] = v
		} else if _, seen := meta.Limits["max"]; !seen {
			meta.Limits["max"] = v
		}
	}
}

// prune drops layouts for nodes no longer in the cluster.
func (c *layoutCache) prune(live map[string]struct{}) {
	// An empty live set means discovery has not synced; pruning would force a full rediscover.
	if len(live) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for node := range c.by {
		if _, ok := live[node]; !ok {
			delete(c.by, node)
		}
	}
}
