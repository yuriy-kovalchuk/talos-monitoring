package metricguard_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// notYetWired are collectors whose tests do not run the guard yet. Each needs a
// test helper that returns both the gathered families and the snapshot section
// the collector wrote, which the packages below do not have.
//
// This is a list to shrink, not a place to add to: a NEW collector must wire
// the guard, and this test is what makes forgetting impossible.
var notYetWired = map[string]bool{
	"block":   true,
	"cpu":     true,
	"hwinfo":  true,
	"nodeapi": true,
	"sensors": true,
	"sysstat": true, // shares a SystemStat response; writes no snapshot section
}

// TestEveryCollectorRunsTheGuard fails when a collector package writes to the
// snapshot but its tests never call metricguard.
//
// The guard is opt-in per collector, which is the same shape of problem it
// exists to solve: something correct that a person has to remember. This turns
// remembering into a failing test.
func TestEveryCollectorRunsTheGuard(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "collectors")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read collectors dir: %v", err)
	}

	var missing, staleAllowlist []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		files, err := filepath.Glob(filepath.Join(dir, name, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", name, err)
		}
		var writesSnapshot, runsGuard bool
		for _, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // walking our own source tree
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			src := string(b)
			if strings.HasSuffix(f, "_test.go") {
				runsGuard = runsGuard || strings.Contains(src, "metricguard.")
				continue
			}
			writesSnapshot = writesSnapshot || strings.Contains(src, "snapshot.Node)")
		}
		switch {
		case writesSnapshot && !runsGuard && !notYetWired[name]:
			missing = append(missing, name)
		case runsGuard && notYetWired[name]:
			staleAllowlist = append(staleAllowlist, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(staleAllowlist)

	for _, n := range missing {
		t.Errorf("collector %q writes to the snapshot but its tests never run metricguard: "+
			"add a TestMetricContractAndSnapshotCoverage (see collectors/volumes)", n)
	}
	for _, n := range staleAllowlist {
		t.Errorf("collector %q now runs the guard; remove it from notYetWired", n)
	}
}
