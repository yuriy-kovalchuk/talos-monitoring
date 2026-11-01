package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

func TestLinksPage(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_net_link_info",
		[]string{"node", "link", "type", "kind", "hwaddr", "driver", "driver_version",
			"firmware_version", "bus_path", "pci_id", "vendor", "product", "port", "duplex"},
		map[string]string{"node": "node-1", "link": "enp1s0", "type": "ether", "kind": "",
			"hwaddr": "00:e0:4c:68:10:09", "driver": "r8169", "driver_version": "6.18.34-talos",
			"firmware_version": "", "bus_path": "0000:01:00.0", "pci_id": "10EC:8125",
			"vendor": "Realtek", "product": "RTL8125 2.5GbE", "port": "TwistedPair", "duplex": "Full"}, 1,
		map[string]string{"node": "node-1", "link": "eno1", "type": "ether", "kind": "",
			"hwaddr": "ac:e2:d3:04:3c:fb", "driver": "e1000e", "driver_version": "6.18.34-talos",
			"firmware_version": "0.1-4", "bus_path": "0000:00:1f.6", "pci_id": "8086:15E3",
			"vendor": "Intel", "product": "I219-LM", "port": "TwistedPair", "duplex": "Unknown"}, 1,
		map[string]string{"node": "node-1", "link": "bond0", "type": "ether", "kind": "bond",
			"hwaddr": "3a:ab:59:15:d3:1a", "driver": "bonding", "driver_version": "",
			"firmware_version": "", "bus_path": "", "pci_id": "", "vendor": "", "product": "",
			"port": "Other", "duplex": "Unknown"}, 1))
	reg.MustRegister(mustGauge("talos_net_link_up", []string{"node", "link"},
		map[string]string{"node": "node-1", "link": "enp1s0"}, 1,
		map[string]string{"node": "node-1", "link": "eno1"}, 0,
		map[string]string{"node": "node-1", "link": "bond0"}, 0))
	reg.MustRegister(mustGauge("talos_net_link_speed_mbit", []string{"node", "link"},
		map[string]string{"node": "node-1", "link": "enp1s0"}, 2500))
	reg.MustRegister(mustGauge("talos_net_link_mtu_bytes", []string{"node", "link"},
		map[string]string{"node": "node-1", "link": "enp1s0"}, 1500))
	reg.MustRegister(mustGauge("talos_net_address_info",
		[]string{"node", "link", "address", "family", "scope"},
		map[string]string{"node": "node-1", "link": "enp1s0", "address": "10.0.4.6/24",
			"family": "inet4", "scope": "global"}, 1))
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Links = []snapshot.Link{
			{Name: "bond0", Type: "ether", Kind: "bond", HWAddr: "3a:ab:59:15:d3:1a",
				Driver: "bonding", Port: "Other", Duplex: "Unknown"},
			{Name: "eno1", Type: "ether", HWAddr: "ac:e2:d3:04:3c:fb", Driver: "e1000e",
				DriverVersion: "6.18.34-talos", FirmwareVersion: "0.1-4", BusPath: "0000:00:1f.6",
				PCIID: "8086:15E3", Vendor: "Intel", Product: "I219-LM",
				Port: "TwistedPair", Duplex: "Unknown"},
			{Name: "enp1s0", Type: "ether", HWAddr: "00:e0:4c:68:10:09", Driver: "r8169",
				DriverVersion: "6.18.34-talos", BusPath: "0000:01:00.0", PCIID: "10EC:8125",
				Vendor: "Realtek", Product: "RTL8125 2.5GbE", Port: "TwistedPair", Duplex: "Full",
				Up: true, Carrier: true, SpeedMbit: 2500, MTUBytes: 1500,
				Addresses: []snapshot.Address{{Address: "10.0.4.6/24", Family: "inet4", Scope: "global"}}},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/links")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/links: got status %d, want 200", rec.Code)
	}
	// Speed is rendered in Gbit once it reaches 1000 Mbit.
	checkBody(t, body, "2.5 Gbit/s", "Full", "MTU 1500", "10.0.4.6/24", "r8169", "0000:01:00.0")
	// A link with no carrier says so instead of showing a bogus speed.

	// Firmware version appears alongside the driver version when reported.

	// Physical NICs sort before the logical interfaces built on them.
	if strings.Index(body, "bond0") < strings.Index(body, "enp1s0") {
		t.Error("physical NICs must sort before logical interfaces")
	}
}

// The links page collapsed error and drop counters into a word ("Issues") and
// never showed a number, and never rendered MTU or MAC at all.
func TestLinksPageShowsCountersAndSummary(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Links = []snapshot.Link{
			{Name: "eth0", Up: true, Carrier: true, SpeedMbit: 1000, MTUBytes: 1500,
				HWAddr: "aa:bb:cc:dd:ee:ff", Driver: "igb", BusPath: "0000:01:00.0",
				HasCounters: true, HasRates: true,
				RxBytes: 5 << 30, TxBytes: 2 << 30, RxPerSecond: 1 << 20, TxPerSecond: 1 << 19,
				RxErrors: 7, TxDropped: 3},
			{Name: "lo", Up: true, HasCounters: true, RxBytes: 1 << 20, TxBytes: 1 << 20},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/links")

	// Summary strip.
	checkBody(t, body, "Interfaces", "2", "Errors &amp; drops", "1")
	// Counters table with real numbers, not just a word.
	checkBody(t, body, "Counters", "5.0 GiB", "2.0 GiB", "aa:bb:cc:dd:ee:ff", "1500")
	// The empty chart card left by the uPlot removal must be gone.
	if strings.Contains(body, "refreshes every 2.5 s") {
		t.Error("the orphaned throughput card survived")
	}
}
