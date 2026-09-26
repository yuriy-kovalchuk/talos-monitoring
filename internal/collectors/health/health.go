// Package health collects the node's own verdict on whether it is working:
// the init system's per-service state, the aggregated machine stage and
// readiness, and any diagnostic warnings Talos is currently raising.
//
// This is the only first-party "is the OS actually up" signal. Every hardware
// panel can be green — sensors nominal, disks present, links carrying — while
// etcd or the kubelet is dead, because nothing else the exporter collects
// looks at the services at all.
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

// Collector reads Services, MachineStatus and Diagnostics from the COSI
// "runtime" namespace.
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

// WithSnapshot points the collector at a snapshot store. Returns the receiver
// so it can be chained in the registry list.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
//
// Live, not Inventory: a dead service is the thing an operator wants to know
// about soonest, and the inventory cadence would hide a stopped etcd for
// minutes. Three COSI lists per node per scrape is a cheap price for that.
func (c *Collector) Class() collector.Class { return collector.Live }

// Collect lists the three kinds and registers them.
//
// An empty list is not an error, matching every other collector here: the RPC
// failing is the failure. Diagnostics being empty is in fact the healthy
// state, and a node in maintenance mode legitimately runs few services.
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

// list lists a whole kind from the node's COSI state (an empty ID in the
// metadata means "all resources of this kind").

// registerServices exports the init system's per-service state.
//
// Running, healthy and unknown are three separate families rather than one
// encoded state gauge, because Talos genuinely reports three independent
// booleans and collapsing them loses information.
//
// "unknown" means nothing ever checked, not that a check failed: Talos sets it
// from `status.Healthy == nil` (system/health/status.go), and Healthy is a
// pointer that stays nil until a health check produces a result. Two kinds of
// service never produce one, permanently:
//
//   - health checks are opt-in through the optional HealthcheckedService
//     interface (system/service.go); `dashboard` does not implement it;
//   - extension services cannot have one at all — the extension service spec
//     (machinery/extensions/services) has no health-check field, so every
//     `ext-*` service is unknown by construction.
//
// Both therefore report healthy=false with unknown=true forever, and the alert
// that means "something is broken" is
//
//	talos_service_healthy == 0 and talos_service_health_unknown == 0
//
// Alerting on healthy == 0 alone fires permanently on a healthy cluster.
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

// registerMachine exports the aggregated boot stage and readiness.
//
// The stage is a mutable string attribute, so it rides on its own _info metric
// per the metric contract: a node moving from running to upgrading moves the
// series rather than changing a value that PromQL cannot interpret.
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
		// Zero-initialised so `> 0` works on a healthy node, where the
		// _info metric below has no series at all.
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

// registerDiagnostics exports Talos's own warnings.
//
// The count is zero-initialised because the per-warning _info metric has no
// series on a healthy node, and "no series" is indistinguishable from "the
// collector never ran" without it.
//
// The spec also carries a Details slice of free-text remediation lines. It is
// deliberately not collected: it is unbounded prose, and the ID plus message
// already identify the warning (each maps to talos.dev/diagnostic/<id>).
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

// health declares no DefaultTTL: it follows --scrape-interval like the other
// Live collectors.
var _ collector.Collector = (*Collector)(nil)
