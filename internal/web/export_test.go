package web

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

// ShortCPUModelForTest exposes shortCPUModel to the external test package.
func ShortCPUModelForTest(s string) string { return shortCPUModel(s) }

// WritePartialErrorForTest runs writePartial's error path (template failure
// → 500). Healthy endpoints never fail a template render, so the external
// test package needs a direct handle to pin the 5xx contract.
func WritePartialErrorForTest(w http.ResponseWriter) {
	d := &dashboard{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.writePartial(w, pagesTmpl, "no-such-block", map[string]any{})
}

// NodeDataForTest reports which datasets a node page section loads, as
// (system, cpu, memory, pci, disks, sensors, status, summary, links, health,
// identity, gpu, timesync, kernel, volumeLayer, diskIO).
func NodeDataForTest(section string) [16]bool {
	d := nodeDataFor(section)
	return [16]bool{d.system, d.cpu, d.memory, d.pci, d.disks, d.sensors, d.status,
		d.summary, d.links, d.health, d.identity, d.gpu, d.timesync, d.kernel, d.volumeLayer, d.diskIO}
}

// DMIValueForTest exposes dmiValue to the external test package.
func DMIValueForTest(s string) string { return dmiValue(s) }

// RenderErrorForTest runs render's content-failure path (template error →
// 500). A healthy page never fails a template render, so the external test
// package needs a direct handle to pin the 5xx contract: an empty 200 would
// read as a blank dashboard, not a failed render.
func RenderErrorForTest(w http.ResponseWriter, r *http.Request) {
	d := &dashboard{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		list:    func() []nodes.Node { return nil },
		status:  func(string) (string, bool) { return "", false },
		gather:  prometheus.NewRegistry(),
		volumes: func() map[string]k8svolumes.Volume { return nil },
	}
	d.render(w, r, pageData{Template: "no-such-block"})
}
