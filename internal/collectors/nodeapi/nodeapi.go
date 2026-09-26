// Package nodeapi collects the slowly-changing per-node status from the
// Talos machine service (Version, Time and SystemStat boot/process data)
// and exposes it as Prometheus metrics.
//
// The fast-changing CPU data (per-mode time, usage, live frequency) lives in the
// cpu collector; nodeapi is re-collected on the scrape interval.
package nodeapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	timepb "github.com/siderolabs/talos/pkg/machinery/api/time"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/sysstat"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the registry name of the nodeapi collector.
const Name = "nodeapi"

// machineAPI is the subset of the per-node Talos client used by the
// collector (seam for tests).
type machineAPI interface {
	Version(ctx context.Context) (*machinepb.VersionResponse, error)
	Time(ctx context.Context) (*timepb.TimeResponse, error)
	SystemStat(ctx context.Context) (*machinepb.SystemStatResponse, error)
	LoadAvg(ctx context.Context) (*machinepb.LoadAvgResponse, error)
}

// talosAPI adapts the per-node machinery client to machineAPI.
type talosAPI struct {
	client *talosclient.Client
}

func (t talosAPI) Version(ctx context.Context) (*machinepb.VersionResponse, error) {
	return t.client.Version(ctx)
}

func (t talosAPI) Time(ctx context.Context) (*timepb.TimeResponse, error) {
	return t.client.Time(ctx)
}

func (t talosAPI) SystemStat(ctx context.Context) (*machinepb.SystemStatResponse, error) {
	return t.client.MachineClient.SystemStat(ctx, &empty.Empty{})
}

func (t talosAPI) LoadAvg(ctx context.Context) (*machinepb.LoadAvgResponse, error) {
	return t.client.MachineClient.LoadAvg(ctx, &empty.Empty{})
}

// Collector implements collector.Collector for slow node status.
type Collector struct {
	newAPI func(node *collector.NodeClient) machineAPI

	// snap receives the typed identity and liveness alongside the metrics.
	snap *snapshot.Store

	// sysstat shares the SystemStat response with the cpu collector, which
	// wants the per-core CPU time out of the same message.
	sysstat *sysstat.Cache
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// WithSysStat shares a SystemStat cache with the other collectors that read it.
func (c *Collector) WithSysStat(s *sysstat.Cache) *Collector {
	c.sysstat = s
	return c
}

// New returns the nodeapi collector.
func New() *Collector {
	return &Collector{
		newAPI: func(node *collector.NodeClient) machineAPI {
			return talosAPI{client: node.Client}
		},
	}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Live }

// Collect calls the machine service RPCs and registers:
//
//	talos_node_version_info{node,version,sha,arch,built} 1
//	talos_node_uptime_seconds{node}
//	talos_node_processes{node,state}
//	talos_node_load1{node}, talos_node_load5{node}, talos_node_load15{node}
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	api := c.newAPI(node)
	name := node.Node.Name

	ver, err := api.Version(ctx)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	if len(ver.GetMessages()) == 0 || ver.GetMessages()[0].GetVersion() == nil {
		return errors.New("version response is empty")
	}
	verInfo := ver.GetMessages()[0].GetVersion()

	tResp, err := api.Time(ctx)
	if err != nil {
		return fmt.Errorf("time: %w", err)
	}
	if len(tResp.GetMessages()) == 0 || tResp.GetMessages()[0].GetRemotetime() == nil {
		return errors.New("time response is empty")
	}
	nodeNow := tResp.GetMessages()[0].GetRemotetime().AsTime()

	sys, err := c.sysstat.Get(ctx, name, api.SystemStat)
	if err != nil {
		return fmt.Errorf("system stat: %w", err)
	}
	if len(sys.GetMessages()) == 0 || sys.GetMessages()[0].GetBootTime() == 0 {
		return errors.New("system stat response is empty")
	}
	stat := sys.GetMessages()[0]

	v := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_version_info",
		Help: "Talos version of the node (value 1).",
	}, []string{"node", "version", "sha", "arch", "built"})
	v.WithLabelValues(name, verInfo.GetTag(), verInfo.GetSha(), verInfo.GetArch(), verInfo.GetBuilt()).Set(1)
	reg.MustRegister(v)

	// BootTime is Unix seconds since the node booted.
	bt := stat.GetBootTime()
	if bt == 0 || bt > math.MaxInt64 {
		return fmt.Errorf("implausible boot time %d", bt)
	}
	boot := time.Unix(int64(bt), 0) //nolint:gosec // bt range-checked above
	if !boot.Before(nodeNow) {
		return fmt.Errorf("boot time %d is not before node time %s", bt, nodeNow.Format(time.RFC3339))
	}
	u := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_uptime_seconds",
		Help: "Node uptime in seconds (node clock).",
	}, []string{"node"})
	u.WithLabelValues(name).Set(nodeNow.Sub(boot).Seconds())
	reg.MustRegister(u)

	p := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_processes",
		Help: "Process counts reported by the node (state: running/blocked).",
	}, []string{"node", "state"})
	p.WithLabelValues(name, "running").Set(float64(stat.GetProcessRunning()))
	p.WithLabelValues(name, "blocked").Set(float64(stat.GetProcessBlocked()))
	reg.MustRegister(p)

	// Load average is best-effort: it is the least important thing this
	// collector reports, and losing the version, uptime and process counts
	// because of it would be a bad trade. A failure leaves the series absent
	// rather than reporting a zero, which on a load metric would read as an
	// idle machine.
	load1, load5, load15, haveLoad := 0.0, 0.0, 0.0, false
	if la, err := api.LoadAvg(ctx); err == nil && len(la.GetMessages()) > 0 {
		m := la.GetMessages()[0]
		load1, load5, load15, haveLoad = m.GetLoad1(), m.GetLoad5(), m.GetLoad15(), true

		l := func(name, help string) *prometheus.GaugeVec {
			return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, []string{"node"})
		}
		g1 := l("talos_node_load1", "1-minute load average: mean number of runnable plus uninterruptible tasks.")
		g5 := l("talos_node_load5", "5-minute load average.")
		g15 := l("talos_node_load15", "15-minute load average.")
		g1.WithLabelValues(name).Set(load1)
		g5.WithLabelValues(name).Set(load5)
		g15.WithLabelValues(name).Set(load15)
		reg.MustRegister(g1, g5, g15)
	}

	if c.snap != nil {
		rt := snapshot.Runtime{
			Version:   verInfo.GetTag(),
			SHA:       verInfo.GetSha(),
			Arch:      verInfo.GetArch(),
			Built:     verInfo.GetBuilt(),
			Uptime:    nodeNow.Sub(boot).Seconds(),
			HasUptime: true,
			Running:   stat.GetProcessRunning(),
			Blocked:   stat.GetProcessBlocked(),
			HasLoad:   haveLoad,
			Load1:     load1,
			Load5:     load5,
			Load15:    load15,
		}
		c.snap.Update(name, func(n *snapshot.Node) { n.Runtime = &rt })
	}
	return nil
}
