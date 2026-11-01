package etcd

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/metricguard"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

const (
	self  = 0x8ecba9ca968d7a0a
	other = 0x5b4f0bbc139fe8d0
)

type fakeAPI struct {
	status  *machine.EtcdMemberStatus
	members int
	alarms  []*machine.EtcdMemberAlarm
	err     error
}

func (f *fakeAPI) EtcdStatus(context.Context) (*machine.EtcdStatusResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &machine.EtcdStatusResponse{Messages: []*machine.EtcdStatus{{MemberStatus: f.status}}}, nil
}

func (f *fakeAPI) EtcdMemberList(context.Context) (*machine.EtcdMemberListResponse, error) {
	m := make([]*machine.EtcdMember, f.members)
	for i := range m {
		m[i] = &machine.EtcdMember{Id: uint64(i + 1)}
	}
	return &machine.EtcdMemberListResponse{Messages: []*machine.EtcdMembers{{Members: m}}}, nil
}

func (f *fakeAPI) EtcdAlarmList(context.Context) (*machine.EtcdAlarmListResponse, error) {
	return &machine.EtcdAlarmListResponse{Messages: []*machine.EtcdAlarm{{MemberAlarms: f.alarms}}}, nil
}

// healthy is valefor's live status: a follower with a 640 MB file holding 108 MB of data.
func healthy() *fakeAPI {
	return &fakeAPI{
		status: &machine.EtcdMemberStatus{
			MemberId: self, Leader: other, ProtocolVersion: "3.6.0", StorageVersion: "3.6.0",
			DbSize: 640_000_000, DbSizeInUse: 108_000_000,
			RaftIndex: 228207571, RaftTerm: 411, RaftAppliedIndex: 228207571,
		},
		members: 3,
	}
}

func collectWith(t *testing.T, api *fakeAPI) (map[string]*dto.MetricFamily, *snapshot.Node, error) {
	t.Helper()
	snap := snapshot.New()
	c := New().WithSnapshot(snap)
	c.api = func(*collector.NodeClient) etcdAPI { return api }
	reg := prometheus.NewRegistry()
	err := c.Collect(context.Background(), &collector.NodeClient{Node: nodes.Node{Name: "cp1", IP: "10.0.0.1"}}, reg)
	fams, gErr := reg.Gather()
	if gErr != nil {
		t.Fatalf("Gather: %v", gErr)
	}
	out := make(map[string]*dto.MetricFamily, len(fams))
	for _, f := range fams {
		out[f.GetName()] = f
	}
	return out, snap.Node("cp1"), err
}

func value(t *testing.T, fams map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
next:
	for _, m := range fams[name].GetMetric() {
		got := map[string]string{}
		for _, l := range m.GetLabel() {
			got[l.GetName()] = l.GetValue()
		}
		for k, v := range labels {
			if got[k] != v {
				continue next
			}
		}
		return m.GetGauge().GetValue()
	}
	t.Fatalf("%s has no series matching %v", name, labels)
	return 0
}

func TestStatusIsExported(t *testing.T) {
	fams, snap, err := collectWith(t, healthy())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	node := map[string]string{"node": "cp1"}
	for name, want := range map[string]float64{
		"talos_etcd_db_size_bytes":        640_000_000,
		"talos_etcd_db_size_in_use_bytes": 108_000_000,
		"talos_etcd_has_leader":           1,
		"talos_etcd_is_leader":            0,
		"talos_etcd_raft_term":            411,
		"talos_etcd_members":              3,
	} {
		if got := value(t, fams, name, node); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// Formatted as talosctl prints it, so an operator can match the two.
	if got := value(t, fams, "talos_etcd_member_info", map[string]string{"member_id": "8ecba9ca968d7a0a"}); got != 1 {
		t.Error("member_info missing the talosctl-formatted member ID")
	}
	if snap.Etcd == nil || snap.Etcd.Leader || snap.Etcd.Members != 3 {
		t.Errorf("snapshot = %+v", snap.Etcd)
	}
}

func TestLeaderIsThisMember(t *testing.T) {
	api := healthy()
	api.status.Leader = self
	fams, _, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := value(t, fams, "talos_etcd_is_leader", map[string]string{"node": "cp1"}); got != 1 {
		t.Errorf("is_leader = %v, want 1", got)
	}
}

// Leader 0 is etcd's "no leader": the member lost quorum, and it must not read as "someone else leads".
func TestNoLeaderMeansNoQuorum(t *testing.T) {
	api := healthy()
	api.status.Leader = 0
	fams, _, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := value(t, fams, "talos_etcd_has_leader", map[string]string{"node": "cp1"}); got != 0 {
		t.Errorf("has_leader = %v, want 0", got)
	}
	if got := value(t, fams, "talos_etcd_is_leader", map[string]string{"node": "cp1"}); got != 0 {
		t.Errorf("is_leader = %v, want 0", got)
	}
}

// Every alarm type is exported 0/1 so an alert never arms on absent data, and only this member's alarms count.
func TestAlarmsAreThisMembersOwn(t *testing.T) {
	api := healthy()
	api.alarms = []*machine.EtcdMemberAlarm{
		{MemberId: self, Alarm: machine.EtcdMemberAlarm_NOSPACE},
		{MemberId: other, Alarm: machine.EtcdMemberAlarm_CORRUPT},
	}
	fams, snap, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := value(t, fams, "talos_etcd_alarm", map[string]string{"alarm": "NOSPACE"}); got != 1 {
		t.Errorf("NOSPACE = %v, want 1", got)
	}
	if got := value(t, fams, "talos_etcd_alarm", map[string]string{"alarm": "CORRUPT"}); got != 0 {
		t.Errorf("CORRUPT = %v, want 0: it belongs to another member", got)
	}
	if len(snap.Etcd.Alarms) != 1 || snap.Etcd.Alarms[0] != "NOSPACE" {
		t.Errorf("snapshot alarms = %v, want [NOSPACE]", snap.Etcd.Alarms)
	}
}

func TestRPCErrorFailsTheCollection(t *testing.T) {
	wantErr := errors.New("etcd down")
	_, _, err := collectWith(t, &fakeAPI{err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestAppliesOnlyToControlPlanes(t *testing.T) {
	c := New()
	if !c.AppliesTo(nodes.Node{Roles: []string{"control-plane"}}) {
		t.Error("a control plane was skipped")
	}
	if c.AppliesTo(nodes.Node{Roles: []string{"worker"}}) {
		t.Error("a worker was scheduled; Talos answers Unimplemented there")
	}
}

// The structural guard: the metric contract, and every snapshot string reachable from /metrics.
func TestMetricContractAndSnapshotCoverage(t *testing.T) {
	api := healthy()
	api.alarms = []*machine.EtcdMemberAlarm{{MemberId: self, Alarm: machine.EtcdMemberAlarm_NOSPACE}}
	families, snap, err := collectWith(t, api)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams := make([]*dto.MetricFamily, 0, len(families))
	for _, f := range families {
		fams = append(fams, f)
	}
	for _, v := range metricguard.Validate(fams, metricguard.Options{}) {
		t.Error(v)
	}
	for _, v := range metricguard.SnapshotCovered(snap.Etcd, fams, metricguard.Options{}) {
		t.Error(v)
	}
}
