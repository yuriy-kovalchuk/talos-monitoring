// Package kernel collects kernel tunables and modules; the value is drift, not liveness.
package kernel

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "kernel"

// defaultTTL: tunables and modules change on reboot, not during a run.
const defaultTTL = time.Hour

// Collector reads KernelParamStatus and LoadedKernelModule from the COSI runtime namespace.
type Collector struct {
	// state returns the COSI state to query for a node; overridable in tests.
	state func(node *collector.NodeClient) state.CoreState

	// snap receives the typed view alongside the metrics. nil disables it.
	snap *snapshot.Store
}

// New returns the kernel collector.
func New() *Collector {
	return &Collector{state: collector.DefaultState}
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Inventory }

// DefaultTTL implements collector.DefaultTTLer.
func (c *Collector) DefaultTTL() time.Duration { return defaultTTL }

// Collect lists both kinds and registers them.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	st := c.state(node)
	name := node.Node.Name

	params, err := collector.List(ctx, st, "KernelParamStatus", runtimeres.KernelParamStatusType)
	if err != nil {
		return err
	}
	modules, err := collector.List(ctx, st, "LoadedKernelModule", runtimeres.LoadedKernelModuleType)
	if err != nil {
		return err
	}

	view := snapshot.Kernel{}
	if err := registerParams(name, params, reg, &view); err != nil {
		return err
	}
	if err := registerModules(name, modules, reg, &view); err != nil {
		return err
	}

	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.Kernel = &view })
	}
	return nil
}

// list lists a whole kind from the node's COSI state.

// registerParams exports params as labels (a tunable is not a measurement) plus unsupported/drifted gauges.
func registerParams(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Kernel) error {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_param_info",
		Help: "Kernel parameter (value 1) with its current and default values as labels. Values are labels because a tunable has no unit and nothing to aggregate.",
	}, []string{"node", "param", "current", "default"})
	unsupported := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_param_unsupported",
		Help: "1 when the running kernel does not recognise the parameter, so the configured value is silently ignored.",
	}, []string{"node", "param"})
	unsupportedCount := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_params_unsupported",
		Help: "Number of configured kernel parameters the running kernel does not recognise. Zero on a healthy node.",
	}, []string{"node"})
	drifted := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_param_drifted",
		Help: "1 when the parameter's current value differs from the one Talos recorded before it wrote its own (usually, but not always, the kernel's compiled-in default).",
	}, []string{"node", "param"})
	driftedCount := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_params_drifted",
		Help: "Number of kernel parameters whose current value differs from the one recorded before Talos wrote to them.",
	}, []string{"node"})
	reg.MustRegister(info, unsupported, unsupportedCount, drifted, driftedCount)

	unsupportedTotal, driftedTotal := 0, 0
	view.Params = make([]snapshot.KernelParam, 0, len(l.Items))
	for _, r := range l.Items {
		kp, ok := r.(*runtimeres.KernelParamStatus)
		if !ok {
			return fmt.Errorf("unexpected resource %T in KernelParamStatus list", r)
		}
		spec := kp.TypedSpec()
		param := kp.Metadata().ID()
		info.WithLabelValues(nodeName, param, spec.Current, spec.Default).Set(1)
		unsupported.WithLabelValues(nodeName, param).Set(collector.BoolValue(spec.Unsupported))
		if spec.Unsupported {
			unsupportedTotal++
		}
		drifted.WithLabelValues(nodeName, param).Set(collector.BoolValue(spec.Current != spec.Default))
		if spec.Current != spec.Default {
			driftedTotal++
		}
		view.Params = append(view.Params, snapshot.KernelParam{
			Name: param, Current: spec.Current, Default: spec.Default,
			Unsupported: spec.Unsupported,
		})
	}
	// Zero-initialised: 0 distinguishes a healthy node from a collector that never ran.
	unsupportedCount.WithLabelValues(nodeName).Set(float64(unsupportedTotal))
	driftedCount.WithLabelValues(nodeName).Set(float64(driftedTotal))
	sort.Slice(view.Params, func(i, j int) bool { return view.Params[i].Name < view.Params[j].Name })
	return nil
}

// registerModules exports the module list; dependencies are not collected (§1.1 the other way).
func registerModules(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Kernel) error {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_module_info",
		Help: "Loaded kernel module (value 1), one series per module, with its load state.",
	}, []string{"node", "module", "state"})
	size := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_module_size_bytes",
		Help: "Memory the loaded module occupies, in bytes.",
	}, []string{"node", "module"})
	refs := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_module_reference_count",
		Help: "Number of users holding a reference to the module; 0 means it could be unloaded.",
	}, []string{"node", "module"})
	count := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_modules",
		Help: "Number of kernel modules loaded on the node.",
	}, []string{"node"})
	reg.MustRegister(info, size, refs, count)

	count.WithLabelValues(nodeName).Set(float64(len(l.Items)))
	view.Modules = make([]snapshot.KernelModule, 0, len(l.Items))
	for _, r := range l.Items {
		km, ok := r.(*runtimeres.LoadedKernelModule)
		if !ok {
			return fmt.Errorf("unexpected resource %T in LoadedKernelModule list", r)
		}
		spec := km.TypedSpec()
		module := km.Metadata().ID()
		info.WithLabelValues(nodeName, module, spec.State).Set(1)
		size.WithLabelValues(nodeName, module).Set(float64(spec.Size))
		refs.WithLabelValues(nodeName, module).Set(float64(spec.ReferenceCount))
		view.Modules = append(view.Modules, snapshot.KernelModule{
			Name: module, State: spec.State,
			SizeBytes: float64(spec.Size), ReferenceCount: spec.ReferenceCount,
		})
	}
	sort.Slice(view.Modules, func(i, j int) bool { return view.Modules[i].Name < view.Modules[j].Name })
	return nil
}

var _ collector.Collector = (*Collector)(nil)
