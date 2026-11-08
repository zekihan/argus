// Package nodes checks Kubernetes nodes: conditions, cordon and version skew
// persisting beyond a window, metrics-server usage and Longhorn node disks.
package nodes

import (
	"context"
	"sort"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
)

const Route = "nodes"

var dimensions = []string{"ready", "memory_pressure", "disk_pressure", "pid_pressure", "network", "schedulable", "kubelet_version", "cpu", "memory", "longhorn_disk"}

type Node struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Conditions  []kube.Condition  `json:"conditions"`
		Allocatable map[string]string `json:"allocatable"`
		NodeInfo    struct {
			KubeletVersion string `json:"kubeletVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type NodeMetrics struct {
	Metadata  kube.Metadata     `json:"metadata"`
	Timestamp string            `json:"timestamp"`
	Usage     map[string]string `json:"usage"`
}

// metricsMaxAge bounds how old a metrics-server measurement may be; a stuck
// metrics pipeline must not keep reporting old, low usage as healthy.
const metricsMaxAge = 5 * time.Minute

type LonghornNode struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		Conditions []kube.Condition `json:"conditions"`
		DiskStatus map[string]struct {
			Conditions       []kube.Condition `json:"conditions"`
			StorageAvailable int64            `json:"storageAvailable"`
			StorageMaximum   int64            `json:"storageMaximum"`
		} `json:"diskStatus"`
	} `json:"status"`
}

type Sample struct {
	Nodes []Node
	// ServerVersion is the API server's gitVersion: the control plane version.
	ServerVersion string
	Metrics       []NodeMetrics
	Longhorn      []LonghornNode
}

type Collector interface {
	Collect(ctx context.Context) (Sample, error)
}

type API struct {
	Client            *kube.Client
	Metrics           bool
	LonghornNamespace string
}

func (a API) Collect(ctx context.Context) (Sample, error) {
	var s Sample
	var err error
	if s.Nodes, err = kube.List[Node](ctx, a.Client, "/api/v1/nodes", ""); err != nil {
		return Sample{}, err
	}
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := a.Client.Get(ctx, "/version", &version); err != nil {
		return Sample{}, err
	}
	s.ServerVersion = version.GitVersion
	if a.Metrics {
		if s.Metrics, err = kube.List[NodeMetrics](ctx, a.Client, "/apis/metrics.k8s.io/v1beta1/nodes", ""); err != nil {
			return Sample{}, err
		}
	}
	if a.LonghornNamespace != "" {
		if s.Longhorn, err = kube.List[LonghornNode](ctx, a.Client, "/apis/longhorn.io/v1beta2/namespaces/"+a.LonghornNamespace+"/nodes", ""); err != nil {
			return Sample{}, err
		}
	}
	return s, nil
}

type Check struct {
	config config.Nodes
	api    Collector
	seen   *runner.FirstSeen
}

func New(c config.Nodes, api Collector) *Check {
	return &Check{config: c, api: api, seen: runner.NewFirstSeen()}
}

func (c *Check) Name() string { return "nodes" }

func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "node", Dimensions: dimensions}
	for _, name := range c.config.Expected {
		collection.Items = append(collection.Items, result.Item{Target: name})
	}
	return []result.Collection{collection}
}

func (c *Check) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	sample, err := c.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	// Evaluate after reading: objects updated during the read are not "future".
	now := clock()
	return []result.Collection{c.Evaluate(sample, now)}, nil
}

func (c *Check) Evaluate(s Sample, now time.Time) result.Collection {
	c.seen.Begin(now)
	defer c.seen.End()
	nodes := map[string]Node{}
	for _, n := range s.Nodes {
		nodes[n.Metadata.Name] = n
	}
	metrics := map[string]NodeMetrics{}
	for _, m := range s.Metrics {
		metrics[m.Metadata.Name] = m
	}
	longhorn := map[string]LonghornNode{}
	for _, l := range s.Longhorn {
		longhorn[l.Metadata.Name] = l
	}
	collection := result.Collection{Route: Route, Check: "node", Dimensions: dimensions}
	expected := map[string]bool{}
	var missing, unexpected []string
	for _, name := range c.config.Expected {
		expected[name] = true
		node, ok := nodes[name]
		if !ok {
			missing = append(missing, name)
			results := map[string]string{"ready": result.Reason("missing")}
			for _, dim := range dimensions[1:] {
				results[dim] = result.NA
			}
			collection.Items = append(collection.Items, result.Item{Target: name, Results: results, Values: map[string]any{}})
			continue
		}
		collection.Items = append(collection.Items, c.node(node, s, metrics, longhorn, now))
	}
	for name := range nodes {
		if !expected[name] {
			unexpected = append(unexpected, name)
		}
	}
	collection.Results = map[string]string{"inventory": result.Inventory(missing, unexpected)}
	return collection
}

// condition applies a node-condition rule. Ready must be True; the pressure
// conditions must be present and False.
func condition(conditions []kube.Condition, kind, healthy, failing string) string {
	c, ok := kube.FindCondition(conditions, kind)
	switch {
	case !ok:
		return result.Reason("missing_condition")
	case c.Status == healthy:
		return result.OK
	case c.Status == "Unknown":
		return result.Reason("unknown")
	}
	return result.Reason(failing)
}

func (c *Check) node(node Node, s Sample, metrics map[string]NodeMetrics, longhorn map[string]LonghornNode, now time.Time) result.Item {
	name := node.Metadata.Name
	conds := node.Status.Conditions
	persist := time.Duration(c.config.PersistSeconds) * time.Second
	values := map[string]any{"kubeletVersion": result.SafeName(node.Status.NodeInfo.KubeletVersion), "observedSinceSeconds": c.seen.Observed(now).Seconds()}
	results := map[string]string{
		"ready":           condition(conds, "Ready", "True", "not_ready"),
		"memory_pressure": condition(conds, "MemoryPressure", "False", "pressure"),
		"disk_pressure":   condition(conds, "DiskPressure", "False", "pressure"),
		"pid_pressure":    condition(conds, "PIDPressure", "False", "pressure"),
		"network":         result.NA,
	}
	// Only NetworkUnavailable may be absent (k3s with flannel never sets it).
	if _, ok := kube.FindCondition(conds, "NetworkUnavailable"); ok {
		results["network"] = condition(conds, "NetworkUnavailable", "False", "unavailable")
	}
	// Cordons and version skew are normal during a rolling drain; fail only
	// when they persist.
	cordoned := c.seen.Active(name+"/cordoned", node.Spec.Unschedulable, now)
	values["cordoned"] = node.Spec.Unschedulable
	results["schedulable"] = result.OK
	if node.Spec.Unschedulable {
		values["cordonedObservedSeconds"] = cordoned.Seconds()
		if cordoned > persist {
			results["schedulable"] = result.Reason("cordoned", result.Labeled("for", result.Dur(cordoned.Seconds())))
		}
	}
	skewed := node.Status.NodeInfo.KubeletVersion != s.ServerVersion
	skew := c.seen.Active(name+"/skew", skewed, now)
	values["controlPlaneVersion"] = result.SafeName(s.ServerVersion)
	results["kubelet_version"] = result.OK
	if skewed {
		values["skewObservedSeconds"] = skew.Seconds()
		if skew > persist {
			results["kubelet_version"] = result.Reason("skewed", result.Name(node.Status.NodeInfo.KubeletVersion),
				result.Labeled("control_plane", result.Name(s.ServerVersion)), result.Labeled("for", result.Dur(skew.Seconds())))
		}
	}
	results["cpu"], results["memory"] = result.NA, result.NA
	if c.config.MetricsEnabled() {
		m, ok := metrics[name]
		if fresh := metricsFreshness(m, ok, now, values); fresh != result.OK {
			results["cpu"], results["memory"] = fresh, fresh
		} else {
			results["cpu"] = usage(m.Usage["cpu"], node.Status.Allocatable["cpu"], ok, c.config.CPUPercent, "cpu", values)
			results["memory"] = usage(m.Usage["memory"], node.Status.Allocatable["memory"], ok, c.config.MemoryPercent, "memory", values)
		}
	}
	results["longhorn_disk"] = result.NA
	if l, ok := longhorn[name]; ok && c.config.LonghornNamespace != "" {
		results["longhorn_disk"] = c.longhornDisk(l, values)
	}
	return result.Item{Target: name, Results: result.Set(dimensions, results), Values: values}
}

func metricsFreshness(m NodeMetrics, found bool, now time.Time, values map[string]any) string {
	if !found {
		return result.Reason("metrics_missing")
	}
	measured, err := kube.ParseTime(m.Timestamp)
	if err != nil {
		return result.Reason("metrics_invalid")
	}
	age := now.Sub(measured)
	values["metricsAgeSeconds"] = age.Seconds()
	switch {
	case age < -time.Minute:
		return result.Reason("metrics_invalid")
	case age > metricsMaxAge:
		return result.Reason("metrics_stale", result.Labeled("age", result.Dur(age.Seconds())), result.Labeled("max", result.Dur(metricsMaxAge.Seconds())))
	}
	return result.OK
}

func usage(used, allocatable string, found bool, threshold int, resource string, values map[string]any) string {
	if !found {
		return result.Reason("metrics_missing")
	}
	u, errUsed := kube.Quantity(used)
	a, errAllocatable := kube.Quantity(allocatable)
	if errUsed != nil || errAllocatable != nil || a <= 0 {
		return result.Reason("metrics_invalid")
	}
	percent := u / a * 100
	values[resource+"Usage"], values[resource+"Allocatable"], values[resource+"Percent"] = u, a, percent
	if percent > float64(threshold) {
		return result.Reason("high", result.Pct(percent), result.Labeled("max", result.Pct(float64(threshold))))
	}
	return result.OK
}

func (c *Check) longhornDisk(l LonghornNode, values map[string]any) string {
	if !kube.ConditionTrue(l.Status.Conditions, "Ready") {
		return result.Reason("node_not_ready")
	}
	if !kube.ConditionTrue(l.Status.Conditions, "Schedulable") {
		return result.Reason("node_not_schedulable")
	}
	names := make([]string, 0, len(l.Status.DiskStatus))
	for name := range l.Status.DiskStatus {
		names = append(names, name)
	}
	sort.Strings(names)
	var available, maximum int64
	for _, name := range names {
		disk := l.Status.DiskStatus[name]
		available += disk.StorageAvailable
		maximum += disk.StorageMaximum
		if !kube.ConditionTrue(disk.Conditions, "Ready") {
			return result.Reason("disk_not_ready", result.Name(name))
		}
		if !kube.ConditionTrue(disk.Conditions, "Schedulable") {
			return result.Reason("disk_not_schedulable", result.Name(name))
		}
		if disk.StorageMaximum <= 0 {
			return result.Reason("disk_size_unknown", result.Name(name))
		}
		free := float64(disk.StorageAvailable) / float64(disk.StorageMaximum) * 100
		if free < float64(c.config.LonghornMinAvailablePercent) {
			return result.Reason("low_space", result.Name(name), result.Labeled("free", result.Pct(free)), result.Labeled("min", result.Pct(float64(c.config.LonghornMinAvailablePercent))))
		}
	}
	if len(names) == 0 {
		return result.Reason("no_disks")
	}
	if maximum > 0 {
		values["longhornStoragePercent"] = float64(maximum-available) / float64(maximum) * 100
	}
	return result.OK
}
