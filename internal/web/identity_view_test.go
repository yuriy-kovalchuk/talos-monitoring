package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// The schematic id is 64 hex characters and differs only somewhere in the
// middle. The page shows the git-length short form an operator can actually
// compare, and keeps the full value on the page for copying.
func TestIdentityPageShortensTheSchematic(t *testing.T) {
	const id = "65cf8364cd0de4cf7b851dc7067a2db83d0ba04f11d8635c6cd3334be6ffb825"
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{
			Schematic:     id,
			KernelVersion: "6.18.34-talos",
			BootedEntry:   "talos-v1.13.4~1.efi",
			Extensions: []snapshot.Extension{
				{Name: "amd-ucode", Version: "20260519"},
				{Name: "iscsi-tools", Version: "v0.2.0"},
			},
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/identity")

	checkBody(t, body, "65cf8364cd0d", id, "talos-v1.13.4~1.efi",
		"kernel 6.18.34-talos", "amd-ucode", "iscsi-tools", "2 extensions")
}

// SecureBoot=false and "no SecurityState resource" render differently: the
// first is a real answer, the second is an absence.
func TestIdentityPageDistinguishesAbsentSecurityState(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{Schematic: "65cf8364cd0de4cf"}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/identity")

	checkBody(t, body, "not reported")
	if strings.Contains(body, "Security posture") {
		t.Error("security posture card rendered with no SecurityState")
	}
	// No extensions is a fact worth stating, not an empty table.
	checkBody(t, body, "No system extensions on this node")
}

func TestIdentityPageRendersSecurityPosture(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Identity = &snapshot.Identity{
			HasSecurityState:        true,
			SecureBoot:              false,
			BootedWithUKI:           true,
			ModuleSignatureEnforced: true,
			SELinuxState:            "enabled, permissive",
			FIPSState:               "disabled",
			Cmdline:                 "talos.platform=metal selinux=1 module.sig_enforce=1",
		}
	})
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1/identity")

	checkBody(t, body, "Security posture", "enabled, permissive", "enforced")
	// The kernel command line moved to its own page.
	if strings.Contains(body, "module.sig_enforce=1") {
		t.Error("kernel cmdline still rendered on the Image page")
	}
}

func TestIdentityPageWithoutDataRendersEmptyState(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/nodes/node-1/identity")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/identity: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "No image data yet")
}
