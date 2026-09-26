package volumes

import (
	"context"
	"errors"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

type fakeState struct {
	state.CoreState
	byType map[resource.Type]resource.List
	err    error
}

func (f fakeState) List(_ context.Context, k resource.Kind, _ ...state.ListOption) (resource.List, error) {
	if f.err != nil {
		return resource.List{}, f.err
	}
	return f.byType[k.Type()], nil
}

type volOpt func(*block.VolumeStatusSpec)

func withEncryption(provider block.EncryptionProviderType, failedSyncs int) volOpt {
	return func(s *block.VolumeStatusSpec) {
		s.EncryptionProvider = provider
		s.EncryptionFailedSyncs = make([]string, failedSyncs)
	}
}

func volume(id string, kind block.VolumeType, phase block.VolumePhase, size uint64, opts ...volOpt) resource.Resource {
	vs := block.NewVolumeStatus(v1alpha1.NamespaceName, id)
	spec := vs.TypedSpec()
	spec.Type, spec.Phase, spec.Size = kind, phase, size
	for _, o := range opts {
		o(spec)
	}
	return vs
}

func collectWith(t *testing.T, byType map[resource.Type]resource.List) (map[string]*dto.MetricFamily, *snapshot.Node) {
	t.Helper()
	snap := snapshot.New()
	c := New().WithSnapshot(snap)
	c.state = func(*collector.NodeClient) state.CoreState { return fakeState{byType: byType} }
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(),
		&collector.NodeClient{Node: nodes.Node{Name: "node-1", IP: "10.0.0.1"}}, reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}
	return byName, snap.Node("node-1")
}

func gaugeFor(t *testing.T, families map[string]*dto.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	f, ok := families[name]
	if !ok {
		t.Fatalf("family %q not registered", name)
	}
next:
	for _, m := range f.GetMetric() {
		labels := make(map[string]string, len(m.GetLabel()))
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		for k, v := range want {
			if labels[k] != v {
				continue next
			}
		}
		return m.GetGauge().GetValue()
	}
	t.Fatalf("family %q has no sample matching %v", name, want)
	return 0
}

func has(families map[string]*dto.MetricFamily, name string, label, value string) bool {
	f, ok := families[name]
	if !ok {
		return false
	}
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == label && l.GetValue() == value {
				return true
			}
		}
	}
	return false
}

// Talos models directories, overlays and symlinks as volumes too. They are
// kept (dropping them would hide a real failure) but every series carries a
// type label so a query can filter to real storage.
func TestPseudoVolumesCarryATypeLabel(t *testing.T) {
	families, _ := collectWith(t, map[resource.Type]resource.List{
		block.VolumeStatusType: {Items: []resource.Resource{
			volume("EPHEMERAL", block.VolumeTypePartition, block.VolumePhaseReady, 125725310976),
			volume("/var/log", block.VolumeTypeDirectory, block.VolumePhaseReady, 0),
		}},
	})

	if got := gaugeFor(t, families, "talos_machine_volume_ready",
		map[string]string{"node": "node-1", "volume": "EPHEMERAL", "type": "partition"}); got != 1 {
		t.Errorf("EPHEMERAL ready = %v, want 1", got)
	}
	if !has(families, "talos_machine_volume_ready", "type", "directory") {
		t.Error("directory pseudo-volume dropped instead of labelled")
	}
	// A directory has no capacity of its own: absent, never 0, which would
	// read as an empty disk in any aggregation.
	if has(families, "talos_machine_volume_capacity_bytes", "volume", "/var/log") {
		t.Error("directory volume reported a capacity")
	}
}

// A volume that fails to unlock is a silent data-loss path: there is no
// filesystem, so no usage metric goes wrong to signal it.
func TestFailedVolumeIsNotReadyAndCounted(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		block.VolumeStatusType: {Items: []resource.Resource{
			volume("EPHEMERAL", block.VolumeTypePartition, block.VolumePhaseFailed, 100,
				withEncryption(block.EncryptionProviderLUKS2, 2)),
			volume("STATE", block.VolumeTypePartition, block.VolumePhaseReady, 100),
		}},
	})

	if got := gaugeFor(t, families, "talos_machine_volume_ready",
		map[string]string{"node": "node-1", "volume": "EPHEMERAL", "type": "partition"}); got != 0 {
		t.Errorf("failed volume ready = %v, want 0", got)
	}
	if got := gaugeFor(t, families, "talos_machine_volumes_not_ready", map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("not_ready count = %v, want 1", got)
	}
	if got := gaugeFor(t, families, "talos_machine_volume_info", map[string]string{
		"node": "node-1", "volume": "EPHEMERAL", "phase": "failed",
	}); got != 1 {
		t.Errorf("phase label missing")
	}
	if got := gaugeFor(t, families, "talos_machine_volume_encryption_failed_syncs",
		map[string]string{"node": "node-1", "volume": "EPHEMERAL"}); got != 2 {
		t.Errorf("failed syncs = %v, want 2", got)
	}
	if snap.Volumes.Volumes[0].EncryptionProvider != "luks2" {
		t.Errorf("snapshot provider = %q, want luks2", snap.Volumes.Volumes[0].EncryptionProvider)
	}
}

// An unencrypted volume gets no encryption series at all, so a
// `talos_machine_volume_encrypted` query returns only the volumes that are.
func TestUnencryptedVolumeHasNoEncryptionSeries(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		block.VolumeStatusType: {Items: []resource.Resource{
			volume("META", block.VolumeTypePartition, block.VolumePhaseReady, 1048576),
		}},
	})
	if has(families, "talos_machine_volume_encrypted", "volume", "META") {
		t.Error("unencrypted volume reported an encryption series")
	}
	if snap.Volumes.Volumes[0].EncryptionProvider != "" {
		t.Errorf("snapshot provider = %q, want empty", snap.Volumes.Volumes[0].EncryptionProvider)
	}
}

func TestSystemDiskIsExported(t *testing.T) {
	sd := block.NewSystemDisk(v1alpha1.NamespaceName, "system-disk")
	sd.TypedSpec().DiskID, sd.TypedSpec().DevPath = "nvme0n1", "/dev/nvme0n1"
	families, snap := collectWith(t, map[resource.Type]resource.List{
		block.SystemDiskType: {Items: []resource.Resource{sd}},
	})
	if got := gaugeFor(t, families, "talos_system_disk_info", map[string]string{
		"node": "node-1", "disk": "nvme0n1", "dev_path": "/dev/nvme0n1",
	}); got != 1 {
		t.Errorf("system disk info missing")
	}
	if snap.Volumes.SystemDisk != "nvme0n1" {
		t.Errorf("snapshot system disk = %q", snap.Volumes.SystemDisk)
	}
}

func TestEmptyListsAreNotAnError(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{})
	if snap.Volumes == nil {
		t.Fatal("snapshot Volumes is nil")
	}
	if got := gaugeFor(t, families, "talos_machine_volumes_not_ready", map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("not_ready = %v, want 0", got)
	}
}

func TestListErrorFailsTheCollection(t *testing.T) {
	c := New()
	wantErr := errors.New("permission denied")
	c.state = func(*collector.NodeClient) state.CoreState { return fakeState{err: wantErr} }
	err := c.Collect(context.Background(),
		&collector.NodeClient{Node: nodes.Node{Name: "node-1"}}, prometheus.NewRegistry())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestCollectorIdentity(t *testing.T) {
	c := New()
	if c.Name() != "volumes" {
		t.Errorf("Name = %q, want volumes", c.Name())
	}
	if c.Class() != collector.Inventory {
		t.Errorf("Class = %v, want Inventory", c.Class())
	}
	if c.DefaultTTL() != defaultTTL {
		t.Errorf("DefaultTTL = %v, want %v", c.DefaultTTL(), defaultTTL)
	}
}

// §1.1: the UI is a subset of the metrics. The storage page renders a failed
// volume's error message, so that message has to be exported — it rides on its
// own _info metric because it is free text and mutable, and putting it on the
// status metric would move every volume's series whenever the text changed.
func TestVolumeErrorMessageIsExported(t *testing.T) {
	withError := func(msg string) volOpt {
		return func(s *block.VolumeStatusSpec) { s.ErrorMessage = msg }
	}
	families, snap := collectWith(t, map[resource.Type]resource.List{
		block.VolumeStatusType: {Items: []resource.Resource{
			volume("EPHEMERAL", block.VolumeTypePartition, block.VolumePhaseFailed, 100,
				withError("failed to unlock: no key slot matched")),
			volume("STATE", block.VolumeTypePartition, block.VolumePhaseReady, 100),
		}},
	})

	if got := gaugeFor(t, families, "talos_machine_volume_error_info", map[string]string{
		"node": "node-1", "volume": "EPHEMERAL",
		"error": "failed to unlock: no key slot matched",
	}); got != 1 {
		t.Errorf("volume error message not exported = %v, want 1", got)
	}
	// A healthy volume produces no series at all, so the family stays empty
	// on a healthy node rather than carrying an empty-string label per volume.
	if has(families, "talos_machine_volume_error_info", "volume", "STATE") {
		t.Error("a volume with no error reported an error series")
	}
	if snap.Volumes.Volumes[0].ErrorMessage == "" {
		t.Error("snapshot lost the error message")
	}
}

// The structural guard: everything this collector writes to the snapshot must
// also be reachable from /metrics, and its families must obey the contract.
// This is the check that would have caught the volume error message shipping
// to the UI with no metric behind it.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		block.VolumeStatusType: {Items: []resource.Resource{
			volume("EPHEMERAL", block.VolumeTypePartition, block.VolumePhaseFailed, 125725310976,
				withEncryption(block.EncryptionProviderLUKS2, 1),
				func(s *block.VolumeStatusSpec) {
					s.ErrorMessage = "failed to unlock: no key slot matched"
					s.Filesystem = block.FilesystemTypeXFS
					s.Location = "/dev/nvme0n1p4"
					s.PrettySize = "126 GB"
				}),
		}},
		block.MountStatusType: {Items: []resource.Resource{}},
		block.SystemDiskType:  {Items: []resource.Resource{}},
	})

	var fams []*dto.MetricFamily
	for _, f := range families {
		fams = append(fams, f)
	}
	opts := metricguard.Options{
		UncoveredStrings: map[string]bool{
			// PrettySize is Talos's own rendering of SizeBytes, which is
			// exported as talos_machine_volume_capacity_bytes. Exporting the
			// pretty form too would be a second series for one quantity.
			"Volumes.PrettySize": true,
		},
	}
	for _, v := range metricguard.Validate(fams, opts) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(snap.Volumes, fams, opts) {
		t.Error(v)
	}
}
