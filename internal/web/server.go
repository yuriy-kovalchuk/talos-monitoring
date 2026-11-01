// Package web serves /metrics, /health, /ready and the dashboard from embedded assets.
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

	// Fonts are vendored, not from a CDN, so the dashboard renders on a cluster with no egress.
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

// Every asset comes from this binary, so the CSP needs no external source and there is no inline script.
const csp = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'"

// securityHeaders hardens an unauthenticated, cluster-internal surface; isolation stays the operator's job.
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

// staticMaxAge is safe because assets are embedded and the ETag comes from their content.
const staticMaxAge = 24 * time.Hour

// assetsETag hashes the asset tree once: a build-identity ETag served a stale app.js for a day.
var assetVersion = sync.OnceValue(assetsETag)

// AssetVersion is appended to asset URLs and used as the cacheStatic ETag.
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
		// Unreachable for an embedded FS; fall back to the build identity.
		return `"` + version.Version + "-" + version.Commit + `"`
	}
	return `"` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
}

// cacheStatic adds validators so page loads do not re-fetch htmx and the fonts.
func cacheStatic(h http.Handler) http.Handler {
	etag := AssetVersion()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(staticMaxAge.Seconds())))
		w.Header().Set("ETag", etag)
		// If-None-Match is a list whose entries may be weak; a substring test would over-match.
		for candidate := range strings.SplitSeq(r.Header.Get("If-None-Match"), ",") {
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
