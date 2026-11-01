package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

func TestNodeDetailSensors(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_sensor_value",
		[]string{"node", "chip", "chip_name", "sensor", "kind", "label"},
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon1", "chip_name": "k10temp", "sensor": "temp1", "kind": "temperature", "label": "Tctl"}, 61,
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon2", "chip_name": "nvme", "sensor": "temp1", "kind": "temperature", "label": "Composite"}, 44.85,
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon3", "chip_name": "amdgpu", "sensor": "in0", "kind": "voltage", "label": "vddgfx"}, 1.175))
	// Only the nvme publishes a threshold; k10temp does not, which is normal.
	reg.MustRegister(mustGauge("talos_sensor_limit",
		[]string{"node", "chip", "chip_name", "sensor", "kind", "label", "limit"},
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon2", "chip_name": "nvme", "sensor": "temp1", "kind": "temperature", "label": "Composite", "limit": "critical"}, 85))
	reg.MustRegister(mustGauge("talos_sensor_alarm",
		[]string{"node", "chip", "chip_name", "sensor", "kind", "label"},
		map[string]string{"node": "node-1", "chip": "/sys/class/hwmon/hwmon2", "chip_name": "nvme", "sensor": "temp1", "kind": "temperature", "label": "Composite"}, 0))

	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Sensors = []snapshot.Sensor{
			{Chip: "/sys/class/hwmon/hwmon1", ChipName: "k10temp", Sensor: "temp1",
				Kind: "temperature", Label: "Tctl", Value: 61},
			{Chip: "/sys/class/hwmon/hwmon2", ChipName: "nvme", Sensor: "temp1",
				Kind: "temperature", Label: "Composite", Value: 44.85,
				Limits: map[string]float64{"critical": 85}, HasAlarm: true, Alarm: 0},
			{Chip: "/sys/class/hwmon/hwmon3", ChipName: "amdgpu", Sensor: "in0",
				Kind: "voltage", Label: "vddgfx", Value: 1.175},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/sensors")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/sensors: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, `id="sensors-live"`, `hx-get="/partials/nodes/node-1/sensors"`, `hx-trigger="every 30s"`)
	// Values render at a per-kind precision: %g turned a low voltage into
	// scientific notation.
	checkBody(t, body, "61.0", "44.9", "1.175")
	// A reading with a threshold shows how close it is; one without says so
	// rather than inventing a limit.
	checkBody(t, body, "52%", "85.0", "critical", "not reported")
	// Closest to its limit sorts first, even though it is the cooler sensor:
	// 44.9C of 85C beats 61C with no stated ceiling. Compare inside the table,
	// since the health strip names sensors above it.
	tbl := body[strings.Index(body, "<tbody>"):]
	if strings.Index(tbl, "Composite") > strings.Index(tbl, "Tctl") {
		t.Error("a sensor with a threshold must sort above one without")
	}
	// The health strip leads with the two numbers worth knowing.
	checkBody(t, body, "61.0 °C", "k10temp/Tctl", "52%", "nvme/Composite")
}
