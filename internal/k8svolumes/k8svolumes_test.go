package k8svolumes

import (
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func newTestManager(t *testing.T) (*Manager, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), reg), reg
}

func pv(name, ns, claim, class, driver, phase string, capacity int64) *corev1.PersistentVolume {
	v := &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(capacity, resource.BinarySI),
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.PersistentVolumePhase(phase)},
	}
	v.Name = name
	v.Spec.StorageClassName = class
	if claim != "" {
		v.Spec.ClaimRef = &corev1.ObjectReference{Namespace: ns, Name: claim}
	}
	if driver != "" {
		v.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: driver}
	}
	return v
}

// familyMetrics returns the metrics of one family, or nil when absent.
func familyMetrics(t *testing.T, reg *prometheus.Registry, family string) []*dto.Metric {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() == family {
			return f.GetMetric()
		}
	}
	return nil
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// The normal PV lifecycle is Pending → Bound. The upsert sets label values
// on a long-lived registry, so without a delete first the old label
// combination survives as a frozen series and count by (phase) inflates.
func TestUpsertPhaseChangeReplacesStaleSeries(t *testing.T) {
	m, reg := newTestManager(t)
	m.upsert(pv("pv1", "ns", "claim1", "local", "csi.example.com", "Pending", 1<<30))
	m.upsert(pv("pv1", "ns", "claim1", "local", "csi.example.com", "Bound", 1<<30))

	got := familyMetrics(t, reg, "talos_pv_info")
	if len(got) != 1 {
		t.Fatalf("talos_pv_info has %d series after phase change, want 1 (the Pending series must be replaced)", len(got))
	}
	if phase := labelValue(got[0], "phase"); phase != "Bound" {
		t.Errorf("phase = %q, want Bound", phase)
	}
}

func TestRemoveDeletesPVSeries(t *testing.T) {
	m, reg := newTestManager(t)
	m.upsert(pv("pv1", "ns", "claim1", "local", "", "Bound", 1<<30))
	m.upsert(pv("pv2", "ns", "claim2", "other", "driver.example.com", "Available", 2<<30))

	if got := familyMetrics(t, reg, "talos_monitoring_k8s_persistentvolumes"); len(got) != 1 || got[0].GetGauge().GetValue() != 2 {
		t.Fatalf("total = %v, want 2", got)
	}

	m.remove(pv("pv1", "ns", "claim1", "local", "", "Bound", 1<<30))

	for _, family := range []string{"talos_pv_info", "talos_pv_capacity_bytes"} {
		for _, mt := range familyMetrics(t, reg, family) {
			if labelValue(mt, "pv") == "pv1" {
				t.Errorf("%s still carries the removed pv1", family)
			}
		}
	}
	if got := familyMetrics(t, reg, "talos_monitoring_k8s_persistentvolumes"); len(got) != 1 || got[0].GetGauge().GetValue() != 1 {
		t.Fatalf("total = %v, want 1 after removal", got)
	}
}
