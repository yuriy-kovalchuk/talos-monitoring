// Package nodes discovers cluster nodes from the Kubernetes API and keeps a thread-safe view of them.
package nodes

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// retryInterval is the wait before retrying Kubernetes client construction.
const retryInterval = 10 * time.Second

// roleLabelPrefix is the Kubernetes convention for node role labels.
const roleLabelPrefix = "node-role.kubernetes.io/"

// Node is the monitor's view of a single Kubernetes (Talos) node.
type Node struct {
	Name        string
	IP          string // InternalIP; "" if not assigned yet
	Hostname    string
	KubeVersion string
	OS          string
	Arch        string
	Roles       []string // never empty; "worker" when no role labels are set
}

// Manager owns the node discovery loop and the snapshot.
type Manager struct {
	log *slog.Logger

	mu    sync.RWMutex
	nodes map[string]Node

	gauge prometheus.Gauge
	info  *prometheus.GaugeVec

	clientset kubernetes.Interface // pre-built (tests); otherwise built in Run
}

// New creates a Manager. Discovery starts when Run is called.
func New(log *slog.Logger, reg prometheus.Registerer) *Manager {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "talos_monitoring_k8s_nodes",
		Help: "Number of Kubernetes nodes currently discovered. A gauge: it falls when a node leaves.",
	})
	reg.MustRegister(gauge)
	// Discovery reads these, no collector does: without this family the UI would show them alone (§1.1).
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "talos_node_k8s_info",
		Help: "Kubernetes-side identity of a discovered node (value 1): internal IP, hostname, roles, kubelet version, OS and architecture.",
	}, []string{"node", "ip", "hostname", "roles", "kube_version", "os", "arch"})
	reg.MustRegister(info)
	return &Manager{
		log:   log,
		nodes: make(map[string]Node),
		gauge: gauge,
		info:  info,
	}
}

// SetClientset overrides the Kubernetes client; call it before Run (tests inject a fake).
func (m *Manager) SetClientset(cs kubernetes.Interface) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientset = cs
}

// Run starts discovery and blocks until ctx is done; client construction is retried, never fatal.
func (m *Manager) Run(ctx context.Context) {
	m.mu.RLock()
	cs := m.clientset
	m.mu.RUnlock()
	if cs == nil {
		for {
			var err error
			cs, err = kubernetesClientset()
			if err == nil {
				break
			}
			m.log.Error("kubernetes client unavailable, retrying", "err", err, "retry_in", retryInterval.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
			}
		}
	}
	m.runInformer(ctx, cs)
}

// Nodes returns a copy of the current node set, sorted by name.
func (m *Manager) Nodes() []Node {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Ready reports whether at least one node has been discovered.
func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.nodes) > 0
}

func (m *Manager) runInformer(ctx context.Context, cs kubernetes.Interface) {
	factory := informers.NewSharedInformerFactory(cs, 0)
	informer := factory.Core().V1().Nodes().Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: m.addNode,
		UpdateFunc: func(_, obj any) {
			m.addNode(obj)
		},
		DeleteFunc: m.deleteNode,
	}); err != nil {
		m.log.Error("adding node event handler failed", "err", err)
		return
	}
	factory.Start(ctx.Done())
	<-ctx.Done()
}

func (m *Manager) addNode(obj any) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	node := fromK8s(n)
	m.mu.Lock()
	_, existed := m.nodes[node.Name]
	m.nodes[node.Name] = node
	m.gauge.Set(float64(len(m.nodes)))
	m.publishInfo()
	m.mu.Unlock()
	if existed {
		m.log.Debug("node updated", "node", node.Name, "ip", node.IP)
	} else {
		m.log.Info("node discovered", "node", node.Name, "ip", node.IP, "roles", strings.Join(node.Roles, ","))
	}
}

func (m *Manager) deleteNode(obj any) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	m.mu.Lock()
	delete(m.nodes, n.Name)
	m.gauge.Set(float64(len(m.nodes)))
	m.publishInfo()
	m.mu.Unlock()
	m.log.Info("node removed", "node", n.Name)
}

// publishInfo resets the family wholesale: every label is mutable, so a stale series would linger.
func (m *Manager) publishInfo() {
	m.info.Reset()
	for _, n := range m.nodes {
		m.info.WithLabelValues(n.Name, n.IP, n.Hostname, strings.Join(n.Roles, ","),
			n.KubeVersion, n.OS, n.Arch).Set(1)
	}
}

// NewClientset resolves the client (in-cluster, then kubeconfig) so other watchers share the rules.
func NewClientset() (kubernetes.Interface, error) { return kubernetesClientset() }

// kubernetesClientset resolves in-cluster config first, then the default kubeconfig rules.
func kubernetesClientset() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			rules, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes config: %w", err)
		}
	}
	return kubernetes.NewForConfig(cfg)
}

func fromK8s(n *corev1.Node) Node {
	node := Node{
		Name:        n.Name,
		KubeVersion: n.Status.NodeInfo.KubeletVersion,
		OS:          n.Status.NodeInfo.OperatingSystem,
		Arch:        n.Status.NodeInfo.Architecture,
	}
	for _, addr := range n.Status.Addresses {
		switch {
		case addr.Type == corev1.NodeInternalIP && addr.Address != "" && node.IP == "":
			node.IP = addr.Address
		case addr.Type == corev1.NodeHostName && addr.Address != "" && node.Hostname == "":
			node.Hostname = addr.Address
		}
	}
	roles := make([]string, 0, 1)
	for label := range n.Labels {
		if r, ok := strings.CutPrefix(label, roleLabelPrefix); ok && r != "" {
			roles = append(roles, r)
		}
	}
	if len(roles) == 0 {
		roles = append(roles, "worker")
	}
	sort.Strings(roles)
	node.Roles = roles
	return node
}
