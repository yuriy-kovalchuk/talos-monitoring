// Package health reports the node's own verdict: service state, machine stage and diagnostics.
package health

import (
	"context"
	"fmt"
	"sort"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "health"

// Collector reads Services, MachineStatus and Diagnostics from the COSI runtime namespace.
type Collector struct {
	// state returns the COSI state to query for a node; overridable in tests.
	state func(node *collector.NodeClient) state.CoreState

	// snap receives the typed view alongside the metrics. nil disables it.
	snap *snapshot.Store
}

// New returns the health collector.
func New() *Collector {
	return &Collector{state: collector.DefaultState}
}

// WithSnapshot points the collector at a snapshot store and returns the receiver to chain.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Live, not Inventory: a stopped etcd must not wait for the inventory cadence.
func (c *Collector) Class() collector.Class { return collector.Live }

// Collect lists the three kinds; an empty list is not an error, only a failed RPC is.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	st := c.state(node)
	name := node.Node.Name

	services, err := collector.List(ctx, st, "Services", v1alpha1.ServiceType)
	if err != nil {
		return err
	}
	machine, err := collector.List(ctx, st, "MachineStatus", runtimeres.MachineStatusType)
	if err != nil {
		return err
	}
	diagnostics, err := collector.List(ctx, st, "Diagnostics", runtimeres.DiagnosticType)
	if err != nil {
		return err
	}

	view := snapshot.Health{}
	if err := registerServices(name, services, reg, &view); err != nil {
		return err
	}
	if err := registerMachine(name, machine, reg, &view); err != nil {
		return err
	}
	if err := registerDiagnostics(name, diagnostics, reg, &view); err != nil {
		return err
	}

	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.Health = &view })
	}
	return nil
}

// list lists every resource of a kind (an empty ID means all of them).

// registerServices exports running, healthy and unknown as three families: Talos reports three booleans.
func registerServices(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Health) error {
	g := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help},
			[]string{"node", "service"})
	}
	running := g("talos_service_running", "1 when the init system reports the service as running.")
	healthy := g("talos_service_healthy",
		"1 when the service's health check passes. 0 with talos_service_health_unknown=1 means the service publishes no health check, not that it is unhealthy.")
	unknown := g("talos_service_health_unknown", "1 when the service publishes no health check, so talos_service_healthy carries no information.")
	reg.MustRegister(running, healthy, unknown)

	view.Services = make([]snapshot.Service, 0, len(l.Items))
	for _, r := range l.Items {
		svc, ok := r.(*v1alpha1.Service)
		if !ok {
			return fmt.Errorf("unexpected resource %T in Service list", r)
		}
		spec := svc.TypedSpec()
		id := svc.Metadata().ID()
		running.WithLabelValues(nodeName, id).Set(collector.BoolValue(spec.Running))
		healthy.WithLabelValues(nodeName, id).Set(collector.BoolValue(spec.Healthy))
		unknown.WithLabelValues(nodeName, id).Set(collector.BoolValue(spec.Unknown))
		view.Services = append(view.Services, snapshot.Service{
			ID:      id,
			Running: spec.Running,
			Healthy: spec.Healthy,
			Unknown: spec.Unknown,
		})
	}
	sort.Slice(view.Services, func(i, j int) bool { return view.Services[i].ID < view.Services[j].ID })
	return nil
}

// registerMachine exports the boot stage and readiness; stage is a mutable _info attribute.
func registerMachine(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Health) error {
	stage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_stage_info",
		Help: "Machine boot stage (value 1): booting, installing, maintenance, running, rebooting, shutting down, resetting, upgrading.",
	}, []string{"node", "stage"})
	ready := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_ready",
		Help: "1 when the machine has met every condition for its current stage.",
	}, []string{"node"})
	unmetCount := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_unmet_conditions",
		Help: "Number of conditions preventing the machine from being ready. Zero on a healthy node.",
	}, []string{"node"})
	unmet := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_machine_unmet_condition_info",
		Help: "One series (value 1) per condition preventing readiness, with its reason.",
	}, []string{"node", "condition", "reason"})
	reg.MustRegister(stage, ready, unmetCount, unmet)

	for _, r := range l.Items {
		ms, ok := r.(*runtimeres.MachineStatus)
		if !ok {
			return fmt.Errorf("unexpected resource %T in MachineStatus list", r)
		}
		spec := ms.TypedSpec()
		stage.WithLabelValues(nodeName, spec.Stage.String()).Set(1)
		ready.WithLabelValues(nodeName).Set(collector.BoolValue(spec.Status.Ready))
		// Zero-initialised so > 0 works on a healthy node with no _info series.
		unmetCount.WithLabelValues(nodeName).Set(float64(len(spec.Status.UnmetConditions)))

		view.Stage = spec.Stage.String()
		view.Ready = spec.Status.Ready
		view.HasMachineStatus = true
		view.UnmetConditions = make([]snapshot.UnmetCondition, 0, len(spec.Status.UnmetConditions))
		for _, cond := range spec.Status.UnmetConditions {
			unmet.WithLabelValues(nodeName, cond.Name, cond.Reason).Set(1)
			view.UnmetConditions = append(view.UnmetConditions,
				snapshot.UnmetCondition{Name: cond.Name, Reason: cond.Reason})
		}
	}
	return nil
}

// registerDiagnostics exports Talos's warnings; the free-text Details are not collected.
func registerDiagnostics(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Health) error {
	count := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_diagnostics",
		Help: "Number of active Talos diagnostic warnings on the node. Zero on a healthy node.",
	}, []string{"node"})
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_diagnostic_info",
		Help: "One series (value 1) per active diagnostic warning; id maps to talos.dev/diagnostic/<id>.",
	}, []string{"node", "id", "message"})
	reg.MustRegister(count, info)

	count.WithLabelValues(nodeName).Set(float64(len(l.Items)))
	view.Diagnostics = make([]snapshot.Diagnostic, 0, len(l.Items))
	for _, r := range l.Items {
		d, ok := r.(*runtimeres.Diagnostic)
		if !ok {
			return fmt.Errorf("unexpected resource %T in Diagnostic list", r)
		}
		id, msg := d.Metadata().ID(), d.TypedSpec().Message
		info.WithLabelValues(nodeName, id, msg).Set(1)
		view.Diagnostics = append(view.Diagnostics, snapshot.Diagnostic{ID: id, Message: msg})
	}
	sort.Slice(view.Diagnostics, func(i, j int) bool { return view.Diagnostics[i].ID < view.Diagnostics[j].ID })
	return nil
}

// health follows --scrape-interval like the other Live collectors.
var _ collector.Collector = (*Collector)(nil)
