// Package timesync reports clock discipline: Talos's synced verdict and the kernel's adjtimex view.
package timesync

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	timeres "github.com/siderolabs/talos/pkg/machinery/resources/time"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "time"

// Collector reads TimeStatus and AdjtimeStatus from the COSI runtime namespace.
type Collector struct {
	// state returns the COSI state to query for a node; overridable in tests.
	state func(node *collector.NodeClient) state.CoreState

	// snap receives the typed view alongside the metrics. nil disables it.
	snap *snapshot.Store
}

// New returns the time collector.
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

// Live: drift is a moving quantity and the point is noticing it turn bad.
func (c *Collector) Class() collector.Class { return collector.Live }

// Collect lists both kinds and registers them.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	st := c.state(node)
	name := node.Node.Name

	status, err := collector.List(ctx, st, "TimeStatus", timeres.StatusType)
	if err != nil {
		return err
	}
	adjtime, err := collector.List(ctx, st, "AdjtimeStatus", timeres.AdjtimeStatusType)
	if err != nil {
		return err
	}

	view := snapshot.TimeSync{}
	if err := registerStatus(name, status, reg, &view); err != nil {
		return err
	}
	if err := registerAdjtime(name, adjtime, reg, &view); err != nil {
		return err
	}

	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.TimeSync = &view })
	}
	return nil
}

// list lists a whole kind from the node's COSI state.

// registerStatus exports Talos's view; epoch is a gauge because it resets on reboot.
func registerStatus(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.TimeSync) error {
	g := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, []string{"node"})
	}
	synced := g("talos_time_synced", "1 when Talos considers the clock synchronised to its time servers.")
	disabled := g("talos_time_sync_disabled", "1 when time synchronisation is disabled in the machine configuration.")
	epoch := g("talos_time_epoch",
		"Number of times the clock has been stepped since boot. Resets to 0 on reboot, so it is a gauge; a change means the clock jumped.")
	reg.MustRegister(synced, disabled, epoch)

	for _, r := range l.Items {
		ts, ok := r.(*timeres.Status)
		if !ok {
			return fmt.Errorf("unexpected resource %T in TimeStatus list", r)
		}
		spec := ts.TypedSpec()
		synced.WithLabelValues(nodeName).Set(collector.BoolValue(spec.Synced))
		disabled.WithLabelValues(nodeName).Set(collector.BoolValue(spec.SyncDisabled))
		epoch.WithLabelValues(nodeName).Set(float64(spec.Epoch))

		view.HasStatus = true
		view.Synced = spec.Synced
		view.SyncDisabled = spec.SyncDisabled
		view.Epoch = spec.Epoch
	}
	return nil
}

// registerAdjtime exports the kernel view in seconds; STA_UNSYNC and Talos's verdict differ.
func registerAdjtime(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.TimeSync) error {
	g := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, []string{"node"})
	}
	offset := g("talos_time_offset_seconds", "Current clock offset the kernel is correcting for, in seconds. Signed: negative means the clock is behind.")
	maxError := g("talos_time_max_error_seconds", "Kernel's maximum error estimate for the clock, in seconds.")
	estError := g("talos_time_est_error_seconds", "Kernel's estimated error for the clock, in seconds.")
	freq := g("talos_time_frequency_adjustment_ratio",
		"Frequency correction the kernel applies to the local oscillator. 1.0 is no correction; 1.000033 means the crystal runs 33 ppm slow.")
	kernelSynced := g("talos_time_kernel_synced",
		"1 when the kernel's clock is synchronised (STA_UNSYNC clear). Distinct from talos_time_synced, which is Talos's own ntpd verdict.")
	stateInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_time_state_info",
		Help: "Kernel clock state (value 1): TIME_OK, TIME_INS, TIME_DEL, TIME_OOP, TIME_WAIT, TIME_ERROR.",
	}, []string{"node", "state"})
	reg.MustRegister(offset, maxError, estError, freq, kernelSynced, stateInfo)

	for _, r := range l.Items {
		as, ok := r.(*timeres.AdjtimeStatus)
		if !ok {
			return fmt.Errorf("unexpected resource %T in AdjtimeStatus list", r)
		}
		spec := as.TypedSpec()
		offset.WithLabelValues(nodeName).Set(spec.Offset.Seconds())
		maxError.WithLabelValues(nodeName).Set(spec.MaxError.Seconds())
		estError.WithLabelValues(nodeName).Set(spec.EstError.Seconds())
		freq.WithLabelValues(nodeName).Set(spec.FrequencyAdjustmentRatio)
		kernelSynced.WithLabelValues(nodeName).Set(collector.BoolValue(spec.SyncStatus))
		stateInfo.WithLabelValues(nodeName, spec.State).Set(1)

		view.HasAdjtime = true
		view.OffsetSeconds = spec.Offset.Seconds()
		view.MaxErrorSeconds = spec.MaxError.Seconds()
		view.EstErrorSeconds = spec.EstError.Seconds()
		view.FrequencyRatio = spec.FrequencyAdjustmentRatio
		view.KernelSynced = spec.SyncStatus
		view.State = spec.State
	}
	return nil
}

var _ collector.Collector = (*Collector)(nil)
