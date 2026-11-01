// Package sensors reads hwmon and thermal-zone values through the Talos file API (needs os:admin).
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

// inputRe matches sensor value files; power*_average alone exists on discrete AMD cards.
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

	// layouts caches each node's discovered sensor set, which cannot change at runtime.
	layouts *layoutCache

	// snap receives the typed readings alongside the metrics.
	snap *snapshot.Store

	// gate trips on the first permission error (reads need os:admin); the collector then no-ops.
	gate *collector.PermissionGate
}

// New returns the sensors collector.
func New(log *slog.Logger) *Collector {
	return &Collector{
		api:     defaultAPI,
		log:     log,
		now:     time.Now,
		layouts: newLayoutCache(),
		gate:    collector.NewPermissionGate(log, "sensors collector disabled", " and restart the exporter to re-enable"),
	}
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

// Collect reads each sensor's value and alarm files; the layout itself is cached per layoutTTL.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	if c.gate.Off() {
		return nil // self-disabled (os:reader); the disable warning has the hint
	}
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	api := c.api(node)
	name := node.Node.Name
	now := c.now()

	l, ok := c.layouts.get(name, now)
	if !ok {
		sensors, err := c.discover(ctx, api)
		if err != nil {
			if collector.IsPermissionError(err) {
				c.gate.Trip(name, err)
				return nil
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

	values, alarms, permErr := c.readValues(ctx, api, l.sensors)
	if permErr != nil {
		c.gate.Trip(name, permErr)
		return nil
	}

	m := newSensorMetrics(reg)
	alarm := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_sensor_alarm",
		Help: "1 when the chip has raised its own alarm flag for this sensor. Unitless, so one family covers every sensor kind.",
	}, []string{"node", "chip", "chip_name", "sensor", "kind", "label"})
	// Chip ownership is its own family: one series per chip instead of one per reading.
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

// sensorUnits gives each kind its own family: one unit per name, as node_exporter does.
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

// sensorLabels is one reading's identity; kind is absent because the name carries it.
var sensorLabels = []string{"node", "chip", "chip_name", "sensor", "label"}

// sensorMetrics holds a value and a limit gauge per kind, created on demand.
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

// The two sentinels NVMe drives report for an unset threshold.
const (
	absoluteZeroC   = -273.15
	implausiblyHotC = 1000.0
)

// isUnsetThreshold drops a driver's "not set" sentinel: an unknown value is an absent series.
func isUnsetThreshold(kind string, celsius float64) bool {
	// Only temperatures have physical bounds; a 0 RPM or 0 V limit is legitimate.
	return kind == "temperature" && (celsius <= absoluteZeroC || celsius >= implausiblyHotC)
}

// limitName turns talos_sensor_temperature_celsius into ..._limit_celsius, keeping the unit suffix.
func limitName(metric string) string {
	i := strings.LastIndex(metric, "_")
	return metric[:i] + "_limit" + metric[i:]
}

// readValues reads value and alarm files concurrently, keyed by index into sensors.
func (c *Collector) readValues(ctx context.Context, api fileAPI, sensors []sensorMeta) (map[int]float64, map[int]float64, error) {
	values := make(map[int]float64, len(sensors))
	alarms := make(map[int]float64, len(sensors))
	var permErr error

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, readConcurrency)
	for i, meta := range sensors {
		wg.Add(1)
		go func(i int, meta sensorMeta) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			raw, err := collector.ReadFileTrimmed(ctx, api, meta.InputPath)
			if err != nil {
				if collector.IsPermissionError(err) {
					mu.Lock()
					if permErr == nil {
						permErr = err
					}
					mu.Unlock()
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
			if raw, err := collector.ReadFileTrimmed(ctx, api, meta.AlarmPath); err == nil {
				if f, err := strconv.ParseFloat(raw, 64); err == nil {
					mu.Lock()
					alarms[i] = f
					mu.Unlock()
				}
			}
		}(i, meta)
	}
	wg.Wait()
	return values, alarms, permErr
}

// listDir returns sorted full paths of dir's entries, skipping failed stats.
func listDir(ctx context.Context, api fileAPI, dir string) ([]string, error) {
	out, _, err := listDirLinks(ctx, api, dir)
	return out, err
}

// listDirLinks adds each entry's symlink target, which names the owning device.
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

// pciBDF takes the LAST PCI address in a sysfs symlink target: the device that owns the chip.
func pciBDF(link string) string {
	m := bdfRe.FindAllString(link, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

// scaleValue scales raw hwmon values to display units (m°C, mV, µW, Hz per the ABI).
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

// Degraded flags the disable on every node: it is cluster-wide, and nil otherwise looks healthy.
func (c *Collector) Degraded(string) []collector.Degradation {
	if c.gate.Off() {
		return []collector.Degradation{{Reason: collector.ReasonPermission, Stopped: true}}
	}
	return nil
}

// Prune drops the discovered sensor layout, the largest per-node cache here.
func (c *Collector) Prune(live map[string]struct{}) {
	c.layouts.prune(live)
}
