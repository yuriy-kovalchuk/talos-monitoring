package network

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
)

// procNetDev holds every interface's counters, so throughput costs one read per
// scrape however many interfaces the node has (91 on the reference node).
const procNetDev = "/proc/net/dev"

// ifCounters is one interface's cumulative counters.
type ifCounters struct {
	rxBytes, rxPackets, rxErrors, rxDropped uint64
	txBytes, txPackets, txErrors, txDropped uint64
}

// counterSample is the previous scrape's counters for a node, used to turn
// cumulative values into a rate.
type counterSample struct {
	at   time.Time
	byIf map[string]ifCounters
}

// registerCounters reads /proc/net/dev, exports the cumulative counters and
// derives per-second rates against the previous sample.
//
// Counters are exported as counters so PromQL can rate() them itself; the
// derived rate exists because the dashboard needs a number now, without a
// range query. This mirrors how the cpu collector exports jiffies alongside a
// usage percentage.

// linkStats is one interface's counters plus the throughput derived from the
// previous sample. HasRate is false on the first scrape after a restart.
type linkStats struct {
	ifCounters
	HasRate bool
	RxRate  float64
	TxRate  float64
}

// registerCounters exports the /proc/net/dev counters and returns them so the
// caller can fold them into the snapshot. A nil return means they were
// unavailable (permission denied, or nothing matched).
func (c *Collector) registerCounters(ctx context.Context, api fileAPI, nodeName string, kept map[string]bool, reg prometheus.Registerer) map[string]linkStats {
	if c.countersDisabled.Load() {
		return nil
	}
	raw, err := collector.ReadFile(ctx, api, procNetDev)
	if err != nil {
		if collector.IsPermissionError(err) && c.countersDisabled.CompareAndSwap(false, true) {
			c.log.Warn("network throughput disabled: reading /proc/net/dev needs the os:admin role",
				"node", nodeName, "err", err,
				"hint", "give the monitor ServiceAccount the os:admin role; interface state is unaffected")
		}
		return nil
	}
	current := parseProcNetDev(raw, kept)
	if len(current) == 0 {
		return nil
	}

	rxBytes := counterVec("talos_net_link_rx_bytes_total", "Bytes received on the interface since boot.")
	txBytes := counterVec("talos_net_link_tx_bytes_total", "Bytes transmitted on the interface since boot.")
	rxPackets := counterVec("talos_net_link_rx_packets_total", "Packets received on the interface since boot.")
	txPackets := counterVec("talos_net_link_tx_packets_total", "Packets transmitted on the interface since boot.")
	rxErrors := counterVec("talos_net_link_rx_errors_total", "Receive errors on the interface since boot.")
	txErrors := counterVec("talos_net_link_tx_errors_total", "Transmit errors on the interface since boot.")
	rxDropped := counterVec("talos_net_link_rx_dropped_total", "Received packets dropped on the interface since boot.")
	txDropped := counterVec("talos_net_link_tx_dropped_total", "Transmitted packets dropped on the interface since boot.")
	// These are rates computed inside the exporter, over its own scrape
	// interval rather than a window the querier chooses. They exist for the
	// dashboard's live graph; the HELP says so, so nobody prefers them to
	// rate() by accident.
	rxRate := collector.GaugeVec("talos_net_link_rx_bytes_per_second",
		"Receive throughput, derived over the exporter's own scrape interval. Absent on the "+
			"first scrape after a restart. For querying prefer rate(talos_net_link_rx_bytes_total[window]), "+
			"which uses your window instead of ours.", "node", "link")
	txRate := collector.GaugeVec("talos_net_link_tx_bytes_per_second",
		"Transmit throughput, derived over the exporter's own scrape interval. Absent on the "+
			"first scrape after a restart. For querying prefer rate(talos_net_link_tx_bytes_total[window]), "+
			"which uses your window instead of ours.", "node", "link")
	for _, m := range []prometheus.Collector{rxBytes, txBytes, rxPackets, txPackets,
		rxErrors, txErrors, rxDropped, txDropped, rxRate, txRate} {
		reg.MustRegister(m)
	}

	now := c.now()
	prev, hadPrev := c.takePrev(nodeName, current, now)
	stats := make(map[string]linkStats, len(current))

	for name, cur := range current {
		stats[name] = linkStats{ifCounters: cur}
		// Fresh counters per Collect: start at 0 and add the cumulative value.
		rxBytes.WithLabelValues(nodeName, name).Add(float64(cur.rxBytes))
		txBytes.WithLabelValues(nodeName, name).Add(float64(cur.txBytes))
		rxPackets.WithLabelValues(nodeName, name).Add(float64(cur.rxPackets))
		txPackets.WithLabelValues(nodeName, name).Add(float64(cur.txPackets))
		rxErrors.WithLabelValues(nodeName, name).Add(float64(cur.rxErrors))
		txErrors.WithLabelValues(nodeName, name).Add(float64(cur.txErrors))
		rxDropped.WithLabelValues(nodeName, name).Add(float64(cur.rxDropped))
		txDropped.WithLabelValues(nodeName, name).Add(float64(cur.txDropped))

		if !hadPrev {
			continue
		}
		old, ok := prev.byIf[name]
		if !ok {
			continue
		}
		secs := now.Sub(prev.at).Seconds()
		if secs <= 0 {
			continue
		}
		// A counter that went backwards means the interface was reset or
		// replaced; skip rather than emit a negative or huge rate.
		rx, rxOK := perSecond(cur.rxBytes, old.rxBytes, secs)
		tx, txOK := perSecond(cur.txBytes, old.txBytes, secs)
		st := stats[name]
		if rxOK {
			rxRate.WithLabelValues(nodeName, name).Set(rx)
			st.HasRate, st.RxRate = true, rx
		}
		if txOK {
			txRate.WithLabelValues(nodeName, name).Set(tx)
			st.HasRate, st.TxRate = true, tx
		}
		stats[name] = st
	}
	return stats
}

func perSecond(cur, old uint64, secs float64) (float64, bool) {
	if cur < old {
		return 0, false
	}
	return float64(cur-old) / secs, true
}

// takePrev swaps in the new sample and returns the previous one.
func (c *Collector) takePrev(node string, cur map[string]ifCounters, now time.Time) (counterSample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.prev[node]
	c.prev[node] = counterSample{at: now, byIf: cur}
	return prev, ok
}

// parseProcNetDev parses /proc/net/dev, keeping only the named interfaces.
// Lines are "iface: rx_bytes rx_packets rx_errs rx_drop ... tx_bytes ...".
func parseProcNetDev(raw string, kept map[string]bool) map[string]ifCounters {
	out := map[string]ifCounters{}
	for _, line := range strings.Split(raw, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if !kept[name] {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		out[name] = ifCounters{
			rxBytes: u(f[0]), rxPackets: u(f[1]), rxErrors: u(f[2]), rxDropped: u(f[3]),
			txBytes: u(f[8]), txPackets: u(f[9]), txErrors: u(f[10]), txDropped: u(f[11]),
		}
	}
	return out
}

func u(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func counterVec(name, help string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, []string{"node", "link"})
}

// fileAPI is the part of the Talos client used to read /proc (test seam).
type fileAPI interface {
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// Prune implements collector.Pruner: drop counter samples for departed nodes.
func (c *Collector) Prune(live map[string]struct{}) {
	// An empty set means discovery has not synced yet, not that the cluster
	// is empty — pruning here would drop every node's state on a transient
	// watch blip. app.prune() guards this too; the Pruner contract says
	// implementations must, so they do.
	if len(live) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for node := range c.prev {
		if _, ok := live[node]; !ok {
			delete(c.prev, node)
		}
	}
}
