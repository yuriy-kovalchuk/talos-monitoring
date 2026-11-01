// Package identity reports what software a node runs: schematic, extensions, boot entry, security.
package identity

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
const Name = "identity"

// defaultTTL: an image does not change without a reboot.
const defaultTTL = time.Hour

// Talos reports schematic and modules.dep as extensions; neither is installed software.
const (
	schematicName = "schematic"
	modulesDep    = "modules.dep"
)

// Collector reads the image and security resources from the COSI runtime namespace.
type Collector struct {
	// state returns the COSI state to query for a node; overridable in tests.
	state func(node *collector.NodeClient) state.CoreState

	// snap receives the typed view alongside the metrics. nil disables it.
	snap *snapshot.Store
}

// New returns the identity collector.
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

// Class implements collector.Collector.
func (c *Collector) Class() collector.Class { return collector.Inventory }

// DefaultTTL implements collector.DefaultTTLer.
func (c *Collector) DefaultTTL() time.Duration { return defaultTTL }

// Collect lists the four kinds; an empty list is not an error, only a failed RPC is.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	st := c.state(node)
	name := node.Node.Name

	extensions, err := collector.List(ctx, st, "ExtensionStatus", runtimeres.ExtensionStatusType)
	if err != nil {
		return err
	}
	cmdline, err := collector.List(ctx, st, "KernelCmdline", runtimeres.KernelCmdlineType)
	if err != nil {
		return err
	}
	booted, err := collector.List(ctx, st, "BootedEntry", runtimeres.BootedEntryType)
	if err != nil {
		return err
	}
	security, err := collector.List(ctx, st, "SecurityState", runtimeres.SecurityStateType)
	if err != nil {
		return err
	}

	view := snapshot.Identity{}
	if err := registerExtensions(name, extensions, reg, &view); err != nil {
		return err
	}
	if err := registerBoot(name, cmdline, booted, reg, &view); err != nil {
		return err
	}
	if err := registerSecurity(name, security, reg, &view); err != nil {
		return err
	}

	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.Identity = &view })
	}
	return nil
}

// list lists a whole kind from the node's COSI state.

// registerExtensions exports the extensions and gives the schematic its own family.
func registerExtensions(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Identity) error {
	ext := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_extension_info",
		Help: "Installed system extension (value 1), one series per extension. Includes the virtual `schematic` and `modules.dep` entries Talos reports alongside real extensions.",
	}, []string{"node", "extension", "version"})
	schematic := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_schematic_info",
		Help: "Image Factory schematic the node's image was built from (value 1). Absent on an image not built by the factory.",
	}, []string{"node", "schematic"})
	reg.MustRegister(ext, schematic)

	view.Extensions = make([]snapshot.Extension, 0, len(l.Items))
	for _, r := range l.Items {
		es, ok := r.(*runtimeres.ExtensionStatus)
		if !ok {
			return fmt.Errorf("unexpected resource %T in ExtensionStatus list", r)
		}
		md := es.TypedSpec().Metadata
		ext.WithLabelValues(nodeName, md.Name, md.Version).Set(1)

		switch md.Name {
		case schematicName:
			// The schematic's "version" is the schematic id itself.
			schematic.WithLabelValues(nodeName, md.Version).Set(1)
			view.Schematic = md.Version
		case modulesDep:
			// Synthetic: the combined module index, versioned by kernel.
			view.KernelVersion = md.Version
		default:
			view.Extensions = append(view.Extensions, snapshot.Extension{
				Name: md.Name, Version: md.Version,
			})
		}
	}
	sort.Slice(view.Extensions, func(i, j int) bool { return view.Extensions[i].Name < view.Extensions[j].Name })
	return nil
}

// registerBoot exports the cmdline as one label per node, comparable across the fleet.
func registerBoot(nodeName string, cmdlines, booted resource.List, reg prometheus.Registerer, view *snapshot.Identity) error {
	cmdlineG := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_kernel_cmdline_info",
		Help: "Kernel command line the node booted with (value 1), one series per node.",
	}, []string{"node", "cmdline"})
	bootedG := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_booted_entry_info",
		Help: "Boot entry the node came up on (value 1); the A/B slot, e.g. talos-v1.13.4~1.efi.",
	}, []string{"node", "entry"})
	reg.MustRegister(cmdlineG, bootedG)

	for _, r := range cmdlines.Items {
		kc, ok := r.(*runtimeres.KernelCmdline)
		if !ok {
			return fmt.Errorf("unexpected resource %T in KernelCmdline list", r)
		}
		view.Cmdline = kc.TypedSpec().Cmdline
		cmdlineG.WithLabelValues(nodeName, view.Cmdline).Set(1)
	}
	for _, r := range booted.Items {
		be, ok := r.(*runtimeres.BootedEntry)
		if !ok {
			return fmt.Errorf("unexpected resource %T in BootedEntry list", r)
		}
		view.BootedEntry = be.TypedSpec().BootedEntry
		bootedG.WithLabelValues(nodeName, view.BootedEntry).Set(1)
	}
	return nil
}

// registerSecurity exports the posture; SELinux and FIPS are multi-valued, so they are _info.
func registerSecurity(nodeName string, l resource.List, reg prometheus.Registerer, view *snapshot.Identity) error {
	b := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, []string{"node"})
	}
	secureBoot := b("talos_security_secure_boot", "1 when the node booted with SecureBoot enabled.")
	uki := b("talos_security_booted_with_uki", "1 when the node booted from a Unified Kernel Image.")
	modSig := b("talos_security_module_signature_enforced", "1 when the kernel refuses unsigned modules.")
	selinux := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_security_selinux_state_info",
		Help: "SELinux state (value 1): disabled, `enabled, permissive`, or `enabled, enforcing`.",
	}, []string{"node", "state"})
	fips := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_security_fips_state_info",
		Help: "FIPS state (value 1): disabled, enabled, or `enabled, strict`.",
	}, []string{"node", "state"})
	reg.MustRegister(secureBoot, uki, modSig, selinux, fips)

	for _, r := range l.Items {
		ss, ok := r.(*runtimeres.SecurityState)
		if !ok {
			return fmt.Errorf("unexpected resource %T in SecurityState list", r)
		}
		spec := ss.TypedSpec()
		secureBoot.WithLabelValues(nodeName).Set(collector.BoolValue(spec.SecureBoot))
		uki.WithLabelValues(nodeName).Set(collector.BoolValue(spec.BootedWithUKI))
		modSig.WithLabelValues(nodeName).Set(collector.BoolValue(spec.ModuleSignatureEnforced))
		selinux.WithLabelValues(nodeName, spec.SELinuxState.String()).Set(1)
		fips.WithLabelValues(nodeName, spec.FIPSState.String()).Set(1)

		view.HasSecurityState = true
		view.SecureBoot = spec.SecureBoot
		view.BootedWithUKI = spec.BootedWithUKI
		view.ModuleSignatureEnforced = spec.ModuleSignatureEnforced
		view.SELinuxState = spec.SELinuxState.String()
		view.FIPSState = spec.FIPSState.String()
	}
	return nil
}

var _ collector.Collector = (*Collector)(nil)
