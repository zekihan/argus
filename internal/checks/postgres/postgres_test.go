package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
)

var now = time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC)

// clusterJSON mirrors a live PGO v5 PostgresCluster status.
const clusterJSON = `{"metadata":{"namespace":"db","name":"db","uid":"u"},
"spec":{"instances":[{"name":"postgres","replicas":2}],"proxy":{"pgBouncer":{"replicas":2}}},
"status":{"instances":[{"name":"postgres","readyReplicas":2,"replicas":2,"updatedReplicas":2}],
"proxy":{"pgBouncer":{"readyReplicas":2,"replicas":2}},
"conditions":[{"type":"PGBackRestReplicaRepoReady","status":"True"},{"type":"ProxyAvailable","status":"True","message":"password=pgo-secret"}],
"pgbackrest":{"scheduledBackups":[
{"cronJobName":"db-repo1-full","repo":"repo1","startTime":"2026-11-01T02:10:00Z","completionTime":"2026-11-01T02:11:50Z","succeeded":1},
{"cronJobName":"db-repo1-incr","repo":"repo1","startTime":"2026-11-07T08:10:00Z","failed":1},
{"cronJobName":"db-repo1-incr","repo":"repo1","startTime":"2026-11-07T14:10:00Z","completionTime":"2026-11-07T14:10:31Z","succeeded":1}]}}}`

func decode(t *testing.T, value string) Cluster {
	t.Helper()
	var out Cluster
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type fakeAPI struct {
	clusters []Cluster
	err      error
}

func (f *fakeAPI) Collect(context.Context) ([]Cluster, error) { return f.clusters, f.err }

var cfg = config.Postgres{Clusters: []config.NamespacedName{{Namespace: "db", Name: "db"}}}

func run(t *testing.T, clusters ...Cluster) (*checktest.Harness, map[string]string) {
	h := checktest.New(New(cfg, &fakeAPI{clusters: clusters}), now).Refresh(t)
	_, e := h.Get(t, Route+"/db/db")
	return h, e.Results
}

func TestHealthyClusterAndRecoveredBackup(t *testing.T) {
	h, results := run(t, decode(t, clusterJSON))
	if code, e := h.Get(t, Route); code != 200 {
		t.Fatal(results, e)
	}
	h.NoLeak(t, "pgo-secret")
}

func TestInstancesProxyConditionsAndBackups(t *testing.T) {
	c := decode(t, clusterJSON)
	c.Status.Instances[0].ReadyReplicas = 1
	if _, r := run(t, c); r["instances"] != "not_ready(postgres)" {
		t.Fatal(r)
	}
	c = decode(t, clusterJSON)
	c.Status.Instances[0].UpdatedReplicas = 1
	if _, r := run(t, c); r["instances"] != "not_ready(postgres)" {
		t.Fatal(r)
	}
	c = decode(t, clusterJSON)
	c.Status.Instances = nil
	if _, r := run(t, c); r["instances"] != "not_ready(postgres)" {
		t.Fatal(r)
	}
	c = decode(t, clusterJSON)
	c.Status.Proxy.PGBouncer.ReadyReplicas = 0
	if _, r := run(t, c); r["proxy"] != "not_ready(ready 0, desired 2)" {
		t.Fatal(r)
	}
	c = decode(t, clusterJSON)
	c.Spec.Proxy = nil
	if _, r := run(t, c); r["proxy"] != "n/a" {
		t.Fatal(r)
	}
	c = decode(t, clusterJSON)
	c.Status.Conditions[1].Status = "False"
	if _, r := run(t, c); r["conditions"] != "false(ProxyAvailable)" {
		t.Fatal(r)
	}
	// A missing condition, such as PostgresDataInitialized, is not a failure.
	c = decode(t, clusterJSON)
	c.Status.Conditions = nil
	if _, r := run(t, c); r["conditions"] != "ok" {
		t.Fatal(r)
	}
	c = decode(t, clusterJSON)
	c.Status.PGBackRest.ScheduledBackups = c.Status.PGBackRest.ScheduledBackups[:2]
	if _, r := run(t, c); r["scheduled_backups"] != "latest_failed(db-repo1-incr)" {
		t.Fatal(r)
	}
}

func TestScheduledBackupTimestampAgainstCollectionTime(t *testing.T) {
	past := now.Add(-time.Second).Format(time.RFC3339)
	present := now.Format(time.RFC3339)
	future := now.Add(time.Second).Format(time.RFC3339)
	for _, tc := range []struct {
		name, start, completion string
		want                    string
		status                  int
	}{
		{"future start", future, "", "future_timestamp", 503},
		{"future completion", past, future, "future_timestamp", 503},
		{"future completion without start", "", future, "future_timestamp", 503},
		{"completed at collection time without start", "", present, "ok", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := decode(t, clusterJSON)
			// The later success record would otherwise supersede a failed run.
			backups := cluster.Status.PGBackRest.ScheduledBackups
			backups[2].StartTime, backups[2].CompletionTime = tc.start, tc.completion
			h, results := run(t, cluster)
			code, _ := h.Get(t, Route)
			if got := results["scheduled_backups"]; got != tc.want || code != tc.status {
				t.Fatalf("scheduled_backups = %q, aggregate status = %d, want %q and %d", got, code, tc.want, tc.status)
			}
		})
	}
}

func TestInventoryDriftBothWays(t *testing.T) {
	other := decode(t, clusterJSON)
	other.Metadata.Namespace, other.Metadata.Name = "app", "app"
	h, r := run(t, other)
	if r["instances"] != "missing" {
		t.Fatal(r)
	}
	if code, e := h.Get(t, Route); code != 503 || e.Results["inventory"] != "missing(1): db/db; unexpected(1): app/app" {
		t.Fatal(e.Results)
	}
}
