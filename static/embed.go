// Package static embeds the dashboard frontend assets: HTML templates,
// CSS and JS. Everything the UI needs lives under this directory and is
// loaded (embedded) from here into the single binary.
package static

import "embed"

// FS holds the dashboard assets (css/, js/, html/).
//
//go:embed all:css all:js all:html all:fonts
var FS embed.FS
