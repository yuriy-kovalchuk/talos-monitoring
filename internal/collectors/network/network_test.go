package network

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	talosnet "github.com/siderolabs/talos/pkg/machinery/resources/network"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

const procNetDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 542646122121 614831192    0    0    0     0          0         0 542646122121 614831192    0    0    0     0       0          0
 bond0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
enp1s0: 10670948523965 8157755557    0 2095    0     0          0       853 1959971847888 2221378325    0  299    0     0       0          0
lxc9f2a1: 123 4 0 0 0 0 0 0 456 7 0 0 0 0 0 0`

func TestParseProcNetDev(t *testing.T) {
	// Only the interfaces the link filter kept are parsed: a node running a CNI
	// has ~90 entries here, nearly all pod veths.
	kept := map[string]bool{"enp1s0": true, "bond0": true}
	got := parseProcNetDev(procNetDevFixture, kept)

	if len(got) != 2 {
		t.Fatalf("parsed %d interfaces, want 2 (lo and the CNI veth must be skipped): %v", len(got), got)
	}
	c := got["enp1s0"]
	if c.rxBytes != 10670948523965 {
		t.Errorf("rxBytes = %d, want 10670948523965", c.rxBytes)
	}
	if c.txBytes != 1959971847888 {
		t.Errorf("txBytes = %d, want 1959971847888", c.txBytes)
	}
	if c.rxDropped != 2095 || c.txDropped != 299 {
		t.Errorf("dropped rx/tx = %d/%d, want 2095/299", c.rxDropped, c.txDropped)
	}
	if c.rxErrors != 0 || c.txErrors != 0 {
		t.Errorf("errors rx/tx = %d/%d, want 0/0", c.rxErrors, c.txErrors)
	}
	if _, ok := got["lo"]; ok {
		t.Error("loopback must not be parsed: it is not in the kept set")
	}
}

func TestPerSecondRejectsCounterResets(t *testing.T) {
	// An interface that was replaced restarts its counters. A naive subtraction
	// on unsigned values would wrap to an astronomical rate.
	if _, ok := perSecond(100, 500, 5); ok {
		t.Error("a counter going backwards must not produce a rate")
	}
	got, ok := perSecond(1500, 500, 5)
	if !ok || got != 200 {
		t.Errorf("perSecond(1500, 500, 5) = %v, %v; want 200, true", got, ok)
	}
}

func TestParseProcNetDevIgnoresShortLines(t *testing.T) {
	kept := map[string]bool{"eth0": true}
	if got := parseProcNetDev("eth0: 1 2 3\n", kept); len(got) != 0 {
		t.Errorf("a truncated line must be skipped, got %v", got)
	}
	if got := parseProcNetDev(strings.Repeat("garbage\n", 3), kept); len(got) != 0 {
		t.Errorf("lines without a colon must be skipped, got %v", got)
	}
}

// fakeState serves LinkStatus and AddressStatus lists.
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

// fakeFiles serves /proc/net/dev.
type fakeFiles struct {
	body string
	err  error
}

func (f fakeFiles) Read(context.Context, string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func link(name, kind string, up bool, speed int, mtu uint32) resource.Resource {
	l := talosnet.NewLinkStatus(talosnet.NamespaceName, name)
	spec := l.TypedSpec()
	spec.Type = nethelpers.LinkEther
	spec.Kind = kind
	spec.MTU = mtu
	spec.HardwareAddr = nethelpers.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 1}
	spec.Driver = "igb"
	spec.BusPath = "0000:01:00.0"
	if up {
		spec.OperationalState = nethelpers.OperStateUp
		spec.LinkState = true
	}
	spec.SpeedMegabits = speed
	spec.Port = nethelpers.Port(0)
	return l
}

// index sets the ifindex pair LinkStatus uses to relate a slave to its bond or
// bridge.
func index(l resource.Resource, idx, master uint32) {
	spec := l.(*talosnet.LinkStatus).TypedSpec()
	spec.Index = idx
	spec.MasterIndex = master
}

func address(linkName, addr string) resource.Resource {
	a := talosnet.NewAddressStatus(talosnet.NamespaceName, linkName+"/"+addr)
	spec := a.TypedSpec()
	spec.LinkName = linkName
	spec.Address = netip.MustParsePrefix(addr)
	spec.Family = nethelpers.FamilyInet4
	spec.Scope = nethelpers.ScopeGlobal
	return a
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// netEnv builds a collector over fake COSI and file sources with a clock the
// test controls, so rate derivation is deterministic.
type netEnv struct {
	c     *Collector
	snap  *snapshot.Store
	now   time.Time
	files *fakeFiles
}

func newNetEnv(t *testing.T, links, addrs []resource.Resource, procNetDev string) *netEnv {
	t.Helper()
	e := &netEnv{
		snap:  snapshot.New(),
		now:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		files: &fakeFiles{body: procNetDev},
	}
	e.c = New(discardLog()).WithSnapshot(e.snap)
	e.c.state = func(*collector.NodeClient) state.CoreState {
		return fakeState{byType: map[resource.Type]resource.List{
			talosnet.LinkStatusType:    {Items: links},
			talosnet.AddressStatusType: {Items: addrs},
		}}
	}
	e.c.newAPI = func(*collector.NodeClient) fileAPI { return e.files }
	e.c.now = func() time.Time { return e.now }
	return e
}

func (e *netEnv) collect(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	node := &collector.NodeClient{Node: nodes.Node{Name: "node-1", IP: "10.0.0.1"}}
	if err := e.c.Collect(context.Background(), node, reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(fams))
	for _, f := range fams {
		out[f.GetName()] = f
	}
	return out
}

func sample(t *testing.T, fams map[string]*dto.MetricFamily, name string, want map[string]string) (float64, bool) {
	t.Helper()
	f, ok := fams[name]
	if !ok {
		return 0, false
	}
next:
	for _, m := range f.GetMetric() {
		for k, v := range want {
			found := false
			for _, l := range m.GetLabel() {
				if l.GetName() == k && l.GetValue() == v {
					found = true
				}
			}
			if !found {
				continue next
			}
		}
		if m.GetCounter() != nil {
			return m.GetCounter().GetValue(), true
		}
		return m.GetGauge().GetValue(), true
	}
	return 0, false
}

const twoScrapeDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
enp1s0: 1000 10 1 2 0 0 0 0 2000 20 3 4 0 0 0 0`

const twoScrapeDev2 = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
enp1s0: 11000 110 1 2 0 0 0 0 12000 120 3 4 0 0 0 0`

func TestCollectExportsLinkStateAndAddresses(t *testing.T) {
	e := newNetEnv(t, []resource.Resource{link("enp1s0", "", true, 1000, 1500)},
		[]resource.Resource{address("enp1s0", "10.0.0.10/24")}, twoScrapeDev)
	fams := e.collect(t)

	if v, ok := sample(t, fams, "talos_net_link_up", map[string]string{"node": "node-1", "link": "enp1s0"}); !ok || v != 1 {
		t.Errorf("link_up = %v (found=%v), want 1", v, ok)
	}
	if v, _ := sample(t, fams, "talos_net_link_mtu_bytes", map[string]string{"link": "enp1s0"}); v != 1500 {
		t.Errorf("mtu = %v, want 1500", v)
	}
	// Speed is reported in megabits and exported in bits per the base-unit rule.
	if v, _ := sample(t, fams, "talos_net_link_speed_bits_per_second", map[string]string{"link": "enp1s0"}); v != 1e9 {
		t.Errorf("speed = %v, want 1e9", v)
	}
	if _, ok := sample(t, fams, "talos_net_address_info", map[string]string{"link": "enp1s0", "address": "10.0.0.10/24"}); !ok {
		t.Error("address_info missing")
	}
	n := e.snap.Node("node-1")
	if len(n.Links) != 1 || n.Links[0].Name != "enp1s0" {
		t.Fatalf("snapshot links = %+v", n.Links)
	}
	if len(n.Links[0].Addresses) != 1 {
		t.Errorf("snapshot addresses = %+v", n.Links[0].Addresses)
	}
}

// A bond's counters already include its slaves', so anything that adds up a
// node's throughput has to know which interfaces are slaves. LinkStatus relates
// them by interface index; the exporter resolves that to a name so both the
// links page and PromQL can count the traffic once.
func TestBondSlavesReportTheirMaster(t *testing.T) {
	bond, eno1, eno2 := link("bond0", "bond", true, 1000, 1500),
		link("eno1", "", true, 1000, 1500), link("eno2", "", true, 1000, 1500)
	index(bond, 10, 0)
	index(eno1, 11, 10)
	index(eno2, 12, 10)

	e := newNetEnv(t, []resource.Resource{bond, eno1, eno2},
		[]resource.Resource{address("bond0", "10.0.0.10/24")}, twoScrapeDev)
	fams := e.collect(t)

	want := map[string]string{"bond0": "", "eno1": "bond0", "eno2": "bond0"}
	for _, l := range e.snap.Node("node-1").Links {
		if l.Master != want[l.Name] {
			t.Errorf("snapshot %s master = %q, want %q", l.Name, l.Master, want[l.Name])
		}
	}
	if _, ok := sample(t, fams, "talos_net_link_info", map[string]string{"link": "eno1", "master": "bond0"}); !ok {
		t.Error("link_info has no master=bond0 series for eno1")
	}
	if _, ok := sample(t, fams, "talos_net_link_info", map[string]string{"link": "bond0", "master": ""}); !ok {
		t.Error("link_info has no empty-master series for the bond itself")
	}
}

// Rates need two samples. The first scrape must publish counters but no rate,
// rather than a rate computed against zero, which would report the whole
// since-boot total as if it happened in one interval.
func TestRatesNeedTwoSamples(t *testing.T) {
	e := newNetEnv(t, []resource.Resource{link("enp1s0", "", true, 1000, 1500)}, nil, twoScrapeDev)

	fams := e.collect(t)
	if v, _ := sample(t, fams, "talos_net_link_rx_bytes_total", map[string]string{"link": "enp1s0"}); v != 1000 {
		t.Errorf("rx_bytes_total = %v, want 1000", v)
	}
	if _, ok := sample(t, fams, "talos_net_link_rx_bytes_per_second", map[string]string{"link": "enp1s0"}); ok {
		t.Error("a rate was published on the first scrape, with nothing to diff against")
	}
	if n := e.snap.Node("node-1"); n.Links[0].HasRates {
		t.Error("snapshot claims a rate on the first scrape")
	}

	// 10s later, +10000 bytes rx => 1000 B/s.
	e.now = e.now.Add(10 * time.Second)
	e.files.body = twoScrapeDev2
	fams = e.collect(t)
	if v, ok := sample(t, fams, "talos_net_link_rx_bytes_per_second", map[string]string{"link": "enp1s0"}); !ok || v != 1000 {
		t.Errorf("rx rate = %v (found=%v), want 1000", v, ok)
	}
	if v, _ := sample(t, fams, "talos_net_link_tx_bytes_per_second", map[string]string{"link": "enp1s0"}); v != 1000 {
		t.Errorf("tx rate = %v, want 1000", v)
	}
	if v, _ := sample(t, fams, "talos_net_link_rx_errors_total", map[string]string{"link": "enp1s0"}); v != 1 {
		t.Errorf("rx_errors = %v, want 1", v)
	}
}

// A counter that went backwards means the interface was reset or replaced.
// Emitting the difference would report a huge negative or wrapped rate.
func TestCounterResetSuppressesTheRate(t *testing.T) {
	e := newNetEnv(t, []resource.Resource{link("enp1s0", "", true, 1000, 1500)}, nil, twoScrapeDev2)
	e.collect(t)

	e.now = e.now.Add(10 * time.Second)
	e.files.body = twoScrapeDev // counters went backwards
	fams := e.collect(t)
	if v, ok := sample(t, fams, "talos_net_link_rx_bytes_per_second", map[string]string{"link": "enp1s0"}); ok && v != 0 {
		t.Errorf("rate %v published across a counter reset", v)
	}
}

// /proc/net/dev needs os:admin. A permission error must disable the counters
// for the process lifetime without failing the scrape — link state comes from
// COSI and is unaffected.
func TestPermissionErrorDisablesCountersOnly(t *testing.T) {
	e := newNetEnv(t, []resource.Resource{link("enp1s0", "", true, 1000, 1500)}, nil, twoScrapeDev)
	e.files.err = status.Error(codes.PermissionDenied, "denied")

	fams := e.collect(t) // must not error
	if _, ok := sample(t, fams, "talos_net_link_up", map[string]string{"link": "enp1s0"}); !ok {
		t.Error("link state was lost when the counter read was refused")
	}
	if _, ok := sample(t, fams, "talos_net_link_rx_bytes_total", map[string]string{"link": "enp1s0"}); ok {
		t.Error("counters exported despite a permission error")
	}
	if !e.c.countersDisabled.Load() {
		t.Error("collector did not self-disable its counter reads")
	}
	// Second round must not retry the refused read.
	e.files.err = errors.New("must not be called")
	e.collect(t)
}

func TestListErrorFailsTheCollection(t *testing.T) {
	e := newNetEnv(t, nil, nil, twoScrapeDev)
	wantErr := errors.New("cosi down")
	e.c.state = func(*collector.NodeClient) state.CoreState { return fakeState{err: wantErr} }
	err := e.c.Collect(context.Background(),
		&collector.NodeClient{Node: nodes.Node{Name: "node-1"}}, prometheus.NewRegistry())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
	}
}

// Prune drops per-node counter samples for departed nodes, so a cluster whose
// node names churn does not grow the map without bound.
func TestPruneDropsDepartedNodes(t *testing.T) {
	e := newNetEnv(t, []resource.Resource{link("enp1s0", "", true, 1000, 1500)}, nil, twoScrapeDev)
	e.collect(t)
	if len(e.c.prev) != 1 {
		t.Fatalf("prev has %d entries, want 1", len(e.c.prev))
	}
	e.c.Prune(map[string]struct{}{"other": {}})
	if len(e.c.prev) != 0 {
		t.Errorf("prev kept %d entries for a departed node", len(e.c.prev))
	}
	// An empty live set means discovery has not synced and must prune nothing.
	e.collect(t)
	e.c.Prune(map[string]struct{}{})
	if len(e.c.prev) != 1 {
		t.Error("an empty live set pruned real state; discovery may simply not have synced")
	}
}

func TestCollectorIdentity(t *testing.T) {
	c := New(discardLog())
	if c.Name() != "network" {
		t.Errorf("Name = %q, want network", c.Name())
	}
	if c.Class() != collector.Live {
		t.Errorf("Class = %v, want Live", c.Class())
	}
}

// The structural guard: every string this collector writes to the snapshot
// must be reachable from /metrics, and its families must obey the contract.
// A bonded topology, so the guard sees a non-empty Master rather than skipping
// an empty one.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	bond, enp := link("bond0", "bond", true, 1000, 1500), link("enp1s0", "", true, 1000, 1500)
	index(bond, 10, 0)
	index(enp, 11, 10)
	e := newNetEnv(t, []resource.Resource{bond, enp},
		[]resource.Resource{address("enp1s0", "10.0.0.10/24")}, twoScrapeDev)
	e.collect(t)
	e.now = e.now.Add(10 * time.Second)
	e.files.body = twoScrapeDev2
	fams := e.collect(t)

	list := make([]*dto.MetricFamily, 0, len(fams))
	for _, f := range fams {
		list = append(list, f)
	}
	opts := metricguard.Options{
		UncoveredStrings: map[string]bool{
			// Driver and firmware versions are joined into one `versions`
			// label on talos_net_link_info; the snapshot keeps them apart for
			// the UI, so neither raw string matches a label on its own.
			"Links.DriverVersion":   true,
			"Links.FirmwareVersion": true,
		},
	}
	for _, v := range metricguard.Validate(list, opts) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(e.snap.Node("node-1").Links, list, opts) {
		t.Error(v)
	}
}

// TestDegradedReflectsCounterDisable: link state and addresses are still
// collected when /proc/net/dev is denied, so the round reaches the node and the
// degradation is not Stopped - only throughput goes dark.
func TestDegradedReflectsCounterDisable(t *testing.T) {
	c := New(discardLog())
	if d := c.Degraded("n1"); len(d) != 0 {
		t.Fatalf("a live collector reported %v", d)
	}
	c.countersDisabled.Store(true)
	d := c.Degraded("n1")
	if len(d) != 1 || d[0].Reason != collector.ReasonPermission || d[0].Stopped {
		t.Errorf("Degraded = %+v, want one non-stopped %q degradation", d, collector.ReasonPermission)
	}
}
