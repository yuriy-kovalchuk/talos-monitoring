// Package app wires together the monitor's components and owns their lifecycle.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/block"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/cpu"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/diskio"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/gpu"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/health"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/hwinfo"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/identity"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/kernel"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/network"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/nodeapi"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/sensors"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/sysstat"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/timesync"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/volumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/history"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/k8svolumes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/version"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/web"
)

// Defaults for the configurable scrape windows.
const (
	defaultScrapeInterval    = 30 * time.Second
	defaultInventoryInterval = 5 * time.Minute
)

// Options holds serve-time configuration.
type Options struct {
	// ScrapeInterval is how often Live collectors (sensors, CPU stats)
	// re-query the nodes (0 → 30s). Shorter = fresher metrics, more load
	// on the Talos API: every scrape issues calls on every node.
	ScrapeInterval time.Duration
	// InventoryInterval is how often Inventory collectors (hardware, disks)
	// re-query the nodes (0 → 5m).
	InventoryInterval time.Duration
	// CollectorEnabled holds per-collector enabled decisions, keyed by name.
	// A missing name stays enabled; an explicit false registers the collector
	// but keeps it out of the scrape loop. Unregistered names are skipped.
	CollectorEnabled map[string]bool
	// CollectorTTLs holds per-collector TTL overrides by name (absent or
	// zero = the class default).
	CollectorTTLs map[string]time.Duration
	// HistoryWindow is how long the in-memory graph history keeps samples
	// (0 → 10m). RAM-only; durable history stays with Prometheus.
	HistoryWindow time.Duration
	// CPUFreqInterval throttles the per-core current-frequency pass on nodes
	// that need one read per core (0 → every cpu tick). Only those nodes pay
	// it: machines whose /proc/cpuinfo is already per-core keep the single read.
	CPUFreqInterval time.Duration

	// CPUModeSeconds enables talos_node_cpu_seconds_total, the per-core,
	// per-mode CPU time counters. Off by default: 10 series per thread per
	// node, and nothing in the UI reads them. Turn it on to compute usage in
	// PromQL rather than trusting the exporter's own ratio.
	CPUModeSeconds bool
}

// schedulerTick is the always-on scrape loop period. Refresh() only
// collects (node, collector) pairs past their TTL, so a sub-TTL tick is a
// cheap map scan; the tick bounds how late a cadence can run.
const schedulerTick = time.Second

// pruneInterval is how often data for departed nodes is dropped.
const pruneInterval = 30 * time.Second

// App holds the monitor's components.
type App struct {
	log        *slog.Logger
	reg        *prometheus.Registry
	srv        *http.Server
	nodes      *nodes.Manager
	talosPool  *nodes.TalosClientPool
	scraper    *collector.Scraper
	hist       *history.Store
	snap       *snapshot.Store
	sysstat    *sysstat.Cache
	collectors *collector.Registry
	pvs        *k8svolumes.Manager
}

// New creates an App.
func New(log *slog.Logger, opts Options) (*App, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	nm := nodes.New(log, reg)
	pvm := k8svolumes.New(log, reg)
	tp := nodes.NewTalosClientPool(log, nm.Nodes)

	scrapeInterval := opts.ScrapeInterval
	if scrapeInterval <= 0 {
		scrapeInterval = defaultScrapeInterval
	}
	inventoryInterval := opts.InventoryInterval
	if inventoryInterval <= 0 {
		inventoryInterval = defaultInventoryInterval
	}
	if scrapeInterval < 10*time.Second {
		log.Warn("short scrape interval: every scrape queries all nodes, expect "+
			"(nodes x collectors) Talos API calls per interval; sub-10s intervals are not recommended for large clusters",
			"interval", scrapeInterval)
	}

	hist := history.New(opts.HistoryWindow)
	snap := snapshot.New()
	stats := sysstat.New()

	colReg := collector.NewRegistry()
	for _, c := range []collector.Collector{
		cpu.New(log, hist, opts.CPUFreqInterval, opts.CPUModeSeconds).WithSnapshot(snap).WithSysStat(stats),
		nodeapi.New().WithSnapshot(snap).WithSysStat(stats),
		sensors.New(log).WithSnapshot(snap),
		block.New().WithSnapshot(snap),
		health.New().WithSnapshot(snap),
		identity.New().WithSnapshot(snap),
		timesync.New().WithSnapshot(snap),
		kernel.New().WithSnapshot(snap),
		volumes.New().WithSnapshot(snap),
		gpu.New(log).WithSnapshot(snap),
		diskio.New().WithSnapshot(snap),
		network.New(log).WithSnapshot(snap),
		hwinfo.NewSystem().WithSnapshot(snap),
		hwinfo.NewProcessor().WithSnapshot(snap),
		hwinfo.NewMemory().WithSnapshot(snap),
		hwinfo.NewPCI(log).WithSnapshot(snap),
	} {
		if err := colReg.Register(c); err != nil {
			return nil, err
		}
	}
	colReg.SetEnabled(opts.CollectorEnabled)
	enabled := colReg.Enabled()
	enabledNames := make([]string, 0, len(enabled))
	for _, c := range enabled {
		enabledNames = append(enabledNames, c.Name())
	}
	log.Info("collectors enabled", "collectors", strings.Join(enabledNames, ","))

	scraper := collector.NewScraper(reg, collector.Options{
		Log:               log,
		Registry:          colReg,
		ListNodes:         nm.Nodes,
		Clients:           tp.Get,
		ClientsReady:      tp.Ready,
		ScrapeInterval:    scrapeInterval,
		InventoryInterval: inventoryInterval,
		TTLOverrides:      opts.CollectorTTLs,
	})
	logCadences(log, scraper)
	gatherer := collector.NewGatherer(reg, scraper)

	a := &App{
		log:        log,
		reg:        reg,
		nodes:      nm,
		talosPool:  tp,
		scraper:    scraper,
		hist:       hist,
		snap:       snap,
		sysstat:    stats,
		collectors: colReg,
		pvs:        pvm,
	}
	a.srv = &http.Server{
		Handler:           web.NewRouter(log, gatherer, a.ready, nm.Nodes, nodeStatus(tp, scraper), hist, snap, pvm.Volumes),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	return a, nil
}

// ready is /ready: both halves of startup must have completed.
//
// Discovery alone is not readiness. The Kubernetes API answers whether the
// cluster *has* nodes; the Talos client pool answers whether the exporter can
// actually reach them (the talos.dev ServiceAccount Secret has been mounted and
// parsed). Wiring this to discovery alone let the pod report Ready while every
// collector was failing on a missing talosconfig — which is exactly the state
// the chart's readiness probe exists to surface, and what the README tells
// operators to expect ("it stays 0/1 and logs talosconfig unavailable").
func (a *App) ready() bool {
	return a.nodes.Ready() && a.talosPool.Ready()
}

// logCadences logs the effective per-collector scrape cadence (sorted by
// name) at startup.
func logCadences(log *slog.Logger, scraper *collector.Scraper) {
	cads := scraper.TTLs()
	if len(cads) == 0 {
		return
	}
	names := make([]string, 0, len(cads))
	for name := range cads {
		names = append(names, name)
	}
	sort.Strings(names)
	kv := make([]any, 0, len(names)*2)
	for _, name := range names {
		kv = append(kv, name, cads[name].String())
	}
	log.Info("scrape cadences (always-on; the UI and /metrics are pure viewers)", kv...)
}

// Run starts node discovery, the Talos client pool, the always-on scraper
// and the HTTP server, blocking until ctx is done.
func (a *App) Run(ctx context.Context, listen string) error {
	go a.nodes.Run(ctx)
	go a.watchVolumes(ctx)
	go a.talosPool.Run(ctx)
	go a.schedule(ctx)

	a.log.Info("starting talos-monitoring",
		"version", version.Version,
		"commit", version.Commit,
		"build_date", version.BuildDate,
	)
	a.srv.Addr = listen
	errCh := make(chan error, 1)
	go func() {
		a.log.Info("listening", "addr", listen)
		if err := a.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.log.Info("shutting down")
		return a.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// schedule drives the always-on scraper: every second, Refresh() collects
// only the (node, collector) pairs whose TTL has expired. Scraping is
// independent of the UI and of /metrics — both are pure viewers of the
// cached results.
func (a *App) schedule(ctx context.Context) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	// Pruning only matters when a node leaves the cluster, which the informer
	// reports in its own time; running it every second copied the node list
	// 60x a minute to do nothing (finding 5.7).
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.scraper.Refresh(ctx)
		case <-prune.C:
			a.prune()
		}
	}
}

// watchVolumes starts the PersistentVolume watcher once a Kubernetes client is
// available. Failures are retried; without PV permissions the node-side usage
// metrics still work, the volumes are just unnamed.
func (a *App) watchVolumes(ctx context.Context) {
	for {
		cs, err := nodes.NewClientset()
		if err == nil {
			a.pvs.Run(ctx, cs)
			return
		}
		a.log.Error("kubernetes client unavailable for the volume watcher, retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// prune drops per-node state for nodes that left the cluster: graph history,
// the typed snapshot, the shared SystemStat cache, and whatever each collector
// keeps of its own (previous samples, cached inventories, sensor layouts).
//
// An empty node list means discovery has not synced, not that the cluster is
// empty, and prunes nothing — the same rule the scraper follows.
func (a *App) prune() {
	ns := a.nodes.Nodes()
	if len(ns) == 0 {
		return
	}
	live := make(map[string]struct{}, len(ns))
	for _, n := range ns {
		live[n.Name] = struct{}{}
	}
	a.hist.Prune(live)
	a.snap.Prune(live)
	a.sysstat.Prune(live)
	for _, c := range a.collectors.All() {
		if p, ok := c.(collector.Pruner); ok {
			p.Prune(live)
		}
	}
}

// nodeStatus is the dashboard's view of a node: the Talos version last seen by
// the client pool, and whether the node is reachable.
//
// Reachability comes from the scraper, not the pool. The pool sets its verified
// flag once and never clears it, so a node that died after being verified
// rendered as up forever (finding 1.2). The scraper re-evaluates every round,
// which is what the "up" column, the header pill and talos_node_up all mean.
func nodeStatus(tp *nodes.TalosClientPool, scraper *collector.Scraper) func(string) (string, bool) {
	return func(name string) (string, bool) {
		version, _ := tp.Status(name)
		up, known := scraper.NodeUp(name)
		return version, known && up
	}
}
