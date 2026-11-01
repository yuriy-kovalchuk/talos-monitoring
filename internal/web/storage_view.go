package web

import (
	"fmt"
	"sort"

	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// volumeRow is a mounted PV: Talos usage joined to Kubernetes identity on the PV name.
type volumeRow struct {
	PV           string
	Namespace    string
	Claim        string
	StorageClass string
	Driver       string
	Node         string
	Device       string
	Kind         string // "block" | "network"
	Size         string
	Used         string
	Requested    string
	UsedPct      int
	Named        bool // false when Kubernetes has no PV for this mount
}

// Label is namespace/claim when Kubernetes knows the volume, else the raw PV name.
func (v volumeRow) Label() string {
	if v.Namespace != "" && v.Claim != "" {
		return v.Namespace + "/" + v.Claim
	}
	return v.PV
}

// filesystemRow is one of the node's own filesystems.
type filesystemRow struct {
	MountPoint string
	Device     string
	Size       string
	Used       string
	Avail      string
	UsedPct    int
}

// volumesFrom renders the mounted PersistentVolumes with their Kubernetes identity.
func (d *dashboard) volumesFrom(n *snapshot.Node, node string) []volumeRow {
	if n == nil {
		return nil
	}
	var known map[string]k8svolumes.Volume
	if d.volumes != nil {
		known = d.volumes()
	}

	var out []volumeRow
	for _, fs := range n.Filesystems {
		if fs.PV == "" {
			continue
		}
		// Binary units throughout: Kubernetes states capacity in GiB, so SI GB misreads the overhead.
		row := volumeRow{
			PV:     fs.PV,
			Node:   node,
			Size:   humanize.IBytes(uint64(fs.SizeBytes)),
			Used:   humanize.IBytes(uint64(fs.UsedBytes)),
			Device: fs.Device,
			Kind:   fs.Kind,
		}
		if fs.SizeBytes > 0 {
			row.UsedPct = pctOfF(fs.UsedBytes, fs.SizeBytes)
		}
		if v, ok := known[fs.PV]; ok {
			row.Named = true
			row.Namespace, row.Claim = v.Namespace, v.Claim
			row.StorageClass, row.Driver = v.StorageClass, v.Driver
			if v.Capacity > 0 {
				row.Requested = humanize.IBytes(uint64(v.Capacity))
			}
		}
		out = append(out, row)
	}
	// Fullest first: the reason to look at this table is to find what is filling up.
	sort.Slice(out, func(i, j int) bool {
		if out[i].UsedPct != out[j].UsedPct {
			return out[i].UsedPct > out[j].UsedPct
		}
		return out[i].Label() < out[j].Label()
	})
	return out
}

// filesystemsFrom renders the node's own filesystems; PVs are listed separately.
func filesystemsFrom(n *snapshot.Node) []filesystemRow {
	if n == nil {
		return nil
	}
	var out []filesystemRow
	for _, fs := range n.Filesystems {
		if fs.PV != "" || fs.SizeBytes <= 0 {
			continue
		}
		out = append(out, filesystemRow{
			MountPoint: fs.Mountpoint,
			Device:     fs.Device,
			Size:       humanize.IBytes(uint64(fs.SizeBytes)),
			Used:       humanize.IBytes(uint64(fs.UsedBytes)),
			Avail:      humanize.IBytes(uint64(fs.AvailBytes)),
			UsedPct:    pctOfF(fs.UsedBytes, fs.SizeBytes),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MountPoint < out[j].MountPoint })
	return out
}

// diskDevice is one row on the disks card, from talos_block_disk_info labels.
type diskDevice struct {
	Device     string
	Size       string // pretty size from the metric
	Model      string
	Serial     string
	Transport  string
	Media      string // "CD-ROM" | "HDD" | "SSD"
	WWID       string
	Attachment string // "local" | "network" | "virtual"
}

// diskDevicesFrom renders the disk rows and the locally attached total.
func diskDevicesFrom(n *snapshot.Node) ([]diskDevice, string) {
	if n == nil || len(n.Disks) == 0 {
		return nil, ""
	}
	out := make([]diskDevice, 0, len(n.Disks))
	totalBytes := uint64(0)
	for _, d := range n.Disks {
		media := "SSD"
		switch {
		case d.CDROM:
			media = "CD-ROM"
		case d.Rotational:
			media = "HDD"
		case d.Transport == "":
			media = "–" // loop/virtual devices have no media type
		}
		if d.Attachment == "local" {
			totalBytes += uint64(d.SizeBytes)
		}
		out = append(out, diskDevice{
			Device:     d.Device,
			Size:       humanize.Bytes(uint64(d.SizeBytes)),
			Model:      dmiValue(d.Model),
			Serial:     dmiValue(d.Serial),
			Transport:  d.Transport,
			Media:      media,
			WWID:       d.WWID,
			Attachment: d.Attachment,
		})
	}
	return out, humanize.Bytes(totalBytes)
}

// volumeRowLayer is one Talos volume on the disks page.
type volumeRowLayer struct {
	ID         string
	Type       string
	Phase      string
	Ready      bool
	Filesystem string
	Location   string
	Size       string
	Encrypted  bool
	Provider   string
	FailedSync int
	Error      string
}

// volumeLayerView folds away healthy pseudo-volumes: 19 of 22 on the reference cluster.
type volumeLayerView struct {
	Available      bool
	Volumes        []volumeRowLayer
	Pseudo         int
	NotReady       int
	SystemDisk     string
	SystemDiskPath string
}

// realVolumeTypes are the volume kinds backed by actual storage.
var realVolumeTypes = map[string]bool{"partition": true, "disk": true, "external": true}

// volumeLayerFrom builds the volume layer block from the snapshot.
func volumeLayerFrom(n *snapshot.Node) volumeLayerView {
	if n == nil || n.Volumes == nil {
		return volumeLayerView{}
	}
	v := n.Volumes
	view := volumeLayerView{
		Available:  true,
		SystemDisk: v.SystemDisk, SystemDiskPath: v.SystemDiskPath,
	}
	for _, vol := range v.Volumes {
		if !vol.Ready {
			view.NotReady++
		}
		// A pseudo-volume folds only while healthy; a failed one stays visible.
		if !realVolumeTypes[vol.Type] && vol.Ready {
			view.Pseudo++
			continue
		}
		size := vol.PrettySize
		if size == "" && vol.SizeBytes > 0 {
			size = humanize.IBytes(uint64(vol.SizeBytes))
		}
		view.Volumes = append(view.Volumes, volumeRowLayer{
			ID: vol.ID, Type: vol.Type, Phase: vol.Phase, Ready: vol.Ready,
			Filesystem: vol.Filesystem, Location: vol.Location, Size: size,
			Encrypted: vol.EncryptionProvider != "", Provider: vol.EncryptionProvider,
			FailedSync: vol.EncryptionFailedSyncs, Error: vol.ErrorMessage,
		})
	}
	return view
}

// diskIORow shows since-boot counters; there is no rate window here and the header says so.
type diskIORow struct {
	Device   string
	Kind     string
	Read     string
	Written  string
	Reads    string
	Writes   string
	InFlight float64
	Busy     string // io_time as a share of uptime, when uptime is known
	// busyPct is the number behind Busy, so the summary can rank without re-parsing.
	busyPct float64
}

// diskIOFrom builds the disk I/O table from the snapshot.
func diskIOFrom(n *snapshot.Node) []diskIORow {
	if n == nil || len(n.DiskIO) == 0 {
		return nil
	}
	uptime := 0.0
	if n.Runtime != nil && n.Runtime.HasUptime {
		uptime = n.Runtime.Uptime
	}
	out := make([]diskIORow, 0, len(n.DiskIO))
	for _, d := range n.DiskIO {
		row := diskIORow{
			Device: d.Device, Kind: d.Kind,
			Read:     humanize.IBytes(uint64(d.ReadBytes)),
			Written:  humanize.IBytes(uint64(d.WrittenBytes)),
			Reads:    humanize.Comma(int64(d.ReadsCompleted)),
			Writes:   humanize.Comma(int64(d.WritesCompleted)),
			InFlight: d.IOInProgress,
		}
		// Share of uptime with I/O outstanding; blank without an uptime rather than a false 0%.
		if uptime > 0 {
			row.busyPct = d.IOTimeSeconds / uptime * 100
			row.Busy = fmt.Sprintf("%.1f%%", row.busyPct)
		}
		out = append(out, row)
	}
	return out
}

// storageSummary is the triage strip: four questions before five tables.
type storageSummary struct {
	Capacity        string
	Disks           int
	HasBusiest      bool
	BusiestDevice   string
	BusiestBusy     string
	VolumesTotal    int
	VolumesNotReady int
	PVs             int
	PVFullest       string
	PVFullestPct    int
}

// storageSummaryFrom derives the strip from the page's tables so they cannot drift.
func storageSummaryFrom(p *pageData) storageSummary {
	sum := storageSummary{
		Capacity: p.DisksTotal,
		Disks:    p.DisksLocal,
		PVs:      len(p.Volumes),
	}
	// Partitions are skipped: their time is also counted in the parent disk.
	best := -1.0
	for _, d := range p.DiskIO {
		if d.Kind != "disk" || d.Busy == "" {
			continue
		}
		if pct := d.busyPct; pct > best {
			best, sum.BusiestDevice, sum.BusiestBusy, sum.HasBusiest = pct, d.Device, d.Busy, true
		}
	}
	if p.VolumeLayer.Available {
		// Total includes the folded-away pseudo-volumes, matching what Talos models.
		sum.VolumesTotal = len(p.VolumeLayer.Volumes) + p.VolumeLayer.Pseudo
		sum.VolumesNotReady = p.VolumeLayer.NotReady
	}
	// Volumes arrive fullest-first, so the head is the one to name.
	if len(p.Volumes) > 0 {
		sum.PVFullest, sum.PVFullestPct = p.Volumes[0].Label(), p.Volumes[0].UsedPct
	}
	return sum
}
