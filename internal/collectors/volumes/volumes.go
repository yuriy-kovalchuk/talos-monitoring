// Package volumes reports Talos's own volume layer (talos_machine_volume_*, not the PersistentVolumes).
package volumes

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "volumes"

// defaultTTL: phases change at boot, but a failed unlock must not hide for an hour.
const defaultTTL = 5 * time.Minute

// Collector reads VolumeStatus, MountStatus and SystemDisk from the COSI runtime namespace.
type Collector struct {
	// state returns the COSI state to query for a node; overridable in tests.
	state func(node *collector.NodeClient) state.CoreState

	// snap receives the typed view alongside the metrics. nil disables it.
	snap *snapshot.Store
}

// New returns the volumes collector.
func New() *Collector {
	return &Collector{state: collector.DefaultState}
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Inventory }

// DefaultTTL implements collector.DefaultTTLer.
func (c *Collector) DefaultTTL() time.Duration { return defaultTTL }

// Collect lists the three kinds and registers them.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	st := c.state(node)
	name := node.Node.Name

	vols, err := collector.List(ctx, st, "VolumeStatus", block.VolumeStatusType)
	if err != nil {
		return err
	}
	mounts, err := collector.List(ctx, st, "MountStatus", block.MountStatusType)
	if err != nil {
		return err
	}
	sysDisks, err := collector.List(ctx, st, "SystemDisk", block.SystemDiskType)
	if err != nil {
		return err
	}

	view := snapshot.Volumes{}
	if err := registerVolumes(name, vols, reg, &view); err != nil {
		return err
	}
	if err := registerMounts(name, mounts, reg, &view); err != nil {
		return err
	}
	if err := registerSystemDisk(name, sysDisks, reg, &view); err != nil {
		return err
	}

	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.Volumes = &view })
	}
	return nil
}

// list lists a whole kind from the node's COSI state.

// registerVolumes keys phase as an _info family: the eight phases are not an ordered scale.
func registerVolumes(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Volumes) error {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_volume_info",
		Help: "Volume status (value 1): its type, phase and filesystem. Type distinguishes real partitions from the directory/overlay/symlink pseudo-volumes Talos also models here.",
	}, []string{"node", "volume", "type", "phase", "filesystem", "location"})
	ready := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_volume_ready",
		Help: "1 when the volume reached its ready phase. A 0 on an encrypted volume means it did not unlock.",
	}, []string{"node", "volume", "type"})
	size := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_volume_capacity_bytes",
		Help: "Volume capacity in bytes, as Talos provisioned it. Absent for volumes with no size of their own (directories, symlinks).",
	}, []string{"node", "volume", "type"})
	encrypted := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_volume_encrypted",
		Help: "1 when the volume was unlocked through an encryption provider.",
	}, []string{"node", "volume", "provider"})
	failedSyncs := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_volume_encryption_failed_syncs",
		Help: "Number of encryption keys that failed to sync for the volume. Non-zero means a key slot is stale and the volume may not unlock after the next key rotation.",
	}, []string{"node", "volume"})
	// A mutable free-text error goes on its own _info so it cannot move the main series.
	errInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_volume_error_info",
		Help: "One series (value 1) per volume reporting an error, with the message. Absent on a healthy node.",
	}, []string{"node", "volume", "error"})
	notReady := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_volumes_not_ready",
		Help: "Number of volumes not in the ready phase. Zero on a healthy node.",
	}, []string{"node"})
	reg.MustRegister(info, ready, size, encrypted, failedSyncs, errInfo, notReady)

	notReadyTotal := 0
	view.Volumes = make([]snapshot.Volume, 0, len(l.Items))
	for _, r := range l.Items {
		vs, ok := r.(*block.VolumeStatus)
		if !ok {
			return fmt.Errorf("unexpected resource %T in VolumeStatus list", r)
		}
		spec := vs.TypedSpec()
		id := vs.Metadata().ID()
		kind := spec.Type.String()
		isReady := spec.Phase == block.VolumePhaseReady

		info.WithLabelValues(nodeName, id, kind, spec.Phase.String(),
			spec.Filesystem.String(), spec.Location).Set(1)
		ready.WithLabelValues(nodeName, id, kind).Set(collector.BoolValue(isReady))
		if !isReady {
			notReadyTotal++
		}
		// Absent, not zero: a directory volume has no capacity.
		if spec.Size > 0 {
			size.WithLabelValues(nodeName, id, kind).Set(float64(spec.Size))
		}
		if spec.ErrorMessage != "" {
			errInfo.WithLabelValues(nodeName, id, spec.ErrorMessage).Set(1)
		}
		if spec.EncryptionProvider != block.EncryptionProviderNone {
			encrypted.WithLabelValues(nodeName, id, spec.EncryptionProvider.String()).Set(1)
			failedSyncs.WithLabelValues(nodeName, id).Set(float64(len(spec.EncryptionFailedSyncs)))
		}

		view.Volumes = append(view.Volumes, snapshot.Volume{
			ID: id, Type: kind, Phase: spec.Phase.String(), Ready: isReady,
			Filesystem: spec.Filesystem.String(), Location: spec.Location,
			SizeBytes: float64(spec.Size), PrettySize: spec.PrettySize,
			EncryptionProvider:    providerName(spec.EncryptionProvider),
			EncryptionFailedSyncs: len(spec.EncryptionFailedSyncs),
			ErrorMessage:          spec.ErrorMessage,
		})
	}
	notReady.WithLabelValues(nodeName).Set(float64(notReadyTotal))
	sort.Slice(view.Volumes, func(i, j int) bool { return view.Volumes[i].ID < view.Volumes[j].ID })
	return nil
}

// providerName returns "" for none so the UI tests presence, not a magic string.
func providerName(p block.EncryptionProviderType) string {
	if p == block.EncryptionProviderNone {
		return ""
	}
	return p.String()
}

// registerMounts exports mount metadata only; usage bytes come from the block collector.
func registerMounts(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Volumes) error {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_mount_info",
		Help: "Mounted volume (value 1): source device, target path and filesystem. Usage bytes for the same mount come from talos_filesystem_* (the Mounts RPC); MountStatus does not carry them.",
	}, []string{"node", "volume", "source", "target", "filesystem"})
	readOnly := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_mount_read_only",
		Help: "1 when the mount is read-only.",
	}, []string{"node", "volume"})
	quota := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_mount_project_quota_supported",
		Help: "1 when the filesystem supports project quotas, which is what Talos uses for ephemeral storage limits.",
	}, []string{"node", "volume"})
	reg.MustRegister(info, readOnly, quota)

	view.Mounts = make([]snapshot.Mount, 0, len(l.Items))
	for _, r := range l.Items {
		ms, ok := r.(*block.MountStatus)
		if !ok {
			return fmt.Errorf("unexpected resource %T in MountStatus list", r)
		}
		spec := ms.TypedSpec()
		id := ms.Metadata().ID()
		info.WithLabelValues(nodeName, id, spec.Source, spec.Target, spec.Filesystem.String()).Set(1)
		readOnly.WithLabelValues(nodeName, id).Set(collector.BoolValue(spec.ReadOnly))
		quota.WithLabelValues(nodeName, id).Set(collector.BoolValue(spec.ProjectQuotaSupport))

		view.Mounts = append(view.Mounts, snapshot.Mount{
			ID: id, Source: spec.Source, Target: spec.Target,
			Filesystem: spec.Filesystem.String(), ReadOnly: spec.ReadOnly,
			ProjectQuota: spec.ProjectQuotaSupport,
		})
	}
	sort.Slice(view.Mounts, func(i, j int) bool { return view.Mounts[i].ID < view.Mounts[j].ID })
	return nil
}

// registerSystemDisk exports which disk Talos installed itself on.
func registerSystemDisk(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Volumes) error {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_system_disk_info",
		Help: "The disk Talos is installed on (value 1), one series per node.",
	}, []string{"node", "disk", "dev_path"})
	reg.MustRegister(info)

	for _, r := range l.Items {
		sd, ok := r.(*block.SystemDisk)
		if !ok {
			return fmt.Errorf("unexpected resource %T in SystemDisk list", r)
		}
		spec := sd.TypedSpec()
		info.WithLabelValues(nodeName, spec.DiskID, spec.DevPath).Set(1)
		view.SystemDisk = spec.DiskID
		view.SystemDiskPath = spec.DevPath
	}
	return nil
}

var _ collector.Collector = (*Collector)(nil)
