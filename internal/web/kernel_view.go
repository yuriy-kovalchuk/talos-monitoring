package web

import (
	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// kernelParamRow is one tunable; Drifted is the column an operator scans first.
type kernelParamRow struct {
	Name        string
	Current     string
	Default     string
	Drifted     bool
	Unsupported bool
}

// kernelModuleRow is one loaded module.
type kernelModuleRow struct {
	Name  string
	State string
	Size  string
	Refs  int
}

// kernelView is the kernel block on the identity page.
type kernelView struct {
	Available   bool
	Params      []kernelParamRow
	Modules     []kernelModuleRow
	Unsupported int
	Drifted     int
}

// kernelFrom builds the kernel block from the snapshot.
func kernelFrom(n *snapshot.Node) kernelView {
	if n == nil || n.Kernel == nil {
		return kernelView{}
	}
	view := kernelView{Available: true}
	for _, p := range n.Kernel.Params {
		drifted := p.Current != p.Default
		if drifted {
			view.Drifted++
		}
		if p.Unsupported {
			view.Unsupported++
		}
		view.Params = append(view.Params, kernelParamRow{
			Name: p.Name, Current: p.Current, Default: p.Default,
			Drifted: drifted, Unsupported: p.Unsupported,
		})
	}
	for _, m := range n.Kernel.Modules {
		view.Modules = append(view.Modules, kernelModuleRow{
			Name: m.Name, State: m.State, Size: humanize.IBytes(uint64(m.SizeBytes)), Refs: m.ReferenceCount,
		})
	}
	return view
}
