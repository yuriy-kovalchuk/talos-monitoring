// Package sensors collects live hardware sensor values from node sysfs
// through the Talos file API (LS + Read RPCs) and exposes them as
// Prometheus gauges.
//
// Sources: /sys/class/hwmon/hwmon* (chip values: temperature, fan,
// voltage, power, frequency) and /sys/class/thermal/thermal_zone* (ACPI
// zones). File reads through the Talos API require the os:admin role;
// when the ServiceAccount only has os:reader the collector self-disables
// after the first permission error and logs a hint, so a misconfigured
// role does not spam the logs or hammer the API every interval.
package sensors

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "sensors"

// Sysfs roots scanned per node.
const (
	hwmonRoot   = "/sys/class/hwmon"
	thermalRoot = "/sys/class/thermal"
)

// familyKind maps sysfs sensor prefixes to the metric's kind label.
var familyKind = map[string]string{
	"temp":  "temperature",
	"fan":   "fan",
	"in":    "voltage",
	"power": "power",
	"freq":  "frequency",
}

// inputRe matches sysfs sensor value files (temp1_input, fan0_input, ...).
//
// power*_average is in there because a discrete AMD card publishes only that —
// no power*_input at all — so matching `_input` alone read the integrated GPU's
// 7 W and silently omitted the 300 W card next to it. Where a chip has both
// (the iGPU does), _input wins; see discover.
var inputRe = regexp.MustCompile(`^(temp|fan|in|power|freq)([0-9]+)_(input|average)$`)

var (
	chipRe = regexp.MustCompile(`^/sys/class/hwmon/hwmon[0-9]+$`)
	// A PCI address as sysfs writes it in a device path: 0000:03:00.0.
	bdfRe  = regexp.MustCompile(`[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]`)
	zoneRe = regexp.MustCompile(`^/sys/class/thermal/thermal_zone[0-9]+$`)
)

// fileAPI is the part of the Talos client the collector uses (test seam).
type fileAPI interface {
	LS(ctx context.Context, req *machine.ListRequest) (machine.MachineService_ListClient, error)
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// Collector collects hwmon chip and thermal zone values from one node.
type Collector struct {
	// api returns the file API to use for a node; overridable in tests.
	api func(node *collector.NodeClient) fileAPI
	log *slog.Logger
	now func() time.Time

	// layouts caches each node's discovered sensor set: chip names, labels and
	// thresholds do not change while a machine runs.
	layouts *layoutCache

	// snap receives the typed readings alongside the metrics.
	snap *snapshot.Store

	// disabled is set on the first permission error: file reads need
	// os:admin, so retrying every interval would only repeat the same
	// failure. The collector then no-ops until the process restarts
	// (after the role is fixed).
	disabled atomic.Bool
}

// New returns the sensors collector.
func New(log *slog.Logger) *Collector {
	return &Collector{api: defaultAPI, log: log, now: time.Now, layouts: newLayoutCache()}
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

func defaultAPI(node *collector.NodeClient) fileAPI { return node.Client }

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Live }

// Collect reports every sensor's current value, its thresholds and any alarm
// the chip has raised.
//
// The sensor layout (chip names, labels, thresholds) is discovered once per
// layoutTTL and reused; each scrape then reads only the value and alarm files,
// concurrently. Rediscovering everything each time made this the most
// expensive collector in the project.
//
// A permission error self-disables the collector (see the package comment) and
// is not counted as a scrape failure.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	if c.disabled.Load() {
		return nil // self-disabled (os:reader); the disable warning has the hint
	}
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	api := c.api(node)
	name := node.Node.Name
	now := c.now()

	l, ok := c.layouts.get(name, now)
	if !ok {
		sensors, err := c.discover(ctx, api, name)
		if err != nil {
			if collector.IsPermissionError(err) {
				return c.disable(name)
			}
			return err
		}
		if len(sensors) == 0 {
			return nil // a VM with no hwmon chips is not a failure
		}
		l = layout{ts: now, sensors: sensors}
		c.layouts.put(name, l)
		c.log.Debug("sensor layout discovered", "node", name, "sensors", len(sensors))
	}

	values, alarms, permDenied := c.readValues(ctx, api, l.sensors)
	if permDenied {
		return c.disable(name)
	}

	m := newSensorMetrics(reg)
	alarm := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_sensor_alarm",
		Help: "1 when the chip has raised its own alarm flag for this sensor. Unitless, so one family covers every sensor kind.",
	}, []string{"node", "chip", "chip_name", "sensor", "kind", "label"})
	// Which device a chip belongs to, from the sysfs symlink under
	// /sys/class/hwmon. It is its own family rather than a label on all eleven
	// sensor families: one series per chip instead of one per reading, and no
	// existing label set changes. A chip with no PCI parent gets no series —
	// an unknown value is an absent series, never a placeholder.
	chipInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_sensor_chip_info",
		Help: "Identity of a hwmon chip (value 1): the PCI device that owns it, where it has one. Join on `chip` to attribute a reading to a GPU or an NVMe drive.",
	}, []string{"node", "chip", "chip_name", "pci"})
	reg.MustRegister(alarm, chipInfo)

	for _, meta := range l.sensors {
		if meta.PCI != "" {
			chipInfo.WithLabelValues(name, meta.Chip, meta.ChipName, meta.PCI).Set(1)
		}
	}

	reported := 0
	readings := make([]snapshot.Sensor, 0, len(l.sensors))
	for i, meta := range l.sensors {
		v, ok := values[i]
		if !ok {
			continue // unreadable this round; the next scrape retries
		}
		reported++
		m.set(name, meta, v)
		if a, ok := alarms[i]; ok {
			alarm.WithLabelValues(name, meta.Chip, meta.ChipName, meta.Sensor, meta.Kind, meta.Label).Set(a)
		}
		r := snapshot.Sensor{
			Chip: meta.Chip, ChipName: meta.ChipName, Sensor: meta.Sensor,
			Kind: meta.Kind, Label: meta.Label, Value: v,
		}
		if len(meta.Limits) > 0 {
			r.Limits = maps.Clone(meta.Limits)
		}
		if a, ok := alarms[i]; ok {
			r.HasAlarm, r.Alarm = true, a
		}
		readings = append(readings, r)
	}
	if reported == 0 {
		return errors.New("no sensor values readable")
	}
	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.Sensors = readings })
	}
	return nil
}

// sensorUnits maps a sensor kind to its own metric family.
//
// One family per unit, not one family with a `kind` label: a single
// talos_sensor_value carrying °C, RPM, volts, watts and MHz meant sum() added
// volts to RPM and topk() ranked a 4 000 RPM fan above an 85 °C critical
// temperature. The unit belongs in the metric name, which is also where
// node_exporter puts it (node_hwmon_temp_celsius, node_hwmon_fan_rpm, ...).
//
// scale converts the collector's display units to base units. Only frequency
// needs it: readValues yields MHz for the UI, and the metric must be hertz to
// match talos_node_cpu_freq_hertz.
var sensorUnits = map[string]struct {
	metric string
	unit   string
	scale  float64
}{
	"temperature": {"talos_sensor_temperature_celsius", "degrees Celsius", 1},
	"fan":         {"talos_sensor_fan_rpm", "revolutions per minute", 1},
	"voltage":     {"talos_sensor_voltage_volts", "volts", 1},
	"power":       {"talos_sensor_power_watts", "watts", 1},
	"frequency":   {"talos_sensor_frequency_hertz", "hertz", 1e6},
}

// sensorLabels is the identity of one reading. `kind` is deliberately absent:
// it is the metric name now.
var sensorLabels = []string{"node", "chip", "chip_name", "sensor", "label"}

// sensorMetrics holds one value gauge and one limit gauge per sensor kind,
// created on demand so a node exposes only the kinds it actually has.
type sensorMetrics struct {
	reg    prometheus.Registerer
	values map[string]*prometheus.GaugeVec
	limits map[string]*prometheus.GaugeVec
}

func newSensorMetrics(reg prometheus.Registerer) *sensorMetrics {
	return &sensorMetrics{
		reg:    reg,
		values: map[string]*prometheus.GaugeVec{},
		limits: map[string]*prometheus.GaugeVec{},
	}
}

// set records one reading and whatever thresholds its driver publishes.
func (m *sensorMetrics) set(node string, meta sensorMeta, v float64) {
	u, ok := sensorUnits[meta.Kind]
	if !ok {
		return // an unrecognised kind has no defined unit, so no metric
	}
	labels := []string{node, meta.Chip, meta.ChipName, meta.Sensor, meta.Label}

	g, ok := m.values[meta.Kind]
	if !ok {
		g = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: u.metric,
			Help: "Hardware sensor reading in " + u.unit + ".",
		}, sensorLabels)
		m.reg.MustRegister(g)
		m.values[meta.Kind] = g
	}
	g.WithLabelValues(labels...).Set(v * u.scale)

	if len(meta.Limits) == 0 {
		return
	}
	l, ok := m.limits[meta.Kind]
	if !ok {
		l = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: limitName(u.metric),
			Help: "Threshold the chip reports for a sensor, in " + u.unit +
				" (limit: critical, max, min). Absent when the driver publishes none.",
		}, append(append([]string{}, sensorLabels...), "limit"))
		m.reg.MustRegister(l)
		m.limits[meta.Kind] = l
	}
	for kind, lv := range meta.Limits {
		if isUnsetThreshold(meta.Kind, lv*u.scale) {
			continue
		}
		l.WithLabelValues(append(append([]string{}, labels...), kind)...).Set(lv * u.scale)
	}
}

// absoluteZeroC and implausiblyHotC bracket the sentinels NVMe drives report
// for a threshold that is not set: temp*_min reads -273150 m°C (0 Kelvin) and
// temp*_max reads 65261850 m°C, the top of the raw 16-bit range.
const (
	absoluteZeroC   = -273.15
	implausiblyHotC = 1000.0
)

// isUnsetThreshold reports whether a threshold is the driver's "not set"
// sentinel rather than a real limit.
//
// Passing the low one through made min(talos_sensor_temperature_limit_celsius)
// return absolute zero, and a "minimum threshold" panel read -273.15 °C; the
// high one put 65261.85 °C into max() and made every percent-of-limit
// calculation on that sensor read near zero. An unknown value is an absent
// series here, never a sentinel — the same rule the rest of the exporter
// follows for missing readings.
func isUnsetThreshold(kind string, celsius float64) bool {
	// Only temperatures have physical bounds to test against; a 0 RPM fan
	// limit or 0 V voltage limit is a legitimate reading. Silicon melts well
	// below 1000 °C, so anything above it is the sentinel, not a threshold.
	return kind == "temperature" && (celsius <= absoluteZeroC || celsius >= implausiblyHotC)
}

// limitName turns talos_sensor_temperature_celsius into
// talos_sensor_temperature_limit_celsius: the unit stays the suffix.
func limitName(metric string) string {
	i := strings.LastIndex(metric, "_")
	return metric[:i] + "_limit" + metric[i:]
}

// readValues reads every sensor's value and alarm file concurrently. The
// returned maps are keyed by index into sensors; a missing entry means the file
// was not readable this round.
func (c *Collector) readValues(ctx context.Context, api fileAPI, sensors []sensorMeta) (map[int]float64, map[int]float64, bool) {
	values := make(map[int]float64, len(sensors))
	alarms := make(map[int]float64, len(sensors))
	var perm atomic.Bool

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, readConcurrency)
	for i, meta := range sensors {
		wg.Add(1)
		go func(i int, meta sensorMeta) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			raw, err := readScalar(ctx, api, meta.InputPath)
			if err != nil {
				if collector.IsPermissionError(err) {
					perm.Store(true)
				}
				return
			}
			v, ok := scaleValue(raw, meta.Family)
			if !ok {
				return // "N/A" or "faulty": not a reading
			}
			mu.Lock()
			values[i] = v
			mu.Unlock()

			if meta.AlarmPath == "" {
				return
			}
			if raw, err := readScalar(ctx, api, meta.AlarmPath); err == nil {
				if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
					mu.Lock()
					alarms[i] = f
					mu.Unlock()
				}
			}
		}(i, meta)
	}
	wg.Wait()
	return values, alarms, perm.Load()
}

// listDir returns the full paths of the direct entries under dir, sorted.
// Entries whose stat failed carry an error in FileInfo and are skipped.
func listDir(ctx context.Context, api fileAPI, dir string) ([]string, error) {
	out, _, err := listDirLinks(ctx, api, dir)
	return out, err
}

// listDirLinks is listDir plus each entry's symlink target, keyed by full path.
// Everything under /sys/class is a symlink into /sys/devices, and the target is
// the only thing that says which device a chip belongs to.
func listDirLinks(ctx context.Context, api fileAPI, dir string) ([]string, map[string]string, error) {
	stream, err := api.LS(ctx, &machine.ListRequest{Root: dir})
	if err != nil {
		return nil, nil, err
	}
	var out []string
	links := map[string]string{}
	for {
		fi, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if fi.GetError() != "" {
			continue
		}
		out = append(out, fi.GetName())
		if l := fi.GetLink(); l != "" {
			links[fi.GetName()] = l
		}
	}
	sort.Strings(out)
	return out, links, nil
}

// pciBDF pulls the device's own PCI address out of a sysfs symlink target.
//
// /sys/class/hwmon/hwmon3 points at
// ../../devices/pci0000:00/0000:00:01.1/0000:01:00.0/0000:02:00.0/0000:03:00.0/hwmon/hwmon3
// — a chain of bridges ending at the device itself, so the LAST address is the
// one that owns the chip. A chip with no PCI parent (a thermal zone, an ACPI
// device) returns "", and gets no series rather than a guess.
func pciBDF(link string) string {
	m := bdfRe.FindAllString(link, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

// readScalar reads a small sysfs file and returns its trimmed content.
func readScalar(ctx context.Context, api fileAPI, path string) (string, error) {
	s, err := collector.ReadFile(ctx, api, path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// scaleValue parses a raw sysfs sensor value and scales it to human
// units. Temperature (m°C) and voltage (mV) follow the hwmon ABI
// (milli-); fan counts are RPM as-is. On AMD platforms (amdgpu hwmon)
// freq is reported in Hz and power in µW — the same scaling the
// reference script used (verified live: sclk 1.786e9 → 1786 MHz, PPT
// 2.116e7 → 21.16 W).
func scaleValue(raw, prefix string) (float64, bool) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	switch prefix {
	case "fan":
		return v, true
	case "power", "freq":
		return v / 1e6, true
	default:
		return v / 1000, true
	}
}

// disable marks the collector self-disabled and logs the hint once.
func (c *Collector) disable(nodeName string) error {
	if c.disabled.CompareAndSwap(false, true) {
		c.log.Warn("sensors collector disabled: file reads need the os:admin role (got a permission error)",
			"node", nodeName,
			"hint", "give the monitor ServiceAccount the os:admin role (Helm value talos.roles) and restart the exporter to re-enable")
	}
	return nil
}

// Degraded implements collector.DegradedReporter.
//
// The disable flag is cluster-wide, so every node reports it. That is the real
// blast radius of one node's permission error, and a per-node series is what
// makes it visible: the collector returns nil without a single RPC, so the
// scrape otherwise looks entirely healthy.
func (c *Collector) Degraded(string) []collector.Degradation {
	if c.disabled.Load() {
		return []collector.Degradation{{Reason: collector.ReasonPermission, Stopped: true}}
	}
	return nil
}

// Prune implements collector.Pruner: drop the discovered sensor layout for
// departed nodes. This is the largest per-node cache in the project — one
// sensorMeta per sensor, each with paths and a limits map.
func (c *Collector) Prune(live map[string]struct{}) {
	c.layouts.prune(live)
}
