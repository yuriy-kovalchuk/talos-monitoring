// Command talos-monitoring exports Talos node hardware data as Prometheus metrics
// and serves a built-in htmx dashboard.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/app"
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
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/timesync"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collectors/volumes"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var listen, logLevel string
	var cpuModeSeconds bool
	var historyWindow, cpuFreqInterval time.Duration
	var scrapeInterval, inventoryInterval, cpuTTL, nodeapiTTL, sensorsTTL, blockTTL time.Duration
	var systemTTL, processorTTL, memoryTTL, pciTTL time.Duration

	root := &cobra.Command{
		Use:           "talos-monitoring",
		Short:         "Talos node hardware monitor (Prometheus metrics + htmx dashboard)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// One enabled flag per registered collector, registered below as
	// --collectors.<name>.enabled (default on, the old --collectors=all
	// behaviour).
	var (
		enableBlock, enableCPU, enableDiskio, enableGpu, enableHealth bool
		enableIdentity, enableKernel, enableMemory, enableNetwork     bool
		enableNodeapi, enablePci, enableProcessor, enableSensors      bool
		enableSystem, enableTime, enableVolumes                       bool
	)
	collectorFlags := []struct {
		name string
		on   *bool
	}{
		{block.Name, &enableBlock},
		{cpu.Name, &enableCPU},
		{diskio.Name, &enableDiskio},
		{gpu.Name, &enableGpu},
		{health.Name, &enableHealth},
		{identity.Name, &enableIdentity},
		{kernel.Name, &enableKernel},
		{hwinfo.NameMemory, &enableMemory},
		{network.Name, &enableNetwork},
		{nodeapi.Name, &enableNodeapi},
		{hwinfo.NamePCI, &enablePci},
		{hwinfo.NameProcessor, &enableProcessor},
		{sensors.Name, &enableSensors},
		{hwinfo.NameSystem, &enableSystem},
		{timesync.Name, &enableTime},
		{volumes.Name, &enableVolumes},
	}

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the metrics and dashboard server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var level slog.Level
			if err := level.UnmarshalText([]byte(logLevel)); err != nil {
				return fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

			if scrapeInterval <= 0 {
				return fmt.Errorf("--scrape-interval must be > 0")
			}
			if inventoryInterval <= 0 {
				return fmt.Errorf("--inventory-interval must be > 0")
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			enabled := make(map[string]bool, len(collectorFlags))
			for _, f := range collectorFlags {
				enabled[f.name] = *f.on
			}

			opts := app.Options{
				ScrapeInterval:    scrapeInterval,
				InventoryInterval: inventoryInterval,
				CollectorEnabled:  enabled,
				HistoryWindow:     historyWindow,
				CPUFreqInterval:   cpuFreqInterval,
				CPUModeSeconds:    cpuModeSeconds,
			}
			for name, ttl := range map[string]time.Duration{
				cpu.Name:             cpuTTL,
				nodeapi.Name:         nodeapiTTL,
				sensors.Name:         sensorsTTL,
				block.Name:           blockTTL,
				hwinfo.NameSystem:    systemTTL,
				hwinfo.NameProcessor: processorTTL,
				hwinfo.NameMemory:    memoryTTL,
				hwinfo.NamePCI:       pciTTL,
			} {
				if ttl <= 0 {
					continue
				}
				if opts.CollectorTTLs == nil {
					opts.CollectorTTLs = map[string]time.Duration{}
				}
				opts.CollectorTTLs[name] = ttl
			}

			a, err := app.New(log, opts)
			if err != nil {
				return err
			}
			return a.Run(ctx, listen)
		},
	}
	serve.Flags().StringVar(&listen, "listen", ":8080", "address to listen on")
	serve.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	for _, f := range collectorFlags {
		serve.Flags().BoolVar(f.on, "collectors."+f.name+".enabled", true,
			"enable the "+f.name+" collector")
	}
	serve.Flags().DurationVar(&scrapeInterval, "scrape-interval", 30*time.Second,
		"how often the live nodeapi and sensors data is re-collected from the nodes. "+
			"The fast cpu collector defaults to 5s (see --collectors.cpu.ttl). "+
			"Shorter = fresher metrics but more load on the Talos API: every scrape issues calls on every node.")
	// Currently unreachable: every Inventory-class collector declares its own
	// 1h DefaultTTL, which wins over this. It stays as the class default for
	// future collectors that do not.
	serve.Flags().DurationVar(&inventoryInterval, "inventory-interval", 5*time.Minute,
		"how often static hardware inventory is re-collected when a collector has no own default")
	serve.Flags().DurationVar(&historyWindow, "history.window", 10*time.Minute,
		"how long the in-memory frequency samples are kept; the CPU page reports an "+
			"observed range over this window because a single reading runs high "+
			"(RAM-only — durable history stays with Prometheus via /metrics)")
	serve.Flags().BoolVar(&cpuModeSeconds, "collectors.cpu.mode-seconds", false,
		"export talos_node_cpu_seconds_total (CPU time per core and mode, like "+
			"node_cpu_seconds_total). Off by default: it is 10 series per thread per node "+
			"and dominates /metrics; talos_node_cpu_usage_ratio is derived from the same counters")
	serve.Flags().DurationVar(&cpuFreqInterval, "collectors.cpu.freq-interval", 0,
		"how often per-core frequency is re-read on nodes that need one read per core "+
			"(intel_pstate; 0 = every cpu tick). Machines whose /proc/cpuinfo is already "+
			"per-core (amd_pstate) always use a single read and ignore this.")
	serve.Flags().DurationVar(&cpuTTL, "collectors.cpu.ttl", 0,
		"TTL override for the cpu collector (0 = 5s: CPU usage + live frequency)")
	serve.Flags().DurationVar(&nodeapiTTL, "collectors.nodeapi.ttl", 0,
		"TTL override for the nodeapi collector (0 = scrape interval: version, uptime, processes)")
	serve.Flags().DurationVar(&sensorsTTL, "collectors.sensors.ttl", 0,
		"TTL override for the sensors collector (0 = scrape interval, live data)")
	serve.Flags().DurationVar(&blockTTL, "collectors.block.ttl", 0,
		"TTL override for the block collector (0 = scrape interval: filesystem and volume usage; "+
			"the disk list behind it is re-listed hourly regardless)")
	serve.Flags().DurationVar(&systemTTL, "collectors.system.ttl", 0,
		"TTL override for the system collector (0 = 1h: system information)")
	serve.Flags().DurationVar(&processorTTL, "collectors.processor.ttl", 0,
		"TTL override for the processor collector (0 = 1h: CPU sockets)")
	serve.Flags().DurationVar(&memoryTTL, "collectors.memory.ttl", 0,
		"TTL override for the memory collector (0 = scrape interval: /proc/meminfo usage; "+
			"the DIMM inventory behind it is re-listed hourly regardless)")
	serve.Flags().DurationVar(&pciTTL, "collectors.pci.ttl", 0,
		"TTL override for the pci collector (0 = 1h: PCI devices)")

	root.AddCommand(serve)
	return root
}
