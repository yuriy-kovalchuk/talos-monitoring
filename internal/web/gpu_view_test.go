package web_test

import (
	"net/http"
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// A driver that publishes no counters must say so rather than render 0%.
func TestGPUPageDistinguishesAbsentFromZero(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.GPUs = []snapshot.GPU{
			{Card: "card0", Driver: "amdgpu", Slot: "0000:03:00.0",
				HasBusy: true, BusyPercent: 0,
				HasVRAM: true, VRAMTotalBytes: 34208743424, VRAMUsedBytes: 32394584064},
			{Card: "card1", Driver: "i915", Slot: "0000:00:02.0"},
			{Card: "card2", Driver: "amdgpu", Slot: "0000:41:00.0",
				HasBusy: true, BusyPercent: 0, Suspended: true},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/gpu")

	// card0 is genuinely idle at 0%, and 94% of its VRAM is gone.
	checkBody(t, body, "amdgpu", "94%", "0%")
	// card1 reports nothing, which must not read as an idle, empty GPU.
	checkBody(t, body, "not reported by i915")
	// card2 is asleep: its 0 is derived, and the state must say so rather
	// than the driver being blamed for not reporting.
	checkBody(t, body, "asleep")
}

func TestGPUPageWithoutCardsExplainsWhy(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/gpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/gpu: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "No GPU with a driver bound")
}
