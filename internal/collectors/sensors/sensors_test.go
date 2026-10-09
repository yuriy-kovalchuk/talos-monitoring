package sensors

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
)

// fakeStream satisfies machine.MachineService_ListClient for tests
// (the embedded nil grpc.ClientStream covers the unused methods).
type fakeStream struct {
	grpc.ClientStream
	items []*machine.FileInfo
	i     int
}

func (s *fakeStream) Recv() (*machine.FileInfo, error) {
	if s.i >= len(s.items) {
		return nil, io.EOF
	}
	fi := s.items[s.i]
	s.i++
	return fi, nil
}

// fakeAPI is an in-memory file API keyed by path.
type fakeAPI struct {
	ls      map[string][]string // dir root -> entry paths
	links   map[string]string   // entry path -> symlink target
	files   map[string]string   // path -> content
	errLS   map[string]error    // dir root -> error
	errRead map[string]error    // path -> error
	calls   int

	// Per-path counters, so a test can assert what is re-read each scrape.
	// Guarded: the collector reads values concurrently.
	mu      sync.Mutex
	reads   map[string]int
	lsCalls map[string]int
}

func (a *fakeAPI) count(m *map[string]int, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[key]++
}

// countOf reads a counter under the lock.
func (a *fakeAPI) countOf(m map[string]int, key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return m[key]
}

func (a *fakeAPI) LS(_ context.Context, req *machine.ListRequest) (machine.MachineService_ListClient, error) {
	a.count(&a.lsCalls, req.GetRoot())
	if err, ok := a.errLS[req.GetRoot()]; ok {
		return nil, err
	}
	var items []*machine.FileInfo
	for _, p := range a.ls[req.GetRoot()] {
		items = append(items, &machine.FileInfo{Name: p, Link: a.links[p]})
	}
	return &fakeStream{items: items}, nil
}

func (a *fakeAPI) Read(_ context.Context, path string) (io.ReadCloser, error) {
	a.count(&a.reads, path)
	if err, ok := a.errRead[path]; ok {
		return nil, err
	}
	c, ok := a.files[path]
	if !ok {
		return nil, errors.New("no such file")
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

func testNode() *collector.NodeClient {
	return &collector.NodeClient{}
}

func testCollector(api *fakeAPI) *Collector {
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.api = func(*collector.NodeClient) fileAPI { return api }
	return c
}

// realNodeFixture mirrors the sensor files of a real cluster node
// (controlplane-1, AMD platform): read 2026-08-22 via `talosctl read`.
func realNodeFixture() *fakeAPI {
	hwmon := func(chip string, entries ...string) string { return "/sys/class/hwmon/" + chip }
	return &fakeAPI{
		ls: map[string][]string{
			"/sys/class/hwmon": {
				"/sys/class/hwmon/.",
				"/sys/class/hwmon/hwmon0",
				"/sys/class/hwmon/hwmon1",
				"/sys/class/hwmon/hwmon2",
				"/sys/class/hwmon/hwmon3",
			},
			hwmon("hwmon0"): {hwmon("hwmon0") + "/name", hwmon("hwmon0") + "/temp1_input"},
			hwmon("hwmon1"): {
				hwmon("hwmon1") + "/name",
				hwmon("hwmon1") + "/temp1_input",
				hwmon("hwmon1") + "/temp1_label",
			},
			hwmon("hwmon2"): {
				hwmon("hwmon2") + "/name",
				hwmon("hwmon2") + "/temp1_input",
				hwmon("hwmon2") + "/temp1_label",
			},
			hwmon("hwmon3"): {
				hwmon("hwmon3") + "/name",
				hwmon("hwmon3") + "/freq1_input",
				hwmon("hwmon3") + "/freq1_label",
				hwmon("hwmon3") + "/in0_input",
				hwmon("hwmon3") + "/in0_label",
				hwmon("hwmon3") + "/in1_input",
				hwmon("hwmon3") + "/in1_label",
				hwmon("hwmon3") + "/power1_input",
				hwmon("hwmon3") + "/power1_label",
				hwmon("hwmon3") + "/temp1_input",
				hwmon("hwmon3") + "/temp1_label",
				hwmon("hwmon3") + "/temp1_crit", // must be ignored (not an *_input)
			},
			"/sys/class/thermal": {
				"/sys/class/thermal/cooling_device0", // ignored (no match)
				"/sys/class/thermal/thermal_zone0",
			},
		},
		files: map[string]string{
			"/sys/class/hwmon/hwmon0/name":          "acpitz\n",
			"/sys/class/hwmon/hwmon0/temp1_input":   "20000\n",
			"/sys/class/hwmon/hwmon1/name":          "k10temp\n",
			"/sys/class/hwmon/hwmon1/temp1_input":   "61000\n",
			"/sys/class/hwmon/hwmon1/temp1_label":   "Tctl\n",
			"/sys/class/hwmon/hwmon2/name":          "nvme\n",
			"/sys/class/hwmon/hwmon2/temp1_input":   "44850\n",
			"/sys/class/hwmon/hwmon2/temp1_label":   "Composite\n",
			"/sys/class/hwmon/hwmon3/name":          "amdgpu\n",
			"/sys/class/hwmon/hwmon3/freq1_input":   "1786000000\n",
			"/sys/class/hwmon/hwmon3/freq1_label":   "sclk\n",
			"/sys/class/hwmon/hwmon3/in0_input":     "1175\n",
			"/sys/class/hwmon/hwmon3/in0_label":     "vddgfx\n",
			"/sys/class/hwmon/hwmon3/in1_input":     "745\n",
			"/sys/class/hwmon/hwmon3/in1_label":     "vddnb\n",
			"/sys/class/hwmon/hwmon3/power1_input":  "21160000\n",
			"/sys/class/hwmon/hwmon3/power1_label":  "PPT\n",
			"/sys/class/hwmon/hwmon3/temp1_input":   "59000\n",
			"/sys/class/hwmon/hwmon3/temp1_label":   "edge\n",
			"/sys/class/thermal/thermal_zone0/type": "acpitz\n",
			"/sys/class/thermal/thermal_zone0/temp": "20000\n",
		},
		errLS:   map[string]error{},
		errRead: map[string]error{},
	}
}

// gathered returns the node's talos_sensor_value series as label→value.
func gathered(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		// One family per unit now, so collect them all and key by name.
		if !strings.HasPrefix(f.GetName(), "talos_sensor_") ||
			strings.HasSuffix(f.GetName(), "_alarm") ||
			strings.Contains(f.GetName(), "_limit_") {
			continue
		}
		for _, m := range f.GetMetric() {
			sort.Slice(m.GetLabel(), func(i, j int) bool {
				return m.GetLabel()[i].GetName() < m.GetLabel()[j].GetName()
			})
			var b strings.Builder
			b.WriteString(f.GetName())
			b.WriteString("|")
			for _, lp := range m.GetLabel() {
				b.WriteString(lp.GetName())
				b.WriteString("=")
				b.WriteString(lp.GetValue())
				b.WriteString(",")
			}
			out[strings.TrimSuffix(b.String(), ",")] = m.GetGauge().GetValue()
		}
	}
	return out
}

// wantSeries builds the key gathered() produces. The kind is the metric name
// now, not a label, so each unit has its own family.
func wantSeries(chip, chipName, sensor, kind, label string) string {
	metric := map[string]string{
		"temperature": "talos_sensor_temperature_celsius",
		"fan":         "talos_sensor_fan_rpm",
		"voltage":     "talos_sensor_voltage_volts",
		"power":       "talos_sensor_power_watts",
		"frequency":   "talos_sensor_frequency_hertz",
	}[kind]
	return metric + "|chip=" + chip + ",chip_name=" + chipName + ",label=" + label + ",node=,sensor=" + sensor
}

func TestCollectSensors(t *testing.T) {
	api := realNodeFixture()
	c := testCollector(api)
	reg := prometheus.NewRegistry()

	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := gathered(t, reg)
	want := map[string]float64{
		wantSeries("/sys/class/hwmon/hwmon0", "acpitz", "temp1", "temperature", ""):        20,
		wantSeries("/sys/class/hwmon/hwmon1", "k10temp", "temp1", "temperature", "Tctl"):   61,
		wantSeries("/sys/class/hwmon/hwmon2", "nvme", "temp1", "temperature", "Composite"): 44.85,
		// Frequency is hertz in the metric (the collector reads MHz for the UI).
		wantSeries("/sys/class/hwmon/hwmon3", "amdgpu", "freq1", "frequency", "sclk"):       1786e6,
		wantSeries("/sys/class/hwmon/hwmon3", "amdgpu", "in0", "voltage", "vddgfx"):         1.175,
		wantSeries("/sys/class/hwmon/hwmon3", "amdgpu", "in1", "voltage", "vddnb"):          0.745,
		wantSeries("/sys/class/hwmon/hwmon3", "amdgpu", "power1", "power", "PPT"):           21.16,
		wantSeries("/sys/class/hwmon/hwmon3", "amdgpu", "temp1", "temperature", "edge"):     59,
		wantSeries("/sys/class/thermal/thermal_zone0", "acpitz", "temp", "temperature", ""): 20,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d series, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("series %s = %v, want %v", k, got[k], v)
		}
	}
}

func TestCollectPermissionDeniedSelfDisables(t *testing.T) {
	api := realNodeFixture()
	api.errLS["/sys/class/hwmon"] = status.Error(codes.PermissionDenied, "permission denied")
	c := testCollector(api)
	reg := prometheus.NewRegistry()

	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("first Collect (permission error) must self-disable silently, got %v", err)
	}
	if fams, _ := reg.Gather(); len(fams) != 0 {
		t.Fatalf("expected no metrics after permission error, got %d families", len(fams))
	}

	callsAfterFirst := api.calls
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if api.calls != callsAfterFirst {
		t.Errorf("disabled collector must not touch the API: calls went %d -> %d", callsAfterFirst, api.calls)
	}
}

func TestCollectNoSensors(t *testing.T) {
	api := &fakeAPI{
		ls:    map[string][]string{"/sys/class/hwmon": {"/sys/class/hwmon/."}, "/sys/class/thermal": {"/sys/class/thermal/."}},
		files: map[string]string{},
	}
	c := testCollector(api)
	reg := prometheus.NewRegistry()

	// A machine with no hwmon chips and no thermal zones is a VM, not a
	// failure. Reporting it as an error made the scrape-error counter climb
	// forever and logged a warning every interval (review finding 1.5).
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("a node with no sensors must not be an error, got %v", err)
	}
	fams, _ := reg.Gather()
	for _, f := range fams {
		if len(f.GetMetric()) > 0 {
			t.Errorf("expected no metrics, got %s with %d series", f.GetName(), len(f.GetMetric()))
		}
	}
}

func TestCollectSkipsNonNumericAndScalesFan(t *testing.T) {
	api := &fakeAPI{
		ls: map[string][]string{
			"/sys/class/hwmon": {"/sys/class/hwmon/hwmon0"},
			"/sys/class/hwmon/hwmon0": {
				"/sys/class/hwmon/hwmon0/name",
				"/sys/class/hwmon/hwmon0/temp1_input",
				"/sys/class/hwmon/hwmon0/fan1_input",
			},
			"/sys/class/thermal": {"/sys/class/thermal/."},
		},
		files: map[string]string{
			"/sys/class/hwmon/hwmon0/name":        "test\n",
			"/sys/class/hwmon/hwmon0/temp1_input": "N/A\n",  // not a value — skipped
			"/sys/class/hwmon/hwmon0/fan1_input":  "2500\n", // RPM, no scaling
		},
	}
	c := testCollector(api)
	reg := prometheus.NewRegistry()

	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := gathered(t, reg)
	if len(got) != 1 {
		t.Fatalf("got %d series, want 1: %v", len(got), got)
	}
	if v := got[wantSeries("/sys/class/hwmon/hwmon0", "test", "fan1", "fan", "")]; v != 2500 {
		t.Errorf("fan1 = %v, want 2500", v)
	}
}

func TestCollectChipListErrorNotDisabled(t *testing.T) {
	api := realNodeFixture()
	api.errLS["/sys/class/hwmon/hwmon1"] = errors.New("boom")
	c := testCollector(api)
	reg := prometheus.NewRegistry()

	err := c.Collect(context.Background(), testNode(), reg)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected chip list error to propagate, got %v", err)
	}
	// Non-permission error: the collector stays enabled.
	delete(api.errLS, "/sys/class/hwmon/hwmon1")
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect after transient error: %v", err)
	}
}

func TestLayoutIsDiscoveredOnceAndValuesRereadEachScrape(t *testing.T) {
	api := realNodeFixture()
	c := testCollector(api)
	t0 := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return t0 }

	for i := 0; i < 3; i++ {
		if err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry()); err != nil {
			t.Fatalf("collect %d: %v", i, err)
		}
	}
	// Chip names and labels describe the hardware and cannot change while the
	// machine runs; re-reading them every scrape is what made this collector
	// half of all Talos API traffic.
	if n := api.countOf(api.reads, "/sys/class/hwmon/hwmon1/name"); n != 1 {
		t.Errorf("chip name read %d times across 3 scrapes, want 1", n)
	}
	if n := api.countOf(api.lsCalls, "/sys/class/hwmon"); n != 1 {
		t.Errorf("hwmon root listed %d times across 3 scrapes, want 1", n)
	}
	// Values must be fresh every time.
	if n := api.countOf(api.reads, "/sys/class/hwmon/hwmon1/temp1_input"); n != 3 {
		t.Errorf("sensor value read %d times across 3 scrapes, want 3", n)
	}

	// Past the layout TTL the machine is re-inspected: a drive can be added.
	c.now = func() time.Time { return t0.Add(layoutTTL + time.Minute) }
	if err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry()); err != nil {
		t.Fatalf("collect after TTL: %v", err)
	}
	if n := api.countOf(api.lsCalls, "/sys/class/hwmon"); n != 2 {
		t.Errorf("hwmon root listed %d times, want 2 (rediscovery past the TTL)", n)
	}
}

func TestLimitsAndAlarmsAreExported(t *testing.T) {
	api := &fakeAPI{
		ls: map[string][]string{
			"/sys/class/hwmon":   {"/sys/class/hwmon/hwmon0"},
			"/sys/class/thermal": {"/sys/class/thermal/."},
			"/sys/class/hwmon/hwmon0": {
				"/sys/class/hwmon/hwmon0/name",
				"/sys/class/hwmon/hwmon0/temp1_input",
				"/sys/class/hwmon/hwmon0/temp1_label",
				"/sys/class/hwmon/hwmon0/temp1_crit",
				"/sys/class/hwmon/hwmon0/temp1_max",
				"/sys/class/hwmon/hwmon0/temp1_crit_alarm",
			},
		},
		files: map[string]string{
			"/sys/class/hwmon/hwmon0/name":             "coretemp",
			"/sys/class/hwmon/hwmon0/temp1_input":      "55000",
			"/sys/class/hwmon/hwmon0/temp1_label":      "Package id 0",
			"/sys/class/hwmon/hwmon0/temp1_crit":       "100000",
			"/sys/class/hwmon/hwmon0/temp1_max":        "80000",
			"/sys/class/hwmon/hwmon0/temp1_crit_alarm": "1",
		},
	}
	c := testCollector(api)
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	fams, _ := reg.Gather()
	get := func(name string, want map[string]string) (float64, bool) {
		for _, f := range fams {
			if f.GetName() != name {
				continue
			}
		metric:
			for _, m := range f.GetMetric() {
				got := map[string]string{}
				for _, l := range m.GetLabel() {
					got[l.GetName()] = l.GetValue()
				}
				for k, v := range want {
					if got[k] != v {
						continue metric
					}
				}
				return m.GetGauge().GetValue(), true
			}
		}
		return 0, false
	}
	// A reading without its threshold cannot be judged; both must be exported
	// so the UI and PromQL can compute the same percentage.
	if v, ok := get("talos_sensor_temperature_celsius", map[string]string{"label": "Package id 0"}); !ok || v != 55 {
		t.Errorf("value = %v %v, want 55", v, ok)
	}
	if v, ok := get("talos_sensor_temperature_limit_celsius", map[string]string{"label": "Package id 0", "limit": "critical"}); !ok || v != 100 {
		t.Errorf("critical limit = %v %v, want 100", v, ok)
	}
	if v, ok := get("talos_sensor_temperature_limit_celsius", map[string]string{"label": "Package id 0", "limit": "max"}); !ok || v != 80 {
		t.Errorf("max limit = %v %v, want 80", v, ok)
	}
	if v, ok := get("talos_sensor_alarm", map[string]string{"label": "Package id 0"}); !ok || v != 1 {
		t.Errorf("alarm = %v %v, want 1", v, ok)
	}
}

// TestOneFamilyPerUnit pins the split. A single talos_sensor_value carrying
// °C, RPM, volts, watts and MHz meant sum() added volts to RPM, and the
// obvious first query — topk(5, talos_sensor_value) — ranked a 4 000 RPM fan
// above an 85 °C critical temperature.
func TestOneFamilyPerUnit(t *testing.T) {
	c := testCollector(realNodeFixture())
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range fams {
		names[f.GetName()] = true
	}

	// The mixed-unit families are gone.
	for _, gone := range []string{"talos_sensor_value", "talos_sensor_limit"} {
		if names[gone] {
			t.Errorf("%s still exists: it mixed units in one family", gone)
		}
	}
	// Every kind the fixture has gets its own unit-suffixed family.
	for _, want := range []string{
		"talos_sensor_temperature_celsius",
		"talos_sensor_voltage_volts",
		"talos_sensor_power_watts",
		"talos_sensor_frequency_hertz",
	} {
		if !names[want] {
			t.Errorf("missing %s", want)
		}
	}
	// talos_sensor_alarm is deliberately NOT split: a flag is unitless, so one
	// family covers every kind. It is absent here only because this fixture
	// has no alarm files; TestLimitsAndAlarmsAreExported covers it.
	// `kind` must not survive as a label: it is the metric name now.
	for _, f := range fams {
		if !strings.HasPrefix(f.GetName(), "talos_sensor_") || f.GetName() == "talos_sensor_alarm" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "kind" {
					t.Errorf("%s still carries a kind label", f.GetName())
				}
			}
		}
	}
}

// NVMe brackets an unset threshold with sentinels at both ends: temp*_min reads
// -273150 m°C (0 Kelvin) and temp*_max reads 65261850 m°C. Exporting the low one
// made min() over the limit family return absolute zero; the high one put
// 65261.85 °C into max() and made percent-of-limit on that sensor read near
// zero. An unset threshold is an absent series, not a sentinel.
func TestUnsetTemperatureThresholdIsNotExported(t *testing.T) {
	api := &fakeAPI{
		ls: map[string][]string{
			"/sys/class/hwmon":        {"/sys/class/hwmon/hwmon0"},
			"/sys/class/hwmon/hwmon0": {"/sys/class/hwmon/hwmon0/name", "/sys/class/hwmon/hwmon0/temp1_input", "/sys/class/hwmon/hwmon0/temp1_min", "/sys/class/hwmon/hwmon0/temp1_max", "/sys/class/hwmon/hwmon0/temp1_crit"},
			"/sys/class/thermal":      {},
		},
		files: map[string]string{
			"/sys/class/hwmon/hwmon0/name":        "nvme",
			"/sys/class/hwmon/hwmon0/temp1_input": "44850",
			"/sys/class/hwmon/hwmon0/temp1_min":   "-273150",  // unset, low
			"/sys/class/hwmon/hwmon0/temp1_max":   "65261850", // unset, high
			"/sys/class/hwmon/hwmon0/temp1_crit":  "84850",    // real
		},
	}
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.api = func(*collector.NodeClient) fileAPI { return api }
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(),
		testNode(), reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var mins, crits int
	for _, f := range fams {
		if f.GetName() != "talos_sensor_temperature_limit_celsius" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != "limit" {
					continue
				}
				switch l.GetValue() {
				case "min":
					mins++
					t.Errorf("unset min threshold exported as %v", m.GetGauge().GetValue())
				case "max":
					mins++
					t.Errorf("unset max threshold exported as %v", m.GetGauge().GetValue())
				case "critical":
					crits++
				}
			}
		}
	}
	if mins != 0 {
		t.Errorf("%d unset min thresholds exported, want 0", mins)
	}
	// The real threshold on the same sensor must survive.
	if crits != 1 {
		t.Errorf("critical thresholds exported = %d, want 1", crits)
	}
}

// A hwmon chip is a symlink into /sys/devices, and the last PCI address in the
// target is the device that owns it. Without that link a two-GPU node's
// readings cannot be told apart, and guessing by hwmon numbering mislabels them.
func TestChipInfoCarriesTheOwningPCIDevice(t *testing.T) {
	api := &fakeAPI{
		ls: map[string][]string{
			"/sys/class/hwmon":        {"/sys/class/hwmon/hwmon0", "/sys/class/hwmon/hwmon1"},
			"/sys/class/hwmon/hwmon0": {"/sys/class/hwmon/hwmon0/name", "/sys/class/hwmon/hwmon0/temp1_input"},
			"/sys/class/hwmon/hwmon1": {"/sys/class/hwmon/hwmon1/name", "/sys/class/hwmon/hwmon1/temp1_input"},
			"/sys/class/thermal":      {},
		},
		links: map[string]string{
			// A bridge chain ending at the card itself.
			"/sys/class/hwmon/hwmon0": "../../devices/pci0000:00/0000:00:01.1/0000:01:00.0/0000:03:00.0/hwmon/hwmon0",
			// A thermal zone has no PCI parent at all.
			"/sys/class/hwmon/hwmon1": "../../devices/virtual/thermal/thermal_zone0/hwmon1",
		},
		files: map[string]string{
			"/sys/class/hwmon/hwmon0/name":        "amdgpu",
			"/sys/class/hwmon/hwmon0/temp1_input": "40000",
			"/sys/class/hwmon/hwmon1/name":        "acpitz",
			"/sys/class/hwmon/hwmon1/temp1_input": "43000",
		},
	}
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.api = func(*collector.NodeClient) fileAPI { return api }
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]string{}
	for _, f := range fams {
		if f.GetName() != "talos_sensor_chip_info" {
			continue
		}
		for _, mt := range f.GetMetric() {
			var chip, pci string
			for _, l := range mt.GetLabel() {
				switch l.GetName() {
				case "chip":
					chip = l.GetValue()
				case "pci":
					pci = l.GetValue()
				}
			}
			got[chip] = pci
		}
	}
	if want := "0000:03:00.0"; got["/sys/class/hwmon/hwmon0"] != want {
		t.Errorf("hwmon0 pci = %q, want %q (the last address in the chain, not a bridge)",
			got["/sys/class/hwmon/hwmon0"], want)
	}
	if _, ok := got["/sys/class/hwmon/hwmon1"]; ok {
		t.Errorf("hwmon1 has no PCI parent but got a series: %q", got["/sys/class/hwmon/hwmon1"])
	}
}

// A discrete AMD card publishes power1_average and no power1_input, while the
// integrated GPU beside it publishes both. Matching only _input read the iGPU's
// 7 W and silently omitted the 300 W card; taking both would have counted the
// iGPU twice. The board limit lives in power1_cap, not power1_max.
func TestPowerFallsBackToAverageAndReadsTheCap(t *testing.T) {
	api := &fakeAPI{
		ls: map[string][]string{
			"/sys/class/hwmon": {"/sys/class/hwmon/hwmon3", "/sys/class/hwmon/hwmon4"},
			// discrete: an average and a cap, no _input
			"/sys/class/hwmon/hwmon3": {
				"/sys/class/hwmon/hwmon3/name",
				"/sys/class/hwmon/hwmon3/power1_average",
				"/sys/class/hwmon/hwmon3/power1_cap",
				"/sys/class/hwmon/hwmon3/power1_cap_max",
			},
			// integrated: both, so _input must win and _average must not
			// arrive as a second sensor
			"/sys/class/hwmon/hwmon4": {
				"/sys/class/hwmon/hwmon4/name",
				"/sys/class/hwmon/hwmon4/power1_input",
				"/sys/class/hwmon/hwmon4/power1_average",
			},
			"/sys/class/thermal": {},
		},
		files: map[string]string{
			"/sys/class/hwmon/hwmon3/name":           "amdgpu",
			"/sys/class/hwmon/hwmon3/power1_average": "300000000",
			"/sys/class/hwmon/hwmon3/power1_cap":     "300000000",
			"/sys/class/hwmon/hwmon3/power1_cap_max": "300000000",
			"/sys/class/hwmon/hwmon4/name":           "amdgpu",
			"/sys/class/hwmon/hwmon4/power1_input":   "7064000",
			"/sys/class/hwmon/hwmon4/power1_average": "9136000",
		},
	}
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.api = func(*collector.NodeClient) fileAPI { return api }
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	byChip := map[string]float64{}
	caps := map[string]float64{}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			var chip, limit string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "chip":
					chip = l.GetValue()
				case "limit":
					limit = l.GetValue()
				}
			}
			switch f.GetName() {
			case "talos_sensor_power_watts":
				if _, dup := byChip[chip]; dup {
					t.Errorf("%s reported twice: _input and _average both became sensors", chip)
				}
				byChip[chip] = m.GetGauge().GetValue()
			case "talos_sensor_power_limit_watts":
				caps[chip+"/"+limit] = m.GetGauge().GetValue()
			}
		}
	}
	if got := byChip["/sys/class/hwmon/hwmon3"]; got != 300 {
		t.Errorf("discrete card = %v W, want 300 (from power1_average)", got)
	}
	if got := byChip["/sys/class/hwmon/hwmon4"]; got != 7.064 {
		t.Errorf("integrated GPU = %v W, want 7.064 (from power1_input, not the average)", got)
	}
	if got := caps["/sys/class/hwmon/hwmon3/cap"]; got != 300 {
		t.Errorf("board power cap = %v W, want 300", got)
	}
}

// TestDegradedReflectsSelfDisable: the disable flag is cluster-wide, so every
// node reports it - that is the real blast radius of one node's permission
// error. Stopped tells the scraper the round reached the node zero times, so
// the round cannot vote on talos_node_up.
func TestDegradedReflectsSelfDisable(t *testing.T) {
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if d := c.Degraded("n1"); len(d) != 0 {
		t.Fatalf("a live collector reported %v", d)
	}
	if err := c.disable("n1"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	d := c.Degraded("n1")
	if len(d) != 1 || d[0].Reason != collector.ReasonPermission || !d[0].Stopped {
		t.Errorf("Degraded = %+v, want one stopped %q degradation", d, collector.ReasonPermission)
	}
}
