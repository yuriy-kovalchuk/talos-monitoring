package web_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/web"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

var testNodes = []nodes.Node{{
	Name:        "node-1",
	IP:          "10.0.0.10",
	Hostname:    "node-1",
	KubeVersion: "v1.35.0",
	OS:          "linux",
	Arch:        "arm64",
	Roles:       []string{"control-plane"},
}}

// newHandler builds a router; status nil means no node is verified.
func newHandler(t *testing.T, ready, withNodes bool, status func(string) (string, bool)) http.Handler {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	return newHandlerWithReg(t, reg, ready, withNodes, status)
}

// testSnap is the snapshot store the current test is building. Fixtures fill
// it through snapFor(t); newHandlerWithReg hands it to the router.
//
// The dashboard reads typed structs now, so a fixture describes a node the way
// a collector does rather than by hand-assembling the metric labels the UI used
// to re-parse. The assertions on rendered HTML are unchanged, which is what
// makes them a safety net for the inversion.
var testSnap *snapshot.Store

// snapFor returns the store for this test, resetting it on first use.
func snapFor(t *testing.T) *snapshot.Store {
	t.Helper()
	if testSnap == nil {
		testSnap = snapshot.New()
		t.Cleanup(func() { testSnap = nil })
	}
	return testSnap
}

// newHandlerWithReg builds a router with a caller-provided registry.
func newHandlerWithReg(t *testing.T, reg *prometheus.Registry, ready, withNodes bool, status func(string) (string, bool)) http.Handler {
	t.Helper()
	list := func() []nodes.Node { return nil }
	if withNodes {
		list = func() []nodes.Node { return testNodes }
	}
	if status == nil {
		status = func(string) (string, bool) { return "", false }
	}
	return web.NewRouter(discardLog, reg, func() bool { return ready }, list, status,
		history.New(10*time.Minute), snapFor(t), testVolumes)
}

// testVolumes is the PersistentVolume snapshot the join uses. Two of the
// fixtures' PVs are known to Kubernetes; anything else renders unnamed.
func testVolumes() map[string]k8svolumes.Volume {
	return map[string]k8svolumes.Volume{
		"pvc-1111": {Name: "pvc-1111", Namespace: "forgejo", Claim: "forgejo-data",
			StorageClass: "truenas-iscsi", Driver: "org.democratic-csi.iscsi", Capacity: 50 << 30},
	}
}

func do(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec, string(body)
}

func checkBody(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func mustGauge(name string, labelNames []string, pairs ...any) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: "test fixture"}, labelNames)
	for i := 0; i+1 < len(pairs); i += 2 {
		labels := pairs[i].(map[string]string)
		var val float64
		switch v := pairs[i+1].(type) {
		case float64:
			val = v
		case int:
			val = float64(v)
		default:
			panic(fmt.Sprintf("mustGauge: value must be float64 or int, got %T", pairs[i+1]))
		}
		lvs := make([]string, 0, len(labelNames))
		for _, ln := range labelNames {
			lvs = append(lvs, labels[ln])
		}
		g.WithLabelValues(lvs...).Set(val)
	}
	return g
}

// keyedGauge registers a {node,<key>} gauge from a map of key -> value.
func keyedGauge(t *testing.T, reg *prometheus.Registry, name, key, node string, values map[string]float64) {
	t.Helper()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: "test fixture"},
		[]string{"node", key})
	reg.MustRegister(g)
	for k, v := range values {
		g.WithLabelValues(node, k).Set(v)
	}
}

// snapNode fills one node's snapshot section. Fixtures describe a node the way
// a collector does now, instead of hand-assembling the labels the UI used to
// re-parse.
func snapNode(t *testing.T, node string, fn func(*snapshot.Node)) {
	t.Helper()
	snapFor(t).Update(node, fn)
}

// processorMeasures emits the per-socket gauges for one uniform CPU model.
func processorMeasures(t *testing.T, reg *prometheus.Registry, node string, sockets []string,
	cores, coresEnabled, threads, maxMHz, bootMHz float64) {
	t.Helper()
	for name, v := range map[string]float64{
		"talos_hw_processor_cores":         cores,
		"talos_hw_processor_cores_enabled": coresEnabled,
		"talos_hw_processor_threads":       threads,
		"talos_hw_processor_max_hertz":     maxMHz * 1e6,
		"talos_hw_processor_boot_hertz":    bootMHz * 1e6,
		"talos_hw_processor_status":        0,
	} {
		per := make(map[string]float64, len(sockets))
		for _, sock := range sockets {
			per[sock] = v
		}
		keyedGauge(t, reg, name, "socket", node, per)
	}
}

// moduleMeasures emits the per-DIMM gauges for uniform modules.
func moduleMeasures(t *testing.T, reg *prometheus.Registry, node string, slots []string, sizeMiB, speedMTs float64) {
	t.Helper()
	size, speed := make(map[string]float64, len(slots)), make(map[string]float64, len(slots))
	for _, sl := range slots {
		size[sl] = sizeMiB * 1024 * 1024
		speed[sl] = speedMTs * 1e6
	}
	keyedGauge(t, reg, "talos_hw_memory_module_size_bytes", "slot", node, size)
	keyedGauge(t, reg, "talos_hw_memory_module_speed_transfers_per_second", "slot", node, speed)
}

// diskMeasures emits the per-disk gauges. rotational lists the spinning disks.
func diskMeasures(t *testing.T, reg *prometheus.Registry, node string, sizes map[string]float64, rotational ...string) {
	t.Helper()
	keyedGauge(t, reg, "talos_block_disk_size_bytes", "device", node, sizes)
	rot := make(map[string]float64, len(sizes))
	for dev := range sizes {
		rot[dev] = 0
	}
	for _, dev := range rotational {
		rot[dev] = 1
	}
	keyedGauge(t, reg, "talos_block_disk_rotational", "device", node, rot)
}

// attentionCard returns just the attention card's markup, so an assertion
// about what the card says cannot be satisfied by the tables below it.
func attentionCard(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "Needs attention")
	if i < 0 {
		t.Fatal("no attention card on the overview")
	}
	// The fleet tiles follow the card; anything past that is another section
	// and must not satisfy an assertion about what the card says.
	j := strings.Index(body[i:], `<div class="stats">`)
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}
