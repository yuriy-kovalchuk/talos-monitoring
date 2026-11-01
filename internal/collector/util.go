package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"

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

// PermissionGate is the shared os:admin tripwire: the first permission error disables those reads for the process.
type PermissionGate struct {
	subject  string // what gets switched off, so the log names what the operator lost
	hintTail string // appended to the role hint: what keeps being reported
	log      *slog.Logger
	off      atomic.Bool
}

// NewPermissionGate builds a gate whose single warning names the subject and the os:admin requirement.
func NewPermissionGate(log *slog.Logger, subject, hintTail string) *PermissionGate {
	return &PermissionGate{subject: subject, hintTail: hintTail, log: log}
}

// Trip switches the gate off, logging once on the first call only.
func (g *PermissionGate) Trip(node string, err error) {
	if g.off.CompareAndSwap(false, true) {
		g.log.Warn(g.subject+": file reads need the os:admin role (got a permission error)",
			"node", node, "err", err, "hint", adminRoleHint+g.hintTail)
	}
}

// Off reports whether the gated file reads are switched off.
func (g *PermissionGate) Off() bool { return g.off.Load() }

// GaugeVec builds a labeled gauge family; the label slice is easy to get wrong by hand.
func GaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// DefaultState is the per-node COSI state every COSI collector uses; tests fake it.
func DefaultState(node *NodeClient) state.CoreState {
	return node.Client.COSI
}

// List reads every resource of kind, wrapping the error with the collector's own name for it.
func List(ctx context.Context, st state.CoreState, name string, kind resource.Type) (resource.List, error) {
	md := resource.NewMetadata(v1alpha1.NamespaceName, kind, "", resource.VersionUndefined)
	l, err := st.List(ctx, md)
	if err != nil {
		return resource.List{}, fmt.Errorf("list %s: %w", name, err)
	}
	return l, nil
}
