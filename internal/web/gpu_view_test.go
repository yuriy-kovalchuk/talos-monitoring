package web_test

import (
	"net/http"
	"strings"
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

func TestGPUPageWithoutGPUsRenders(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/gpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/gpu: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, `href="/nodes/node-1/pci"`)
}

// Under os:reader a GPU is known from the PCI inventory only: it must render, named, without claiming absent counters.
func TestGPUPageShowsReaderTierRows(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.PCI = []snapshot.PCIDevice{{BDF: "0000:03:00.0", Product: "Navi 48 [Radeon AI PRO R9700]"}}
		n.GPUs = []snapshot.GPU{{Card: "card0", Driver: "amdgpu", Slot: "0000:03:00.0", PCIID: "1002:7551", CountersDenied: true}}
	})
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/gpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/gpu: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "Navi 48 [Radeon AI PRO R9700]", "0000:03:00.0", "os:admin")
	if strings.Contains(body, "not reported by") {
		t.Error("a GPU whose counters were refused is blamed on its driver")
	}
}

// NVIDIA exposes no counters to the Talos API at any role, so its card must not send the operator after os:admin.
func TestGPUPageDoesNotAskForAdminOnNvidia(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.PCI = []snapshot.PCIDevice{{BDF: "0000:01:00.0", Product: "GP104 [GeForce GTX 1070]"}}
		n.GPUs = []snapshot.GPU{{Card: "card1", Driver: "nvidia", Slot: "0000:01:00.0", PCIID: "10de:1b81", CountersDenied: true}}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/gpu")
	checkBody(t, body, "GP104 [GeForce GTX 1070]", "0000:01:00.0")
	if strings.Contains(body, "os:admin") {
		t.Error("an NVIDIA card is told os:admin would unlock its counters")
	}
}

// A display device with no driver has no card and no counters; that is not an os:admin gap.
func TestGPUPageDeviceWithoutDriver(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.GPUs = []snapshot.GPU{{Slot: "0000:00:02.0", PCIID: "8086:1912"}}
	})
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/gpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/gpu: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "0000:00:02.0")
	for _, wrong := range []string{"os:admin", "not reported by"} {
		if strings.Contains(body, wrong) {
			t.Errorf("a driverless device rendered %q", wrong)
		}
	}
}
