// Package metricguard turns this project's metric conventions into checks a collector test can run.
package metricguard

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

// unitWords are units that belong in a name's suffix, never in a label.
var unitWords = map[string]bool{
	"bytes": true, "seconds": true, "hertz": true, "celsius": true, "volts": true,
	"watts": true, "mhz": true, "ghz": true, "ms": true, "percent": true,
	"pct": true, "ratio": true, "kb": true, "mb": true, "gb": true,
}

// Options relaxes checks a collector has a documented reason to fail.
type Options struct {
	// ClusterScoped are families that legitimately carry no node label (talos_pv_*).
	ClusterScoped map[string]bool

	// UncoveredStrings are snapshot fields deliberately absent from any label; each needs a reason.
	UncoveredStrings map[string]bool
}

// Validate returns one message per metric-contract violation in a gathered family set.
func Validate(fams []*dto.MetricFamily, opts Options) []string {
	var out []string
	for _, f := range fams {
		name := f.GetName()
		if !strings.HasPrefix(name, "talos_") {
			continue
		}
		isCounter := f.GetType() == dto.MetricType_COUNTER
		if strings.HasSuffix(name, "_total") && !isCounter {
			out = append(out, fmt.Sprintf("%s: name ends in _total but type is %s", name, f.GetType()))
		}
		if isCounter && !strings.HasSuffix(name, "_total") {
			out = append(out, fmt.Sprintf("%s: is a counter but the name does not end in _total", name))
		}
		if f.GetHelp() == "" {
			out = append(out, name+": no HELP text")
		}
		for _, m := range f.GetMetric() {
			if strings.HasSuffix(name, "_info") && m.GetGauge().GetValue() != 1 {
				out = append(out, fmt.Sprintf("%s: _info family carries the value %v; _info metrics hold identity in labels and always read 1",
					name, m.GetGauge().GetValue()))
			}
			hasNode := false
			for _, l := range m.GetLabel() {
				if l.GetName() == "node" {
					hasNode = true
				}
				if unitWords[strings.ToLower(l.GetName())] {
					out = append(out, fmt.Sprintf("%s: label %q is a unit; units belong in the metric name, not a label", name, l.GetName()))
				}
			}
			if !hasNode && !opts.ClusterScoped[name] {
				out = append(out, name+": per-node family with no `node` label")
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

// SnapshotCovered enforces §1.1 for strings: every non-empty snapshot string must be a label value.
func SnapshotCovered(section any, fams []*dto.MetricFamily, opts Options) []string {
	labels := map[string]bool{}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				labels[l.GetValue()] = true
			}
		}
	}
	var out []string
	for _, fv := range strFields(reflect.ValueOf(section), "") {
		if fv.value == "" || opts.UncoveredStrings[fv.path] {
			continue
		}
		if !labels[fv.value] {
			out = append(out, fmt.Sprintf("snapshot field %s = %q appears in no metric label: "+
				"the UI can render it but Prometheus cannot (§1.1)", fv.path, fv.value))
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

type strField struct{ path, value string }

// strFields collects string fields with dotted paths; slice elements share their field's path.
func strFields(v reflect.Value, path string) []strField {
	var out []strField
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			out = append(out, strFields(v.Elem(), path)...)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			out = append(out, strFields(v.Index(i), path)...)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			p := t.Field(i).Name
			if path != "" {
				p = path + "." + p
			}
			out = append(out, strFields(v.Field(i), p)...)
		}
	case reflect.String:
		out = append(out, strField{path: path, value: v.String()})
	}
	return out
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
