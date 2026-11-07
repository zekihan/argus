// Package longhorn ports backup_monitor.py: backup freshness and robustness of
// the expected backup PVCs, backup targets and system backups, expanded with
// replica, state, scheduling and snapshot dimensions.
package longhorn

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
)

const (
	VolumesRoute = "longhorn/volumes"
	SystemRoute  = "longhorn/system"
	// transitionalFor is how long a volume may stay in a non-steady state.
	transitionalFor = 15 * time.Minute
)

// volume and robustness are backup_monitor.py's per-volume checks; backup is
// its freshness rule. The rest are new.
var dimensions = []string{"volume", "backup", "robustness", "state", "replicas", "scheduled", "snapshots"}

type Volume struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		Size             string `json:"size"`
		NumberOfReplicas int    `json:"numberOfReplicas"`
	} `json:"spec"`
	Status struct {
		State            string           `json:"state"`
		Robustness       string           `json:"robustness"`
		LastBackupAt     *string          `json:"lastBackupAt"`
		ActualSize       int64            `json:"actualSize"`
		CurrentNodeID    string           `json:"currentNodeID"`
		Conditions       []kube.Condition `json:"conditions"`
		KubernetesStatus struct {
			Namespace    string `json:"namespace"`
			PVCName      string `json:"pvcName"`
			LastPVCRefAt string `json:"lastPVCRefAt"`
		} `json:"kubernetesStatus"`
	} `json:"status"`
}

type Replica struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		VolumeName string `json:"volumeName"`
		FailedAt   string `json:"failedAt"`
		HealthyAt  string `json:"healthyAt"`
	} `json:"spec"`
	Status struct {
		CurrentState string `json:"currentState"`
	} `json:"status"`
}

type BackupTarget struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		// Raw, so a non-boolean value fails this target rather than the sample.
		Available json.RawMessage `json:"available"`
	} `json:"status"`
}

type SystemBackup struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		CreatedAt *string `json:"createdAt"`
		State     string  `json:"state"`
	} `json:"status"`
}

type Sample struct {
	Volumes       []Volume
	Replicas      []Replica
	BackupTargets []BackupTarget
	SystemBackups []SystemBackup
}

type Collector interface {
	Collect(ctx context.Context) (Sample, error)
}

// API reads only the Longhorn namespace (a namespace Role).
type API struct {
	Client    *kube.Client
	Namespace string
}

func (a API) Collect(ctx context.Context) (Sample, error) {
	path := "/apis/longhorn.io/v1beta2/namespaces/" + a.Namespace
	var s Sample
	var err error
	if s.Volumes, err = kube.List[Volume](ctx, a.Client, path+"/volumes", ""); err != nil {
		return Sample{}, err
	}
	if s.Replicas, err = kube.List[Replica](ctx, a.Client, path+"/replicas", ""); err != nil {
		return Sample{}, err
	}
	if s.BackupTargets, err = kube.List[BackupTarget](ctx, a.Client, path+"/backuptargets", ""); err != nil {
		return Sample{}, err
	}
	if s.SystemBackups, err = kube.List[SystemBackup](ctx, a.Client, path+"/systembackups", ""); err != nil {
		return Sample{}, err
	}
	return s, nil
}

type Check struct {
	config config.Longhorn
	api    Collector
	seen   *runner.FirstSeen
}

func New(c config.Longhorn, api Collector) *Check {
	return &Check{config: c, api: api, seen: runner.NewFirstSeen()}
}

func (c *Check) Name() string { return "longhorn" }

func (c *Check) expected() []string {
	expected := append([]string(nil), c.config.ExpectedPVCs...)
	sort.Strings(expected)
	return expected
}

func (c *Check) Describe() []result.Collection {
	volumes := result.Collection{Route: VolumesRoute, Check: "longhorn/volume", Dimensions: dimensions}
	for _, key := range c.expected() {
		volumes.Items = append(volumes.Items, result.Item{Target: key})
	}
	return []result.Collection{volumes, {Route: SystemRoute, Check: SystemRoute, Singleton: true}}
}

func (c *Check) Collect(ctx context.Context, now time.Time) ([]result.Collection, error) {
	sample, err := c.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	return c.Evaluate(sample, now), nil
}

// BackupResult is backup_monitor.py's backup_problem.
func BackupResult(v Volume, now time.Time, maximum int) (string, map[string]any) {
	values := map[string]any{}
	if v.Status.LastBackupAt == nil || *v.Status.LastBackupAt == "" {
		created, err := kube.ParseTime(v.Metadata.CreationTimestamp)
		if err != nil {
			return result.Reason("no_backup"), values
		}
		return result.Reason("no_backup", result.Labeled("created", result.Dur(now.Sub(created).Seconds()))), values
	}
	last, err := kube.ParseTime(*v.Status.LastBackupAt)
	if err != nil {
		return result.Reason("backup_timestamp_invalid"), values
	}
	age := now.Sub(last).Seconds()
	values["lastBackupAt"], values["lastBackupAgeSeconds"] = last.Format(time.RFC3339), age
	switch {
	case age < 0:
		return result.Reason("backup_in_future", result.Dur(age)), values
	case age > float64(maximum):
		return result.Reason("backup_stale", result.Labeled("last", result.Dur(age)), result.Labeled("max", result.Dur(float64(maximum)))), values
	}
	return result.OK, values
}

func (c *Check) Evaluate(s Sample, now time.Time) []result.Collection {
	c.seen.Begin(now)
	defer c.seen.End()
	expected := map[string]bool{}
	for _, key := range c.config.ExpectedPVCs {
		expected[key] = true
	}
	mapped := map[string][]Volume{}
	var unexpected []string
	group := "recurring-job-group.longhorn.io/" + c.config.BackupGroup
	for _, v := range s.Volumes {
		k := v.Status.KubernetesStatus
		key := k.Namespace + "/" + k.PVCName
		// Historical PVC references do not prove that the expected claim exists.
		if k.LastPVCRefAt == "" {
			mapped[key] = append(mapped[key], v)
		}
		if v.Metadata.Labels[group] == "enabled" && (!expected[key] || k.LastPVCRefAt != "") {
			name := v.Metadata.Name
			if name == "" {
				name = "unknown"
			}
			unexpected = append(unexpected, name)
		}
	}
	volumes := result.Collection{Route: VolumesRoute, Check: "longhorn/volume", Dimensions: dimensions}
	var missing []string
	for _, key := range c.expected() {
		matches := mapped[key]
		item := result.Item{Target: key, Values: map[string]any{}}
		switch len(matches) {
		case 0:
			missing = append(missing, key)
			item.Results = notApplicable(result.Reason("missing"))
		case 1:
			item = c.volume(key, matches[0], s.Replicas, now)
		default:
			item.Results = notApplicable(result.Reason("ambiguous", result.Labeled("volumes", result.Int(int64(len(matches))))))
		}
		volumes.Items = append(volumes.Items, item)
	}
	inventory := result.Inventory(missing, unexpected)
	volumes.Results = map[string]string{"inventory": inventory}
	system := result.Collection{Route: SystemRoute, Check: SystemRoute, Singleton: true, Values: map[string]any{}}
	system.Results = map[string]string{"inventory": inventory, "backup_targets": targets(s.BackupTargets, system.Values),
		"system_backup": c.systemBackup(s.SystemBackups, now, system.Values)}
	return []result.Collection{volumes, system}
}

func notApplicable(volume string) map[string]string {
	results := map[string]string{"volume": volume}
	for _, dim := range dimensions[1:] {
		results[dim] = result.NA
	}
	return results
}

func conditionResult(conditions []kube.Condition, kind, failWhen, code string) string {
	condition, ok := kube.FindCondition(conditions, kind)
	switch {
	case !ok:
		return result.NA
	case condition.Status == failWhen:
		return result.Reason(code)
	case condition.Status == "True" || condition.Status == "False":
		return result.OK
	}
	return result.Reason("unknown")
}

func (c *Check) volume(key string, v Volume, replicas []Replica, now time.Time) result.Item {
	attached := v.Status.State == "attached"
	results := map[string]string{"volume": result.OK}
	var values map[string]any
	results["backup"], values = BackupResult(v, now, c.config.MaxBackupAgeSeconds)
	switch robustness := v.Status.Robustness; {
	case robustness == "healthy":
		results["robustness"] = result.OK
	case robustness == "degraded" || robustness == "faulted":
		// Failing whether or not the volume is attached, as backup_monitor.py does.
		results["robustness"] = result.Reason(robustness)
	case robustness == "unknown" && !attached:
		results["robustness"] = result.NA
	default:
		results["robustness"] = result.Reason("unknown")
	}
	switch state := v.Status.State; state {
	case "attached", "detached":
		c.seen.Active(key, false, now)
		results["state"] = result.OK
	default:
		since := c.seen.Active(key, true, now)
		results["state"] = result.OK
		if since > transitionalFor {
			results["state"] = result.Reason("stuck", result.Name(state), result.Labeled("for", result.Dur(since.Seconds())))
		}
		values["stateObservedSeconds"] = since.Seconds()
	}
	healthy := 0
	for _, r := range replicas {
		if r.Spec.VolumeName == v.Metadata.Name && r.Status.CurrentState == "running" && r.Spec.FailedAt == "" && r.Spec.HealthyAt != "" {
			healthy++
		}
	}
	desired := v.Spec.NumberOfReplicas
	results["replicas"] = result.NA
	if attached {
		results["replicas"] = result.OK
		if healthy < desired {
			results["replicas"] = result.Reason("degraded", result.Labeled("healthy", result.Int(int64(healthy))), result.Labeled("desired", result.Int(int64(desired))))
		}
		values["replicasHealthy"] = healthy
	}
	results["scheduled"] = conditionResult(v.Status.Conditions, "Scheduled", "False", "not_scheduled")
	results["snapshots"] = conditionResult(v.Status.Conditions, "TooManySnapshots", "True", "too_many_snapshots")
	if size, err := strconv.ParseInt(v.Spec.Size, 10, 64); err == nil {
		values["sizeBytes"] = size
	}
	values["actualSizeBytes"] = v.Status.ActualSize
	values["replicasDesired"] = desired
	values["attached"] = attached
	values["volume"] = result.SafeName(v.Metadata.Name)
	values["state"] = result.SafeName(v.Status.State)
	values["robustness"] = result.SafeName(v.Status.Robustness)
	if v.Status.CurrentNodeID != "" {
		values["node"] = result.SafeName(v.Status.CurrentNodeID)
	}
	return result.Item{Target: key, Results: result.Set(dimensions, results), Values: values}
}

func targets(list []BackupTarget, values map[string]any) string {
	values["backupTargets"] = len(list)
	if len(list) == 0 {
		return result.Reason("missing")
	}
	var unavailable []string
	for _, target := range list {
		if string(target.Status.Available) != "true" {
			unavailable = append(unavailable, target.Metadata.Name)
		}
	}
	if len(unavailable) == 0 {
		return result.OK
	}
	sort.Strings(unavailable)
	params := make([]result.Param, len(unavailable))
	for i, name := range unavailable {
		params[i] = result.Name(name)
	}
	return result.Reason("unavailable", params...)
}

// systemBackup is backup_monitor.py's rule: the newest system backup by
// createdAt must be Ready and fresh, and every createdAt must be valid.
func (c *Check) systemBackup(list []SystemBackup, now time.Time, values map[string]any) string {
	if len(list) == 0 {
		return result.Reason("missing")
	}
	var invalid []result.Param
	var latest *SystemBackup
	var latestAt time.Time
	for i := range list {
		b := &list[i]
		if b.Status.CreatedAt == nil {
			invalid = append(invalid, result.Name(b.Metadata.Name))
			continue
		}
		created, err := kube.ParseTime(*b.Status.CreatedAt)
		if err != nil {
			invalid = append(invalid, result.Name(b.Metadata.Name))
			continue
		}
		if latest == nil || created.After(latestAt) {
			latest, latestAt = b, created
		}
	}
	if latest == nil {
		return result.Reason("invalid_created_at", invalid...)
	}
	age := now.Sub(latestAt).Seconds()
	values["systemBackup"], values["systemBackupCreatedAt"], values["systemBackupAgeSeconds"] = result.SafeName(latest.Metadata.Name), latestAt.Format(time.RFC3339), age
	maximum := float64(c.config.SystemBackupMaxAgeSeconds + c.config.SystemBackupGraceSeconds)
	switch {
	case latest.Status.State != "Ready":
		return result.Reason("not_ready", result.Name(latest.Metadata.Name))
	case age < 0 || age > maximum:
		return result.Reason("stale_or_invalid", result.Name(latest.Metadata.Name), result.Labeled("age", result.Dur(age)), result.Labeled("max", result.Dur(maximum)))
	case len(invalid) > 0:
		return result.Reason("invalid_created_at", invalid...)
	}
	return result.OK
}
