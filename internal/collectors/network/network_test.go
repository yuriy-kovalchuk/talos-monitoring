package network

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	talosnet "github.com/siderolabs/talos/pkg/machinery/resources/network"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// devStats builds a NetworkDeviceStats response; a node running a CNI lists ~90 devices, nearly all pod veths.
func devStats(devs ...*machine.NetDev) *machine.NetworkDeviceStatsResponse {
	return &machine.NetworkDeviceStatsResponse{Messages: []*machine.NetworkDeviceStats{{Devices: devs}}}
}

func TestFromDeviceStatsKeepsReportedInterfaces(t *testing.T) {
	resp := devStats(
		&machine.NetDev{Name: "lo", RxBytes: 542646122121},
		&machine.NetDev{Name: "bond0"},
		&machine.NetDev{Name: "enp1s0", RxBytes: 10670948523965, TxBytes: 1959971847888, RxDropped: 2095, TxDropped: 299},
		&machine.NetDev{Name: "lxc9f2a1", RxBytes: 123},
	)
	got := fromDeviceStats(resp, map[string]bool{"enp1s0": true, "bond0": true})
	if len(got) != 2 {
		t.Fatalf("kept %d interfaces, want 2 (lo and the CNI veth must be skipped): %v", len(got), got)
	}
	c := got["enp1s0"]
	if c.rxBytes != 10670948523965 || c.txBytes != 1959971847888 {
		t.Errorf("bytes rx/tx = %d/%d", c.rxBytes, c.txBytes)
	}
	if c.rxDropped != 2095 || c.txDropped != 299 {
		t.Errorf("dropped rx/tx = %d/%d, want 2095/299", c.rxDropped, c.txDropped)
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

// fakeStats serves NetworkDeviceStats.
type fakeStats struct {
	resp *machine.NetworkDeviceStatsResponse
	err  error
}

func (f *fakeStats) NetworkDeviceStats(context.Context) (*machine.NetworkDeviceStatsResponse, error) {
	return f.resp, f.err
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

// netEnv builds a collector over fake COSI and device-stats sources with a clock the
// test controls, so rate derivation is deterministic.
type netEnv struct {
	c     *Collector
	snap  *snapshot.Store
	now   time.Time
	stats *fakeStats
}

func newNetEnv(t *testing.T, links, addrs []resource.Resource, stats *machine.NetworkDeviceStatsResponse) *netEnv {
	t.Helper()
	e := &netEnv{
		snap:  snapshot.New(),
		now:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		stats: &fakeStats{resp: stats},
	}
	e.c = New().WithSnapshot(e.snap)
	e.c.state = func(*collector.NodeClient) state.CoreState {
		return fakeState{byType: map[resource.Type]resource.List{
			talosnet.LinkStatusType:    {Items: links},
			talosnet.AddressStatusType: {Items: addrs},
		}}
	}
	e.c.newAPI = func(*collector.NodeClient) statsAPI { return e.stats }
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

var (
	twoScrapeDev = devStats(&machine.NetDev{Name: "enp1s0", RxBytes: 1000, RxPackets: 10, RxErrors: 1, RxDropped: 2,
		TxBytes: 2000, TxPackets: 20, TxErrors: 3, TxDropped: 4})
	twoScrapeDev2 = devStats(&machine.NetDev{Name: "enp1s0", RxBytes: 11000, RxPackets: 110, RxErrors: 1, RxDropped: 2,
		TxBytes: 12000, TxPackets: 120, TxErrors: 3, TxDropped: 4})
)

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

// WireGuard and other carrier-less links report operstate "unknown" forever;
// the kernel's verdict for those is the UP and LOWER_UP flags, as ip link shows.
func TestUnknownOperStateLinkIsUpWhenFlagsSayTraffic(t *testing.T) {
	tunnel := link("wg0", "wireguard", false, 0, 1420)
	tunnel.(*talosnet.LinkStatus).TypedSpec().OperationalState = nethelpers.OperStateUnknown
	tunnel.(*talosnet.LinkStatus).TypedSpec().Flags = nethelpers.LinkFlags(nethelpers.LinkUp | nethelpers.LinkLowerUp)
	idle := link("wg1", "wireguard", false, 0, 1420)
	idle.(*talosnet.LinkStatus).TypedSpec().OperationalState = nethelpers.OperStateUnknown
	idle.(*talosnet.LinkStatus).TypedSpec().Flags = nethelpers.LinkFlags(nethelpers.LinkUp)
	e := newNetEnv(t, []resource.Resource{tunnel, idle}, nil, twoScrapeDev)
	fams := e.collect(t)

	for name, want := range map[string]float64{"wg0": 1, "wg1": 0} {
		if v, ok := sample(t, fams, "talos_net_link_up", map[string]string{"link": name}); !ok || v != want {
			t.Errorf("%s link_up = %v (found=%v), want %v", name, v, ok, want)
		}
	}
	for _, l := range e.snap.Node("node-1").Links {
		if l.Name == "wg0" && !l.Up {
			t.Error("snapshot reports the live tunnel as down")
		}
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
	e.stats.resp = twoScrapeDev2
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
	e.stats.resp = twoScrapeDev // counters went backwards
	fams := e.collect(t)
	if v, ok := sample(t, fams, "talos_net_link_rx_bytes_per_second", map[string]string{"link": "enp1s0"}); ok && v != 0 {
		t.Errorf("rate %v published across a counter reset", v)
	}
}

// The counters come from an os:reader-safe RPC, so a failure is a real error, counted like any other.
func TestDeviceStatsErrorFailsTheCollection(t *testing.T) {
	e := newNetEnv(t, []resource.Resource{link("enp1s0", "", true, 1000, 1500)}, nil, twoScrapeDev)
	wantErr := status.Error(codes.Unavailable, "apid down")
	e.stats.err = wantErr
	err := e.c.Collect(context.Background(),
		&collector.NodeClient{Node: nodes.Node{Name: "node-1"}}, prometheus.NewRegistry())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
	}
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
	c := New()
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
	e.stats.resp = twoScrapeDev2
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
