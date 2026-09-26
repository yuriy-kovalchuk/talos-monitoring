// Package web exposes the HTTP endpoints: /metrics, /health, /ready and
// the dashboard UI (templates and assets embedded from static/).
package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/version"
	"github.com/yuriy-kovalchuk/talos-monitoring/static"
)

// NewRouter builds the HTTP handler tree.
func NewRouter(log *slog.Logger, reg prometheus.Gatherer, ready func() bool, listNodes func() []nodes.Node, nodeStatus func(name string) (string, bool), hist *history.Store, snap *snapshot.Store,
	volumes func() map[string]k8svolumes.Volume) http.Handler {
	mux := http.NewServeMux()
	d := &dashboard{log: log, list: listNodes, status: nodeStatus, gather: reg, hist: hist, snap: snap, volumes: volumes}

	mux.Handle("GET /health", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	mux.Handle("GET /ready", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	// Dashboard
	mux.Handle("GET /", http.HandlerFunc(d.overview))
	mux.Handle("GET /nodes", http.HandlerFunc(d.nodesPage))
	mux.Handle("GET /nodes/{name}", http.HandlerFunc(d.nodePage))
	mux.Handle("GET /nodes/{name}/{section}", http.HandlerFunc(d.nodeSection))
	mux.Handle("GET /about", http.HandlerFunc(d.about))
	mux.Handle("GET /partials/status", http.HandlerFunc(d.statusPartial))
	mux.Handle("GET /partials/overview", http.HandlerFunc(d.overviewPartial))
	mux.Handle("GET /partials/nodes/{name}/{card}", http.HandlerFunc(d.nodePartial))

	// Static assets, embedded in the binary. Fonts are vendored rather than
	// pulled from Google's CDN so the dashboard renders on a cluster with no
	// egress (finding 5.4).
	for _, dir := range []string{"css", "js", "fonts"} {
		sub, err := fs.Sub(static.FS, dir)
		if err != nil {
			panic(err)
		}
		prefix := "/static/" + dir + "/"
		mux.Handle("GET "+prefix, http.StripPrefix(prefix, cacheStatic(http.FileServerFS(sub))))
	}

	return securityHeaders(logRequests(log, mux))
}

// csp is deliberately strict: every asset is served from this binary, so
// nothing needs to load from anywhere else. 'unsafe-inline' covers the style
// attributes the templates use for bar widths; there is no inline script.
const csp = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'"

// securityHeaders hardens the unauthenticated surface. The dashboard and
// /metrics carry chassis serials, system UUIDs and disk WWIDs and are meant to
// be cluster-internal (network isolation is the operator's responsibility),
// but headers cost nothing and remove the easy problems: MIME sniffing,
// framing, referrer leaks.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("Content-Security-Policy", csp)
		h.ServeHTTP(w, r)
	})
}

// staticMaxAge is how long a browser may reuse an asset without asking. The
// assets are embedded in the binary, so they only change when the binary does,
// and the ETag below is derived from the build so a new version invalidates
// them immediately.
const staticMaxAge = 24 * time.Hour

// assetsETag hashes the embedded asset tree once, at startup.
//
// It used to be the build identity (version + commit), on the reasoning that
// assets are baked into the binary and change together. That is true of a
// release, and false of every development build: editing app.js without
// committing leaves the commit — and so the ETag — unchanged, while
// Cache-Control says the browser may reuse the old file for a day. That is
// exactly how a stale app.js kept the node switcher from working after its
// handler moved out of the markup.
//
// Hashing the content cannot get this wrong, and costs one walk of a few
// hundred kilobytes at boot.
// assetVersion is computed once: hashing the tree on every request would be
// pointless work for a value that cannot change while the process runs.
var assetVersion = sync.OnceValue(assetsETag)

// AssetVersion identifies the embedded asset tree. Templates append it to
// asset URLs so a new build cannot be served from a stale cache, and
// cacheStatic uses it as the ETag.
func AssetVersion() string { return assetVersion() }

func assetsETag() string {
	h := sha256.New()
	err := fs.WalkDir(static.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := static.FS.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write(b)
		return nil
	})
	if err != nil {
		// Unreachable for an embedded FS; fall back to the build identity
		// rather than serving no validator at all.
		return `"` + version.Version + "-" + version.Commit + `"`
	}
	return `"` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
}

// cacheStatic adds validators to the embedded assets. Without them every page
// load re-fetched the full htmx bundle,
// plus the fonts.
func cacheStatic(h http.Handler) http.Handler {
	etag := AssetVersion()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(staticMaxAge.Seconds())))
		w.Header().Set("ETag", etag)
		// If-None-Match is a comma-separated list, and each entry may carry a
		// weak validator prefix. A substring test would also match a tag that
		// merely contains ours.
		for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
			candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
			if candidate == etag || candidate == "*" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start).String())
	})
}
