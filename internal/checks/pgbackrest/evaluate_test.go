package pgbackrest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/state"
)

var testNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func testConfig() config.PGBackRest {
	return config.PGBackRest{MaxAgeSeconds: 28800, Clusters: []config.Cluster{{Namespace: "db", Name: "db", Repos: map[string]config.Repository{"repo1": {}, "repo2": {MaxAgeSeconds: 36000}}}}}
}

func fixture[T any](t *testing.T, value string) T {
	t.Helper()
	var object T
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func testCluster(t *testing.T) kube.Cluster {
	return fixture[kube.Cluster](t, `{"metadata":{"namespace":"db","name":"db","uid":"cluster-uid","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"backups":{"pgbackrest":{"repos":[{"name":"repo1"},{"name":"repo2"}]}}},"status":{"pgbackrest":{"scheduledBackups":[{"repo":"repo1","succeeded":1,"completionTime":"2026-10-02T05:00:00Z"},{"repo":"repo2","succeeded":1,"completionTime":"2026-10-02T05:00:00Z"}]}}}`)
}

func testCron(t *testing.T) kube.CronJob {
	return fixture[kube.CronJob](t, `{"metadata":{"namespace":"db","name":"db-repo2-full","uid":"cron-uid","creationTimestamp":"2026-01-01T00:00:00Z","labels":{"postgres-operator.crunchydata.com/cluster":"db","postgres-operator.crunchydata.com/pgbackrest-repo":"repo2","postgres-operator.crunchydata.com/pgbackrest-backup":"scheduled"},"ownerReferences":[{"apiVersion":"postgres-operator.crunchydata.com/v1beta1","kind":"PostgresCluster","name":"db","uid":"cluster-uid","controller":true}]},"status":{"lastSuccessfulTime":"2026-10-02T10:00:00Z"}}`)
}

func testJob(t *testing.T) kube.Job {
	return fixture[kube.Job](t, `{"metadata":{"namespace":"db","name":"backup-job","uid":"job-uid","creationTimestamp":"2026-10-02T09:00:00Z","labels":{"postgres-operator.crunchydata.com/cluster":"db","postgres-operator.crunchydata.com/pgbackrest-repo":"repo1","postgres-operator.crunchydata.com/pgbackrest-backup":"scheduled"}},"status":{"succeeded":1,"completionTime":"2026-10-02T11:00:00Z","conditions":[{"type":"Complete","status":"True"}]}}`)
}

func evaluate(t *testing.T, sample Sample, old state.Ledger, now time.Time) (map[string]Result, state.Ledger) {
	t.Helper()
	results, ledger, err := Evaluate(testConfig(), sample, old, now)
	if err != nil {
		t.Fatal(err)
	}
	return results, ledger
}

func TestSourcesAndRetainedSuccess(t *testing.T) {
	cluster := testCluster(t)
	results, old := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow)
	if !results["db/db/repo1"].Healthy || !results["db/db/repo2"].Healthy {
		t.Fatal(results)
	}
	// Failed retries do not negate a successful completion.
	cluster.Status.PGBackRest.ScheduledBackups = []kube.Backup{{Repo: "repo1", Succeeded: 1, CompletionTime: "2026-10-02T05:00:00Z"}}
	results, old = evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{testJob(t)}, CronJobs: []kube.CronJob{testCron(t)}}, old, testNow)
	if results["db/db/repo1"].Source != "Completed Job" || results["db/db/repo2"].Source != "CronJob lastSuccessfulTime" {
		t.Fatal(results)
	}
	// Every live success source can disappear after it has been observed.
	cluster.Status.PGBackRest.ScheduledBackups = []kube.Backup{{Repo: "repo1", Succeeded: 0}}
	results, next := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, old, testNow.Add(time.Hour))
	if !results["db/db/repo1"].Healthy || !results["db/db/repo2"].Healthy || next["db/db/repo1"].LastSuccess != old["db/db/repo1"].LastSuccess {
		t.Fatal(results, next)
	}
	results, _ = evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, next, testNow.Add(10*time.Hour))
	if results["db/db/repo1"].Healthy || results["db/db/repo2"].Healthy {
		t.Fatal("retained completions must expire", results)
	}
}

func TestFreshnessOverrideAndInventory(t *testing.T) {
	cluster := testCluster(t)
	results, _ := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow.Add(2*time.Hour))
	if results["db/db/repo1"].Healthy || !results["db/db/repo2"].Healthy {
		t.Fatal(results)
	}
	results, _ = evaluate(t, Sample{}, nil, testNow)
	if results["db/db/repo1"].Reason != "Expected PostgresCluster is missing" {
		t.Fatal(results)
	}
	cluster.Spec.Backups.PGBackRest.Repos = cluster.Spec.Backups.PGBackRest.Repos[:1]
	results, _ = evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow)
	if results["db/db/repo2"].Reason != "Expected backup repository is missing" {
		t.Fatal(results)
	}
	cluster = testCluster(t)
	extra := cluster.Spec.Backups.PGBackRest.Repos[0]
	extra.Name = "repo3"
	cluster.Spec.Backups.PGBackRest.Repos = append(cluster.Spec.Backups.PGBackRest.Repos, extra)
	other := testCluster(t)
	other.Metadata.Namespace, other.Metadata.Name = "other", "other"
	results, _ = evaluate(t, Sample{Clusters: []kube.Cluster{cluster, other}}, nil, testNow)
	if results["db/db/repo3"].Healthy || results["db/db/repo3"].Reason == "" || results["other/other/repo1"].Reason == "" {
		t.Fatal(results)
	}
	other.Spec.Backups.PGBackRest.Repos = nil
	results, _ = evaluate(t, Sample{Clusters: []kube.Cluster{other}}, nil, testNow)
	if results["other/other/inventory"].Reason == "" {
		t.Fatal(results)
	}
}

func TestInventoryRemovalRetainsDestinationBarrier(t *testing.T) {
	cluster := testCluster(t)
	_, prior := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow)
	oldSuccess := prior["db/db/repo1"].LastSuccess
	withoutInventory := testConfig()
	withoutInventory.Clusters = nil
	cluster.Spec.Backups.PGBackRest.Global = map[string]string{"repo1-path": "/changed"}
	_, removed, err := Evaluate(withoutInventory, Sample{Clusters: []kube.Cluster{cluster}}, prior, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	entry := removed["db/db/repo1"]
	if !entry.LastSuccess.IsZero() || !entry.NotBefore.Equal(testNow.Add(time.Hour)) || entry.RepositoryID == prior["db/db/repo1"].RepositoryID {
		t.Fatal("removed inventory did not retain a destination-change barrier", entry)
	}
	store, err := openStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(removed); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Load()
	if err != nil || persisted["db/db/repo1"] != entry {
		t.Fatal("inventory tombstone did not survive checkpoint recovery", persisted, err)
	}
	removed = persisted
	// A later observed change must survive a clock rollback and a destination
	// revert while the repository remains outside configured inventory.
	cluster.Spec.Backups.PGBackRest.Global = nil
	_, reverted, err := Evaluate(withoutInventory, Sample{Clusters: []kube.Cluster{cluster}}, removed, testNow.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := reverted["db/db/repo1"].NotBefore; !got.Equal(testNow.Add(time.Hour)) {
		t.Fatal("destination barrier moved backwards", got)
	}
	cluster = testCluster(t)
	results, readded, err := Evaluate(testConfig(), Sample{Clusters: []kube.Cluster{cluster}}, reverted, testNow.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if results["db/db/repo1"].Healthy || readded["db/db/repo1"].LastSuccess.Equal(oldSuccess) {
		t.Fatal("re-added inventory reused an old aggregate success", results, readded)
	}
}

func TestLedgerLimitFailsClosedWithoutDroppingTombstones(t *testing.T) {
	previous := make(state.Ledger, 4000)
	created := testNow.Add(-24 * time.Hour)
	for i := range 4000 {
		previous[fmt.Sprintf("n%d/c/repo1", i)] = state.Entry{UID: "uid", Created: created, RepositoryID: "missing"}
	}
	cluster := testCluster(t)
	_, next, err := Evaluate(testConfig(), Sample{Clusters: []kube.Cluster{cluster}}, previous, testNow)
	if err == nil || len(next) <= 4000 {
		t.Fatal("oversized ledger was accepted or tombstones were discarded", len(next), err)
	}
}

func TestInvalidAndPrecreationTimes(t *testing.T) {
	for _, value := range []string{"", "invalid", "2026-10-03T00:00:00Z", "2025-01-01T00:00:00Z", "2026-10-02T11:00:00", "2026-10-02T11:00:00+25:00"} {
		t.Run(value, func(t *testing.T) {
			for _, source := range []string{"status", "job", "cron"} {
				cluster := testCluster(t)
				cluster.Status.PGBackRest.ScheduledBackups = nil
				sample := Sample{Clusters: []kube.Cluster{cluster}}
				key := "db/db/repo1"
				switch source {
				case "status":
					sample.Clusters[0].Status.PGBackRest.ScheduledBackups = []kube.Backup{{Repo: "repo1", Succeeded: 1, CompletionTime: value}}
				case "job":
					job := testJob(t)
					job.Status.CompletionTime = value
					sample.Jobs = []kube.Job{job}
				case "cron":
					cron := testCron(t)
					cron.Status.LastSuccessfulTime = value
					sample.CronJobs = []kube.CronJob{cron}
					key = "db/db/repo2"
				}
				results, _ := evaluate(t, sample, nil, testNow)
				if results[key].Healthy {
					t.Fatalf("accepted %s time %q", source, value)
				}
			}
		})
	}
	cluster := testCluster(t)
	_, old := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow)
	cluster.Status.PGBackRest.ScheduledBackups[0].CompletionTime = "future"
	results, next := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, old, testNow)
	if results["db/db/repo1"].Healthy || next["db/db/repo1"].LastSuccess != old["db/db/repo1"].LastSuccess {
		t.Fatal("invalid sample must fail closed without replacing valid evidence")
	}
}

func TestCronIdentityAndOwnership(t *testing.T) {
	mutations := map[string]func(*kube.CronJob){
		"namespace":  func(c *kube.CronJob) { c.Metadata.Namespace = "other" },
		"cluster":    func(c *kube.CronJob) { c.Metadata.Labels[kube.Prefix+"cluster"] = "other" },
		"repo":       func(c *kube.CronJob) { c.Metadata.Labels[kube.Prefix+"pgbackrest-repo"] = "repo1" },
		"backup":     func(c *kube.CronJob) { c.Metadata.Labels[kube.Prefix+"pgbackrest-backup"] = "manual" },
		"uid":        func(c *kube.CronJob) { c.Metadata.Owners[0].UID = "old-cluster" },
		"empty uid":  func(c *kube.CronJob) { c.Metadata.Owners[0].UID = "" },
		"kind":       func(c *kube.CronJob) { c.Metadata.Owners[0].Kind = "Other" },
		"api":        func(c *kube.CronJob) { c.Metadata.Owners[0].APIVersion = "other/v1" },
		"owner name": func(c *kube.CronJob) { c.Metadata.Owners[0].Name = "other" },
		"controller": func(c *kube.CronJob) { c.Metadata.Owners[0].Controller = false },
		"old object": func(c *kube.CronJob) { c.Metadata.CreationTimestamp = "2025-01-01T00:00:00Z" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cluster, cron := testCluster(t), testCron(t)
			cluster.Status.PGBackRest.ScheduledBackups = nil
			mutate(&cron)
			results, _ := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, CronJobs: []kube.CronJob{cron}}, nil, testNow)
			if results["db/db/repo2"].Healthy {
				t.Fatal("unrelated cron supplied success")
			}
		})
	}
}

func TestJobCompletionAndRecreatedCluster(t *testing.T) {
	cluster := testCluster(t)
	_, old := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow)
	cluster.Status.PGBackRest.ScheduledBackups = nil
	for _, mutate := range []func(*kube.Job){
		func(j *kube.Job) { j.Status.Succeeded = 0 },
		func(j *kube.Job) { j.Status.Conditions = nil },
		func(j *kube.Job) { j.Status.Conditions[0].Status = "False" },
		func(j *kube.Job) { j.Metadata.Namespace = "other" },
		func(j *kube.Job) { j.Metadata.Labels[kube.Prefix+"cluster"] = "other" },
		func(j *kube.Job) { j.Metadata.Labels[kube.Prefix+"pgbackrest-repo"] = "repo2" },
		func(j *kube.Job) { j.Metadata.Labels[kube.Prefix+"pgbackrest-backup"] = "" },
		func(j *kube.Job) { j.Metadata.CreationTimestamp = "2025-01-01T00:00:00Z" },
	} {
		job := testJob(t)
		mutate(&job)
		results, _ := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{job}}, nil, testNow)
		if results["db/db/repo1"].Healthy {
			t.Fatal(results)
		}
	}
	cluster.Metadata.UID = "recreated-uid"
	cluster.Metadata.CreationTimestamp = "2026-10-02T10:30:00Z"
	// An old Job can finish after recreation; its creation time still excludes it.
	results, next := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{testJob(t)}, CronJobs: []kube.CronJob{testCron(t)}}, old, testNow)
	if results["db/db/repo1"].Healthy || results["db/db/repo2"].Healthy || !next["db/db/repo1"].LastSuccess.IsZero() || !next["db/db/repo2"].LastSuccess.IsZero() {
		t.Fatal("old cluster history reused", results)
	}
	job := testJob(t)
	job.Metadata.CreationTimestamp = "2026-10-02T10:45:00Z"
	job.Metadata.Owners = []kube.Owner{{APIVersion: kube.ClusterAPI, Kind: "PostgresCluster", Name: "db", UID: "cluster-uid", Controller: true}}
	results, _ = evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{job}}, nil, testNow)
	if results["db/db/repo1"].Healthy {
		t.Fatal("old controller UID accepted")
	}
	job.Metadata.Owners[0].UID = cluster.Metadata.UID
	results, _ = evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{job}}, nil, testNow)
	if !results["db/db/repo1"].Healthy {
		t.Fatal("current controller rejected")
	}
}

func TestJobCronOwnerChain(t *testing.T) {
	cluster, job, cron := testCluster(t), testJob(t), testCron(t)
	cluster.Status.PGBackRest.ScheduledBackups = nil
	job.Metadata.Labels[kube.Prefix+"pgbackrest-repo"] = "repo2"
	job.Metadata.Owners = []kube.Owner{{Kind: "CronJob", APIVersion: "batch/v1", Name: cron.Metadata.Name, UID: cron.Metadata.UID, Controller: true}}
	cron.Status.LastSuccessfulTime = ""
	results, _ := evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{job}, CronJobs: []kube.CronJob{cron}}, nil, testNow)
	if !results["db/db/repo2"].Healthy {
		t.Fatal(results)
	}
	cron.Metadata.Owners[0].UID = "other"
	results, _ = evaluate(t, Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{job}, CronJobs: []kube.CronJob{cron}}, nil, testNow)
	if results["db/db/repo2"].Healthy {
		t.Fatal(results)
	}
}

func TestMalformedSampleRejected(t *testing.T) {
	for _, mutate := range []func(*kube.Cluster){
		func(c *kube.Cluster) { c.Metadata.Namespace = "bad/name" },
		func(c *kube.Cluster) { c.Metadata.UID = "" },
		func(c *kube.Cluster) { c.Metadata.CreationTimestamp = "future" },
		func(c *kube.Cluster) { c.Spec.Backups.PGBackRest.Repos[0].Name = "bad/repo" },
		func(c *kube.Cluster) {
			c.Spec.Backups.PGBackRest.Repos[0].S3 = map[string]string{"bucket": "s3"}
			c.Spec.Backups.PGBackRest.Repos[0].GCS = map[string]string{"bucket": "gcs"}
		},
		func(c *kube.Cluster) {
			c.Spec.Backups.PGBackRest.Repos = append(c.Spec.Backups.PGBackRest.Repos, c.Spec.Backups.PGBackRest.Repos[0])
		},
	} {
		cluster := testCluster(t)
		mutate(&cluster)
		if _, _, err := Evaluate(testConfig(), Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow); err == nil {
			t.Fatal("invalid sample accepted")
		}
	}
	cluster := testCluster(t)
	if _, _, err := Evaluate(testConfig(), Sample{Clusters: []kube.Cluster{cluster, cluster}}, nil, testNow); err == nil {
		t.Fatal("duplicate sample accepted")
	}
}

func TestCronCompletionBeforeCreationFailsClosed(t *testing.T) {
	const key = "db/db/repo2"
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained=%t", retained), func(t *testing.T) {
			cluster := testCluster(t)
			var old state.Ledger
			if retained {
				_, old = evaluate(t, Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow)
			}
			// Keep a valid live aggregate as well: invalid eligible CronJob
			// evidence must fail closed even if another source is fresh.
			cron := testCron(t)
			birth := testNow.Add(-time.Hour)
			cron.Metadata.CreationTimestamp = birth.Format(time.RFC3339Nano)
			cron.Status.LastSuccessfulTime = birth.Add(-time.Nanosecond).Format(time.RFC3339Nano)
			sample := Sample{Clusters: []kube.Cluster{cluster}, CronJobs: []kube.CronJob{cron}}
			results, next := evaluate(t, sample, old, testNow)
			if results[key].Healthy || results[key].Reason != "Invalid successful backup completion timestamp" {
				t.Fatal("completion before CronJob creation did not fail closed", results[key])
			}
			if !next[key].LastSuccess.Equal(old[key].LastSuccess) || next[key].Source != old[key].Source {
				t.Fatal("invalid sample changed validated evidence", next[key], old[key])
			}
			if !results["db/db/repo1"].Healthy {
				t.Fatal("invalid CronJob affected another repository", results)
			}
			// Creation equality is valid, and correcting the source restores health.
			sample.CronJobs[0].Status.LastSuccessfulTime = birth.Format(time.RFC3339Nano)
			results, next = evaluate(t, sample, next, testNow)
			if !results[key].Healthy || !next[key].LastSuccess.Equal(birth) || results[key].Source != "CronJob lastSuccessfulTime" {
				t.Fatal("valid creation boundary did not recover", results[key], next[key])
			}
		})
	}
}
