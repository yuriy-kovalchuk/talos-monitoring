package nodes

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// newTestManager builds a Manager around a fake clientset and starts Run.
func newTestManager(t *testing.T, objs ...runtime.Object) (*Manager, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := New(testLog, reg)
	m.clientset = fake.NewSimpleClientset(objs...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	return m, reg
}

func testNode(name, ip string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ip},
				{Type: corev1.NodeHostName, Address: name},
			},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion:  "v1.35.0",
				OperatingSystem: "linux",
				Architecture:    "arm64",
			},
		},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDiscoverFromInitialList(t *testing.T) {
	m, _ := newTestManager(t,
		testNode("node-1", "10.0.0.10", map[string]string{"node-role.kubernetes.io/control-plane": ""}),
		testNode("node-2", "10.0.0.11", nil),
	)

	waitFor(t, "two nodes discovered", func() bool { return len(m.Nodes()) == 2 })

	got := m.Nodes()
	if got[0].Name != "node-1" || got[1].Name != "node-2" {
		t.Fatalf("names not sorted by name: %v", got)
	}
	want := Node{
		Name:        "node-1",
		IP:          "10.0.0.10",
		Hostname:    "node-1",
		KubeVersion: "v1.35.0",
		OS:          "linux",
		Arch:        "arm64",
		Roles:       []string{"control-plane"},
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("node-1: got %+v, want %+v", got[0], want)
	}
	if len(got[1].Roles) != 1 || got[1].Roles[0] != "worker" {
		t.Errorf("node-2 roles: got %v, want [worker]", got[1].Roles)
	}
	if !m.Ready() {
		t.Error("Ready() = false, want true")
	}
}

func TestWatchAddAndDelete(t *testing.T) {
	m, _ := newTestManager(t, testNode("node-1", "10.0.0.10", nil))

	waitFor(t, "initial node", func() bool { return len(m.Nodes()) == 1 })

	if _, err := m.clientset.CoreV1().Nodes().Create(t.Context(), testNode("node-2", "10.0.0.11", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "node-2 added via watch", func() bool { return len(m.Nodes()) == 2 })

	if err := m.clientset.CoreV1().Nodes().Delete(t.Context(), "node-2", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "node-2 removed via watch", func() bool {
		for _, n := range m.Nodes() {
			if n.Name == "node-2" {
				return false
			}
		}
		return true
	})
	if m.Ready() != true {
		t.Error("Ready() = false after delete, want true (node-1 remains)")
	}
}

func TestEmptyCluster(t *testing.T) {
	m, reg := newTestManager(t)

	waitFor(t, "gauge to be registered", func() bool {
		fams, err := reg.Gather()
		return err == nil && len(fams) == 1
	})
	if m.Ready() {
		t.Error("Ready() = true, want false (empty cluster)")
	}
	if got := m.Nodes(); len(got) != 0 {
		t.Errorf("Nodes() = %v, want empty", got)
	}
}

func TestFromK8s(t *testing.T) {
	tests := []struct {
		name string
		in   *corev1.Node
		want Node
	}{
		{
			name: "full",
			in:   testNode("n1", "10.0.0.1", map[string]string{"node-role.kubernetes.io/worker": "", "node-role.kubernetes.io/control-plane": ""}),
			want: Node{
				Name: "n1", IP: "10.0.0.1", Hostname: "n1",
				KubeVersion: "v1.35.0", OS: "linux", Arch: "arm64",
				Roles: []string{"control-plane", "worker"},
			},
		},
		{
			name: "no ip, hostname from addresses",
			in: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "n2"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: "host-2"}},
				},
			},
			want: Node{Name: "n2", Hostname: "host-2", Roles: []string{"worker"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fromK8s(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fromK8s: got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestK8sInfoTracksTheNodeSet pins talos_node_k8s_info, the one dashboard fact
// that comes from the Kubernetes node object rather than a collector.
//
// Every label on it is mutable — an IP is re-assigned, a kubelet is upgraded —
// so a Set-only update would leave the pre-change series behind and export the
// node twice, one row with each version. The metric is rebuilt from the node
// set on every change instead; this pins that it is, and that a removed node
// takes its series with it.
func TestK8sInfoTracksTheNodeSet(t *testing.T) {
	m, reg := newTestManager(t,
		testNode("node-1", "10.0.0.10", map[string]string{roleLabelPrefix + "control-plane": ""}))

	const name = "talos_node_k8s_info"
	waitFor(t, "the info series", func() bool {
		n, err := testutil.GatherAndCount(reg, name)
		return err == nil && n == 1
	})

	want := `# HELP talos_node_k8s_info Kubernetes-side identity of a discovered node (value 1): internal IP, hostname, roles, kubelet version, OS and architecture.
# TYPE talos_node_k8s_info gauge
talos_node_k8s_info{arch="arm64",hostname="node-1",ip="10.0.0.10",kube_version="v1.35.0",node="node-1",os="linux",roles="control-plane"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), name); err != nil {
		t.Error(err)
	}

	upgraded := testNode("node-1", "10.0.0.10", map[string]string{roleLabelPrefix + "control-plane": ""})
	upgraded.Status.NodeInfo.KubeletVersion = "v1.36.1"
	if _, err := m.clientset.CoreV1().Nodes().Update(t.Context(), upgraded, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the upgraded kubelet version", func() bool {
		return testutil.ToFloat64(m.info.WithLabelValues(
			"node-1", "10.0.0.10", "node-1", "control-plane", "v1.36.1", "linux", "arm64")) == 1
	})
	if n, err := testutil.GatherAndCount(reg, name); err != nil || n != 1 {
		t.Errorf("after the kubelet upgrade: %d series (err %v), want 1 — the old one was not dropped", n, err)
	}

	if err := m.clientset.CoreV1().Nodes().Delete(t.Context(), "node-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the series to go with the node", func() bool {
		n, err := testutil.GatherAndCount(reg, name)
		return err == nil && n == 0
	})
}
