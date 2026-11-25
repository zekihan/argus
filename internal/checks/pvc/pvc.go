// Package pvc checks filesystem usage from authenticated kubelet summaries.
package pvc

import (
	"context"
	"net"
	"slices"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

const Route = "pvcs"

var dimensions = []string{"bound", "usage"}

type Claim struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		StorageClassName string `json:"storageClassName"`
		VolumeMode       string `json:"volumeMode"`
		VolumeName       string `json:"volumeName"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type Node struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"status"`
}

type VolumeStats struct {
	PVCRef        *config.NamespacedName `json:"pvcRef"`
	Time          string                 `json:"time"`
	CapacityBytes *uint64                `json:"capacityBytes"`
	UsedBytes     *uint64                `json:"usedBytes"`
}

type Summary struct {
	Node struct {
		Name string `json:"nodeName"`
	} `json:"node"`
	Pods *[]struct {
		Volumes []VolumeStats `json:"volume"`
	} `json:"pods"`
}

type Attachment struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		Source struct {
			PersistentVolumeName string `json:"persistentVolumeName"`
		} `json:"source"`
	} `json:"spec"`
}

type Sample struct {
	Attachments          []Attachment
	AttachmentsCollected bool
	Claims               []Claim
	Nodes                []Node
	Stats                []VolumeStats
}

type Collector interface {
	Collect(ctx context.Context) (Sample, error)
}
type API struct {
	Client           *kube.Client
	ExpectedNodes    []string
	CheckAttachments bool
}

// nodeAddress returns the node's first InternalIP, if any.
func nodeAddress(node Node) string {
	for _, candidate := range node.Status.Addresses {
		if candidate.Type == "InternalIP" && net.ParseIP(candidate.Address) != nil {
			return candidate.Address
		}
	}
	return ""
}

// collectNodeStats appends PVC stats of one node's kubelet summary.
func collectNodeStats(s *Sample, pods *[]struct {
	Volumes []VolumeStats `json:"volume"`
},
) error {
	for _, pod := range *pods {
		for _, volume := range pod.Volumes {
			if volume.PVCRef != nil {
				s.Stats = append(s.Stats, volume)
			}
		}
		if len(s.Stats) > 20000 {
			return &kube.Error{Code: kube.CodeBudget}
		}
	}
	return nil
}

func (a API) Collect(ctx context.Context) (Sample, error) {
	var s Sample
	var err error
	if s.Claims, err = kube.List[Claim](
		ctx,
		a.Client,
		"/api/v1/persistentvolumeclaims",
		"",
	); err != nil {
		return s, err
	}
	if s.Nodes, err = kube.List[Node](ctx, a.Client, "/api/v1/nodes", ""); err != nil {
		return s, err
	}
	if a.CheckAttachments {
		if s.Attachments, err = kube.List[Attachment](
			ctx,
			a.Client,
			"/apis/storage.k8s.io/v1/volumeattachments",
			"",
		); err != nil {
			return s, err
		}
		s.AttachmentsCollected = true
	}
	if len(s.Nodes) > 1000 {
		return s, &kube.Error{Code: kube.CodeBudget}
	}
	for _, node := range s.Nodes {
		if !slices.Contains(a.ExpectedNodes, node.Metadata.Name) {
			continue
		}
		var summary Summary
		if err := a.Client.NodeStats(ctx, nodeAddress(node), &summary); err != nil {
			return s, err
		}
		if summary.Node.Name != node.Metadata.Name || summary.Pods == nil {
			return s, &kube.Error{Code: kube.CodeInvalid}
		}
		if err := collectNodeStats(&s, summary.Pods); err != nil {
			return s, err
		}
	}
	return s, nil
}

type Check struct {
	config config.PVCs
	api    Collector
	// Only collection mutates this cache; published results capture a sample copy.
	cached map[string]VolumeStats
}

func New(c config.PVCs, api Collector) *Check {
	return &Check{config: c, api: api, cached: map[string]VolumeStats{}}
}
func (c *Check) Name() string { return Route }
func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "pvc", Dimensions: dimensions}
	for _, target := range c.config.Expected {
		collection.Items = append(collection.Items, result.Item{Target: target.Key()})
	}
	return []result.Collection{collection}
}

func (c *Check) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	s, err := c.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	return []result.Collection{c.Evaluate(s, clock())}, nil
}

func (c *Check) Evaluate(s Sample, now time.Time) result.Collection {
	collection := result.Collection{Route: Route, Check: "pvc", Dimensions: dimensions}
	stats := map[string][]VolumeStats{}
	for _, v := range s.Stats {
		if v.PVCRef != nil {
			stats[v.PVCRef.Key()] = append(stats[v.PVCRef.Key()], v)
		}
	}
	nextCache := map[string]VolumeStats{}
	expected := append(slices.Clone(c.config.Expected), c.config.AllowUnmounted...)
	attachments := map[string][]Attachment{}
	for _, a := range s.Attachments {
		pv := a.Spec.Source.PersistentVolumeName
		attachments[pv] = append(attachments[pv], a)
	}
	found := map[string]bool{}
	var missing []string
	for _, claim := range c.claimPicks(s, expected) {
		key := claim.Metadata.Namespace + "/" + claim.Metadata.Name
		found[key] = true
		measurements := c.freshMeasurements(stats[key], claim)
		collection.Items = append(
			collection.Items,
			c.claimItem(s, claim, measurements, nextCache, attachments, now),
		)
	}
	c.cached = nextCache
	for _, target := range expected {
		if !found[target.Key()] {
			collection.Items = append(
				collection.Items,
				result.Item{
					Target:  target.Key(),
					Results: map[string]string{"bound": "missing", "usage": "metrics_missing"},
				},
			)
			missing = append(missing, target.Key())
		}
	}
	var unexpected []string
	for _, node := range s.Nodes {
		if !slices.Contains(c.config.ExpectedNodes, node.Metadata.Name) {
			unexpected = append(unexpected, node.Metadata.Name)
		}
	}
	missing = append(missing, missingNodes(s.Nodes, c.config.ExpectedNodes)...)
	collection.Results = map[string]string{"inventory": result.Inventory(missing, unexpected)}
	if len(collection.Items) == 0 {
		collection.Results["inventory"] = result.Reason("no_pvcs")
	}
	return collection
}

func (c *Check) claimItem(
	s Sample,
	claim Claim,
	measurements []VolumeStats,
	nextCache map[string]VolumeStats,
	attachments map[string][]Attachment,
	now time.Time,
) result.Item {
	key := claim.Metadata.Namespace + "/" + claim.Metadata.Name
	item := result.Item{
		Target:  key,
		Results: map[string]string{"bound": result.OK},
		Values:  map[string]any{},
	}
	if claim.Status.Phase != "Bound" {
		item.Results["bound"] = result.Reason("not_bound")
	}
	for _, v := range measurements {
		c.acceptClaim(nextCache, key, v, now, claim)
	}
	allowed := slices.ContainsFunc(
		c.config.AllowUnmounted,
		func(n config.NamespacedName) bool { return n.Key() == key },
	)
	canBeIdle := allowed && s.AttachmentsCollected && claim.Spec.VolumeName != ""
	claimAttachments := attachments[claim.Spec.VolumeName]
	item.Values["detached"] = canBeIdle && len(claimAttachments) == 0
	if claim.Spec.VolumeMode == "Block" {
		item.Results["usage"] = result.NA
	} else {
		item.Refine = c.usageRefine(measurements, canBeIdle, claimAttachments)
		item.Refine(now, &item)
	}
	return item
}

func (c *Check) usageRefine(
	measurements []VolumeStats,
	canBeIdle bool,
	claimAttachments []Attachment,
) func(at time.Time, item *result.Item) {
	return func(at time.Time, item *result.Item) {
		usage := c.usage(measurements, at, item.Values)
		if canBeIdle && (usage == "metrics_missing" || usage == "metrics_stale") {
			usage = c.idleUsage(claimAttachments, at, usage, item.Values)
		}
		item.Results["usage"] = usage
	}
}

func (c *Check) acceptClaim(
	nextCache map[string]VolumeStats,
	key string,
	v VolumeStats,
	now time.Time,
	claim Claim,
) {
	candidate := map[string]any{}
	c.usage([]VolumeStats{v}, now, candidate)
	if percent, ok := candidate["usedPercent"].(float64); ok && claim.Metadata.UID != "" {
		cacheKey := key + "/" + claim.Metadata.UID
		previous, exists := nextCache[cacheKey]
		if !exists ||
			percent > float64(*previous.UsedBytes)/float64(*previous.CapacityBytes)*100 {
			nextCache[cacheKey] = v
		}
	}
}

// freshMeasurements drops measurements older than the claim and reuses the
// cached last-fresh reading when none remain.
func (c *Check) freshMeasurements(measurements []VolumeStats, claim Claim) []VolumeStats {
	if created, err := kube.ParseTime(claim.Metadata.CreationTimestamp); err == nil {
		// Kubelet reports the claim name and namespace without its UID.
		measurements = slices.DeleteFunc(measurements, func(v VolumeStats) bool {
			measured, err := kube.ParseTime(v.Time)
			return err == nil && measured.Before(created)
		})
	}
	// Reuse only an absent measurement, never an invalid replacement, and
	// never across a deleted/recreated PVC. The original timestamp expires it.
	cacheKey := claim.Metadata.Namespace + "/" + claim.Metadata.Name + "/" + claim.Metadata.UID
	if len(measurements) == 0 && claim.Metadata.UID != "" {
		if previous, ok := c.cached[cacheKey]; ok {
			measurements = []VolumeStats{previous}
		}
	}
	return measurements
}

// claimPicks filters the claims worth itemizing.
func (c *Check) claimPicks(s Sample, expected []config.NamespacedName) []Claim {
	var picks []Claim
	for _, claim := range s.Claims {
		key := claim.Metadata.Namespace + "/" + claim.Metadata.Name
		if !slices.Contains(c.config.StorageClasses, claim.Spec.StorageClassName) &&
			!slices.ContainsFunc(
				expected,
				func(n config.NamespacedName) bool { return n.Key() == key },
			) {
			continue
		}
		picks = append(picks, claim)
	}
	return picks
}

// missingNodes returns expected node names missing from the sample.
func missingNodes(nodes []Node, expected []string) []string {
	var missing []string
	names := map[string]bool{}
	for _, node := range nodes {
		names[node.Metadata.Name] = true
	}
	for _, name := range expected {
		if !names[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func (c *Check) usage(samples []VolumeStats, now time.Time, values map[string]any) string {
	// A PVC can be mounted by several pods. Use the highest fresh observation;
	// an obsolete pod's stale observation must not mask a current measurement.
	for _, key := range []string{"usedBytes", "capacityBytes", "usedPercent", "measuredAt"} {
		delete(values, key)
	}
	best := -1.0
	failure := "metrics_missing"
	for _, v := range samples {
		measured, err := kube.ParseTime(v.Time)
		if err != nil || measured.After(now.Add(time.Minute)) || v.CapacityBytes == nil ||
			*v.CapacityBytes == 0 ||
			v.UsedBytes == nil ||
			*v.UsedBytes > *v.CapacityBytes {
			failure = "metrics_invalid"
			continue
		}
		if now.Sub(measured) > time.Duration(c.config.MaxStatsAgeSeconds)*time.Second {
			if failure != "metrics_invalid" {
				failure = "metrics_stale"
			}
			continue
		}
		percent := float64(*v.UsedBytes) / float64(*v.CapacityBytes) * 100
		if percent > best {
			best = percent
			values["usedBytes"], values["capacityBytes"], values["usedPercent"], values["measuredAt"] = *v.UsedBytes, *v.CapacityBytes, percent, measured.UTC().
				Format(time.RFC3339)
		}
	}
	if best < 0 {
		return result.Reason(failure)
	}
	if best > float64(c.config.UsedPercent) {
		return result.Reason(
			"high",
			result.Pct(best),
			result.Labeled("max", result.Pct(float64(c.config.UsedPercent))),
		)
	}
	return result.OK
}

// A permitted scheduled workload may finish before kubelet's first volume
// statistics pass. Attachment age bounds this exception across process restarts.
func (c *Check) idleUsage(
	attachments []Attachment,
	now time.Time,
	unavailable string,
	values map[string]any,
) string {
	delete(values, "statsPendingUntil")
	if len(attachments) == 0 {
		return result.NA
	}
	var earliest time.Time
	for _, a := range attachments {
		created, err := kube.ParseTime(a.Metadata.CreationTimestamp)
		if err != nil || created.After(now) {
			return result.Reason("attachment_invalid")
		}
		if earliest.IsZero() || created.Before(earliest) {
			earliest = created
		}
	}
	deadline := earliest.Add(time.Duration(c.config.MaxStatsAgeSeconds) * time.Second)
	if !now.Before(deadline) {
		return unavailable
	}
	values["statsPendingUntil"] = deadline.UTC().Format(time.RFC3339)
	return result.NA
}
