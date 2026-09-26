package collector

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

// benchScraper fills a scraper cache the way a real round does: one
// sub-registry per (node, collector), shaped like the reference cluster —
// per node one socket, six disks, thirty PCI devices, ten sensors, and
// per-thread CPU state.
//
// /metrics walks this on every Prometheus scrape, so it is the hot path that
// scales with the cluster.
func benchScraper(nodeCount, threads int) *Scraper {
	list := make([]nodes.Node, nodeCount)
	for i := range list {
		list[i] = nodes.Node{Name: fmt.Sprintf("node-%03d", i)}
	}
	s := NewScraper(prometheus.NewRegistry(), Options{
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry:  NewRegistry(),
		ListNodes: func() []nodes.Node { return list },
	})
	for _, n := range list {
		s.store(n.Name, "cpu", cpuReg(n.Name, threads))
		s.store(n.Name, "pci", pciReg(n.Name, 30))
		s.store(n.Name, "block", blockReg(n.Name, 6))
		s.store(n.Name, "sensors", sensorReg(n.Name, 10))
	}
	return s
}

func cpuReg(node string, threads int) *prometheus.Registry {
	r := prometheus.NewRegistry()
	freq := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "talos_node_cpu_freq_hertz", Help: "h"}, []string{"node", "cpu"})
	use := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "talos_node_cpu_usage_ratio", Help: "h"}, []string{"node", "cpu"})
	gov := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "talos_node_cpu_governor_info", Help: "h"}, []string{"node", "cpu", "governor"})
	r.MustRegister(freq, use, gov)
	for c := 0; c < threads; c++ {
		id := fmt.Sprintf("%d", c)
		freq.WithLabelValues(node, id).Set(2.4e9)
		use.WithLabelValues(node, id).Set(0.3)
		gov.WithLabelValues(node, id, "powersave").Set(1)
	}
	return r
}

func pciReg(node string, n int) *prometheus.Registry {
	r := prometheus.NewRegistry()
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "talos_hw_pcidevice_info", Help: "h"},
		[]string{"node", "bdf", "class", "subclass", "vendor", "product", "class_id", "subclass_id",
			"vendor_id", "product_id", "driver", "revision", "subsystem_vendor_id", "subsystem_product_id"})
	r.MustRegister(info)
	for i := 0; i < n; i++ {
		info.WithLabelValues(node, fmt.Sprintf("0000:%02x:00.0", i), "Bridge", "", "V", "P",
			"0x06", "0x04", "0x1022", "0x14ed", "pcieport", "0x00", "0x1022", "0x14ed").Set(1)
	}
	return r
}

func blockReg(node string, n int) *prometheus.Registry {
	r := prometheus.NewRegistry()
	size := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "talos_block_disk_size_bytes", Help: "h"}, []string{"node", "device"})
	r.MustRegister(size)
	for i := 0; i < n; i++ {
		size.WithLabelValues(node, fmt.Sprintf("/dev/sd%c", 'a'+i)).Set(1e12)
	}
	return r
}

func sensorReg(node string, n int) *prometheus.Registry {
	r := prometheus.NewRegistry()
	t := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "talos_sensor_temperature_celsius", Help: "h"},
		[]string{"node", "chip", "chip_name", "sensor", "label"})
	r.MustRegister(t)
	for i := 0; i < n; i++ {
		t.WithLabelValues(node, "/sys/class/hwmon/hwmon0", "coretemp", fmt.Sprintf("temp%d", i), "").Set(45)
	}
	return r
}

func benchGather(b *testing.B, nodeCount, threads int) {
	s := benchScraper(nodeCount, threads)
	fams, _ := s.Gather()
	n := 0
	for _, f := range fams {
		n += len(f.Metric)
	}
	b.ReportMetric(float64(n), "series")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Gather(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGather4(b *testing.B)   { benchGather(b, 4, 16) }
func BenchmarkGather20(b *testing.B)  { benchGather(b, 20, 32) }
func BenchmarkGather100(b *testing.B) { benchGather(b, 100, 64) }
