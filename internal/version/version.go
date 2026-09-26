// Package version carries build-time metadata injected via -ldflags:
//
//	-X .../internal/version.Version=...
//	-X .../internal/version.Commit=...
//	-X .../internal/version.BuildDate=...
package version

import "fmt"

var (
	// Version is the application version (e.g. git describe: v0.1.0 or v0.1.0-2-g6d66e63).
	Version = "dev"
	// Commit is the short git commit hash.
	Commit = "unknown"
	// BuildDate is the UTC build timestamp (RFC3339).
	BuildDate = "unknown"
)

// String returns a human-readable build line.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, BuildDate)
}
