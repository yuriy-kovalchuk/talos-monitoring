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
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/etcd"
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
	// ScrapeInterval is how often Live collectors re-query nodes (0 → 30s); shorter means more Talos load.
	ScrapeInterval time.Duration
	// InventoryInterval is how often Inventory collectors re-query nodes (0 → 5m).
	InventoryInterval time.Duration
	// CollectorEnabled is a per-name enable decision; a missing name stays enabled.
	CollectorEnabled map[string]bool
	// CollectorTTLs overrides a collector's TTL by name (absent or zero = class default).
	CollectorTTLs map[string]time.Duration
	// HistoryWindow is how long the in-RAM graph history keeps samples (0 → 10m).
	HistoryWindow time.Duration
	// CPUFreqInterval throttles the per-core current-frequency pass on nodes that need one read per core.
	CPUFreqInterval time.Duration

	// CPUModeSeconds turns on talos_node_cpu_seconds_total: 10 series per thread per node, and no UI page reads them.
	CPUModeSeconds bool
}

// schedulerTick bounds how late a cadence can run; a sub-TTL tick is only a map scan.
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
		etcd.New().WithSnapshot(snap),
		network.New().WithSnapshot(snap),
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
		// IdleTimeout stops an abandoned dashboard tab holding a connection forever; keep it above the scrape interval.
		IdleTimeout: 60 * time.Second,
	}
	return a, nil
}

// ready needs both halves: discovery synced and a talosconfig loaded, or every collector is failing.
func (a *App) ready() bool {
	return a.nodes.Ready() && a.talosPool.Ready()
}

// logCadences logs the effective per-collector cadence at startup.
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

// Run starts discovery, the client pool, the scheduler and the HTTP server, blocking until ctx is done.
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

// schedule ticks the scraper every second; /metrics and the UI are pure viewers of what it cached.
func (a *App) schedule(ctx context.Context) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	// Prune only matters when a node leaves, so it does not need the 1 s tick.
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

// watchVolumes starts the PV watcher once a Kubernetes client exists; without PV rights volumes stay unnamed.
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

// prune drops per-node state for gone nodes; an empty live set means discovery has not synced and prunes nothing.
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

// nodeStatus takes reachability from the scraper, not the pool: the pool's verified flag is never cleared.
func nodeStatus(tp *nodes.TalosClientPool, scraper *collector.Scraper) func(string) (string, bool) {
	return func(name string) (string, bool) {
		version, _ := tp.Status(name)
		up, known := scraper.NodeUp(name)
		return version, known && up
	}
}
