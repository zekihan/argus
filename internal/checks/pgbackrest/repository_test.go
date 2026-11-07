package pgbackrest

import (
	"github.com/zekihan/argus/internal/kube"
	"path/filepath"
	"testing"
	"time"
)

func TestChangedRepositoryRejectsOldCompletion(t *testing.T) {
	before := fixture[kube.Cluster](t, `{"metadata":{"namespace":"db","name":"db","uid":"same-cluster","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"backups":{"pgbackrest":{"repos":[{"name":"repo1","s3":{"bucket":"old-bucket","endpoint":"s3.example.com","region":"eu-west-1"}},{"name":"repo2"}]}}},"status":{"pgbackrest":{"scheduledBackups":[{"repo":"repo1","succeeded":1,"completionTime":"2026-10-02T11:00:00Z"}]}}}`)
	_, ledger := evaluate(t, Sample{Clusters: []kube.Cluster{before}}, nil, testNow)
	after := fixture[kube.Cluster](t, `{"metadata":{"namespace":"db","name":"db","uid":"same-cluster","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"backups":{"pgbackrest":{"repos":[{"name":"repo1","s3":{"bucket":"new-empty-bucket","endpoint":"s3.example.com","region":"eu-west-1"}},{"name":"repo2"}]}}}}`)
	results, _ := evaluate(t, Sample{Clusters: []kube.Cluster{after}}, ledger, testNow)
	if results["db/db/repo1"].Healthy {
		t.Fatalf("new repository with no completion reuses old bucket success: %+v", results["db/db/repo1"])
	}
}

func changeDestination(cluster *kube.Cluster) {
	cluster.Spec.Backups.PGBackRest.Global = map[string]string{"repo1-path": "/new/repo1", "repo2-path": "/new/repo2"}
}

func TestDestinationChangeRejectsRetainedSourcesAndSurvivesRestart(t *testing.T) {
	cluster := testCluster(t)
	sample := Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{testJob(t)}, CronJobs: []kube.CronJob{testCron(t)}}
	_, old := evaluate(t, sample, nil, testNow)
	changeDestination(&sample.Clusters[0])
	// Even completions recorded after the change can belong to old running jobs.
	sample.Jobs[0].Status.CompletionTime = testNow.Add(time.Minute).Format(time.RFC3339)
	sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].StartTime = "2026-10-02T09:00:00Z"
	sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].CompletionTime = testNow.Add(time.Minute).Format(time.RFC3339)
	sample.CronJobs[0].Status.LastSuccessfulTime = testNow.Add(time.Minute).Format(time.RFC3339)
	results, changed := evaluate(t, sample, old, testNow)
	for _, key := range []string{"db/db/repo1", "db/db/repo2"} {
		if results[key].Healthy || !changed[key].LastSuccess.IsZero() || !changed[key].NotBefore.Equal(testNow) {
			t.Fatal(results, changed)
		}
	}
	// Persist and reload the identity-only barrier, rather than manufacturing a success.
	store, err := openStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(changed); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	results, changed = evaluate(t, sample, recovered, testNow.Add(2*time.Minute))
	if results["db/db/repo1"].Healthy || results["db/db/repo2"].Healthy {
		t.Fatal("retained history revived after restart", results)
	}
	// A new Job and newly created CronJob can establish success for the new destination.
	sample.Jobs[0].Metadata.CreationTimestamp = testNow.Add(3 * time.Minute).Format(time.RFC3339)
	sample.Jobs[0].Status.CompletionTime = testNow.Add(4 * time.Minute).Format(time.RFC3339)
	sample.CronJobs[0].Metadata.CreationTimestamp = testNow.Add(3 * time.Minute).Format(time.RFC3339)
	sample.CronJobs[0].Status.LastSuccessfulTime = testNow.Add(4 * time.Minute).Format(time.RFC3339)
	results, changed = evaluate(t, sample, changed, testNow.Add(5*time.Minute))
	if !results["db/db/repo1"].Healthy || !results["db/db/repo2"].Healthy {
		t.Fatal(results)
	}
	sample.Jobs = nil
	sample.CronJobs = nil
	sample.Clusters[0].Status.PGBackRest.ScheduledBackups = nil
	results, _ = evaluate(t, sample, changed, testNow.Add(6*time.Minute))
	if !results["db/db/repo1"].Healthy || !results["db/db/repo2"].Healthy {
		t.Fatal("new checkpoint lost after cleanup", results)
	}
}

func TestDestinationChangeDoesNotPoisonNewEvidence(t *testing.T) {
	sample := Sample{Clusters: []kube.Cluster{testCluster(t)}, Jobs: []kube.Job{testJob(t)}, CronJobs: []kube.CronJob{testCron(t)}}
	_, old := evaluate(t, sample, nil, testNow)
	changeDestination(&sample.Clusters[0])
	_, changed := evaluate(t, sample, old, testNow)
	sample.Jobs[0].Status.CompletionTime = "malformed old completion"
	sample.CronJobs[0].Status.LastSuccessfulTime = "malformed old completion"
	sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].StartTime = testNow.Add(time.Minute).Format(time.RFC3339)
	sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].CompletionTime = testNow.Add(2 * time.Minute).Format(time.RFC3339)
	newJob := testJob(t)
	newJob.Metadata.CreationTimestamp = testNow.Add(time.Minute).Format(time.RFC3339)
	newJob.Status.CompletionTime = testNow.Add(2 * time.Minute).Format(time.RFC3339)
	sample.Jobs = append(sample.Jobs, newJob)
	results, _ := evaluate(t, sample, changed, testNow.Add(3*time.Minute))
	if !results["db/db/repo1"].Healthy {
		t.Fatal(results)
	}
}

func TestDestinationRemovalAndLegacyCheckpoint(t *testing.T) {
	for _, missing := range []string{"repo", "cluster", "legacy"} {
		t.Run(missing, func(t *testing.T) {
			cluster := testCluster(t)
			sample := Sample{Clusters: []kube.Cluster{cluster}}
			_, old := evaluate(t, sample, nil, testNow)
			switch missing {
			case "repo":
				sample.Clusters[0].Spec.Backups.PGBackRest.Repos = nil
			case "cluster":
				sample.Clusters = nil
			case "legacy":
				for key, entry := range old {
					entry.RepositoryID = ""
					old[key] = entry
				}
			}
			_, old = evaluate(t, sample, old, testNow.Add(time.Minute))
			sample.Clusters = []kube.Cluster{testCluster(t)}
			results, old := evaluate(t, sample, old, testNow.Add(2*time.Minute))
			if results["db/db/repo1"].Healthy || !old["db/db/repo1"].LastSuccess.IsZero() {
				t.Fatal("stale success reused", results)
			}
			results, _ = evaluate(t, sample, old, testNow.Add(3*time.Minute))
			if results["db/db/repo1"].Healthy {
				t.Fatal("barrier discarded", results)
			}
		})
	}
}

func TestRepositoryIdentityIgnoresHarmlessChanges(t *testing.T) {
	before := fixture[kube.Cluster](t, `{"spec":{"backups":{"pgbackrest":{"repos":[{"name":"repo1","s3":{"bucket":"backup","endpoint":"s3.example","region":"eu-west-1"},"schedules":{"full":"0 0 * * *"}}]}}}}`)
	after := fixture[kube.Cluster](t, `{"spec":{"backups":{"pgbackrest":{"global":{"repo1-path":"/pgbackrest/repo1","repo1-retention-full":"4","repo1-s3-key":"changed-credential","repo2-path":"/different"},"repos":[{"name":"repo1","s3":{"region":"eu-west-1","endpoint":"s3.example","bucket":"backup"},"schedules":{"full":"0 1 * * *"}}]}}}}`)
	if repositoryIdentity(before, "repo1") != repositoryIdentity(after, "repo1") {
		t.Fatal("harmless config invalidates checkpoints")
	}
	after.Spec.Backups.PGBackRest.Global["repo1-s3-bucket"] = "override"
	overridden := repositoryIdentity(after, "repo1")
	after.Spec.Backups.PGBackRest.Repos[0].S3["bucket"] = "shadowed"
	if overridden != repositoryIdentity(after, "repo1") {
		t.Fatal("shadowed setting changed effective destination")
	}
	if overridden == repositoryIdentity(before, "repo1") {
		t.Fatal("global destination override ignored")
	}
}

func TestRepositoryDestinationFields(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		{"s3 bucket", `"s3":{"bucket":"old"}`, `"s3":{"bucket":"new"}`},
		{"s3 endpoint", `"s3":{"endpoint":"old"}`, `"s3":{"endpoint":"new"}`},
		{"gcs bucket", `"gcs":{"bucket":"old"}`, `"gcs":{"bucket":"new"}`},
		{"azure container", `"azure":{"container":"old"}`, `"azure":{"container":"new"}`},
		{"backend", `"gcs":{"bucket":"same"}`, `"s3":{"bucket":"same"}`},
		{"PVC binding", `"volume":{"volumeClaimSpec":{"volumeName":"old"}}`, `"volume":{"volumeClaimSpec":{"volumeName":"new"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			makeCluster := func(fields string) kube.Cluster {
				return fixture[kube.Cluster](t, `{"spec":{"backups":{"pgbackrest":{"repos":[{"name":"repo1",`+fields+`}]}}}}`)
			}
			if repositoryIdentity(makeCluster(tc.before), "repo1") == repositoryIdentity(makeCluster(tc.after), "repo1") {
				t.Fatal("destination change ignored")
			}
		})
	}
	for _, option := range []string{"path", "azure-account", "storage-host", "storage-port", "sftp-host", "s3-bucket", "type"} {
		t.Run(option, func(t *testing.T) {
			cluster := testCluster(t)
			before := repositoryIdentity(cluster, "repo1")
			cluster.Spec.Backups.PGBackRest.Global = map[string]string{"repo1-" + option: "changed"}
			if before == repositoryIdentity(cluster, "repo1") {
				t.Fatal("global destination ignored")
			}
		})
	}
	before := fixture[kube.Cluster](t, `{"spec":{"backups":{"pgbackrest":{"repos":[{"name":"repo1","volume":{"volumeClaimSpec":{"resources":{"requests":{"storage":"1Gi"}}}}}]}}},"status":{"pgbackrest":{"repos":[{"name":"repo1","volume":"pv-old"}]}}}`)
	after := fixture[kube.Cluster](t, `{"spec":{"backups":{"pgbackrest":{"repos":[{"name":"repo1","volume":{"volumeClaimSpec":{"resources":{"requests":{"storage":"2Gi"}},"autoGrow":true}}}]}}},"status":{"pgbackrest":{"repos":[{"name":"repo1","volume":"pv-old"}]}}}`)
	if repositoryIdentity(before, "repo1") != repositoryIdentity(after, "repo1") {
		t.Fatal("volume size invalidates destination")
	}
	after.Status.PGBackRest.Repos[0].Volume = "pv-new"
	if repositoryIdentity(before, "repo1") == repositoryIdentity(after, "repo1") {
		t.Fatal("bound volume change ignored")
	}
}

func TestBarrierRejectsResumedStatusAndClockRollback(t *testing.T) {
	sample := Sample{Clusters: []kube.Cluster{testCluster(t)}}
	_, old := evaluate(t, sample, nil, testNow)
	changeDestination(&sample.Clusters[0])
	_, old = evaluate(t, sample, old, testNow.Add(10*time.Minute))
	// A resumed old Job can gain a fresh startTime; aggregate status is not proof
	// that its creation (and destination) belongs to the new repository epoch.
	sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].StartTime = testNow.Add(11 * time.Minute).Format(time.RFC3339)
	sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].CompletionTime = testNow.Add(12 * time.Minute).Format(time.RFC3339)
	results, old := evaluate(t, sample, old, testNow.Add(13*time.Minute))
	if results["db/db/repo1"].Healthy {
		t.Fatal("resumed status accepted", results)
	}
	sample.Clusters[0].Spec.Backups.PGBackRest.Global["repo1-path"] = "/changed-again"
	_, old = evaluate(t, sample, old, testNow.Add(5*time.Minute))
	if !old["db/db/repo1"].NotBefore.Equal(testNow.Add(10 * time.Minute)) {
		t.Fatal("rollback lowered boundary", old)
	}
}

func TestMissingClusterClockRollbackKeepsCheckpointLoadable(t *testing.T) {
	_, old := evaluate(t, Sample{Clusters: []kube.Cluster{testCluster(t)}}, nil, testNow)
	birth := old["db/db/repo1"].Created
	_, missing := evaluate(t, Sample{}, old, birth.Add(-time.Hour))
	if missing["db/db/repo1"].NotBefore.Before(birth) {
		t.Fatal("invalid rollback boundary", missing)
	}
	store, err := openStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatal("rollback corrupted checkpoint", err)
	}
}

func TestClockRollbackPreservesSuccessAcrossRestart(t *testing.T) {
	for _, withBoundary := range []bool{false, true} {
		name := "completion in future"
		if withBoundary {
			name = "boundary and completion in future"
		}
		t.Run(name, func(t *testing.T) {
			sample := Sample{Clusters: []kube.Cluster{testCluster(t)}, Jobs: []kube.Job{testJob(t)}}
			_, ledger := evaluate(t, sample, nil, testNow)
			const key = "db/db/repo1"
			want := ledger[key]
			if withBoundary {
				want.NotBefore = want.LastSuccess.Add(-time.Minute)
				ledger[key] = want
			}
			// Kubernetes has already cleaned up all sources of this completion.
			sample.Jobs = nil
			sample.Clusters[0].Status.PGBackRest.ScheduledBackups = nil
			rollback := want.LastSuccess.Add(-2 * time.Minute)
			for range 2 {
				results, next := evaluate(t, sample, ledger, rollback)
				if results[key].Healthy || next[key] != want {
					t.Fatal("rollback must fail closed without losing retained evidence", results[key], next[key])
				}
				ledger = next
			}
			path := filepath.Join(t.TempDir(), "state.json")
			store, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ledger); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			recovered, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			results, recovered := evaluate(t, sample, recovered, rollback)
			if results[key].Healthy || recovered[key] != want {
				t.Fatal("restart during rollback lost evidence or reported healthy", results[key])
			}
			results, recovered = evaluate(t, sample, recovered, testNow)
			if !results[key].Healthy || recovered[key] != want {
				t.Fatal("clock recovery did not restore retained success", results[key])
			}
			// A changed destination still invalidates evidence during rollback.
			changeDestination(&sample.Clusters[0])
			results, changed := evaluate(t, sample, recovered, rollback)
			if results[key].Healthy || !changed[key].LastSuccess.IsZero() {
				t.Fatal("rollback preserved evidence for a changed destination", results[key], changed[key])
			}
		})
	}
}
