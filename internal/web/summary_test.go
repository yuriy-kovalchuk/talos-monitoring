package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

func TestNodeDetail(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1: got status %d, want 200", rec.Code)
	}

	// Node switcher present; no collector metrics registered, so the merged
	// Machine card falls back to a single explanatory message.
	checkBody(t, body, "node-switch", "node-select", "No machine data yet")

	rec, _ = do(t, h, "/nodes/missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("/nodes/missing: got status %d, want 404", rec.Code)
	}
	rec, _ = do(t, h, "/nodes/node-1/bogus")
	if rec.Code != http.StatusNotFound {
		t.Errorf("/nodes/node-1/bogus: got status %d, want 404", rec.Code)
	}
}

func TestNodeOverviewShowsLoadAverage(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{
			Version: "v1.13.4", HasUptime: true, Uptime: 3600,
			HasLoad: true, Load1: 1.92, Load5: 1.53, Load15: 1.47,
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1")
	checkBody(t, body, "Load average", "1.92 · 1.53 · 1.47")
}

// A node whose LoadAvg RPC failed has no load to show; the row must be absent
// rather than rendering zeroes that read as an idle machine.
func TestNodeOverviewHidesLoadWhenAbsent(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{Version: "v1.13.4", HasUptime: true, Uptime: 3600}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1")
	if strings.Contains(body, "Load average") {
		t.Error("load row rendered for a node with no load reading")
	}
}
