package collector

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A role change must take effect without a restart: the gate pauses, re-probes after PermissionRetry, and clears on success.
func TestPermissionGateRetriesAndRecovers(t *testing.T) {
	var logBuf bytes.Buffer
	now := time.Unix(0, 0)
	g := NewPermissionGate(slog.New(slog.NewTextHandler(&logBuf, nil)), "widget reads", "").
		WithClock(func() time.Time { return now })

	if g.Skip() || g.Denied() {
		t.Fatal("a fresh gate is not open")
	}
	denied := errors.New("permission denied")
	g.Trip("n1", denied)
	g.Trip("n2", denied)
	if !g.Skip() || !g.Denied() {
		t.Fatal("a tripped gate must skip and report denied")
	}
	if n := strings.Count(logBuf.String(), "disabled"); n != 1 {
		t.Errorf("got %d disable warnings for one outage, want 1", n)
	}

	now = now.Add(PermissionRetry)
	if g.Skip() {
		t.Error("the gate did not let a probe through after PermissionRetry")
	}
	if !g.Denied() {
		t.Error("a pending probe cleared the degradation before any read succeeded")
	}

	g.Trip("n1", denied) // probe refused: pause again
	if !g.Skip() {
		t.Error("a refused probe did not restart the pause")
	}

	now = now.Add(PermissionRetry)
	g.Clear("n1")
	if g.Skip() || g.Denied() {
		t.Error("a successful read did not re-enable the gate")
	}
	if !strings.Contains(logBuf.String(), "re-enabled") {
		t.Error("recovery was not logged")
	}
}
