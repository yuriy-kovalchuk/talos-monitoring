package network

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
)

// ifCounters is one interface's cumulative counters.
type ifCounters struct {
	rxBytes, rxPackets, rxErrors, rxDropped uint64
	txBytes, txPackets, txErrors, txDropped uint64
}

// counterSample is the previous scrape's counters, for turning totals into rates.
type counterSample struct {
	at   time.Time
	byIf map[string]ifCounters
}

// linkStats is one interface's counters plus derived throughput; HasRate is false on the first scrape.
type linkStats struct {
	ifCounters
	HasRate bool
	RxRate  float64
	TxRate  float64
}

// registerCounters exports the NetworkDeviceStats counters (os:reader-safe) and derives rates for the live graph.
func (c *Collector) registerCounters(ctx context.Context, api statsAPI, nodeName string, kept map[string]bool, reg prometheus.Registerer) (map[string]linkStats, error) {
	resp, err := api.NetworkDeviceStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("network device stats: %w", err)
	}
	current := fromDeviceStats(resp, kept)
	if len(current) == 0 {
		return nil, nil
	}

	rxBytes := counterVec("talos_net_link_rx_bytes_total", "Bytes received on the interface since boot.")
	txBytes := counterVec("talos_net_link_tx_bytes_total", "Bytes transmitted on the interface since boot.")
	rxPackets := counterVec("talos_net_link_rx_packets_total", "Packets received on the interface since boot.")
	txPackets := counterVec("talos_net_link_tx_packets_total", "Packets transmitted on the interface since boot.")
	rxErrors := counterVec("talos_net_link_rx_errors_total", "Receive errors on the interface since boot.")
	txErrors := counterVec("talos_net_link_tx_errors_total", "Transmit errors on the interface since boot.")
	rxDropped := counterVec("talos_net_link_rx_dropped_total", "Received packets dropped on the interface since boot.")
	txDropped := counterVec("talos_net_link_tx_dropped_total", "Transmitted packets dropped on the interface since boot.")
	// Exporter-side rates for the live graph; the HELP says so they are not preferred to rate().
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
		// A counter going backwards means the interface was reset: skip, never emit a wild rate.
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
	return stats, nil
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

// fromDeviceStats keeps the counters of the reported interfaces; the CNI's veths are dropped here.
func fromDeviceStats(resp *machine.NetworkDeviceStatsResponse, kept map[string]bool) map[string]ifCounters {
	out := map[string]ifCounters{}
	for _, m := range resp.GetMessages() {
		for _, d := range m.GetDevices() {
			if !kept[d.GetName()] {
				continue
			}
			out[d.GetName()] = ifCounters{
				rxBytes: d.GetRxBytes(), rxPackets: d.GetRxPackets(), rxErrors: d.GetRxErrors(), rxDropped: d.GetRxDropped(),
				txBytes: d.GetTxBytes(), txPackets: d.GetTxPackets(), txErrors: d.GetTxErrors(), txDropped: d.GetTxDropped(),
			}
		}
	}
	return out
}

func counterVec(name, help string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, []string{"node", "link"})
}

// statsAPI is the part of the Talos client the counters use (test seam).
type statsAPI interface {
	NetworkDeviceStats(ctx context.Context) (*machine.NetworkDeviceStatsResponse, error)
}

// talosStats adapts the Talos client, which has no NetworkDeviceStats wrapper of its own.
type talosStats struct{ client *talosclient.Client }

func (t talosStats) NetworkDeviceStats(ctx context.Context) (*machine.NetworkDeviceStatsResponse, error) {
	return t.client.MachineClient.NetworkDeviceStats(ctx, &emptypb.Empty{})
}

// Prune implements collector.Pruner: drop counter samples for departed nodes.
func (c *Collector) Prune(live map[string]struct{}) {
	// An empty live set means discovery has not synced; the Pruner contract says that prunes nothing.
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
