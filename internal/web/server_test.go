package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/web"
)

func TestEndpoints(t *testing.T) {
	h := newHandler(t, true, false, nil)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/health", http.StatusOK},
		{"/ready", http.StatusOK},
		{"/metrics", http.StatusOK},
	} {
		rec, _ := do(t, h, tc.path)
		if rec.Code != tc.want {
			t.Errorf("%s: got status %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestReadyNotReady(t *testing.T) {
	h := newHandler(t, false, false, nil)
	rec, _ := do(t, h, "/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/ready: got status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestNodeSwitcherAndCookie(t *testing.T) {
	h := newHandler(t, true, true, nil)

	// Opening a node page offers the remember-node cookie...
	rec, body := do(t, h, "/nodes/node-1/cpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes/node-1/cpu: got status %d, want 200", rec.Code)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "tm-node=node-1") {
		t.Errorf("expected a tm-node cookie, got %q", setCookie)
	}
	// ...the section page carries the node switcher with section-aware options...
	checkBody(t, body, `id="node-select"`, `value="/nodes/node-1/cpu" selected`)

	// ...and the dashboard sidebar links to the last viewed node's sections.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "tm-node", Value: "node-1"}) // #nosec G124 -- test request cookie
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	page, _ := io.ReadAll(out.Result().Body)
	checkBody(t, string(page), `href="/nodes/node-1"`, `href="/nodes/node-1/cpu"`, `href="/nodes/node-1/memory"`)
}

func TestAboutPage(t *testing.T) {
	h := newHandler(t, true, false, nil)
	rec, body := do(t, h, "/about")
	if rec.Code != http.StatusOK {
		t.Fatalf("/about: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "commit", "/metrics", "/health", "/ready")
}

func TestStaticAssets(t *testing.T) {
	h := newHandler(t, true, false, nil)

	rec, body := do(t, h, "/static/css/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/css/style.css: got status %d, want 200", rec.Code)
	}
	// A 200 proves routing; this proves the embedded bytes are the real file
	// and not an empty or truncated one.
	if !strings.Contains(body, ".card") {
		t.Error("style.css does not look like the dashboard stylesheet")
	}

	rec, _ = do(t, h, "/static/js/htmx.min.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/js/htmx.min.js: got status %d, want 200", rec.Code)
	}

	rec, body = do(t, h, "/static/js/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/js/app.js: got status %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "node-select") {
		t.Error("app.js does not carry the node switcher")
	}

	// uPlot is gone. Its bytes must not still be shipped in the binary.
	for _, gone := range []string{"/static/js/uplot.min.js", "/static/css/uplot.min.css", "/static/js/graph.js"} {
		if rec, _ := do(t, h, gone); rec.Code != http.StatusNotFound {
			t.Errorf("%s still served (status %d) after the uPlot removal", gone, rec.Code)
		}
	}
}

// A partial that fails to render must return 5xx: htmx keeps the last good
// content on a 5xx, but an empty 200 would swap in nothing and leave the
// polled region dead until a manual reload.
func TestPartialRenderErrorReturns500(t *testing.T) {
	rec := httptest.NewRecorder()
	web.WritePartialErrorForTest(rec)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed partial render: got status %d, want 500", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Result().Body); len(body) == 0 {
		t.Error("failed partial render: empty body, want an error message")
	}
}

// A full page that fails to render must return 5xx, not an empty 200: an
// operator reading a blank dashboard cannot tell a dead render from a
// network problem. The partial path pins the same contract in its own test.
func TestRenderErrorReturns500(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	web.RenderErrorForTest(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed page render: got status %d, want 500", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Result().Body); len(body) == 0 {
		t.Error("failed page render: empty body, want an error message")
	}
}

// Pages are personal (the sidebar follows the last-viewed-node cookie) and
// partials are time-varying, so neither may survive in a proxy cache.
func TestNoStoreHeaders(t *testing.T) {
	h := newHandler(t, true, true, nil)
	for _, path := range []string{"/", "/nodes", "/nodes/node-1/cpu", "/partials/status", "/partials/overview"} {
		rec, _ := do(t, h, path)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestStatusPartial(t *testing.T) {
	// No verified nodes → down pill.
	h := newHandler(t, true, true, nil)
	rec, body := do(t, h, "/partials/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("/partials/status: got status %d, want 200", rec.Code)
	}
	checkBody(t, body, "pill", "0/1 nodes")
	if !strings.Contains(body, "pill-down") {
		t.Error("expected pill-down when no node is verified")
	}

	// All verified → ok pill with version on the pages.
	up := func(string) (string, bool) { return "v1.13.4", true }
	h2 := newHandler(t, true, true, up)
	_, body2 := do(t, h2, "/partials/status")
	if !strings.Contains(body2, "pill-ok") {
		t.Error("expected pill-ok when all nodes are verified")
	}
	_, body3 := do(t, h2, "/")
	checkBody(t, body3, "v1.13.4", "dot-ok")
}

func TestMetricsContent(t *testing.T) {
	h := newHandler(t, true, false, nil)
	rec, body := do(t, h, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: got status %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Error("/metrics: missing go_goroutines")
	}
}

func TestNodeDataForLoadsOnlyWhatThePageRenders(t *testing.T) {
	// Index order: system, cpu, memory, pci, disks, sensors, status, summary,
	// links, health, identity, gpu, timesync, kernel, volumeLayer, diskIO.
	//
	// The last three ride along with an existing page rather than owning one:
	// health also loads timesync, identity also loads kernel, disks also
	// loads the volume layer.
	for _, tc := range []struct {
		section string
		want    [16]bool
	}{
		{"", [16]bool{true, true, true, false, false, false, true, true, true, true, true, true, true, true, false, false}},
		{"cpu", [16]bool{false, true, false, false, false, false, true, false, false, false, false, false, false, false, false, false}},
		{"memory", [16]bool{false, false, true, false, false, false, false, false, false, false, false, false, false, false, false, false}},
		{"storage", [16]bool{false, false, false, false, true, false, false, false, false, false, false, false, false, false, true, true}},
		{"pci", [16]bool{false, false, false, true, false, false, false, false, false, false, false, false, false, false, false, false}},
		{"sensors", [16]bool{false, false, false, false, false, true, false, false, false, false, false, false, false, false, false, false}},
		{"links", [16]bool{false, false, false, false, false, false, false, false, true, false, false, false, false, false, false, false}},
		{"health", [16]bool{false, false, false, false, false, false, false, false, false, true, false, false, true, false, false, false}},
		{"identity", [16]bool{false, false, false, false, false, false, false, false, false, false, true, false, false, false, false, false}},
		// kernel keeps identity too: the cmdline, kernel version and boot entry
		// are the context its tables need.
		{"kernel", [16]bool{false, false, false, false, false, false, false, false, false, false, true, false, false, true, false, false}},
		// gpu loads sensors so the card's own thermals sit on its page.
		{"gpu", [16]bool{false, false, false, false, false, true, false, false, false, false, false, true, false, false, false, false}},
	} {
		if got := web.NodeDataForTest(tc.section); got != tc.want {
			t.Errorf("nodeDataFor(%q) = %v, want %v", tc.section, got, tc.want)
		}
	}
}

func TestCategoryPagesDoNotRenderOtherSections(t *testing.T) {
	// The PCI page must not build sensor groups or the memory table, and the
	// sensors page must not build the PCI table.
	h := newHandler(t, true, true, nil)
	for _, tc := range []struct{ path, mustNotContain string }{
		{"/nodes/node-1/pci", "Hardware · Temperature"},
		{"/nodes/node-1/pci", "Hardware · Memory"},
		{"/nodes/node-1/sensors", "Hardware — PCI"},
		{"/nodes/node-1/memory", "Hardware · Disks"},
	} {
		_, body := do(t, h, tc.path)
		if strings.Contains(body, tc.mustNotContain) {
			t.Errorf("%s rendered %q from another section", tc.path, tc.mustNotContain)
		}
	}
}

func TestNodeLiveCardPartials(t *testing.T) {
	h := newHandler(t, true, true, nil)
	// Every node page now has a live region (finding 5.14): only the overview
	// and sensors refreshed themselves, and the rest needed a manual reload.
	for _, tc := range []struct{ card, id string }{
		{"overview", "node-live"},
		{"cpu", "cpu-live"},
		{"memory", "memory-live"},
		{"storage", "storage-live"},
		{"pci", "pci-live"},
		{"links", "links-live"},
		{"sensors", "sensors-live"},
		{"health", "health-live"},
		{"identity", "identity-live"},
		{"gpu", "gpu-live"},
	} {
		rec, body := do(t, h, "/partials/nodes/node-1/"+tc.card)
		if rec.Code != http.StatusOK {
			t.Fatalf("/partials/nodes/node-1/%s: got status %d, want 200", tc.card, rec.Code)
		}
		// A fragment, not a page: polling the page cost a full render plus a
		// cluster-wide gather on every tick.
		if strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("%s partial returned a full page", tc.card)
		}
		// Exactly one root, carrying its own poll, swapped with outerHTML.
		// hx-select-into-itself nested a copy per tick, and every copy then
		// started its own poller (finding 1.1).
		if n := strings.Count(body, `id="`+tc.id+`"`); n != 1 {
			t.Errorf("%s partial has %d #%s roots, want 1", tc.card, n, tc.id)
		}
		checkBody(t, body, `hx-swap="outerHTML"`, `hx-get="/partials/nodes/node-1/`+tc.card+`"`)
		if strings.Contains(body, "hx-select") {
			t.Errorf("%s partial reintroduced hx-select (the nesting bug)", tc.card)
		}
	}

	if rec, _ := do(t, h, "/partials/nodes/node-1/bogus"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown partial: got status %d, want 404", rec.Code)
	}
	if rec, _ := do(t, h, "/partials/nodes/missing/cpu"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown node: got status %d, want 404", rec.Code)
	}
}

// The charts are gone, and with them the reason a live region could not swap
// its whole card. What remains is the rule that outlived them: no page may
// reference chart markup or the removed graph endpoint.
func TestNoChartMarkupRemains(t *testing.T) {
	h := newHandler(t, true, true, nil)
	for _, path := range []string{
		"/", "/nodes/node-1", "/nodes/node-1/cpu", "/nodes/node-1/links",
		"/partials/nodes/node-1/cpu", "/partials/nodes/node-1/links",
	} {
		_, body := do(t, h, path)
		for _, gone := range []string{"cpu-graph", "cpu-freq-graph", "net-graph", "uplot", "/api/graph"} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still references %q", path, gone)
			}
		}
	}
}

func TestPageTemplatesHaveNoSelfNestingPollers(t *testing.T) {
	// Any element that polls itself must swap outerHTML; hx-select-into-itself
	// with innerHTML nests a copy on every tick (finding 1.1).
	h := newHandler(t, true, true, nil)
	for _, path := range []string{"/", "/nodes/node-1", "/nodes/node-1/cpu", "/nodes/node-1/sensors"} {
		_, body := do(t, h, path)
		if strings.Contains(body, "hx-select") {
			t.Errorf("%s uses hx-select; poll a dedicated partial with outerHTML instead", path)
		}
	}
}

// TestStaticAssetsAreCacheableAndSelfHosted covers findings 5.4 and 5.5. Every
// page load used to re-fetch the htmx and uPlot bundles (~120 KB), and the
// fonts came from Google's CDN, which a cluster with no egress cannot reach.
func TestStaticAssetsAreCacheableAndSelfHosted(t *testing.T) {
	h := newHandler(t, true, true, nil)

	for _, path := range []string{
		"/static/css/style.css",
		"/static/css/fonts.css",
		"/static/js/htmx.min.js",
		"/static/fonts/manrope-latin-var.woff2",
		"/static/fonts/jetbrains-mono-latin-var.woff2",
	} {
		rec, _ := do(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: got status %d, want 200", path, rec.Code)
			continue
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s: no ETag", path)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
			t.Errorf("%s: Cache-Control = %q, want a max-age", path, cc)
		}

		// A conditional request must be answered 304, not with the body again.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("If-None-Match", etag)
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusNotModified {
			t.Errorf("%s: revalidation got %d, want 304", path, rec2.Code)
		}
		if rec2.Body.Len() != 0 {
			t.Errorf("%s: 304 carried a %d-byte body", path, rec2.Body.Len())
		}
	}

	// No page may reach out to a font CDN: an air-gapped cluster would render
	// with the fallback stack instead of the intended typography.
	_, body := do(t, h, "/")
	for _, host := range []string{"fonts.googleapis.com", "fonts.gstatic.com"} {
		if strings.Contains(body, host) {
			t.Errorf("layout still references %s", host)
		}
	}
}

// TestSecurityHeaders: the dashboard and /metrics are unauthenticated and
// carry chassis serials, system UUIDs and disk WWIDs. Network isolation by
// the operator is the real control, but headers are free and close the easy
// holes.
func TestSecurityHeaders(t *testing.T) {
	h := newHandler(t, true, true, nil)
	for _, path := range []string{"/", "/nodes/node-1", "/metrics", "/static/js/app.js"} {
		rec, _ := do(t, h, path)
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Errorf("%s: no Content-Security-Policy", path)
			continue
		}
		// Everything is served from this binary, so nothing may load
		// cross-origin — that is what makes a strict policy affordable here.
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP missing %q", path, want)
			}
		}
		if strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows unsafe-eval", path)
		}
	}
}

// TestIfNoneMatchParsesTheList: the header is a comma-separated list and
// entries may be weak. A substring test would also match an unrelated tag that
// happens to contain ours.
func TestIfNoneMatchParsesTheList(t *testing.T) {
	h := newHandler(t, true, true, nil)
	rec, _ := do(t, h, "/static/js/app.js")
	etag := rec.Header().Get("ETag")

	for _, tc := range []struct {
		header string
		want   int
	}{
		{etag, http.StatusNotModified},
		{`"other", ` + etag, http.StatusNotModified},
		{"W/" + etag, http.StatusNotModified},
		{"*", http.StatusNotModified},
		{`"unrelated"`, http.StatusOK},
		{`"x` + strings.Trim(etag, `"`) + `x"`, http.StatusOK}, // superstring must NOT match
	} {
		req := httptest.NewRequest(http.MethodGet, "/static/js/app.js", nil)
		req.Header.Set("If-None-Match", tc.header)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		if r.Code != tc.want {
			t.Errorf("If-None-Match %s: got %d, want %d", tc.header, r.Code, tc.want)
		}
	}
}

// TestNoInlineScriptOrHandlers: the CSP is script-src 'self', so an inline
// <script> or an on*= attribute is silently blocked at runtime. The node
// switcher was exactly that — an onchange= that would have stopped navigating
// the moment the CSP shipped.
func TestNoInlineScriptOrHandlers(t *testing.T) {
	h := newHandler(t, true, true, nil)
	inlineScript := regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`)
	handler := regexp.MustCompile(`\son[a-z]+\s*=`)

	for _, path := range []string{"/", "/nodes", "/nodes/node-1", "/nodes/node-1/cpu",
		"/nodes/node-1/links", "/nodes/node-1/sensors", "/about"} {
		_, body := do(t, h, path)
		if m := inlineScript.FindString(body); m != "" {
			t.Errorf("%s: inline <script> would be blocked by the CSP: %q", path, m)
		}
		if m := handler.FindString(body); m != "" {
			t.Errorf("%s: inline event handler would be blocked by the CSP: %q", path, m)
		}
	}
	// And the switcher still exists, wired from the external bundle.
	_, body := do(t, h, "/nodes/node-1")
	checkBody(t, body, `id="node-select"`, `/static/js/app.js`)
}

// TestAssetURLsAreVersioned covers the failure that broke the node switcher:
// the ETag was the git commit, so editing a static asset without committing
// left it unchanged while Cache-Control said the browser could reuse the old
// file for a day. A stale graph.js then had no switcher handler, while the
// markup no longer had the inline one.
//
// Two defences: the ETag is now the asset content, and asset URLs carry that
// hash so a new build cannot be served from an old cache at all.
func TestAssetURLsAreVersioned(t *testing.T) {
	h := newHandler(t, true, true, nil)
	_, body := do(t, h, "/nodes/node-1")

	v := strings.Trim(web.AssetVersion(), `"`)
	if v == "" {
		t.Fatal("empty asset version")
	}
	for _, asset := range []string{
		"/static/js/app.js", "/static/js/htmx.min.js", "/static/css/style.css",
	} {
		want := asset + "?v=" + v
		if !strings.Contains(body, want) {
			t.Errorf("layout references %s without the version query (want %s)", asset, want)
		}
	}

	// The ETag must be the content hash, not the build identity: a dev build
	// keeps the same version and commit across asset edits.
	rec, _ := do(t, h, "/static/js/app.js")
	if got := rec.Header().Get("ETag"); got != web.AssetVersion() {
		t.Errorf("ETag = %q, want the asset hash %q", got, web.AssetVersion())
	}
	if strings.Contains(rec.Header().Get("ETag"), "dev") {
		t.Error("ETag still derives from the build identity")
	}
}

// The overview refreshes itself every 5s by re-rendering overview-body. Any
// dataset the full page sets but the partial forgets vanishes on the first
// refresh — the page looks right until you leave it open for five seconds.
func TestOverviewPartialRendersEverythingTheFullPageDoes(t *testing.T) {
	snapNode(t, "node-1", func(n *snapshot.Node) {
		n.Health = &snapshot.Health{
			HasMachineStatus: true, Stage: "running", Ready: true,
			Services: []snapshot.Service{{ID: "etcd", Running: true, Healthy: true}},
		}
		n.TimeSync = &snapshot.TimeSync{HasStatus: true, Synced: true}
		n.Identity = &snapshot.Identity{Schematic: "65cf8364cd0de4cf7b85"}
	})
	h := newHandler(t, true, true, nil)

	_, full := do(t, h, "/")
	_, partial := do(t, h, "/partials/overview")

	for _, want := range []string{"System status", "running", "1 / 1", "synced", "Images"} {
		if !strings.Contains(full, want) {
			t.Errorf("overview page missing %q", want)
		}
		if !strings.Contains(partial, want) {
			t.Errorf("overview PARTIAL missing %q — it will vanish on the first refresh", want)
		}
	}
}
