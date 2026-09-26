package kernel

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

func param(id, current, def string, unsupported bool) resource.Resource {
	kp := runtimeres.NewKernelParamStatus(v1alpha1.NamespaceName, id)
	spec := kp.TypedSpec()
	spec.Current, spec.Default, spec.Unsupported = current, def, unsupported
	return kp
}

func module(id, state string, size, refs int) resource.Resource {
	km := runtimeres.NewLoadedKernelModule(v1alpha1.NamespaceName, id)
	spec := km.TypedSpec()
	spec.State, spec.Size, spec.ReferenceCount = state, size, refs
	return km
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

// A tunable the running kernel does not recognise is silently ignored, and
// this flag is the only signal that the machineconfig did not take effect.
func TestUnsupportedParamIsFlaggedAndCounted(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		runtimeres.KernelParamStatusType: {Items: []resource.Resource{
			param("proc.sys.fs.aio-max-nr", "1048576", "65536", false),
			param("proc.sys.net.core.typo_here", "", "", true),
		}},
	})

	if got := gaugeFor(t, families, "talos_kernel_param_unsupported",
		map[string]string{"node": "node-1", "param": "proc.sys.net.core.typo_here"}); got != 1 {
		t.Errorf("unsupported flag = %v, want 1", got)
	}
	if got := gaugeFor(t, families, "talos_kernel_params_unsupported",
		map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("unsupported count = %v, want 1", got)
	}
	// Current and default ride as labels: a tunable has no unit and nothing
	// to aggregate, so drift is a label comparison.
	if got := gaugeFor(t, families, "talos_kernel_param_info", map[string]string{
		"node": "node-1", "param": "proc.sys.fs.aio-max-nr",
		"current": "1048576", "default": "65536",
	}); got != 1 {
		t.Errorf("param_info missing its current/default labels")
	}
	if len(snap.Kernel.Params) != 2 {
		t.Errorf("snapshot params = %d, want 2", len(snap.Kernel.Params))
	}
}

// Drift is a gauge and not left to the reader because PromQL cannot compare
// two labels of the same series: without it, "22 of 32 tunables differ from
// default" is a number the UI can show and no query can.
func TestDriftedParamIsFlaggedAndCounted(t *testing.T) {
	families, _ := collectWith(t, map[resource.Type]resource.List{
		runtimeres.KernelParamStatusType: {Items: []resource.Resource{
			param("proc.sys.fs.aio-max-nr", "1048576", "65536", false),
			param("proc.sys.kernel.dmesg_restrict", "1", "1", false),
		}},
	})

	if got := gaugeFor(t, families, "talos_kernel_param_drifted",
		map[string]string{"node": "node-1", "param": "proc.sys.fs.aio-max-nr"}); got != 1 {
		t.Errorf("drifted flag on a changed tunable = %v, want 1", got)
	}
	if got := gaugeFor(t, families, "talos_kernel_param_drifted",
		map[string]string{"node": "node-1", "param": "proc.sys.kernel.dmesg_restrict"}); got != 0 {
		t.Errorf("drifted flag on a tunable at its default = %v, want 0", got)
	}
	if got := gaugeFor(t, families, "talos_kernel_params_drifted",
		map[string]string{"node": "node-1"}); got != 1 {
		t.Errorf("drifted count = %v, want 1", got)
	}
}

// Zero-initialised so "nothing unsupported" is distinguishable from "the
// collector never ran".
func TestUnsupportedCountIsZeroOnAHealthyNode(t *testing.T) {
	families, _ := collectWith(t, map[resource.Type]resource.List{
		runtimeres.KernelParamStatusType: {Items: []resource.Resource{
			param("proc.sys.fs.aio-max-nr", "1048576", "65536", false),
		}},
	})
	if got := gaugeFor(t, families, "talos_kernel_params_unsupported",
		map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("unsupported count = %v, want 0", got)
	}
}

func TestModulesExportStateSizeAndRefs(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		runtimeres.LoadedKernelModuleType: {Items: []resource.Resource{
			module("amdgpu", "Live", 16965632, 2),
			module("nvme", "Live", 61440, 0),
		}},
	})

	if got := gaugeFor(t, families, "talos_kernel_modules", map[string]string{"node": "node-1"}); got != 2 {
		t.Errorf("module count = %v, want 2", got)
	}
	if got := gaugeFor(t, families, "talos_kernel_module_size_bytes",
		map[string]string{"node": "node-1", "module": "amdgpu"}); got != 16965632 {
		t.Errorf("amdgpu size = %v", got)
	}
	if got := gaugeFor(t, families, "talos_kernel_module_reference_count",
		map[string]string{"node": "node-1", "module": "nvme"}); got != 0 {
		t.Errorf("nvme refcount = %v, want 0", got)
	}
	if got := gaugeFor(t, families, "talos_kernel_module_info",
		map[string]string{"node": "node-1", "module": "amdgpu", "state": "Live"}); got != 1 {
		t.Errorf("module_info missing")
	}
	// Sorted, so the UI table is stable between scrapes.
	if snap.Kernel.Modules[0].Name != "amdgpu" || snap.Kernel.Modules[1].Name != "nvme" {
		t.Errorf("modules not sorted: %+v", snap.Kernel.Modules)
	}
}

func TestEmptyListsAreNotAnError(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{})
	if snap.Kernel == nil {
		t.Fatal("snapshot Kernel is nil")
	}
	if got := gaugeFor(t, families, "talos_kernel_modules", map[string]string{"node": "node-1"}); got != 0 {
		t.Errorf("module count = %v, want 0", got)
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
	if c.Name() != "kernel" {
		t.Errorf("Name = %q, want kernel", c.Name())
	}
	if c.Class() != collector.Inventory {
		t.Errorf("Class = %v, want Inventory", c.Class())
	}
}

// The structural guard: every string this collector writes to the snapshot
// must be reachable from /metrics, and its families must obey the contract.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		runtimeres.KernelParamStatusType: {Items: []resource.Resource{
			param("proc.sys.fs.aio-max-nr", "1048576", "65536", false),
		}},
		runtimeres.LoadedKernelModuleType: {Items: []resource.Resource{
			module("amdgpu", "Live", 16965632, 2),
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
	for _, v := range metricguard.SnapshotCovered(snap.Kernel, fams, opts) {
		t.Error(v)
	}
}
