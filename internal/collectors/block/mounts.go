package block

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// mountAPI is the part of the Talos client used for filesystem usage.
type mountAPI interface {
	Mounts(ctx context.Context) (*machinepb.MountsResponse, error)
}

type mountTalosAPI struct{ client *talosclient.Client }

func (m mountTalosAPI) Mounts(ctx context.Context) (*machinepb.MountsResponse, error) {
	return m.client.MachineClient.Mounts(ctx, &empty.Empty{})
}

// csiMountRe matches the pod-level CSI mount and captures the PersistentVolume
// name, which is the join key to Kubernetes.
//
// Each volume appears twice on a node: once as the driver's `globalmount` and
// once per pod under `kubernetes.io~csi/<pv>/mount`. Anchoring on the pod-level
// path deduplicates. The pod UID in the path is deliberately NOT used as a
// label — it changes on every pod restart and would churn the series.
var csiMountRe = regexp.MustCompile(`/kubernetes\.io~csi/(pvc-[0-9a-f-]+)/mount$`)

// kubeletPrefix marks mounts that belong to workloads rather than the node.
const kubeletPrefix = "/var/lib/kubelet/"

// registerMounts exports per-volume and per-filesystem usage.
//
// Talos reports only size and available; used is derived as size - available.
// That is what `talosctl mounts` shows, and it equals df's Used only when the
// filesystem has no reserved blocks. On the reference cluster the CSI volumes
// are created with no reserve (a 52 GiB empty volume reports 0.02 GiB used, not
// the 2.6 GiB a 5% ext4 reserve would), so the two agree — but the raw size and
// available are exported alongside so nobody has to take the derived value.
func registerMounts(nodeName string, mounts []*machinepb.MountStat, reg prometheus.Registerer) []snapshot.Filesystem {
	volSize := gaugeVec("talos_volume_size_bytes", "Filesystem size of a mounted PersistentVolume.", "node", "pv")
	volAvail := gaugeVec("talos_volume_available_bytes", "Space available on a mounted PersistentVolume.", "node", "pv")
	volUsed := gaugeVec("talos_volume_used_bytes", "Space used on a mounted PersistentVolume, derived as size - available (includes reserved blocks, so it can exceed df's Used).", "node", "pv")
	volInfo := gaugeVec("talos_volume_info", "Mounted PersistentVolume identity (value 1).", "node", "pv", "device", "filesystem")

	fsSize := gaugeVec("talos_filesystem_size_bytes", "Size of a node filesystem.", "node", "mountpoint")
	fsAvail := gaugeVec("talos_filesystem_available_bytes", "Space available on a node filesystem.", "node", "mountpoint")
	fsUsed := gaugeVec("talos_filesystem_used_bytes", "Space used on a node filesystem, derived as size - available.", "node", "mountpoint")
	fsInfo := gaugeVec("talos_filesystem_info", "Node filesystem identity (value 1).", "node", "mountpoint", "device")

	for _, g := range []*prometheus.GaugeVec{volSize, volAvail, volUsed, volInfo, fsSize, fsAvail, fsUsed, fsInfo} {
		reg.MustRegister(g)
	}

	// A device can carry several bind mounts of the same filesystem (/var and
	// /opt report identical numbers on Talos); keep the first, shortest path.
	seenDevice := map[string]bool{}
	var out []snapshot.Filesystem

	for _, m := range mounts {
		size, avail := m.GetSize(), m.GetAvailable()
		if size == 0 {
			continue // the read-only squashfs root reports 0 and would read as 100% full
		}
		used := uint64(0)
		if size > avail {
			used = size - avail
		}
		path, dev := m.GetMountedOn(), m.GetFilesystem()

		if pv := csiMountRe.FindStringSubmatch(path); pv != nil {
			volSize.WithLabelValues(nodeName, pv[1]).Set(float64(size))
			volAvail.WithLabelValues(nodeName, pv[1]).Set(float64(avail))
			volUsed.WithLabelValues(nodeName, pv[1]).Set(float64(used))
			volInfo.WithLabelValues(nodeName, pv[1], dev, mountFilesystem(dev)).Set(1)
			out = append(out, snapshot.Filesystem{
				Mountpoint: path, Device: dev, PV: pv[1], Kind: mountFilesystem(dev),
				SizeBytes: float64(size), AvailBytes: float64(avail), UsedBytes: float64(used),
			})
			continue
		}
		if strings.HasPrefix(path, kubeletPrefix) {
			continue // other workload mounts: secrets, downward-api, projected
		}
		// Node filesystems: only real block devices. Everything else Talos
		// reports ("none", "rootfs") is a bind mount or tmpfs view of one.
		if !strings.HasPrefix(dev, "/dev/") || seenDevice[dev] {
			continue
		}
		seenDevice[dev] = true
		fsSize.WithLabelValues(nodeName, path).Set(float64(size))
		fsAvail.WithLabelValues(nodeName, path).Set(float64(avail))
		fsUsed.WithLabelValues(nodeName, path).Set(float64(used))
		fsInfo.WithLabelValues(nodeName, path, dev).Set(1)
		out = append(out, snapshot.Filesystem{
			Mountpoint: path, Device: dev,
			SizeBytes: float64(size), AvailBytes: float64(avail), UsedBytes: float64(used),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })
	return out
}

// mountFilesystem labels how the volume is reached: a block device, or a
// network export (an NFS-backed PV reports "host:/export" as its device).
func mountFilesystem(device string) string {
	if strings.HasPrefix(device, "/dev/") {
		return "block"
	}
	if strings.Contains(device, ":/") {
		return "network"
	}
	return "other"
}

func gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}
