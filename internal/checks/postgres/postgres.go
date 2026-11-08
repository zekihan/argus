// Package postgres checks Crunchy PostgresClusters: instance and PgBouncer
// readiness, False conditions, the latest scheduled backup per CronJob, and
// inventory drift in both directions.
package postgres

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

const Route = "postgres"

var dimensions = []string{"instances", "proxy", "conditions", "scheduled_backups"}

type replicas struct {
	Replicas *int `json:"replicas"`
}

type Cluster struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		Shutdown  *bool `json:"shutdown"`
		Instances []struct {
			Name     string `json:"name"`
			Replicas *int   `json:"replicas"`
		} `json:"instances"`
		Proxy *struct {
			PGBouncer *replicas `json:"pgBouncer"`
		} `json:"proxy"`
	} `json:"spec"`
	Status struct {
		Instances []struct {
			Name            string `json:"name"`
			Replicas        int    `json:"replicas"`
			ReadyReplicas   int    `json:"readyReplicas"`
			UpdatedReplicas int    `json:"updatedReplicas"`
		} `json:"instances"`
		Proxy struct {
			PGBouncer struct {
				Replicas      int `json:"replicas"`
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"pgBouncer"`
		} `json:"proxy"`
		Conditions []kube.Condition `json:"conditions"`
		PGBackRest struct {
			ScheduledBackups []struct {
				CronJobName    string `json:"cronJobName"`
				StartTime      string `json:"startTime"`
				CompletionTime string `json:"completionTime"`
				Succeeded      int    `json:"succeeded"`
				Failed         int    `json:"failed"`
			} `json:"scheduledBackups"`
		} `json:"pgbackrest"`
	} `json:"status"`
}

type Collector interface {
	Collect(ctx context.Context) ([]Cluster, error)
}

// API lists PostgresClusters cluster-wide, for inventory drift.
type API struct{ Client *kube.Client }

func (a API) Collect(ctx context.Context) ([]Cluster, error) {
	return kube.List[Cluster](ctx, a.Client, "/apis/"+kube.ClusterAPI+"/postgresclusters", "")
}

type Check struct {
	config config.Postgres
	api    Collector
}

func New(c config.Postgres, api Collector) *Check { return &Check{config: c, api: api} }

func (c *Check) Name() string { return "postgres" }

func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "postgres/cluster", Dimensions: dimensions}
	for _, cluster := range c.config.Clusters {
		collection.Items = append(collection.Items, result.Item{Target: cluster.Key()})
	}
	return []result.Collection{collection}
}

func (c *Check) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	clusters, err := c.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	// Evaluate after reading: objects updated during the read are not "future".
	now := clock()
	return []result.Collection{c.Evaluate(clusters, now)}, nil
}

func (c *Check) Evaluate(clusters []Cluster, now time.Time) result.Collection {
	byKey := map[string]Cluster{}
	for _, cluster := range clusters {
		byKey[cluster.Metadata.Namespace+"/"+cluster.Metadata.Name] = cluster
	}
	collection := result.Collection{Route: Route, Check: "postgres/cluster", Dimensions: dimensions}
	expected := map[string]bool{}
	var missing, unexpected []string
	for _, e := range c.config.Clusters {
		expected[e.Key()] = true
		cluster, ok := byKey[e.Key()]
		if !ok {
			missing = append(missing, e.Key())
			collection.Items = append(collection.Items, result.Item{Target: e.Key(), Values: map[string]any{},
				Results: map[string]string{"instances": result.Reason("missing"), "proxy": result.NA, "conditions": result.NA, "scheduled_backups": result.NA}})
			continue
		}
		collection.Items = append(collection.Items, evaluate(e.Key(), cluster))
	}
	for key := range byKey {
		if !expected[key] {
			unexpected = append(unexpected, key)
		}
	}
	collection.Results = map[string]string{"inventory": result.Inventory(missing, unexpected)}
	return collection
}

func desired(r *int) int {
	if r == nil {
		return 1
	}
	return *r
}

func evaluate(key string, cluster Cluster) result.Item {
	results := map[string]string{}
	values := map[string]any{}
	status := cluster.Status
	// Instances: every instance set ready and updated against its desired replicas.
	if cluster.Spec.Shutdown != nil && *cluster.Spec.Shutdown {
		results["instances"], results["proxy"] = result.NA, result.NA
		values["shutdown"] = true
	} else {
		var failing []result.Param
		ready, want := 0, 0
		for _, spec := range cluster.Spec.Instances {
			n := desired(spec.Replicas)
			want += n
			found := false
			for _, s := range status.Instances {
				if s.Name == spec.Name {
					found = true
					ready += s.ReadyReplicas
					if s.ReadyReplicas < n || s.UpdatedReplicas < n {
						failing = append(failing, result.Name(spec.Name))
					}
				}
			}
			if !found {
				failing = append(failing, result.Name(spec.Name))
			}
		}
		results["instances"] = result.OK
		if len(cluster.Spec.Instances) == 0 {
			results["instances"] = result.Reason("no_instances")
		} else if len(failing) > 0 {
			results["instances"] = result.Reason("not_ready", failing...)
		}
		values["instancesReady"], values["instancesDesired"] = ready, want
		results["proxy"] = result.NA
		if cluster.Spec.Proxy != nil && cluster.Spec.Proxy.PGBouncer != nil {
			n := desired(cluster.Spec.Proxy.PGBouncer.Replicas)
			got := status.Proxy.PGBouncer.ReadyReplicas
			values["proxyReady"], values["proxyDesired"] = got, n
			results["proxy"] = result.OK
			if got < n {
				results["proxy"] = result.Reason("not_ready", result.Labeled("ready", result.Int(int64(got))), result.Labeled("desired", result.Int(int64(n))))
			}
		}
	}
	// Any present condition that is False; absent conditions are not failures.
	var falseTypes []string
	for _, condition := range status.Conditions {
		if condition.Status == "False" {
			falseTypes = append(falseTypes, condition.Type)
		}
	}
	results["conditions"] = result.OK
	if len(falseTypes) > 0 {
		sort.Strings(falseTypes)
		params := make([]result.Param, len(falseTypes))
		for i, t := range falseTypes {
			params[i] = result.Name(t)
		}
		results["conditions"] = result.Reason("false", params...)
	}
	results["scheduled_backups"] = scheduledBackups(cluster)
	return result.Item{Target: key, Results: result.Set(dimensions, results), Values: values}
}

// scheduledBackups fails when the latest run of any backup CronJob failed.
func scheduledBackups(cluster Cluster) string {
	type run struct {
		start             time.Time
		failed, succeeded int
	}
	latest := map[string]run{}
	for _, b := range cluster.Status.PGBackRest.ScheduledBackups {
		start, err := kube.ParseTime(b.StartTime)
		if err != nil {
			start, err = kube.ParseTime(b.CompletionTime)
			if err != nil {
				continue
			}
		}
		if current, ok := latest[b.CronJobName]; !ok || start.After(current.start) {
			latest[b.CronJobName] = run{start: start, failed: b.Failed, succeeded: b.Succeeded}
		}
	}
	if len(latest) == 0 {
		return result.NA
	}
	var failed []string
	for name, r := range latest {
		if r.failed > 0 && r.succeeded == 0 {
			failed = append(failed, name)
		}
	}
	if len(failed) == 0 {
		return result.OK
	}
	sort.Strings(failed)
	return result.Reason("latest_failed", result.Name(strings.Join(failed, ",")))
}
