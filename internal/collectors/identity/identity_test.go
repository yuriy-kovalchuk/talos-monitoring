package identity

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

func extension(id, name, version string) resource.Resource {
	es := runtimeres.NewExtensionStatus(v1alpha1.NamespaceName, id)
	md := &es.TypedSpec().Metadata
	md.Name, md.Version = name, version
	return es
}

func cmdline(s string) resource.Resource {
	kc := runtimeres.NewKernelCmdline()
	kc.TypedSpec().Cmdline = s
	return kc
}

func bootedEntry(s string) resource.Resource {
	be := runtimeres.NewBootedEntrySpec()
	be.TypedSpec().BootedEntry = s
	return be
}

func securityState(secureBoot, uki, modSig bool, selinux runtimeres.SELinuxState, fips runtimeres.FIPSState) resource.Resource {
	ss := runtimeres.NewSecurityStateSpec(v1alpha1.NamespaceName)
	spec := ss.TypedSpec()
	spec.SecureBoot, spec.BootedWithUKI, spec.ModuleSignatureEnforced = secureBoot, uki, modSig
	spec.SELinuxState, spec.FIPSState = selinux, fips
	return ss
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

// The schematic is reported as an extension, but it is the fleet-consistency
// signal and must also be its own family so PromQL can group by it directly.
func TestSchematicGetsItsOwnFamily(t *testing.T) {
	const id = "65cf8364cd0de4cf7b851dc7067a2db83d0ba04f11d8635c6cd3334be6ffb825"
	families, node := collectWith(t, map[resource.Type]resource.List{
		runtimeres.ExtensionStatusType: {Items: []resource.Resource{
			extension("0", "amd-ucode", "20260519"),
			extension("4", "schematic", id),
		}},
	})

	if got := gaugeFor(t, families, "talos_schematic_info",
		map[string]string{"node": "node-1", "schematic": id}); got != 1 {
		t.Errorf("talos_schematic_info = %v, want 1", got)
	}
	// Still present in the raw extension listing, because that is what the
	// node reports.
	if got := gaugeFor(t, families, "talos_extension_info",
		map[string]string{"node": "node-1", "extension": "schematic", "version": id}); got != 1 {
		t.Errorf("schematic missing from talos_extension_info")
	}
	if node.Identity.Schematic != id {
		t.Errorf("snapshot schematic = %q, want %q", node.Identity.Schematic, id)
	}
}

// The virtual entries are not installed software: counting them as extensions
// would report 6 extensions on a node that has 4.
func TestVirtualEntriesAreNotSnapshotExtensions(t *testing.T) {
	_, node := collectWith(t, map[resource.Type]resource.List{
		runtimeres.ExtensionStatusType: {Items: []resource.Resource{
			extension("0", "amd-ucode", "20260519"),
			extension("1", "iscsi-tools", "v0.2.0"),
			extension("4", "schematic", "65cf83"),
			extension("modules.dep", "modules.dep", "6.18.34-talos"),
		}},
	})

	if len(node.Identity.Extensions) != 2 {
		t.Errorf("snapshot extensions = %d (%+v), want 2 real ones",
			len(node.Identity.Extensions), node.Identity.Extensions)
	}
	for _, e := range node.Identity.Extensions {
		if e.Name == "schematic" || e.Name == "modules.dep" {
			t.Errorf("virtual entry %q listed as an installed extension", e.Name)
		}
	}
	// modules.dep is versioned by the kernel it indexes, which is worth
	// keeping even though the entry itself is synthetic.
	if node.Identity.KernelVersion != "6.18.34-talos" {
		t.Errorf("kernel version = %q, want 6.18.34-talos", node.Identity.KernelVersion)
	}
}

func TestSecurityStateExportsBooleansAndStates(t *testing.T) {
	families, node := collectWith(t, map[resource.Type]resource.List{
		runtimeres.SecurityStateType: {Items: []resource.Resource{
			securityState(false, true, true,
				runtimeres.SELinuxStatePermissive, runtimeres.FIPSStateDisabled),
		}},
	})

	for name, want := range map[string]float64{
		"talos_security_secure_boot":               0,
		"talos_security_booted_with_uki":           1,
		"talos_security_module_signature_enforced": 1,
	} {
		if got := gaugeFor(t, families, name, map[string]string{"node": "node-1"}); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// Multi-valued states ride on a label so a transition moves the series.
	if got := gaugeFor(t, families, "talos_security_selinux_state_info",
		map[string]string{"node": "node-1", "state": "enabled, permissive"}); got != 1 {
		t.Errorf("selinux state series missing")
	}
	if got := gaugeFor(t, families, "talos_security_fips_state_info",
		map[string]string{"node": "node-1", "state": "disabled"}); got != 1 {
		t.Errorf("fips state series missing")
	}
	if !node.Identity.HasSecurityState {
		t.Error("HasSecurityState = false with a SecurityState resource present")
	}
}

// SecureBoot=false and "the resource was absent" are different facts, and the
// booleans alone cannot tell them apart.
func TestSnapshotMarksAbsentSecurityState(t *testing.T) {
	_, node := collectWith(t, map[resource.Type]resource.List{
		runtimeres.ExtensionStatusType: {Items: []resource.Resource{
			extension("0", "amd-ucode", "20260519"),
		}},
	})
	if node.Identity == nil {
		t.Fatal("snapshot Identity is nil")
	}
	if node.Identity.HasSecurityState {
		t.Error("HasSecurityState = true with no SecurityState resource")
	}
}

func TestBootExportsCmdlineAndEntry(t *testing.T) {
	const cl = "talos.platform=metal console=tty0 selinux=1 module.sig_enforce=1"
	families, node := collectWith(t, map[resource.Type]resource.List{
		runtimeres.KernelCmdlineType: {Items: []resource.Resource{cmdline(cl)}},
		runtimeres.BootedEntryType:   {Items: []resource.Resource{bootedEntry("talos-v1.13.4~1.efi")}},
	})

	if got := gaugeFor(t, families, "talos_kernel_cmdline_info",
		map[string]string{"node": "node-1", "cmdline": cl}); got != 1 {
		t.Errorf("cmdline series missing")
	}
	if got := gaugeFor(t, families, "talos_booted_entry_info",
		map[string]string{"node": "node-1", "entry": "talos-v1.13.4~1.efi"}); got != 1 {
		t.Errorf("booted entry series missing")
	}
	if node.Identity.Cmdline != cl {
		t.Errorf("snapshot cmdline = %q, want %q", node.Identity.Cmdline, cl)
	}
}

// A node with no extensions and no security state is not a failure.
func TestEmptyListsAreNotAnError(t *testing.T) {
	_, node := collectWith(t, map[resource.Type]resource.List{})
	if node.Identity == nil {
		t.Fatal("snapshot Identity is nil")
	}
	if len(node.Identity.Extensions) != 0 || node.Identity.Schematic != "" {
		t.Errorf("empty node reported identity data: %+v", node.Identity)
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
	if n := snap.Node("node-1"); n != nil && n.Identity != nil {
		t.Error("snapshot was written despite the RPC failing")
	}
}

func TestCollectorIdentity(t *testing.T) {
	c := New()
	if c.Name() != "identity" {
		t.Errorf("Name = %q, want identity", c.Name())
	}
	// Inventory: an image does not change without a reboot.
	if c.Class() != collector.Inventory {
		t.Errorf("Class = %v, want Inventory", c.Class())
	}
	if c.DefaultTTL() != defaultTTL {
		t.Errorf("DefaultTTL = %v, want %v", c.DefaultTTL(), defaultTTL)
	}
}

// The structural guard: every string this collector writes to the snapshot
// must be reachable from /metrics, and its families must obey the contract.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	families, snap := collectWith(t, map[resource.Type]resource.List{
		runtimeres.ExtensionStatusType: {Items: []resource.Resource{
			extension("0", "amd-ucode", "20260519"),
			extension("4", "schematic", "65cf8364cd0de4cf"),
			extension("modules.dep", "modules.dep", "6.18.34-talos"),
		}},
		runtimeres.KernelCmdlineType: {Items: []resource.Resource{cmdline("talos.platform=metal selinux=1")}},
		runtimeres.BootedEntryType:   {Items: []resource.Resource{bootedEntry("talos-v1.13.4~1.efi")}},
		runtimeres.SecurityStateType: {Items: []resource.Resource{
			securityState(false, true, true, runtimeres.SELinuxStatePermissive, runtimeres.FIPSStateDisabled),
		}},
	})
	var fams []*dto.MetricFamily
	for _, f := range families {
		fams = append(fams, f)
	}
	opts := metricguard.Options{UncoveredStrings: map[string]bool{
		// The kernel version comes from the virtual modules.dep entry and
		// IS exported, as talos_extension_info{extension="modules.dep"}.
		// It appears here under a different field name only.
	}}
	for _, v := range metricguard.Validate(fams, opts) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(snap.Identity, fams, opts) {
		t.Error(v)
	}
}
