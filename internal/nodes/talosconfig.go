package nodes

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// TalosConfig holds the parsed talosconfig used to talk to node APIs.
//
// Resolution is mandatory-path-first: in-cluster the chart sets
// $TALOSCONFIG to the mounted serviceaccounts.talos.dev secret (Talos's
// built-in apps & in-cluster authentication); locally the talosctl default
// ~/.talos/config is used. Machinery's default path rules provide the same
// order, including the service-account mount path as the last in-cluster
// fallback.
type TalosConfig struct {
	Path         string
	ContextName  string
	Fingerprint  [sha256.Size]byte // raw file content; detects rotation/edits
	CertNotAfter time.Time
	context      *talosconfig.Context
}

// Context returns the parsed machinery config context (TLS material).
func (t TalosConfig) Context() *talosconfig.Context {
	return t.context
}

// LoadTalosConfig reads the talosconfig using the standard resolution
// order. It never creates or modifies files.
func LoadTalosConfig() (TalosConfig, error) {
	paths, err := talosconfig.GetDefaultPaths()
	if err != nil {
		return TalosConfig{}, fmt.Errorf("talosconfig paths: %w", err)
	}

	var (
		raw  []byte
		used string
	)
	for _, p := range paths {
		b, err := os.ReadFile(p.Path)
		if err == nil {
			raw, used = b, p.Path
			break
		}
	}
	if raw == nil {
		seen := make([]string, 0, len(paths))
		for _, p := range paths {
			seen = append(seen, p.Path)
		}
		return TalosConfig{}, fmt.Errorf("no readable talosconfig (looked at: %s)", strings.Join(seen, ", "))
	}

	cfg, err := talosconfig.FromBytes(raw)
	if err != nil {
		return TalosConfig{}, fmt.Errorf("parse talosconfig %s: %w", used, err)
	}
	ctx, ok := cfg.Contexts[cfg.Context]
	if !ok {
		return TalosConfig{}, fmt.Errorf("talosconfig %s: context %q not found", used, cfg.Context)
	}

	tc := TalosConfig{
		Path:        used,
		ContextName: cfg.Context,
		Fingerprint: sha256.Sum256(raw),
		context:     ctx,
	}
	if notAfter, err := clientCertExpiry(ctx.Crt); err != nil {
		return TalosConfig{}, fmt.Errorf("talosconfig %s: client certificate: %w", used, err)
	} else {
		tc.CertNotAfter = notAfter
	}
	return tc, nil
}

func clientCertExpiry(crtB64 string) (time.Time, error) {
	crt, err := base64.StdEncoding.DecodeString(crtB64)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode: %w", err)
	}
	block, _ := pem.Decode(crt)
	if block == nil {
		return time.Time{}, errors.New("no PEM block")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse: %w", err)
	}
	return parsed.NotAfter, nil
}

// inCluster reports whether we appear to run inside a Kubernetes pod.
func inCluster() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}
