// Package network collects per-node network interface state from the Talos
// COSI "network" namespace and exposes it as Prometheus metrics.
//
// Only interfaces that describe the machine are reported. A Talos node running
// a CNI carries ~70 links, almost all of them veth pairs belonging to pods;
// those churn constantly and say nothing about the hardware, so the collector
// keeps physical NICs plus the logical interfaces an operator configures
// (bonds, bridges, VLANs, wireguard).
package network

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	talosnet "github.com/siderolabs/talos/pkg/machinery/resources/network"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the registry name of the network collector.
const Name = "network"

// keptKinds are the logical interface kinds worth reporting. A physical NIC has
// no kind at all, which is how it is recognised; everything else not listed
// here (veth, vxlan, dummy, tunnels) belongs to the CNI, not the machine.
var keptKinds = map[string]bool{
	"bond": true, "bridge": true, "vlan": true, "wireguard": true,
}

// speedUnknown is what the kernel reports for a link with no carrier: the
// driver returns -1, which arrives as the unsigned sentinel. Exporting it would
// put 4294967295 Mbit/s on a graph.
const speedUnknown = 4294967295

// Collector implements collector.Collector for network interfaces.
type Collector struct {
	state  func(node *collector.NodeClient) state.CoreState
	newAPI func(node *collector.NodeClient) fileAPI
	log    *slog.Logger

	// snap receives the typed interface list alongside the metrics.
	snap *snapshot.Store
	now  func() time.Time

	mu   sync.Mutex
	prev map[string]counterSample // keyed by node name

	// countersDisabled is set on the first permission error: /proc/net/dev
	// needs os:admin. Interface state comes from COSI and is unaffected.
	countersDisabled atomic.Bool
}

// New returns the network collector.
func New(log *slog.Logger) *Collector {
	return &Collector{
		state:  collector.DefaultState,
		newAPI: func(node *collector.NodeClient) fileAPI { return node.Client },
		log:    log,
		now:    time.Now,
		prev:   make(map[string]counterSample),
	}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector: link state is live data.
func (c *Collector) Class() collector.Class { return collector.Live }

// defaultTTL matches the cpu collector: throughput is only as smooth as the
// sampling interval, and one /proc/net/dev read covers every interface.
const defaultTTL = 5 * time.Second

// DefaultTTL implements collector.DefaultTTLer.
func (c *Collector) DefaultTTL() time.Duration { return defaultTTL }

// Collect lists LinkStatus and AddressStatus and registers:
//
//	talos_net_link_info{node,link,...} 1
//	talos_net_link_up{node,link}
//	talos_net_link_carrier{node,link}
//	talos_net_link_speed_mbit{node,link}
//	talos_net_link_mtu_bytes{node,link}
//	talos_net_address_info{node,link,address,family,scope} 1
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	st := c.state(node)
	name := node.Node.Name

	links, err := st.List(ctx, resource.NewMetadata(
		talosnet.NamespaceName, talosnet.LinkStatusType, "", resource.VersionUndefined))
	if err != nil {
		return fmt.Errorf("list LinkStatus: %w", err)
	}

	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_net_link_info",
		Help: "Network interface identity (value 1), one series per interface.",
	}, []string{"node", "link", "type", "kind", "hwaddr", "driver", "driver_version",
		"firmware_version", "bus_path", "pci_id", "vendor", "product", "port", "duplex"})
	up := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_net_link_up",
		Help: "1 when the interface's operational state is up.",
	}, []string{"node", "link"})
	carrier := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_net_link_carrier",
		Help: "1 when the interface has a carrier (a cable is connected and the peer is live).",
	}, []string{"node", "link"})
	speed := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_net_link_speed_bits_per_second",
		Help: "Negotiated link speed in bits per second (2.5GbE reports 2.5e9). Absent when the link has no carrier.",
	}, []string{"node", "link"})
	mtu := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_net_link_mtu_bytes",
		Help: "Interface MTU: the largest payload the link carries in one frame, in bytes.",
	}, []string{"node", "link"})
	for _, g := range []prometheus.Collector{info, up, carrier, speed, mtu} {
		reg.MustRegister(g)
	}

	kept := map[string]bool{}
	var snapLinks []snapshot.Link
	byName := map[string]int{}
	for _, r := range links.Items {
		l, ok := r.(*talosnet.LinkStatus)
		if !ok {
			return fmt.Errorf("unexpected resource %T in LinkStatus list", r)
		}
		spec := l.TypedSpec()
		kind := spec.Kind
		// A physical NIC is an ethernet link with no kind. Requiring the
		// ethernet type as well drops loopback and the "void" queueing dummies
		// (teql0), which have no kind either but describe nothing.
		physical := kind == "" && spec.Type == nethelpers.LinkEther
		if !physical && !keptKinds[kind] {
			continue
		}
		id := l.Metadata().ID()
		kept[id] = true

		info.WithLabelValues(name, id, spec.Type.String(), kind, spec.HardwareAddr.String(),
			spec.Driver, spec.DriverVersion, spec.FirmwareVersion, spec.BusPath, spec.PCIID,
			spec.Vendor, spec.Product, spec.Port.String(), spec.Duplex.String()).Set(1)

		up.WithLabelValues(name, id).Set(collector.BoolValue(spec.OperationalState == nethelpers.OperStateUp))
		carrier.WithLabelValues(name, id).Set(collector.BoolValue(spec.LinkState))
		mtu.WithLabelValues(name, id).Set(float64(spec.MTU))
		sl := snapshot.Link{
			Name:            id,
			Type:            spec.Type.String(),
			Kind:            kind,
			HWAddr:          spec.HardwareAddr.String(),
			Driver:          spec.Driver,
			DriverVersion:   spec.DriverVersion,
			FirmwareVersion: spec.FirmwareVersion,
			BusPath:         spec.BusPath,
			PCIID:           spec.PCIID,
			Vendor:          spec.Vendor,
			Product:         spec.Product,
			Port:            spec.Port.String(),
			Duplex:          spec.Duplex.String(),
			Up:              spec.OperationalState == nethelpers.OperStateUp,
			Carrier:         spec.LinkState,
			MTUBytes:        float64(spec.MTU),
		}
		if s := spec.SpeedMegabits; s > 0 && s < speedUnknown {
			// The kernel reports megabits; the metric is bits per second.
			speed.WithLabelValues(name, id).Set(float64(s) * 1e6)
			sl.SpeedMbit = float64(s)
		}
		byName[id] = len(snapLinks)
		snapLinks = append(snapLinks, sl)
	}

	addrs, err := st.List(ctx, resource.NewMetadata(
		talosnet.NamespaceName, talosnet.AddressStatusType, "", resource.VersionUndefined))
	if err != nil {
		return fmt.Errorf("list AddressStatus: %w", err)
	}
	addrInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_net_address_info",
		Help: "Address configured on an interface (value 1).",
	}, []string{"node", "link", "address", "family", "scope"})
	reg.MustRegister(addrInfo)
	for _, r := range addrs.Items {
		a, ok := r.(*talosnet.AddressStatus)
		if !ok {
			continue
		}
		spec := a.TypedSpec()
		// Only addresses on the interfaces reported above: pod addresses on CNI
		// veths are unbounded and belong to Kubernetes, not the machine.
		if !kept[spec.LinkName] {
			continue
		}
		addrInfo.WithLabelValues(name, spec.LinkName, spec.Address.String(),
			spec.Family.String(), spec.Scope.String()).Set(1)
		if i, ok := byName[spec.LinkName]; ok {
			snapLinks[i].Addresses = append(snapLinks[i].Addresses, snapshot.Address{
				Address: spec.Address.String(),
				Family:  spec.Family.String(),
				Scope:   spec.Scope.String(),
			})
		}
	}

	counters := c.registerCounters(ctx, c.newAPI(node), name, kept, reg)
	if c.snap != nil {
		for i := range snapLinks {
			cur, ok := counters[snapLinks[i].Name]
			if !ok {
				continue
			}
			snapLinks[i].HasCounters = true
			snapLinks[i].HasRates = cur.HasRate
			snapLinks[i].RxPerSecond = cur.RxRate
			snapLinks[i].TxPerSecond = cur.TxRate
			snapLinks[i].RxBytes = float64(cur.rxBytes)
			snapLinks[i].TxBytes = float64(cur.txBytes)
			snapLinks[i].RxDropped = float64(cur.rxDropped)
			snapLinks[i].TxDropped = float64(cur.txDropped)
			snapLinks[i].RxErrors = float64(cur.rxErrors)
			snapLinks[i].TxErrors = float64(cur.txErrors)
		}
		sort.Slice(snapLinks, func(i, j int) bool { return snapLinks[i].Name < snapLinks[j].Name })
		c.snap.Update(name, func(n *snapshot.Node) { n.Links = snapLinks })
	}
	return nil
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}
