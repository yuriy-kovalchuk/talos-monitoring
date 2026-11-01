package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
	"google.golang.org/grpc/codes"
)

// BoolValue converts a boolean to the 0/1 a Prometheus gauge expects.
func BoolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// IsPermissionError matches the gRPC status and the substring: wrapping can drop the code.
func IsPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if talosclient.StatusCode(err) == codes.PermissionDenied {
		return true
	}
	return strings.Contains(err.Error(), "permission denied")
}

// FileReader is any Talos API client that can read a remote file.
type FileReader interface {
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// ReadFile reads a whole file through the Talos API; the 1 MiB cap is a safety ceiling, not a real limit.
func ReadFile(ctx context.Context, api FileReader, path string) (string, error) {
	rc, err := api.Read(ctx, path)
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(rc, 1<<20))
	closeErr := rc.Close()
	if readErr != nil {
		return "", fmt.Errorf("read %s: %w", path, readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close %s: %w", path, closeErr)
	}
	return string(data), nil
}

// ReadFileTrimmed reads a sysfs/procfs file and trims the trailing newline every one has.
func ReadFileTrimmed(ctx context.Context, api FileReader, path string) (string, error) {
	s, err := ReadFile(ctx, api, path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// adminRoleHint is the remediation every file-based collector owes an operator who hit a permission error.
const adminRoleHint = "give the monitor ServiceAccount the os:admin role (Helm value talos.roles)"

// PermissionRetry is how long a tripped gate skips its reads before probing again: roles change without a restart.
const PermissionRetry = 5 * time.Minute

// PermissionGate is the shared os:admin tripwire: a permission error pauses those reads cluster-wide until a probe succeeds.
type PermissionGate struct {
	subject  string // what gets switched off, so the log names what the operator lost
	hintTail string // appended to the role hint: what keeps being reported
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	denied  bool
	retryAt time.Time
}

// NewPermissionGate builds a gate whose warning names the subject and the os:admin requirement.
func NewPermissionGate(log *slog.Logger, subject, hintTail string) *PermissionGate {
	return &PermissionGate{subject: subject, hintTail: hintTail, log: log, now: time.Now}
}

// WithClock replaces the gate's clock (test seam for the retry window).
func (g *PermissionGate) WithClock(now func() time.Time) *PermissionGate {
	g.now = now
	return g
}

// Trip records a permission error and pauses the reads for PermissionRetry, warning only on the transition.
func (g *PermissionGate) Trip(node string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.denied {
		g.log.Warn(g.subject+" disabled: file reads need the os:admin role (got a permission error)",
			"node", node, "err", err, "retry", PermissionRetry, "hint", adminRoleHint+g.hintTail)
	}
	g.denied, g.retryAt = true, g.now().Add(PermissionRetry)
}

// Clear records a successful gated read, re-enabling the reads after a trip.
func (g *PermissionGate) Clear(node string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.denied {
		g.log.Info(g.subject+" re-enabled: file reads are permitted again", "node", node)
	}
	g.denied = false
}

// Skip reports whether to skip the gated reads this round: denied and the next probe is not yet due.
func (g *PermissionGate) Skip() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.denied && g.now().Before(g.retryAt)
}

// Denied reports whether the last gated read was refused; it stays true through a probe until one succeeds.
func (g *PermissionGate) Denied() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.denied
}

// GaugeVec builds a labeled gauge family; the label slice is easy to get wrong by hand.
func GaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// DefaultState is the per-node COSI state every COSI collector uses; tests fake it.
func DefaultState(node *NodeClient) state.CoreState {
	return node.Client.COSI
}

// List reads every resource of kind in the runtime namespace, wrapping the error with the collector's own name for it.
func List(ctx context.Context, st state.CoreState, name string, kind resource.Type) (resource.List, error) {
	return ListIn(ctx, st, v1alpha1.NamespaceName, name, kind)
}

// ListIn is List for another namespace; hardware.* resources live in their own and list empty anywhere else.
func ListIn(ctx context.Context, st state.CoreState, namespace resource.Namespace, name string, kind resource.Type) (resource.List, error) {
	md := resource.NewMetadata(namespace, kind, "", resource.VersionUndefined)
	l, err := st.List(ctx, md)
	if err != nil {
		return resource.List{}, fmt.Errorf("list %s: %w", name, err)
	}
	return l, nil
}
