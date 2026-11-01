package collector

import (
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

// source is one cached collection: its producer and its families; the owner drives de-duplication.
type source struct {
	owner    string
	families []*dto.MetricFamily
}

// mergeFamilies drops duplicate series: one repeated line makes Prometheus reject the whole body.
func mergeFamilies(sources ...source) []*dto.MetricFamily {
	risky, total := planMerge(sources)

	// Families are rebuilt rather than reused, so a cached source list is never mutated.
	byName := make(map[string]*dto.MetricFamily, len(total))
	order := make([]string, 0, len(total))
	var seen map[string]struct{}
	if len(risky) > 0 {
		seen = make(map[string]struct{})
	}

	for _, src := range sources {
		for _, f := range src.families {
			name := f.GetName()
			metrics := f.Metric
			if _, check := risky[name]; check {
				metrics = dedupe(name, metrics, seen)
			}
			existing, ok := byName[name]
			if !ok {
				// Built field by field: dto.MetricFamily embeds a mutex, so copying it by value is not safe (govet copylocks).
				byName[name] = &dto.MetricFamily{
					Name:   f.Name,
					Help:   f.Help,
					Type:   f.Type,
					Unit:   f.Unit,
					Metric: append(make([]*dto.Metric, 0, total[name]), metrics...),
				}
				order = append(order, name)
				continue
			}
			existing.Metric = append(existing.Metric, metrics...)
		}
	}
	out := make([]*dto.MetricFamily, 0, len(byName))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// planMerge narrows the check to shared names and node-less families; a full check cost 27 % of allocations.
func planMerge(sources []source) (map[string]struct{}, map[string]int) {
	owners := make(map[string]string, 128)
	risky := make(map[string]struct{})
	total := make(map[string]int, 128)
	for _, src := range sources {
		for _, f := range src.families {
			name := f.GetName()
			if prev, ok := owners[name]; ok && prev != src.owner {
				risky[name] = struct{}{}
			} else if !ok {
				owners[name] = src.owner
			}
			if len(f.Metric) > 0 && !hasNodeLabel(f.Metric[0]) {
				risky[name] = struct{}{}
			}
			total[name] += len(f.Metric)
		}
	}
	return risky, total
}

// hasNodeLabel reports whether a metric carries the per-node label every per-node family must have.
func hasNodeLabel(m *dto.Metric) bool {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == "node" {
			return true
		}
	}
	return false
}

// dedupe keeps unseen series and returns the input untouched when nothing duplicates.
func dedupe(name string, metrics []*dto.Metric, seen map[string]struct{}) []*dto.Metric {
	var out []*dto.Metric
	for i, m := range metrics {
		key := seriesKey(name, m)
		if _, dup := seen[key]; dup {
			if out == nil { // first duplicate: copy what we kept so far
				out = append(make([]*dto.Metric, 0, len(metrics)), metrics[:i]...)
			}
			continue
		}
		seen[key] = struct{}{}
		if out != nil {
			out = append(out, m)
		}
	}
	if out == nil {
		return metrics
	}
	return out
}

// seriesKey identifies a series the way Prometheus does: name plus the full label set, order normalised.
func seriesKey(name string, m *dto.Metric) string {
	pairs := make([]string, 0, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		pairs = append(pairs, lp.GetName()+"\x00"+lp.GetValue())
	}
	sort.Strings(pairs)
	return name + "\x00" + strings.Join(pairs, "\x00")
}
