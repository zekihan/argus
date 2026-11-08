// Package cronjobs generalises the dmarc and mailvault freshness monitors
// (freshness.py, sync-freshness.py) to any CronJob, with run history detail.
package cronjobs

import (
	"context"
	"sort"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

const Route = "cronjobs"

// Parity dimensions first: schedule and last_success are what the Python
// monitors checked. last_run, active and failures are new detail.
var dimensions = []string{"schedule", "last_success", "last_run", "active", "failures"}

type CronJob struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		Suspend *bool `json:"suspend"`
	} `json:"spec"`
	Status struct {
		LastSuccessfulTime *string `json:"lastSuccessfulTime"`
		LastScheduleTime   string  `json:"lastScheduleTime"`
	} `json:"status"`
}

type Job struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		StartTime      string           `json:"startTime"`
		CompletionTime string           `json:"completionTime"`
		Conditions     []kube.Condition `json:"conditions"`
	} `json:"status"`
}

// Sample is every CronJob and Job in the inventory namespaces.
type Sample struct {
	CronJobs []CronJob
	Jobs     []Job
}

type Collector interface {
	Collect(ctx context.Context) (Sample, error)
}

// API lists CronJobs and Jobs in each inventory namespace (namespace Roles only).
type API struct {
	Client     *kube.Client
	Namespaces []string
}

func (a API) Collect(ctx context.Context) (Sample, error) {
	var sample Sample
	for _, namespace := range a.Namespaces {
		crons, err := kube.List[CronJob](ctx, a.Client, "/apis/batch/v1/namespaces/"+namespace+"/cronjobs", "")
		if err != nil {
			return Sample{}, err
		}
		jobs, err := kube.List[Job](ctx, a.Client, "/apis/batch/v1/namespaces/"+namespace+"/jobs", "")
		if err != nil {
			return Sample{}, err
		}
		sample.CronJobs, sample.Jobs = append(sample.CronJobs, crons...), append(sample.Jobs, jobs...)
	}
	return sample, nil
}

// Namespaces lists the inventory namespaces in a stable order.
func Namespaces(c config.CronJobs) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range c.Items {
		if !seen[item.Namespace] {
			seen[item.Namespace] = true
			out = append(out, item.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

type Check struct {
	config config.CronJobs
	api    Collector
}

func New(c config.CronJobs, api Collector) *Check { return &Check{config: c, api: api} }

func (c *Check) Name() string { return "cronjobs" }

func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "cronjob", Dimensions: dimensions}
	for _, item := range c.config.Items {
		collection.Items = append(collection.Items, result.Item{Target: item.Key()})
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
	return []result.Collection{Evaluate(c.config, sample, now)}, nil
}

// LastSuccess is the parity rule from freshness.py: a success must be recorded,
// parse with a zone, and be between zero and maxAge seconds old.
func LastSuccess(value *string, now time.Time, maxAge int) (string, map[string]any) {
	values := map[string]any{"maxAgeSeconds": maxAge}
	if value == nil || *value == "" {
		return result.Reason("no_success"), values
	}
	completed, err := kube.ParseTime(*value)
	if err != nil {
		return result.Reason("invalid_timestamp"), values
	}
	age := now.Sub(completed).Seconds()
	values["lastSuccess"], values["ageSeconds"] = completed.Format(time.RFC3339), age
	switch {
	case age < 0:
		return result.Reason("future_timestamp", result.Labeled("in", result.Dur(age))), values
	case age > float64(maxAge):
		return result.Reason("stale", result.Labeled("last", result.Dur(age)), result.Labeled("max", result.Dur(float64(maxAge)))), values
	}
	return result.OK, values
}

func finished(job Job) (done, failed bool) {
	if kube.ConditionTrue(job.Status.Conditions, "Failed") {
		return true, true
	}
	return kube.ConditionTrue(job.Status.Conditions, "Complete"), false
}

func created(job Job) time.Time {
	t, err := kube.ParseTime(job.Metadata.CreationTimestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

func Evaluate(c config.CronJobs, sample Sample, now time.Time) result.Collection {
	collection := result.Collection{Route: Route, Check: "cronjob", Dimensions: dimensions}
	crons := map[string]CronJob{}
	for _, cron := range sample.CronJobs {
		crons[cron.Metadata.Namespace+"/"+cron.Metadata.Name] = cron
	}
	var missing []string
	for _, item := range c.Items {
		cron, found := crons[item.Key()]
		if !found {
			missing = append(missing, item.Key())
			collection.Items = append(collection.Items, result.Item{Target: item.Key(), Values: map[string]any{},
				Results: result.Set(dimensions, map[string]string{"schedule": result.Reason("missing"), "last_success": result.Reason("unknown"),
					"last_run": result.NA, "active": result.NA, "failures": result.NA})})
			continue
		}
		results := map[string]string{"schedule": result.OK}
		suspended := cron.Spec.Suspend != nil && *cron.Spec.Suspend
		if suspended {
			results["schedule"] = result.Reason("suspended")
		}
		var values map[string]any
		results["last_success"], values = LastSuccess(cron.Status.LastSuccessfulTime, now, item.MaxAgeSeconds)
		values["suspended"] = suspended
		values["maxRunSeconds"] = item.MaxRunSeconds
		if cron.Status.LastScheduleTime != "" {
			if t, err := kube.ParseTime(cron.Status.LastScheduleTime); err == nil {
				values["lastSchedule"] = t.Format(time.RFC3339)
			}
		}
		var jobs []Job
		for _, job := range sample.Jobs {
			if job.Metadata.Namespace == cron.Metadata.Namespace && kube.OwnedBy(job.Metadata, "CronJob", cron.Metadata.UID) {
				jobs = append(jobs, job)
			}
		}
		sort.SliceStable(jobs, func(i, j int) bool { return created(jobs[i]).After(created(jobs[j])) })
		evaluateRuns(jobs, item, now, results, values)
		collection.Items = append(collection.Items, result.Item{Target: item.Key(), Results: result.Set(dimensions, results), Values: values})
	}
	collection.Results = map[string]string{"inventory": result.Inventory(missing, nil)}
	return collection
}

func evaluateRuns(jobs []Job, item config.CronJob, now time.Time, results map[string]string, values map[string]any) {
	results["last_run"], results["active"], results["failures"] = result.NA, result.OK, result.OK
	maxRun := float64(item.MaxRunSeconds)
	stuck, active, consecutive := 0, 0, 0
	counting, sawFinished := true, false
	for _, job := range jobs {
		done, failed := finished(job)
		start, startErr := kube.ParseTime(job.Status.StartTime)
		if !done {
			active++
			if startErr == nil && now.Sub(start).Seconds() > maxRun {
				stuck++
			}
			continue
		}
		if !sawFinished {
			sawFinished = true
			results["last_run"] = result.OK
			values["lastRun"] = job.Metadata.Name
			if failed {
				results["last_run"] = result.Reason("failed")
			} else if end, err := kube.ParseTime(job.Status.CompletionTime); err == nil && startErr == nil && end.Sub(start).Seconds() > maxRun {
				results["last_run"] = result.Reason("overran", result.Labeled("took", result.Dur(end.Sub(start).Seconds())), result.Labeled("max", result.Dur(maxRun)))
			}
		}
		if counting {
			if failed {
				consecutive++
			} else {
				counting = false
			}
		}
	}
	values["active"], values["consecutiveFailures"] = active, consecutive
	if stuck > 0 {
		results["active"] = result.Reason("stuck", result.Int(int64(stuck)), result.Labeled("max", result.Dur(maxRun)))
	}
	if consecutive > item.MaxConsecutiveFailures {
		results["failures"] = result.Reason("consecutive_failures", result.Int(int64(consecutive)), result.Labeled("max", result.Int(int64(item.MaxConsecutiveFailures))))
	}
}
