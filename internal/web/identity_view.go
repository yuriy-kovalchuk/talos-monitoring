package web

import (
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// extensionRow is one installed system extension on the identity page.
type extensionRow struct {
	Name    string
	Version string
}

// identityView is the identity page; SchematicShort is the 12-char id operators compare.
type identityView struct {
	Available      bool
	Schematic      string
	SchematicShort string
	KernelVersion  string
	BootedEntry    string
	Cmdline        string
	Extensions     []extensionRow

	HasSecurity             bool
	SecureBoot              bool
	BootedWithUKI           bool
	ModuleSignatureEnforced bool
	SELinuxState            string
	FIPSState               string
}

// identityFrom builds the identity page from the snapshot.
func identityFrom(n *snapshot.Node) identityView {
	if n == nil || n.Identity == nil {
		return identityView{}
	}
	id := n.Identity
	view := identityView{
		Available:               true,
		Schematic:               id.Schematic,
		SchematicShort:          shortHash(id.Schematic),
		KernelVersion:           id.KernelVersion,
		BootedEntry:             id.BootedEntry,
		Cmdline:                 id.Cmdline,
		HasSecurity:             id.HasSecurityState,
		SecureBoot:              id.SecureBoot,
		BootedWithUKI:           id.BootedWithUKI,
		ModuleSignatureEnforced: id.ModuleSignatureEnforced,
		SELinuxState:            id.SELinuxState,
		FIPSState:               id.FIPSState,
	}
	for _, e := range id.Extensions {
		view.Extensions = append(view.Extensions, extensionRow{Name: e.Name, Version: e.Version})
	}
	return view
}
