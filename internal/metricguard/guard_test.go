package metricguard_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
)

// gather registers everything and returns the families, the way a collector's
// test does.
func gather(t *testing.T, cs ...prometheus.Collector) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(cs...)
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return fams
}

func gaugeVec(name, help string, labels []string, values ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	g.WithLabelValues(values...).Set(1)
	return g
}

// A guard nobody tests is a guard nobody can trust. Each case is a rule the
// project actually broke at some point.
func TestValidateCatchesEachRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		coll prometheus.Collector
		want string
	}{
		{
			name: "counter without _total",
			coll: func() prometheus.Collector {
				c := prometheus.NewCounterVec(prometheus.CounterOpts{
					Name: "talos_thing_bytes", Help: "h"}, []string{"node"})
				c.WithLabelValues("n1").Add(1)
				return c
			}(),
			want: "does not end in _total",
		},
		{
			name: "_total that is not a counter",
			coll: gaugeVec("talos_thing_total", "h", []string{"node"}, "n1"),
			want: "name ends in _total but type is",
		},
		{
			name: "_info carrying a measurement",
			coll: func() prometheus.Collector {
				g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
					Name: "talos_thing_info", Help: "h"}, []string{"node"})
				g.WithLabelValues("n1").Set(42)
				return g
			}(),
			want: "_info family carries the value 42",
		},
		{
			name: "unit in a label name",
			coll: gaugeVec("talos_thing", "h", []string{"node", "bytes"}, "n1", "5"),
			want: `label "bytes" is a unit`,
		},
		{
			name: "missing node label",
			coll: gaugeVec("talos_thing", "h", []string{"other"}, "x"),
			want: "no `node` label",
		},
		{
			name: "no help text",
			coll: gaugeVec("talos_thing", "", []string{"node"}, "n1"),
			want: "no HELP text",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := metricguard.Validate(gather(t, tc.coll), metricguard.Options{})
			if !containsSub(got, tc.want) {
				t.Errorf("Validate did not report %q; got %v", tc.want, got)
			}
		})
	}
}

func TestValidateAcceptsAConformingFamily(t *testing.T) {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "talos_thing_read_bytes_total", Help: "Bytes read."}, []string{"node", "device"})
	c.WithLabelValues("n1", "sda").Add(1)
	if got := metricguard.Validate(gather(t, c), metricguard.Options{}); len(got) != 0 {
		t.Errorf("a conforming family was reported: %v", got)
	}
}

// Cluster-scoped families (talos_pv_*) legitimately carry no node label.
func TestClusterScopedFamiliesAreExempt(t *testing.T) {
	g := gaugeVec("talos_pv_info", "PV identity.", []string{"pv"}, "pvc-1")
	opts := metricguard.Options{ClusterScoped: map[string]bool{"talos_pv_info": true}}
	if got := metricguard.Validate(gather(t, g), opts); len(got) != 0 {
		t.Errorf("an exempt family was reported: %v", got)
	}
}

type section struct {
	Name   string
	Nested []struct{ Phase string }
	Ptr    *struct{ Deep string }
}

// §1.1: a string in the snapshot with no matching label means the UI can show
// something Prometheus cannot. The walk must reach nested structs, slices and
// pointers — the real bug was a string inside a slice of structs.
func TestSnapshotCoveredWalksNestedValues(t *testing.T) {
	s := section{Name: "covered"}
	s.Nested = append(s.Nested, struct{ Phase string }{Phase: "uncovered-in-slice"})
	s.Ptr = &struct{ Deep string }{Deep: "uncovered-behind-pointer"}

	fams := gather(t, gaugeVec("talos_thing_info", "h", []string{"node", "name"}, "n1", "covered"))
	got := metricguard.SnapshotCovered(s, fams, metricguard.Options{})

	if len(got) != 2 {
		t.Fatalf("expected both uncovered strings, got %v", got)
	}
	if !containsSub(got, "Nested.Phase") || !containsSub(got, "Ptr.Deep") {
		t.Errorf("walk missed a nested or pointed-to field: %v", got)
	}
	// Check the field, not the value: "uncovered-in-slice" contains the
	// substring "covered".
	if containsSub(got, "field Name") {
		t.Error("a string that IS a label value was reported")
	}
}

// Empty strings are absence, not a violation, and the allowlist silences a
// field with a documented reason.
func TestSnapshotCoveredSkipsEmptyAndAllowlisted(t *testing.T) {
	s := section{Name: ""}
	s.Nested = append(s.Nested, struct{ Phase string }{Phase: "deliberate"})
	fams := gather(t, gaugeVec("talos_thing_info", "h", []string{"node"}, "n1"))

	opts := metricguard.Options{UncoveredStrings: map[string]bool{"Nested.Phase": true}}
	if got := metricguard.SnapshotCovered(s, fams, opts); len(got) != 0 {
		t.Errorf("expected no findings, got %v", got)
	}
}

func containsSub(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
