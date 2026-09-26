package health

import (
	"context"
	"errors"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// fakeState implements only List of state.CoreState; the rest panic if used.
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

func service(id string, running, healthy, unknown bool) resource.Resource {
	s := v1alpha1.NewService(id)
	spec := s.TypedSpec()
	spec.Running, spec.Healthy, spec.Unknown = running, healthy, unknown
	return s
}

func machineStatus(stage runtimeres.MachineStage, ready bool, unmet ...runtimeres.UnmetCondition) resource.Resource {
	ms := runtimeres.NewMachineStatus()
	spec := ms.TypedSpec()
	spec.Stage = stage
	spec.Status.Ready = ready
	spec.Status.UnmetConditions = unmet
	return ms
}

func diagnostic(id, msg string) resource.Resource {
	d := runtimeres.NewDiagnostic(v1alpha1.NamespaceName, id)
	d.TypedSpec().Message = msg
	return d
}

// collectWith runs the collector against a fixed resource set and returns the
// gathered metric families plus the snapshot the collector wrote.
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

// gaugeFor finds the sample of family whose labels contain every want pair.
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

func TestServicesExportThreeIndependentStates(t *testing.T) {
	families, _ := collectWith(t, map[resource.Type]resource.List{
		v1alpha1.ServiceType: {Items: []resource.Resource{
			service("etcd", true, true, false),
			service("dashboard", true, false, true),
			service("kubelet", false, false, false),
		}},
	})

	// A service with no health check must not look unhealthy: the
	// distinguishing signal is health_unknown, not healthy alone.
	for _, tc := range []struct {
		svc                             string
		running, healthy, healthUnknown float64
	}{
		{"etcd", 1, 1, 0},
		{"dashboard", 1, 0, 1},
		{"kubelet", 0, 0, 0},
	} {
		l := map[string]string{"node": "node-1", "service": tc.svc}
		if got := gaugeFor(t, families, "talos_service_running", l); got != tc.running {
			t.Errorf("%s running = %v, want %v", tc.svc, got, tc.running)
		}
		if got := gaugeFor(t, families, "talos_service_healthy", l); got != tc.healthy {
			t.Errorf("%s healthy = %v, want %v", tc.svc, got, tc.healthy)
		}
		if got := gaugeFor(t, families, "talos_service_health_unknown", l); got != tc.healthUnknown {
			t.Errorf("%s health_unknown = %v, want %v", tc.svc, got, tc.healthUnknown)
		}
	}
}

func TestMachineStageMovesTheSeries(t *testing.T) {
	families, _ := collectWith(t, map[resource.Type]resource.List{
		runtimeres.MachineStatusType: {Items: []resource.Resource{
			machineStatus(runtimeres.MachineStageUpgrading, false,
				runtimeres.UnmetCondition{Name: "etcd", Reason: "etcd is not healthy"}),
		}},
	})

	// The stage rides on a label so a transition moves the series rather
	// than changing an opaque number.
	if got := gaugeFor(t, families, "talos_machine_stage_info",
		map[string]string{"node": "node-1", "stage": "upgrading"}); got != 1 {
		t.Errorf("stage_info{stage=upgrading} = %v, want 1", got)
	}
	if got := gaugeFor(t, families, "talos_machine_ready", map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("machine_ready = %v, want 0", got)
	}
	if got := gaugeFor(t, families, "talos_machine_unmet_conditions", map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("unmet_conditions = %v, want 1", got)
	}
	if got := gaugeFor(t, families, "talos_machine_unmet_condition_info",
		map[string]string{"node": "node-1", "condition": "etcd", "reason": "etcd is not healthy"}); got != 1 {
		t.Errorf("unmet_condition_info = %v, want 1", got)
	}
}

// A healthy node has no diagnostic and no unmet-condition series at all, so
// the two count gauges must still be present and zero — otherwise "healthy"
// and "collector never ran" are indistinguishable in PromQL.
func TestCountsAreZeroInitialisedOnAHealthyNode(t *testing.T) {
	families, _ := collectWith(t, map[resource.Type]resource.List{
		runtimeres.MachineStatusType: {Items: []resource.Resource{
			machineStatus(runtimeres.MachineStageRunning, true),
		}},
	})

	if got := gaugeFor(t, families, "talos_diagnostics", map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("talos_diagnostics = %v, want 0", got)
	}
	if got := gaugeFor(t, families, "talos_machine_unmet_conditions", map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("talos_machine_unmet_conditions = %v, want 0", got)
	}
	if f, ok := families["talos_diagnostic_info"]; ok && len(f.GetMetric()) != 0 {
		t.Errorf("talos_diagnostic_info has %d series on a healthy node, want 0", len(f.GetMetric()))
	}
}

func TestDiagnosticsExportIDAndMessage(t *testing.T) {
	families, node := collectWith(t, map[resource.Type]resource.List{
		runtimeres.DiagnosticType: {Items: []resource.Resource{
			diagnostic("address-overlap", "Node address overlaps with the pod CIDR"),
		}},
	})

	if got := gaugeFor(t, families, "talos_diagnostics", map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("talos_diagnostics = %v, want 1", got)
	}
	if got := gaugeFor(t, families, "talos_diagnostic_info", map[string]string{
		"node": "node-1", "id": "address-overlap",
		"message": "Node address overlaps with the pod CIDR",
	}); got != 1 {
		t.Errorf("talos_diagnostic_info = %v, want 1", got)
	}
	if len(node.Health.Diagnostics) != 1 || node.Health.Diagnostics[0].ID != "address-overlap" {
		t.Errorf("snapshot diagnostics = %+v, want one address-overlap entry", node.Health.Diagnostics)
	}
}

// The UI must be able to tell "stage is running" from "MachineStatus was
// absent"; an empty Stage string cannot carry that on its own.
func TestSnapshotMarksAbsentMachineStatus(t *testing.T) {
	_, node := collectWith(t, map[resource.Type]resource.List{
		v1alpha1.ServiceType: {Items: []resource.Resource{service("etcd", true, true, false)}},
	})
	if node.Health == nil {
		t.Fatal("snapshot Health is nil")
	}
	if node.Health.HasMachineStatus {
		t.Error("HasMachineStatus = true with no MachineStatus resource")
	}
	if len(node.Health.Services) != 1 {
		t.Errorf("services = %d, want 1", len(node.Health.Services))
	}
}

// An empty node — no services, no machine status, no diagnostics — is not a
// failure. A node in maintenance mode legitimately reports almost nothing.
func TestEmptyListsAreNotAnError(t *testing.T) {
	families, node := collectWith(t, map[resource.Type]resource.List{})
	if node.Health == nil {
		t.Fatal("snapshot Health is nil")
	}
	if got := gaugeFor(t, families, "talos_diagnostics", map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("talos_diagnostics = %v, want 0", got)
	}
}

func TestListErrorFailsTheCollection(t *testing.T) {
	snap := snapshot.New()
	c := New().WithSnapshot(snap)
	wantErr := errors.New("permission denied")
	c.state = func(*collector.NodeClient) state.CoreState { return fakeState{err: wantErr} }

	node := &collector.NodeClient{Node: nodes.Node{Name: "node-1", IP: "10.0.0.1"}}
	err := c.Collect(context.Background(), node, prometheus.NewRegistry())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
	}
	if snap.Node("node-1") != nil && snap.Node("node-1").Health != nil {
		t.Error("snapshot was written despite the RPC failing")
	}
}

func TestCollectorIdentity(t *testing.T) {
	c := New()
	if c.Name() != "health" {
		t.Errorf("Name = %q, want health", c.Name())
	}
	// Live, not Inventory: a dead service must not stay hidden for the
	// inventory interval.
	if c.Class() != collector.Live {
		t.Errorf("Class = %v, want Live", c.Class())
	}
}

// The structural guard: every string this collector writes to the snapshot
// must be reachable from /metrics, and its families must obey the contract.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		v1alpha1.ServiceType: {Items: []resource.Resource{
			service("etcd", true, true, false), service("dashboard", true, false, true),
		}},
		runtimeres.MachineStatusType: {Items: []resource.Resource{
			machineStatus(runtimeres.MachineStageUpgrading, false,
				runtimeres.UnmetCondition{Name: "etcd", Reason: "etcd is not healthy"}),
		}},
		runtimeres.DiagnosticType: {Items: []resource.Resource{
			diagnostic("address-overlap", "Node address overlaps with the pod CIDR"),
		}},
	})
	var fams []*dto.MetricFamily
	for _, f := range families {
		fams = append(fams, f)
	}
	opts := metricguard.Options{}
	for _, v := range metricguard.Validate(fams, opts) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(snap.Health, fams, opts) {
		t.Error(v)
	}
}
