package collector

import (
	"context"
	"fmt"
	"io"
	"strings"

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

// IsPermissionError reports whether err is the Talos API's permission-denied
// response. Checked both as a gRPC status and as a substring, because a
// wrapped file-read error can lose the status code along the way.
func IsPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if talosclient.StatusCode(err) == codes.PermissionDenied {
		return true
	}
	return strings.Contains(err.Error(), "permission denied")
}

// FileReader is satisfied by any Talos API client that can read a remote
// file (the machine API's file-read RPC).
type FileReader interface {
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// ReadFile reads a whole file through the Talos API. The 1 MiB cap is a
// safety ceiling, not a real limit — every sysfs/procfs file this project
// reads is a few bytes to a few KB.
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

// GaugeVec builds a labeled gauge family. A thin wrapper, but every
// collector needs it and the label slice is easy to get wrong by hand.
func GaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// DefaultState returns the per-node COSI state of the Talos client — what
// every COSI-based collector's `state` field points at by default; tests
// override the field with a fake state instead of calling this.
func DefaultState(node *NodeClient) state.CoreState {
	return node.Client.COSI
}

// List reads every resource of kind from st, wrapped with an error naming
// the collector's own name for it.
func List(ctx context.Context, st state.CoreState, name string, kind resource.Type) (resource.List, error) {
	md := resource.NewMetadata(v1alpha1.NamespaceName, kind, "", resource.VersionUndefined)
	l, err := st.List(ctx, md)
	if err != nil {
		return resource.List{}, fmt.Errorf("list %s: %w", name, err)
	}
	return l, nil
}
