package collector

import (
	"testing"
)

func TestRegistryDuplicateName(t *testing.T) {
	r := NewRegistry()
	a := &fakeCollector{name: "dup"}
	b := &fakeCollector{name: "dup"}
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(b); err == nil {
		t.Fatal("expected error on duplicate collector name")
	}
}

func TestRegistrySetEnabled(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"hwinfo", "block", "nodeapi"} {
		if err := r.Register(&fakeCollector{name: name}); err != nil {
			t.Fatal(err)
		}
	}

	// Default: all enabled.
	if got := r.Enabled(); len(got) != 3 {
		t.Fatalf("expected 3 enabled collectors, got %d", len(got))
	}

	// nil re-enables everything.
	r.SetEnabled(nil)
	if got := r.Enabled(); len(got) != 3 {
		t.Fatalf("expected 3 enabled collectors, got %d", len(got))
	}

	// An explicit false disables; a missing name stays enabled.
	r.SetEnabled(map[string]bool{"block": false})
	if got := r.Enabled(); len(got) != 2 || got[0].Name() != "hwinfo" || got[1].Name() != "nodeapi" {
		t.Fatalf("unexpected enabled set: %v", namesOf(got))
	}

	// A name that was never registered is skipped, not an error.
	r.SetEnabled(map[string]bool{"nope": false})
	if got := r.Enabled(); len(got) != 3 {
		t.Fatalf("unregistered name must be skipped, got %v", namesOf(got))
	}

	// All false is an explicit off.
	r.SetEnabled(map[string]bool{"block": false, "hwinfo": false, "nodeapi": false})
	if got := r.Enabled(); len(got) != 0 {
		t.Fatalf("expected no enabled collectors, got %v", namesOf(got))
	}
}

func TestRegistryNames(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeCollector{name: "zeta"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&fakeCollector{name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	got := r.Names()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("expected [alpha zeta], got %v", got)
	}
}

func namesOf(cs []Collector) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name())
	}
	return out
}
