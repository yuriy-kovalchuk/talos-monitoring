package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

func TestNodeDetailPCI(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_hw_pcidevice_info",
		Help: "test fixture",
	}, []string{"node", "bdf", "class", "subclass", "vendor", "product", "class_id", "subclass_id",
		"vendor_id", "product_id", "driver"})
	reg.MustRegister(g)
	g.WithLabelValues("node-1", "0000:01:00.0", "Non-Volatile memory controller", "", "Samsung", "PM9A1",
		"0x01", "0x08", "0x144d", "0xa808", "nvme").Set(1)
	g.WithLabelValues("node-1", "0000:00:02.0", "Display controller", "VGA compatible controller",
		"AMD/ATI", "Radeon Graphics", "0x03", "0x00", "0x1002", "0x1681", "amdgpu").Set(1)
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.PCI = []snapshot.PCIDevice{
			{BDF: "0000:00:02.0", Class: "Display controller", Subclass: "VGA compatible controller",
				Vendor: "AMD/ATI", Product: "Radeon Graphics", ClassID: "0x03", SubclassID: "0x00",
				VendorID: "0x1002", ProductID: "0x1681", Driver: "amdgpu"},
			{BDF: "0000:01:00.0", Class: "Non-Volatile memory controller",
				Vendor: "Samsung", Product: "PM9A1", ClassID: "0x01", SubclassID: "0x08",
				VendorID: "0x144d", ProductID: "0xa808", Driver: "nvme"},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/pci")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/pci: got status %d, want 200", rec.Code)
	}
	// Devices are grouped by class: one card per class, not a flat 35-row dump.
	checkBody(t, body, "0000:00:02.0", "Display controller / VGA compatible controller", "Radeon Graphics", "0x1002:0x1681", "amdgpu", "1 device")
	if n := strings.Count(body, "<details class=\"card\""); n != 2 {
		t.Errorf("got %d class groups, want 2 (one per distinct class)", n)
	}
	// BDFs render sorted: 0000:00:02.0 row before 0000:01:00.0 row.
	if strings.Index(body, "0000:00:02.0") > strings.Index(body, "0000:01:00.0") {
		t.Error("expected 0000:00:02.0 row to render before 0000:01:00.0 row")
	}
}

func TestPCIPageSurfacesDegradedLinks(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(mustGauge("talos_hw_pcidevice_info",
		[]string{"node", "bdf", "class", "subclass", "vendor", "product", "class_id", "subclass_id",
			"vendor_id", "product_id", "driver", "revision", "subsystem_vendor_id", "subsystem_product_id"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "class": "Display controller",
			"subclass": "", "vendor": "AMD", "product": "Radeon 680M", "class_id": "0x03",
			"subclass_id": "0x00", "vendor_id": "0x1002", "product_id": "0x1681", "driver": "amdgpu",
			"revision": "0xc3", "subsystem_vendor_id": "0x1002", "subsystem_product_id": "0x1681"}, 1,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "class": "Mass storage controller",
			"subclass": "", "vendor": "Samsung", "product": "PM9A1", "class_id": "0x01",
			"subclass_id": "0x08", "vendor_id": "0x144d", "product_id": "0xa808", "driver": "nvme",
			"revision": "0x03", "subsystem_vendor_id": "0x126f", "subsystem_product_id": "0x2263"}, 1))
	// The GPU negotiated x4 at 2.5 GT/s on a x16 16 GT/s slot; the NVMe is fine.
	reg.MustRegister(mustGauge("talos_hw_pcidevice_link_gtps",
		[]string{"node", "bdf", "link"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "current"}, 2.5,
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "maximum"}, 16,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "current"}, 8,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "maximum"}, 8))
	reg.MustRegister(mustGauge("talos_hw_pcidevice_link_width",
		[]string{"node", "bdf", "link"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "current"}, 4,
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "link": "maximum"}, 16,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "current"}, 4,
		map[string]string{"node": "node-1", "bdf": "0000:01:00.0", "link": "maximum"}, 4))
	reg.MustRegister(mustGauge("talos_hw_pcidevice_aer_errors_total",
		[]string{"node", "bdf", "severity"},
		map[string]string{"node": "node-1", "bdf": "0000:04:00.0", "severity": "correctable"}, 7))
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.PCI = []snapshot.PCIDevice{
			// Sorted by BDF, as the collector hands them over. The NVMe link is
			// healthy; the GPU negotiated x4 at 2.5 GT/s on a x16 16 GT/s slot.
			{BDF: "0000:01:00.0", Class: "Mass storage controller", Vendor: "Samsung",
				Product: "PM9A1", ClassID: "0x01", SubclassID: "0x08", VendorID: "0x144d",
				ProductID: "0xa808", Driver: "nvme", Revision: "0x03",
				SubsystemVendorID: "0x126f", SubsystemProductID: "0x2263",
				HasLink: true, LinkSpeedGTps: 8, MaxSpeedGTps: 8, LinkWidth: 4, MaxWidth: 4},
			{BDF: "0000:04:00.0", Class: "Display controller", Vendor: "AMD",
				Product: "Radeon 680M", ClassID: "0x03", SubclassID: "0x00", VendorID: "0x1002",
				ProductID: "0x1681", Driver: "amdgpu", Revision: "0xc3",
				SubsystemVendorID: "0x1002", SubsystemProductID: "0x1681",
				HasLink: true, LinkSpeedGTps: 2.5, MaxSpeedGTps: 16, LinkWidth: 4, MaxWidth: 16,
				HasAER: true, AERCorrectable: 7},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/pci")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/pci: got status %d, want 200", rec.Code)
	}
	// The degraded link is stated with what it should have been.
	checkBody(t, body, "2.5 GT/s x4", "of 16 GT/s x16 · degraded", "link degraded", "7 AER errors")
	// The healthy device shows its link without a degraded note.

	// Subsystem identity from sysfs reaches the page.
	checkBody(t, body, "sub 0x1002:0x1681", "rev 0xc3")
	// The class holding the fault sorts first and opens by default.
	if strings.Index(body, "Display controller") > strings.Index(body, "Mass storage controller") {
		t.Error("the class with a degraded link must sort before healthy ones")
	}
	if !strings.Contains(body, `<details class="card" open>`) {
		t.Error("the class needing attention must render open")
	}
}
