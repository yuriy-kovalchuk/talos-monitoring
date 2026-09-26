package nodeapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
	timepb "github.com/siderolabs/talos/pkg/machinery/api/time"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/collector"
	"github.com/yuriy-kovalchuk/talos-monitoring/internal/nodes"
)

const testSha = "abc123def456"

// fakeAPI is a canned machineAPI.
type fakeAPI struct {
	ver   *machinepb.VersionResponse
	tResp *timepb.TimeResponse
	sys   *machinepb.SystemStatResponse
	load  *machinepb.LoadAvgResponse
	err   error

	// loadErr fails only the LoadAvg RPC, so a test can pin that the rest of
	// the collection survives it.
	loadErr error
}

func (f fakeAPI) Version(context.Context) (*machinepb.VersionResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ver, nil
}

func (f fakeAPI) Time(context.Context) (*timepb.TimeResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tResp, nil
}

func (f fakeAPI) LoadAvg(context.Context) (*machinepb.LoadAvgResponse, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.load, nil
}

func (f fakeAPI) SystemStat(context.Context) (*machinepb.SystemStatResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sys, nil
}

var nodeNow = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

// unixSec converts the fixed positive test times to the wire format.
func unixSec(t time.Time) uint64 { return uint64(t.Unix()) } //nolint:gosec // test times are positive

func testNode() *collector.NodeClient {
	return &collector.NodeClient{Node: nodes.Node{Name: "node-a", IP: "10.0.0.1"}}
}

func testCollector(api machineAPI) *Collector {
	c := New()
	c.newAPI = func(*collector.NodeClient) machineAPI { return api }
	return c
}

func versionResponse() *machinepb.VersionResponse {
	return &machinepb.VersionResponse{Messages: []*machinepb.Version{{
		Version: &machinepb.VersionInfo{Tag: "v1.13.4", Sha: testSha, Arch: "amd64", Built: "2026-07-01T00:00:00Z"},
	}}}
}

func timeResponse() *timepb.TimeResponse {
	return &timepb.TimeResponse{Messages: []*timepb.Time{{Remotetime: timestamppb.New(nodeNow)}}}
}

func statResponse() *machinepb.SystemStatResponse {
	return &machinepb.SystemStatResponse{Messages: []*machinepb.SystemStat{{
		BootTime:       unixSec(nodeNow.Add(-1 * time.Hour)),
		ProcessRunning: 5,
		ProcessBlocked: 2,
	}}}
}

// findMetric looks up one metric value by family and label match.
func findMetric(fams []*dto.MetricFamily, family string, want map[string]string) (float64, bool) {
	for _, f := range fams {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			ok := true
			for k, v := range want {
				if labels[k] != v {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
			return 0, true
		}
	}
	return 0, false
}

func collect(t *testing.T, c *Collector, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return fams
}

func TestCollectStatus(t *testing.T) {
	c := testCollector(fakeAPI{ver: versionResponse(), tResp: timeResponse(), sys: statResponse()})
	fams := collect(t, c, prometheus.NewRegistry())

	if v, ok := findMetric(fams, "talos_node_version_info", map[string]string{
		"node": "node-a", "version": "v1.13.4", "sha": testSha, "arch": "amd64", "built": "2026-07-01T00:00:00Z",
	}); !ok || v != 1 {
		t.Errorf("version_info: got %v %v, want 1", v, ok)
	}
	if v, ok := findMetric(fams, "talos_node_uptime_seconds", map[string]string{"node": "node-a"}); !ok || v != 3600 {
		t.Errorf("uptime: got %v %v, want 3600", v, ok)
	}
	if v, ok := findMetric(fams, "talos_node_processes", map[string]string{"node": "node-a", "state": "running"}); !ok || v != 5 {
		t.Errorf("processes running: got %v %v, want 5", v, ok)
	}
	if v, ok := findMetric(fams, "talos_node_processes", map[string]string{"node": "node-a", "state": "blocked"}); !ok || v != 2 {
		t.Errorf("processes blocked: got %v %v, want 2", v, ok)
	}
}

func TestCollectBootTimeNotBeforeNodeTime(t *testing.T) {
	sys := statResponse()
	sys.GetMessages()[0].BootTime = unixSec(nodeNow.Add(1 * time.Hour))
	c := testCollector(fakeAPI{ver: versionResponse(), tResp: timeResponse(), sys: sys})
	err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "boot time") {
		t.Fatalf("err = %v, want boot time error", err)
	}
}

func TestCollectAPIError(t *testing.T) {
	c := testCollector(fakeAPI{err: errors.New("boom")})
	err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want wrapped 'boom'", err)
	}
}

func TestCollectEmptyVersion(t *testing.T) {
	c := testCollector(fakeAPI{
		ver:   &machinepb.VersionResponse{},
		tResp: timeResponse(),
		sys:   statResponse(),
	})
	err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "version response is empty") {
		t.Fatalf("err = %v, want 'version response is empty'", err)
	}
}

func TestCollectEmptySystemStat(t *testing.T) {
	c := testCollector(fakeAPI{
		ver:   versionResponse(),
		tResp: timeResponse(),
		sys:   &machinepb.SystemStatResponse{Messages: []*machinepb.SystemStat{{Cpu: []*machinepb.CPUStat{{}}}}},
	})
	err := c.Collect(context.Background(), testNode(), prometheus.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "system stat response is empty") {
		t.Fatalf("err = %v, want 'system stat response is empty'", err)
	}
}

// Load average is the least important thing this collector reports. Losing the
// version, uptime and process counts because that one RPC failed would be a bad
// trade, so a LoadAvg failure must leave the series absent, not fail the scrape.
func TestLoadAvgFailureDoesNotFailTheCollection(t *testing.T) {
	c := testCollector(fakeAPI{
		ver: versionResponse(), tResp: timeResponse(), sys: statResponse(),
		loadErr: errors.New("unavailable"),
	})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect failed because LoadAvg did: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if _, ok := findMetric(fams, "talos_node_load1", map[string]string{"node": "node-a"}); ok {
		t.Error("load series exported despite the RPC failing; 0.0 would read as an idle node")
	}
	// The rest of the collection is unaffected.
	if _, ok := findMetric(fams, "talos_node_uptime_seconds", map[string]string{"node": "node-a"}); !ok {
		t.Error("uptime was lost when LoadAvg failed")
	}
}

func TestLoadAvgIsExported(t *testing.T) {
	c := testCollector(fakeAPI{
		ver: versionResponse(), tResp: timeResponse(), sys: statResponse(),
		load: &machinepb.LoadAvgResponse{Messages: []*machinepb.LoadAvg{
			{Load1: 1.92, Load5: 1.53, Load15: 1.47},
		}},
	})
	reg := prometheus.NewRegistry()
	if err := c.Collect(context.Background(), testNode(), reg); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for name, want := range map[string]float64{
		"talos_node_load1": 1.92, "talos_node_load5": 1.53, "talos_node_load15": 1.47,
	} {
		got, ok := findMetric(fams, name, map[string]string{"node": "node-a"})
		if !ok {
			t.Fatalf("family %q missing", name)
		}
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}
