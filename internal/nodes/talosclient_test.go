package nodes

import (
	"context"
	"net/url"
	"testing"
)

// newPoolWithConfig builds a pool with a loaded test talosconfig.
func newPoolWithConfig(t *testing.T, listNodes func() []Node) *TalosClientPool {
	t.Helper()
	home := isolateTalosEnv(t)
	ca, crt, key := genCAAndClient(t)
	writeTalosconfig(t, home, ca, crt, key)

	cfg, err := LoadTalosConfig()
	if err != nil {
		t.Fatal(err)
	}
	p := NewTalosClientPool(testLog, listNodes)
	p.setConfig(&cfg)
	t.Cleanup(p.closeAll)
	return p
}

func TestPoolGetCaches(t *testing.T) {
	p := newPoolWithConfig(t, func() []Node { return nil })

	c1, err := p.Get("10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.Get("10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Error("expected the cached client for the same IP")
	}
	c3, err := p.Get("10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if c3 == c1 {
		t.Error("expected a different client for a different IP")
	}
}

func TestPoolGetBeforeConfig(t *testing.T) {
	p := NewTalosClientPool(testLog, func() []Node { return nil })
	if _, err := p.Get("10.0.0.1"); err == nil {
		t.Error("expected error before the talosconfig is loaded")
	}
}

func TestReconcileClosesStaleAndRetries(t *testing.T) {
	current := []Node{{Name: "n1", IP: "127.0.0.1"}}
	p := newPoolWithConfig(t, func() []Node { return current })

	// No live apid on 127.0.0.1: client is created, check fails, not verified.
	p.reconcile(t.Context())
	if _, up := p.Status("n1"); up {
		t.Error("node must not be verified while its Talos API is unreachable")
	}
	first, err := p.Get("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	// n1 disappears, n2 appears; the stale client must be closed.
	current = []Node{{Name: "n2", IP: "127.0.0.2"}}
	p.reconcile(t.Context())

	again, err := p.Get("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if again == first {
		t.Error("stale client was not closed after node removal")
	}
}

func TestPoolReadyReflectsConfig(t *testing.T) {
	p := NewTalosClientPool(testLog, func() []Node { return nil })
	if p.Ready() {
		t.Error("pool must not be ready before the talosconfig is loaded")
	}
	p = newPoolWithConfig(t, func() []Node { return nil })
	if !p.Ready() {
		t.Error("pool must be ready once the talosconfig is loaded")
	}
}

func TestReconcileRebuildsOnConfigChange(t *testing.T) {
	home := isolateTalosEnv(t)
	ca, crt, key := genCAAndClient(t)
	writeTalosconfig(t, home, ca, crt, key)
	cfg, err := LoadTalosConfig()
	if err != nil {
		t.Fatal(err)
	}

	p := NewTalosClientPool(testLog, func() []Node { return []Node{{Name: "n1", IP: "127.0.0.1"}} })
	p.setConfig(&cfg)
	t.Cleanup(p.closeAll)

	first, err := p.Get("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate rotation: the file is replaced (new fingerprint).
	ca2, crt2, key2 := genCAAndClient(t)
	writeTalosconfig(t, home, ca2, crt2, key2)

	p.reconcile(t.Context())

	again, err := p.Get("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if again == first {
		t.Error("clients were not rebuilt after the talosconfig changed")
	}
}

func TestAPIDEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, ip, want string
	}{
		{"ipv4", "10.0.0.1", "https://10.0.0.1:50000"},
		{"ipv6", "fd00::1", "https://[fd00::1]:50000"},
		{"ipv6 loopback", "::1", "https://[::1]:50000"},
		{"ipv6 full", "2001:db8:0:1:1:1:1:1", "https://[2001:db8:0:1:1:1:1:1]:50000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := apidEndpoint(tc.ip)
			if got != tc.want {
				t.Fatalf("apidEndpoint(%q) = %q, want %q", tc.ip, got, tc.want)
			}
			if _, err := url.Parse(got); err != nil {
				t.Fatalf("apidEndpoint(%q) = %q is unparseable: %v", tc.ip, got, err)
			}
		})
	}
}

// TestReVerifiesWhenTheIPChanges: clients are keyed by IP but verification by
// name, so a node that keeps its name and moves IP had its old client closed
// while its verification stayed valid — and reported its predecessor's Talos
// version indefinitely.
func TestReVerifiesWhenTheIPChanges(t *testing.T) {
	p := newPoolWithConfig(t, func() []Node { return []Node{{Name: "n1", IP: "10.0.0.2"}} })
	p.markVerified("n1", "10.0.0.1", "v1.13.4")
	if v, up := p.Status("n1"); !up || v != "v1.13.4" {
		t.Fatalf("precondition: got %q %v", v, up)
	}

	p.reconcile(context.Background())

	// The node moved, so the cached version must be discarded rather than
	// attributed to the new machine.
	if v, up := p.Status("n1"); up || v != "" {
		t.Errorf("after the IP moved: got %q up=%v, want unverified", v, up)
	}
}

// TestKeepsVerificationWhenNothingMoved guards the other direction: a stable
// node must not be re-verified on every reconcile.
func TestKeepsVerificationWhenNothingMoved(t *testing.T) {
	p := newPoolWithConfig(t, func() []Node { return []Node{{Name: "n1", IP: "10.0.0.1"}} })
	p.markVerified("n1", "10.0.0.1", "v1.13.4")
	p.reconcile(context.Background())
	if v, up := p.Status("n1"); !up || v != "v1.13.4" {
		t.Errorf("stable node lost its verification: got %q up=%v", v, up)
	}
}
