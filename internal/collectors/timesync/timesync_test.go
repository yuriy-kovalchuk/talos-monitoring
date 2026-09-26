package timesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	timeres "github.com/siderolabs/talos/pkg/machinery/resources/time"

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

func timeStatus(synced, disabled bool, epoch int) resource.Resource {
	ts := timeres.NewStatus()
	spec := ts.TypedSpec()
	spec.Synced, spec.SyncDisabled, spec.Epoch = synced, disabled, epoch
	return ts
}

func adjtime(offset, maxErr time.Duration, ratio float64, syncStatus bool, state string) resource.Resource {
	as := timeres.NewAdjtimeStatus()
	spec := as.TypedSpec()
	spec.Offset, spec.MaxError, spec.FrequencyAdjustmentRatio = offset, maxErr, ratio
	spec.SyncStatus, spec.State = syncStatus, state
	return as
}

func collectWith(t *testing.T, byType map[resource.Type]resource.List) (map[string]*dto.MetricFamily, *snapshot.Node) {
	t.Helper()
	snap := snapshot.New()
	c := New().WithSnapshot(snap)
	c.state = func(*collector.NodeClient) state.CoreState { return fakeState{byType: byType} }
	reg := prometheus.NewRegistry()
	node := &collector.NodeClient{Node: nodes.Node{Name: "node-1", IP: "10.0.0.1"}}
	if err := c.Collect(context.Background(), node, reg); err != nil {
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

// Durations arrive as Go time.Durations (nanoseconds) and must be exported in
// seconds per the base-unit convention. -85µs is 8.5e-5 s, not -85000.
func TestDurationsAreExportedInSeconds(t *testing.T) {
	node := map[string]string{"node": "node-1"}
	families, snap := collectWith(t, map[resource.Type]resource.List{
		timeres.AdjtimeStatusType: {Items: []resource.Resource{
			adjtime(-85*time.Microsecond, 295*time.Millisecond, 1.0000331520996093, true, "TIME_OK"),
		}},
	})

	if got := gaugeFor(t, families, "talos_time_offset_seconds", node); got != -85e-6 {
		t.Errorf("offset = %v, want -8.5e-05", got)
	}
	if got := gaugeFor(t, families, "talos_time_max_error_seconds", node); got != 0.295 {
		t.Errorf("max_error = %v, want 0.295", got)
	}
	if got := gaugeFor(t, families, "talos_time_frequency_adjustment_ratio", node); got != 1.0000331520996093 {
		t.Errorf("frequency ratio = %v", got)
	}
	if snap.TimeSync.OffsetSeconds != -85e-6 {
		t.Errorf("snapshot offset = %v, want -8.5e-05", snap.TimeSync.OffsetSeconds)
	}
}

// talos_time_synced (Talos ntpd) and talos_time_kernel_synced (STA_UNSYNC)
// are different facts and must not be conflated: they disagree exactly when
// something interesting is happening.
func TestTalosAndKernelSyncAreSeparate(t *testing.T) {
	node := map[string]string{"node": "node-1"}
	families, _ := collectWith(t, map[resource.Type]resource.List{
		timeres.StatusType:        {Items: []resource.Resource{timeStatus(false, false, 3)}},
		timeres.AdjtimeStatusType: {Items: []resource.Resource{adjtime(0, 0, 1, true, "TIME_OK")}},
	})

	if got := gaugeFor(t, families, "talos_time_synced", node); got != 0 {
		t.Errorf("talos_time_synced = %v, want 0", got)
	}
	if got := gaugeFor(t, families, "talos_time_kernel_synced", node); got != 1 {
		t.Errorf("talos_time_kernel_synced = %v, want 1", got)
	}
	if got := gaugeFor(t, families, "talos_time_epoch", node); got != 3 {
		t.Errorf("epoch = %v, want 3", got)
	}
	if got := gaugeFor(t, families, "talos_time_state_info",
		map[string]string{"node": "node-1", "state": "TIME_OK"}); got != 1 {
		t.Errorf("state_info missing")
	}
}

func TestSnapshotMarksAbsentResources(t *testing.T) {
	_, snap := collectWith(t, map[resource.Type]resource.List{
		timeres.StatusType: {Items: []resource.Resource{timeStatus(true, false, 0)}},
	})
	if !snap.TimeSync.HasStatus {
		t.Error("HasStatus = false with a TimeStatus present")
	}
	if snap.TimeSync.HasAdjtime {
		t.Error("HasAdjtime = true with no AdjtimeStatus")
	}
}

func TestListErrorFailsTheCollection(t *testing.T) {
	snap := snapshot.New()
	c := New().WithSnapshot(snap)
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
	if c.Name() != "time" {
		t.Errorf("Name = %q, want time", c.Name())
	}
	if c.Class() != collector.Live {
		t.Errorf("Class = %v, want Live", c.Class())
	}
}

// The structural guard: every string this collector writes to the snapshot
// must be reachable from /metrics, and its families must obey the contract.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		timeres.StatusType:        {Items: []resource.Resource{timeStatus(true, false, 2)}},
		timeres.AdjtimeStatusType: {Items: []resource.Resource{adjtime(-85*time.Microsecond, 295*time.Millisecond, 1.00003, true, "TIME_OK")}},
	})
	var fams []*dto.MetricFamily
	for _, f := range families {
		fams = append(fams, f)
	}
	opts := metricguard.Options{}
	for _, v := range metricguard.Validate(fams, opts) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(snap.TimeSync, fams, opts) {
		t.Error(v)
	}
}
