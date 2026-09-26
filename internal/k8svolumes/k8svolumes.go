// Package k8svolumes watches PersistentVolumes so the node-side filesystem
// usage collected from Talos can be named.
//
// Talos knows a mounted volume only by its PersistentVolume name, which is what
// appears in the kubelet mount path. Everything a human needs — namespace,
// claim, storage class, requested size — lives on the Kubernetes object. The two
// are published as separate metrics joined on the `pv` label, which is the
// idiomatic Prometheus shape: node facts come from the node, cluster facts from
// the API server, and neither collector has to know about the other.
package k8svolumes

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Volume is the monitor's view of one PersistentVolume.
type Volume struct {
	Name         string // the PV name, e.g. pvc-39ea5829-...; the join key
	Namespace    string // claimRef namespace
	Claim        string // claimRef name
	StorageClass string
	Driver       string // CSI driver, when the PV is CSI-backed
	Capacity     int64  // requested capacity in bytes
	Phase        string
}

// Manager watches PersistentVolumes and keeps a snapshot.
type Manager struct {
	log *slog.Logger

	mu      sync.RWMutex
	volumes map[string]Volume

	info     *prometheus.GaugeVec
	capacity *prometheus.GaugeVec
	total    prometheus.Gauge
}

// New creates a Manager and registers its metrics.
func New(log *slog.Logger, reg prometheus.Registerer) *Manager {
	m := &Manager{
		log:     log,
		volumes: make(map[string]Volume),
		info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "talos_pv_info",
			Help: "PersistentVolume identity (value 1). Join to talos_volume_* on the pv label.",
		}, []string{"pv", "namespace", "claim", "storageclass", "driver", "phase"}),
		total: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "talos_monitoring_k8s_persistentvolumes",
			Help: "Number of PersistentVolumes currently discovered. A gauge: it falls when a PV is deleted.",
		}),
	}
	capacity := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_pv_capacity_bytes",
		Help: "Capacity requested by the PersistentVolume, as opposed to the filesystem size Talos reports.",
	}, []string{"pv"})
	m.capacity = capacity
	reg.MustRegister(m.info, m.total, capacity)
	return m
}

// Run starts the informer and blocks until ctx is done. It never fails hard:
// without PersistentVolume permissions the node-side usage metrics still work,
// they are just unnamed.
func (m *Manager) Run(ctx context.Context, cs kubernetes.Interface) {
	factory := informers.NewSharedInformerFactory(cs, 10*time.Minute)
	informer := factory.Core().V1().PersistentVolumes().Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.upsert(obj) },
		UpdateFunc: func(_, obj any) { m.upsert(obj) },
		DeleteFunc: m.remove,
	}); err != nil {
		m.log.Error("adding persistentvolume event handler failed", "err", err)
		return
	}
	factory.Start(ctx.Done())
	<-ctx.Done()
}

// Volumes returns the current snapshot, keyed by PV name.
func (m *Manager) Volumes() map[string]Volume {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Volume, len(m.volumes))
	for k, v := range m.volumes {
		out[k] = v
	}
	return out
}

func (m *Manager) upsert(obj any) {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok {
		return
	}
	v := Volume{Name: pv.Name, StorageClass: pv.Spec.StorageClassName, Phase: string(pv.Status.Phase)}
	if ref := pv.Spec.ClaimRef; ref != nil {
		v.Namespace, v.Claim = ref.Namespace, ref.Name
	}
	if csi := pv.Spec.CSI; csi != nil {
		v.Driver = csi.Driver
	}
	if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		v.Capacity = q.Value()
	}

	m.mu.Lock()
	m.volumes[v.Name] = v
	n := len(m.volumes)
	m.mu.Unlock()

	// The normal PV lifecycle is Pending → Bound; without this delete the
	// old label combination stays in the registry as a frozen series.
	m.info.DeletePartialMatch(prometheus.Labels{"pv": v.Name})
	m.info.WithLabelValues(v.Name, v.Namespace, v.Claim, v.StorageClass, v.Driver, v.Phase).Set(1)
	m.capacity.WithLabelValues(v.Name).Set(float64(v.Capacity))
	m.total.Set(float64(n))
}

func (m *Manager) remove(obj any) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok {
		return
	}
	m.mu.Lock()
	delete(m.volumes, pv.Name)
	n := len(m.volumes)
	m.mu.Unlock()

	m.info.DeletePartialMatch(prometheus.Labels{"pv": pv.Name})
	m.capacity.DeletePartialMatch(prometheus.Labels{"pv": pv.Name})
	m.total.Set(float64(n))
}
