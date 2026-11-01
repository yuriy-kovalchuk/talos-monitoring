package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// A service that publishes no health check must not read as broken. Talos
// reports Healthy=false with Unknown=true for `dashboard` and the extension
// services, so on the reference cluster two of twelve services per node look
// unhealthy to any renderer that reads Healthy alone.
func TestHealthPageSeparatesNoCheckFromFailing(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true,
			Stage:            "running",
			Ready:            true,
			Services: []snapshot.Service{
				{ID: "etcd", Running: true, Healthy: true},
				{ID: "dashboard", Running: true, Unknown: true},
				{ID: "kubelet", Running: true},
				{ID: "trustd"},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/health")

	// One of each state, and the counts that follow from them.
	for _, want := range []string{"healthy", "no check", "failing", "stopped"} {
		if !strings.Contains(body, ">"+want+"</span>") {
			t.Errorf("health page missing the %q service state", want)
		}
	}
	// Unhealthy is failing+stopped = 2. dashboard's missing health check is
	// NOT a fault and must stay out of that count.
	checkBody(t, body, "1 failing · 1 stopped", "4 services",
		"1 without a health check")
}

// Diagnostics and unmet conditions are absent on a healthy node, so their
// cards must not render at all rather than render empty.
func TestHealthPageHidesEmptyDiagnosticsAndConditions(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true, Stage: "running", Ready: true,
			Services: []snapshot.Service{{ID: "etcd", Running: true, Healthy: true}},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/health")

	checkBody(t, body, "none raised", "running")
	if strings.Contains(body, "Unmet conditions") {
		t.Error("unmet-conditions card rendered on a ready node")
	}
	if strings.Contains(body, "<th>Warning</th>") {
		t.Error("diagnostics table rendered with no diagnostics")
	}
}

// A node the health collector has not reached yet renders the empty state,
// not a page full of zeroes that reads as "everything is stopped".
func TestHealthPageWithoutDataRendersEmptyState(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/health: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "No health data yet")
	if strings.Contains(body, "of 0 services") {
		t.Error("empty node rendered service counts instead of the empty state")
	}
}

// Clock offsets are microseconds. Raw seconds render as 4.4597e-05, which is
// unreadable, so the page scales them to the unit an operator thinks in.
func TestHealthPageRendersTheClockStrip(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{HasMachineStatus: true, Stage: "running", Ready: true}
		n.TimeSync = &snapshot.TimeSync{
			HasStatus: true, Synced: true, Epoch: 2,
			HasAdjtime: true, KernelSynced: true, State: "TIME_OK",
			OffsetSeconds: -44.597e-6, MaxErrorSeconds: 0.46,
			FrequencyRatio: 1.0000331520996093,
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/health")

	// "33.2 ppm", not "+33.2": html/template escapes the leading plus to
	// &#43;, which the browser renders as "+" but a substring match will not
	// find. The sign is deliberate — drift has a direction.
	checkBody(t, body, "-44.6 µs", "460.0 ms", "33.2 ppm", "TIME_OK")
	if strings.Contains(body, "4.4597e-05") {
		t.Error("raw seconds leaked into the clock strip")
	}
}

// A control plane's health page carries the etcd card; a member that lost its leader must say so.
func TestHealthPageShowsEtcdOnControlPlanes(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Etcd = &snapshot.Etcd{MemberID: "8ecba9ca968d7a0a", ProtocolVersion: "3.6.0", Members: 3,
			DBSizeBytes: 640e6, DBInUseBytes: 108e6, Alarms: []string{"NOSPACE"}}
	})
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/health: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "8ecba9ca968d7a0a", "NOSPACE", "NO LEADER")
}
