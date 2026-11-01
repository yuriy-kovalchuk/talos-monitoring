package web_test

import (
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// An unsupported kernel parameter is silently ignored by the kernel, so the
// page has to say so explicitly.
func TestKernelPageFlagsUnsupportedParams(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{
			Schematic:     "65cf8364cd0de4cf",
			KernelVersion: "6.18.34-talos",
			Cmdline:       "talos.platform=metal selinux=1 module.sig_enforce=1",
		}
		n.Kernel = &snapshot.Kernel{
			Params: []snapshot.KernelParam{
				{Name: "proc.sys.fs.aio-max-nr", Current: "1048576", Default: "65536"},
				{Name: "proc.sys.net.core.typo", Unsupported: true},
			},
			Modules: []snapshot.KernelModule{
				{Name: "amdgpu", State: "live", SizeBytes: 16965632, ReferenceCount: 2},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/kernel")

	checkBody(t, body, "unsupported: the running kernel ignores this",
		"1 UNSUPPORTED", "1 differ from default", "amdgpu", "1 module",
		"6.18.34-talos", "module.sig_enforce=1")
}
