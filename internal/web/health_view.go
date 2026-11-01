package web

import (
	"fmt"

	"github.com/dustin/go-humanize"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/snapshot"
)

// serviceRow collapses Talos's three booleans into one word, for display only.
type serviceRow struct {
	Name  string
	State string // healthy | failing | no check | stopped
	Tone  string // pill class suffix: ok | warn | down
}

// unmetRow is one condition keeping the machine from being ready.
type unmetRow struct {
	Name   string
	Reason string
}

// diagnosticRow is one warning Talos is raising, with its doc link.
type diagnosticRow struct {
	ID      string
	Message string
	URL     string
}

// healthView is the health page: what the node says about its own state.
type healthView struct {
	Available bool // the collector has reported at least once
	HasStage  bool
	Stage     string
	Ready     bool

	Services    []serviceRow
	Unmet       []unmetRow
	Diagnostics []diagnosticRow

	Total   int
	Healthy int
	Failing int // running, but the health check does not pass
	NoCheck int // running, publishes no health check
	Stopped int
	// Unhealthy is Failing+Stopped; NoCheck is not a fault, so it is excluded.
	Unhealthy int
}

// healthFrom builds the health page from the snapshot.
func healthFrom(n *snapshot.Node) healthView {
	if n == nil || n.Health == nil {
		return healthView{}
	}
	h := n.Health
	view := healthView{
		Available: true,
		HasStage:  h.HasMachineStatus,
		Stage:     h.Stage,
		Ready:     h.Ready,
		Total:     len(h.Services),
		Services:  make([]serviceRow, 0, len(h.Services)),
	}
	for _, svc := range h.Services {
		row := serviceRow{Name: svc.ID}
		switch {
		case !svc.Running:
			row.State, row.Tone = "stopped", "down"
			view.Stopped++
		case svc.Unknown:
			// No health check at all, so Healthy=false carries no information.
			row.State, row.Tone = "no check", "warn"
			view.NoCheck++
		case svc.Healthy:
			row.State, row.Tone = "healthy", "ok"
			view.Healthy++
		default:
			row.State, row.Tone = "failing", "down"
			view.Failing++
		}
		view.Services = append(view.Services, row)
	}
	view.Unhealthy = view.Failing + view.Stopped
	for _, c := range h.UnmetConditions {
		view.Unmet = append(view.Unmet, unmetRow{Name: c.Name, Reason: c.Reason})
	}
	for _, d := range h.Diagnostics {
		view.Diagnostics = append(view.Diagnostics, diagnosticRow{
			ID: d.ID, Message: d.Message,
			URL: "https://talos.dev/diagnostic/" + d.ID,
		})
	}
	return view
}

// timeSyncView pre-formats offsets: raw seconds render as 4.4597e-05, unreadable at a glance.
type timeSyncView struct {
	Available    bool
	Synced       bool
	KernelSynced bool
	SyncDisabled bool
	State        string
	Epoch        int
	Offset       string
	MaxError     string
	DriftPPM     string
}

// timeSyncFrom builds the clock strip from the snapshot.
func timeSyncFrom(n *snapshot.Node) timeSyncView {
	if n == nil || n.TimeSync == nil || !n.TimeSync.HasStatus {
		return timeSyncView{}
	}
	t := n.TimeSync
	view := timeSyncView{
		Available: true, Synced: t.Synced, SyncDisabled: t.SyncDisabled,
		Epoch: t.Epoch, KernelSynced: t.KernelSynced, State: t.State,
	}
	if t.HasAdjtime {
		view.Offset = shortDuration(t.OffsetSeconds)
		view.MaxError = shortDuration(t.MaxErrorSeconds)
		// The kernel reports a ratio; ppm is the unit clock people use.
		view.DriftPPM = fmt.Sprintf("%+.1f ppm", (t.FrequencyRatio-1)*1e6)
	}
	return view
}

// etcdView is a control-plane member's etcd card; nil on workers.
type etcdView struct {
	MemberID  string
	Version   string
	HasLeader bool
	Leader    bool
	Learner   bool
	Members   int
	DBSize    string
	DBInUse   string
	InUsePct  int
	ApplyLag  string // raft entries not yet applied
	Errors    int
	Alarms    []string
}

// etcdFrom builds the etcd card from the snapshot.
func etcdFrom(n *snapshot.Node) *etcdView {
	if n == nil || n.Etcd == nil {
		return nil
	}
	e := n.Etcd
	v := &etcdView{
		MemberID: e.MemberID, Version: e.ProtocolVersion,
		HasLeader: e.HasLeader, Leader: e.Leader, Learner: e.Learner, Members: e.Members,
		DBSize: humanize.Bytes(uint64(e.DBSizeBytes)), DBInUse: humanize.Bytes(uint64(e.DBInUseBytes)),
		ApplyLag: humanize.Comma(int64(e.RaftIndex - e.RaftAppliedIndex)),
		Errors:   e.Errors, Alarms: e.Alarms,
	}
	if e.DBSizeBytes > 0 {
		v.InUsePct = int(e.DBInUseBytes / e.DBSizeBytes * 100)
	}
	return v
}
