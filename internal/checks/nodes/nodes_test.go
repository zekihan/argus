package nodes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
)

var now = time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC)

const version = "v1.37.2+k3s1"

// Shapes from the live cluster: k3s nodes report no NetworkUnavailable.
func node(t *testing.T, name string) Node {
	var n Node
	body := `{"metadata":{"name":"` + name + `"},"status":{"allocatable":{"cpu":"12","memory":"14279684Ki"},"nodeInfo":{"kubeletVersion":"` + version + `"},
	"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"},{"type":"Ready","status":"True"}]}}`
	if err := json.Unmarshal([]byte(body), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func metrics(name, cpu, memory string) NodeMetrics {
	m := NodeMetrics{Timestamp: now.Add(-30 * time.Second).Format(time.RFC3339), Usage: map[string]string{"cpu": cpu, "memory": memory}}
	m.Metadata.Name = name
	return m
}

func longhornNode(t *testing.T, name string, available int64) LonghornNode {
	var l LonghornNode
	body := `{"metadata":{"name":"` + name + `"},"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"True"}],
	"diskStatus":{"permanent":{"conditions":[{"type":"Ready","status":"True","message":"Disk /var/lib/longhorn ready"},{"type":"Schedulable","status":"True"}],"storageAvailable":` +
		jsonInt(available) + `,"storageMaximum":67049664512}}}}`
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

type fakeAPI struct{ sample Sample }

func (f *fakeAPI) Collect(context.Context) (Sample, error) { return f.sample, nil }

func cfg() config.Nodes {
	c := config.Nodes{Expected: []string{"agent", "control"}, LonghornNamespace: "longhorn"}
	full := config.Config{Checks: config.Checks{Nodes: &c}}
	full.ApplyDefaults()
	return *full.Checks.Nodes
}

func healthy(t *testing.T) Sample {
	return Sample{Nodes: []Node{node(t, "agent"), node(t, "control")}, ServerVersion: version,
		Metrics:  []NodeMetrics{metrics("agent", "1500m", "4Gi"), metrics("control", "500m", "2Gi")},
		Longhorn: []LonghornNode{longhornNode(t, "agent", 46556774400)}}
}

func evaluate(t *testing.T, check *Check, s Sample, at time.Time, name string) map[string]string {
	t.Helper()
	c := check.Evaluate(s, at)
	item, ok := c.Find(name)
	if !ok {
		t.Fatal(name)
	}
	return item.Results
}

func TestHealthyNodesAndControlPlaneWithoutLonghorn(t *testing.T) {
	h := checktest.New(New(cfg(), &fakeAPI{sample: healthy(t)}), now).Refresh(t)
	if code, e := h.Get(t, Route); code != 200 {
		t.Fatal(e)
	}
	_, agent := h.Get(t, Route+"/agent")
	_, control := h.Get(t, Route+"/control")
	if agent.Results["longhorn_disk"] != "ok" || control.Results["longhorn_disk"] != "n/a" || agent.Results["network"] != "n/a" || agent.Values["cpuPercent"].(float64) != 12.5 {
		t.Fatal(agent, control)
	}
}

func TestConditionRules(t *testing.T) {
	check := New(cfg(), nil)
	for _, tc := range []struct {
		edit func(*Node)
		dim  string
		want string
	}{
		{func(n *Node) { n.Status.Conditions[3].Status = "False" }, "ready", "not_ready"},
		{func(n *Node) { n.Status.Conditions[3].Status = "Unknown" }, "ready", "unknown"},
		{func(n *Node) { n.Status.Conditions = n.Status.Conditions[:3] }, "ready", "missing_condition"},
		{func(n *Node) { n.Status.Conditions[0].Status = "True" }, "memory_pressure", "pressure"},
		{func(n *Node) { n.Status.Conditions = n.Status.Conditions[1:] }, "memory_pressure", "missing_condition"},
		{func(n *Node) { n.Status.Conditions[1].Status = "True" }, "disk_pressure", "pressure"},
		{func(n *Node) { n.Status.Conditions[2].Status = "Unknown" }, "pid_pressure", "unknown"},
	} {
		s := healthy(t)
		tc.edit(&s.Nodes[0])
		if got := evaluate(t, check, s, now, "agent")[tc.dim]; got != tc.want {
			t.Fatal(tc.dim, got)
		}
	}
	s := healthy(t)
	s.Nodes[0].Status.Conditions = append(s.Nodes[0].Status.Conditions, s.Nodes[0].Status.Conditions[0])
	s.Nodes[0].Status.Conditions[4].Type = "NetworkUnavailable"
	if got := evaluate(t, check, s, now, "agent")["network"]; got != "ok" {
		t.Fatal(got)
	}
	s.Nodes[0].Status.Conditions[4].Status = "True"
	if got := evaluate(t, check, s, now, "agent")["network"]; got != "unavailable" {
		t.Fatal(got)
	}
}

func TestCordonAndSkewFailOnlyAfterPersisting(t *testing.T) {
	check := New(cfg(), nil)
	s := healthy(t)
	s.Nodes[0].Spec.Unschedulable = true
	s.Nodes[0].Status.NodeInfo.KubeletVersion = "v1.36.9+k3s1"
	for _, offset := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		results := evaluate(t, check, s, now.Add(offset), "agent")
		if results["schedulable"] != "ok" || results["kubelet_version"] != "ok" {
			t.Fatal("a rolling drain paged", offset, results)
		}
	}
	results := evaluate(t, check, s, now.Add(2*time.Hour+time.Minute), "agent")
	if results["schedulable"] != "cordoned(for 2h1m)" || !strings.HasPrefix(results["kubelet_version"], "skewed(v1.36.9+k3s1, control_plane v1.37.2+k3s1") {
		t.Fatal(results)
	}
	// Uncordoning clears the first-seen time.
	s.Nodes[0].Spec.Unschedulable = false
	evaluate(t, check, s, now.Add(3*time.Hour), "agent")
	s.Nodes[0].Spec.Unschedulable = true
	if got := evaluate(t, check, s, now.Add(3*time.Hour+time.Minute), "agent")["schedulable"]; got != "ok" {
		t.Fatal(got)
	}
}

// A cordon for a rolling drain makes Longhorn mark the node unschedulable at
// once; that must not page before the cordon window does.
func TestCordonedLonghornNodeDoesNotFailItsDisks(t *testing.T) {
	check := New(cfg(), nil)
	s := healthy(t)
	s.Nodes[0].Spec.Unschedulable = true
	s.Longhorn[0].Status.Conditions[1].Status = "False"
	s.Longhorn[0].Status.Conditions[1].Reason = "KubernetesNodeCordoned"
	results := evaluate(t, check, s, now, "agent")
	if results["longhorn_disk"] != "ok" || results["schedulable"] != "ok" {
		t.Fatal(results)
	}
}

func TestUsageAndLonghornDisks(t *testing.T) {
	check := New(cfg(), nil)
	s := healthy(t)
	s.Metrics[0] = metrics("agent", "11500m", "14000000Ki")
	results := evaluate(t, check, s, now, "agent")
	if results["cpu"] != "high(96%, max 90%)" || results["memory"] != "high(98%, max 90%)" {
		t.Fatal(results)
	}
	s.Metrics = s.Metrics[1:]
	if got := evaluate(t, check, s, now, "agent")["cpu"]; got != "metrics_missing" {
		t.Fatal(got)
	}
	disabled := cfg()
	off := false
	disabled.Metrics = &off
	if got := evaluate(t, New(disabled, nil), s, now, "agent")["cpu"]; got != result.NA {
		t.Fatal(got)
	}
	for timestamp, want := range map[string]string{"": "metrics_invalid", "garbage": "metrics_invalid",
		now.Add(10 * time.Minute).Format(time.RFC3339):  "metrics_invalid",
		now.Add(-10 * time.Minute).Format(time.RFC3339): "metrics_stale(age 10m, max 5m)"} {
		s = healthy(t)
		s.Metrics[0].Timestamp = timestamp
		if got := evaluate(t, check, s, now, "agent"); got["cpu"] != want || got["memory"] != want {
			t.Fatal(timestamp, got)
		}
	}
	s = healthy(t)
	s.Longhorn[0] = longhornNode(t, "agent", 1<<30)
	if got := evaluate(t, check, s, now, "agent")["longhorn_disk"]; got != "low_space(permanent, free 2%, min 10%)" {
		t.Fatal(got)
	}
	s = healthy(t)
	disk := s.Longhorn[0].Status.DiskStatus["permanent"]
	disk.Conditions[0].Status = "False"
	s.Longhorn[0].Status.DiskStatus["permanent"] = disk
	if got := evaluate(t, check, s, now, "agent")["longhorn_disk"]; got != "disk_not_ready(permanent)" {
		t.Fatal(got)
	}
}

func TestInventory(t *testing.T) {
	s := healthy(t)
	s.Nodes = []Node{node(t, "agent"), node(t, "surprise")}
	h := checktest.New(New(cfg(), &fakeAPI{sample: s}), now).Refresh(t)
	if code, e := h.Get(t, Route+"/control"); code != 503 || e.Results["ready"] != "missing" {
		t.Fatal(e)
	}
	if _, e := h.Get(t, Route); e.Results["inventory"] != "missing(1): control; unexpected(1): surprise" {
		t.Fatal(e.Results)
	}
}
