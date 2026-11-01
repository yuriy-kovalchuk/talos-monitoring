package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

func TestNodeDetailDisks(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_block_disk_info",
		Help: "test fixture",
	}, []string{"node", "device", "model", "serial", "wwid", "uuid", "transport", "subsystem",
		"bus_path", "attachment"})
	reg.MustRegister(g)
	g.WithLabelValues("node-1", "/dev/sda", "WDC WD10EZEX", "SATA-SN-1", "wwid-1", "", "sata", "scsi",
		"0000:02:00.0", "local").Set(1)
	g.WithLabelValues("node-1", "/dev/nvme0n1", "SM2263EN NVMe", "NVME-SN-1", "wwid-2", "", "nvme", "nvme",
		"0000:01:00.0", "local").Set(1)
	diskMeasures(t, reg, "node-1", map[string]float64{
		"/dev/sda":     1000204886016,
		"/dev/nvme0n1": 931510603776,
	}, "/dev/sda")
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Disks = []snapshot.Disk{
			{Device: "/dev/nvme0n1", Model: "SM2263EN NVMe", Serial: "NVME-SN-1", WWID: "wwid-2",
				Transport: "nvme", SubSystem: "nvme", BusPath: "0000:01:00.0", Attachment: "local",
				SizeBytes: 931510603776},
			{Device: "/dev/sda", Model: "WDC WD10EZEX", Serial: "SATA-SN-1", WWID: "wwid-1",
				Transport: "sata", SubSystem: "scsi", BusPath: "0000:02:00.0", Attachment: "local",
				SizeBytes: 1000204886016, Rotational: true},
		}
	})

	h := newHandlerWithReg(t, reg, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/storage: got status %d, want 200", rec.Code)
	}
	// Sizes are formatted from talos_block_disk_size_bytes now that the disk's
	// pretty-printed size is no longer carried as a label, so the column reads
	// like every other size in the UI ("932 GB", not Talos's "931.51 GB").
	checkBody(t, body, "/dev/nvme0n1", "932 GB", "SM2263EN NVMe", "HDD", "2 local · 1.9 TB", "local")
	// Devices render sorted: /dev/nvme0n1 row before /dev/sda row.
	if strings.Index(body, "/dev/nvme0n1") > strings.Index(body, "/dev/sda") {
		t.Error("expected /dev/nvme0n1 row to render before /dev/sda row")
	}
}

// The 19 permanently-ready pseudo-volumes are folded away so the three real
// partitions are visible — but a pseudo-volume that FAILED must still show.
func TestStoragePageFoldsHealthyPseudoVolumesOnly(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Volumes = &snapshot.Volumes{
			SystemDisk: "nvme0n1",
			Volumes: []snapshot.Volume{
				{ID: "EPHEMERAL", Type: "partition", Phase: "ready", Ready: true,
					Filesystem: "xfs", Location: "/dev/nvme0n1p4", PrettySize: "126 GB"},
				{ID: "/var/log", Type: "directory", Phase: "ready", Ready: true},
				{ID: "/var/mnt", Type: "directory", Phase: "failed", Ready: false,
					ErrorMessage: "mount target busy"},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")

	checkBody(t, body, "EPHEMERAL", "126 GB", "system disk nvme0n1")
	// The failed directory volume is listed despite being a pseudo-volume.
	checkBody(t, body, "/var/mnt", "mount target busy", "1 NOT READY")
	// The healthy one is folded into the footnote instead.
	checkBody(t, body, "1 always-ready directory, overlay and symlink volume hidden")
	if strings.Contains(body, ">/var/log\n") {
		t.Error("healthy pseudo-volume rendered as a row")
	}
}

// An encrypted volume with stale key slots is a future unlock failure.
func TestStoragePageSurfacesFailedEncryptionSyncs(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Volumes = &snapshot.Volumes{Volumes: []snapshot.Volume{
			{ID: "STATE", Type: "partition", Phase: "ready", Ready: true,
				EncryptionProvider: "luks2", EncryptionFailedSyncs: 2},
		}}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")
	checkBody(t, body, "luks2", "2 encryption keys failed to sync")
}

// The disks page shows cumulative counters, not rates. Busy is io_time as a
// share of uptime, which is meaningless without an uptime to divide by — it
// must stay blank then rather than render a misleading 0%.
func TestStoragePageRendersBlockIO(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Runtime = &snapshot.Runtime{HasUptime: true, Uptime: 1000}
		n.DiskIO = []snapshot.DiskIO{
			{Device: "nvme0n1", Kind: "disk", ReadBytes: 1 << 30, WrittenBytes: 2 << 30,
				ReadsCompleted: 12345, WritesCompleted: 6789, IOTimeSeconds: 250, IOInProgress: 3},
			{Device: "nvme0n1p4", Kind: "partition", ReadBytes: 1 << 20},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")

	checkBody(t, body, "Block I/O", "nvme0n1", "1.0 GiB", "2.0 GiB", "12,345", "25.0%", "partition")
	// The caveat has to be on the page, not just in the docs.
	checkBody(t, body, "partitions also count toward their disk")
}

func TestStoragePageBlankBusyWithoutUptime(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.DiskIO = []snapshot.DiskIO{{Device: "sda", Kind: "disk", IOTimeSeconds: 250}}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/storage")
	if strings.Contains(body, "0.0%") {
		t.Error("busy rendered as 0.0% with no uptime to divide by")
	}
}
