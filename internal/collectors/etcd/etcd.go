// Package etcd reports each control-plane member's etcd health through the Talos API (os:reader-safe).
package etcd

import (
	"context"
	"fmt"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// Name is the collector name (used in the --collectors.<name>.enabled and TTL flags).
const Name = "etcd"

// controlPlaneRole is the Kubernetes role label value etcd members carry.
const controlPlaneRole = "control-plane"

// alarmTypes are exported 0/1 on every member, so an alert never arms on absent data.
var alarmTypes = []machine.EtcdMemberAlarm_AlarmType{machine.EtcdMemberAlarm_NOSPACE, machine.EtcdMemberAlarm_CORRUPT}

// etcdAPI is the part of the Talos client the collector uses (test seam).
type etcdAPI interface {
	EtcdStatus(ctx context.Context) (*machine.EtcdStatusResponse, error)
	EtcdMemberList(ctx context.Context) (*machine.EtcdMemberListResponse, error)
	EtcdAlarmList(ctx context.Context) (*machine.EtcdAlarmListResponse, error)
}

type talosEtcd struct{ c *talosclient.Client }

func (t talosEtcd) EtcdStatus(ctx context.Context) (*machine.EtcdStatusResponse, error) {
	return t.c.EtcdStatus(ctx)
}

// EtcdMemberList asks the cluster, not the local member, so a member that lost quorum shows it.
func (t talosEtcd) EtcdMemberList(ctx context.Context) (*machine.EtcdMemberListResponse, error) {
	return t.c.EtcdMemberList(ctx, &machine.EtcdMemberListRequest{})
}

func (t talosEtcd) EtcdAlarmList(ctx context.Context) (*machine.EtcdAlarmListResponse, error) {
	return t.c.EtcdAlarmList(ctx)
}

// Collector reads EtcdStatus, EtcdMemberList and EtcdAlarmList on control-plane nodes.
type Collector struct {
	api  func(node *collector.NodeClient) etcdAPI
	snap *snapshot.Store
}

// New returns the etcd collector.
func New() *Collector {
	return &Collector{api: func(node *collector.NodeClient) etcdAPI { return talosEtcd{node.Client} }}
}

// WithSnapshot points the collector at a snapshot store.
func (c *Collector) WithSnapshot(s *snapshot.Store) *Collector {
	c.snap = s
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return Name }

// Class implements collector.Collector: leadership and DB growth are live signals.
func (c *Collector) Class() collector.Class { return collector.Live }

// AppliesTo implements collector.NodeFilter: workers run no etcd, and Talos answers Unimplemented there.
func (c *Collector) AppliesTo(n nodes.Node) bool { return slices.Contains(n.Roles, controlPlaneRole) }

// Collect reads this member's status, the cluster's member list and this member's alarms.
func (c *Collector) Collect(ctx context.Context, node *collector.NodeClient, reg prometheus.Registerer) error {
	ctx = talosclient.WithNode(ctx, node.Node.IP)
	api := c.api(node)
	name := node.Node.Name

	statusResp, err := api.EtcdStatus(ctx)
	if err != nil {
		return fmt.Errorf("etcd status: %w", err)
	}
	if len(statusResp.GetMessages()) == 0 || statusResp.GetMessages()[0].GetMemberStatus() == nil {
		return fmt.Errorf("etcd status response is empty")
	}
	status := statusResp.GetMessages()[0].GetMemberStatus()

	membersResp, err := api.EtcdMemberList(ctx)
	if err != nil {
		return fmt.Errorf("etcd member list: %w", err)
	}
	alarmsResp, err := api.EtcdAlarmList(ctx)
	if err != nil {
		return fmt.Errorf("etcd alarm list: %w", err)
	}

	view := snapshot.Etcd{
		MemberID:         memberID(status.GetMemberId()),
		ProtocolVersion:  status.GetProtocolVersion(),
		StorageVersion:   status.GetStorageVersion(),
		DBSizeBytes:      float64(status.GetDbSize()),
		DBInUseBytes:     float64(status.GetDbSizeInUse()),
		HasLeader:        status.GetLeader() != 0,
		Leader:           status.GetLeader() != 0 && status.GetLeader() == status.GetMemberId(),
		Learner:          status.GetIsLearner(),
		RaftTerm:         float64(status.GetRaftTerm()),
		RaftIndex:        float64(status.GetRaftIndex()),
		RaftAppliedIndex: float64(status.GetRaftAppliedIndex()),
		Errors:           len(status.GetErrors()),
	}
	for _, m := range membersResp.GetMessages() {
		view.Members += len(m.GetMembers())
	}
	// The alarm list is cluster-wide; each member reports only its own, so a sum across nodes counts once.
	for _, m := range alarmsResp.GetMessages() {
		for _, a := range m.GetMemberAlarms() {
			if a.GetMemberId() == status.GetMemberId() && a.GetAlarm() != machine.EtcdMemberAlarm_NONE {
				view.Alarms = append(view.Alarms, a.GetAlarm().String())
			}
		}
	}
	slices.Sort(view.Alarms)
	view.Alarms = slices.Compact(view.Alarms)

	register(name, view, reg)
	if c.snap != nil {
		c.snap.Update(name, func(n *snapshot.Node) { n.Etcd = &view })
	}
	return nil
}

// memberID formats an etcd member ID the way talosctl and etcdctl print it.
func memberID(id uint64) string { return fmt.Sprintf("%016x", id) }

// register exports one member's view; every family is per node, so cardinality is the control-plane count.
func register(node string, v snapshot.Etcd, reg prometheus.Registerer) {
	g := func(name, help string) *prometheus.GaugeVec {
		return collector.GaugeVec(name, help, "node")
	}
	info := collector.GaugeVec("talos_etcd_member_info",
		"etcd member identity (value 1): member ID as talosctl prints it, and the protocol and storage versions.",
		"node", "member_id", "protocol_version", "storage_version")
	dbSize := g("talos_etcd_db_size_bytes", "Size of the member's backend database file, in bytes.")
	dbInUse := g("talos_etcd_db_size_in_use_bytes",
		"Bytes of the backend database actually in use. The gap to talos_etcd_db_size_bytes is free space a defragment would return.")
	hasLeader := g("talos_etcd_has_leader", "1 when the member knows a leader; 0 means the cluster has no quorum from this member's view.")
	leader := g("talos_etcd_is_leader", "1 when this member is the raft leader. Exactly one member should report 1.")
	learner := g("talos_etcd_is_learner", "1 while the member is a non-voting learner still catching up.")
	term := g("talos_etcd_raft_term", "Current raft term. It increases on every leader election, so a fast-rising term means leadership churn.")
	index := g("talos_etcd_raft_index", "Index of the latest raft log entry the member has.")
	applied := g("talos_etcd_raft_applied_index",
		"Index of the latest raft entry applied to the backend. A growing gap to talos_etcd_raft_index means the member is falling behind.")
	errs := g("talos_etcd_status_errors", "Number of errors etcd reports in the member's status (for example alarms raised).")
	members := g("talos_etcd_members", "Number of members in the etcd cluster, as this member sees it.")
	alarm := collector.GaugeVec("talos_etcd_alarm",
		"1 while the member has the alarm raised: NOSPACE (database hit its quota; writes refused) or CORRUPT. 0 otherwise.",
		"node", "alarm")
	reg.MustRegister(info, dbSize, dbInUse, hasLeader, leader, learner, term, index, applied, errs, members, alarm)

	info.WithLabelValues(node, v.MemberID, v.ProtocolVersion, v.StorageVersion).Set(1)
	dbSize.WithLabelValues(node).Set(v.DBSizeBytes)
	dbInUse.WithLabelValues(node).Set(v.DBInUseBytes)
	hasLeader.WithLabelValues(node).Set(collector.BoolValue(v.HasLeader))
	leader.WithLabelValues(node).Set(collector.BoolValue(v.Leader))
	learner.WithLabelValues(node).Set(collector.BoolValue(v.Learner))
	term.WithLabelValues(node).Set(v.RaftTerm)
	index.WithLabelValues(node).Set(v.RaftIndex)
	applied.WithLabelValues(node).Set(v.RaftAppliedIndex)
	errs.WithLabelValues(node).Set(float64(v.Errors))
	members.WithLabelValues(node).Set(float64(v.Members))
	for _, a := range alarmTypes {
		alarm.WithLabelValues(node, a.String()).Set(collector.BoolValue(slices.Contains(v.Alarms, a.String())))
	}
}

var (
	_ collector.Collector  = (*Collector)(nil)
	_ collector.NodeFilter = (*Collector)(nil)
)
