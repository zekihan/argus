package pgbackrest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
	"github.com/zekihan/argus/internal/state"
)

const emptyLedger = `{"version":2,"entries":{}}`

type fakeAPI struct {
	mu     sync.Mutex
	sample Sample
	err    error
}

func (f *fakeAPI) Collect(context.Context) (Sample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sample, f.err
}

func (f *fakeAPI) set(update func(*Sample)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(&f.sample)
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func harness(check *Check) *runner.Runner {
	r := runner.New(discard, "test", nil, 3*time.Minute)
	r.Add(check, runner.Timing{Interval: time.Minute, Timeout: 5 * time.Second, MaxAge: 180 * time.Second})
	return r
}

func lookup(t *testing.T, r *runner.Runner, key string, now time.Time) (int, result.Envelope) {
	t.Helper()
	path := Route
	if key != "" {
		path += "/" + key
	}
	code, body := r.Lookup(path, now)
	envelope, ok := body.(result.Envelope)
	if !ok {
		t.Fatalf("%s: %d %+v", path, code, body)
	}
	return code, envelope
}

// liveCluster is testCluster with recent, wall-clock-valid timestamps.
func liveCluster(t *testing.T) kube.Cluster {
	cluster := testCluster(t)
	now := time.Now().UTC()
	cluster.Metadata.CreationTimestamp = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	for i := range cluster.Status.PGBackRest.ScheduledBackups {
		cluster.Status.PGBackRest.ScheduledBackups[i].CompletionTime = now.Add(-time.Hour).Format(time.RFC3339Nano)
	}
	return withStanzas(cluster)
}

func withStanzas(cluster kube.Cluster) kube.Cluster {
	cluster.Status.PGBackRest.Repos = nil
	for _, repo := range cluster.Spec.Backups.PGBackRest.Repos {
		cluster.Status.PGBackRest.Repos = append(cluster.Status.PGBackRest.Repos, struct {
			Name          string `json:"name"`
			Volume        string `json:"volume"`
			StanzaCreated bool   `json:"stanzaCreated"`
		}{Name: repo.Name, StanzaCreated: true})
	}
	return cluster
}

func memoryStore(data string) (*state.Memory, *state.Store) {
	backend := state.NewMemory(data, true)
	return backend, state.New(backend)
}

func TestRestartRecoveryAndFailedCollection(t *testing.T) {
	cluster := liveCluster(t)
	api := &fakeAPI{sample: Sample{Clusters: []kube.Cluster{cluster}}}
	backend, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), api, store, false, discard))
	if code, _ := lookup(t, r, "", time.Now()); code != 503 {
		t.Fatal(code)
	}
	r.Refresh(t.Context(), "pgbackrest")
	if code, e := lookup(t, r, "", time.Now()); code != 200 {
		t.Fatal(code, e)
	}
	api.set(func(s *Sample) { api.err = errors.New("sensitive response body and credentials") })
	r.Refresh(t.Context(), "pgbackrest")
	if code, e := lookup(t, r, "", time.Now()); code != 503 || strings.Contains(e.Summary, "sensitive") || e.Results["collection"] != "collection_error" {
		t.Fatal(code, e)
	}
	cluster.Status.PGBackRest.ScheduledBackups = nil
	api.set(func(s *Sample) { api.err, s.Clusters = nil, []kube.Cluster{cluster} })
	restarted := harness(New(testConfig(), api, state.New(backend), false, discard))
	if code, _ := lookup(t, restarted, "", time.Now()); code != 503 {
		t.Fatal("ledger alone must not mark a startup sample healthy")
	}
	restarted.Refresh(t.Context(), "pgbackrest")
	if code, e := lookup(t, restarted, "", time.Now()); code != 200 {
		t.Fatal("restart lost completion evidence", code, e)
	}
	api.set(func(s *Sample) { s.Clusters[0].Metadata.UID = "replacement" })
	restarted.Refresh(t.Context(), "pgbackrest")
	if code, _ := lookup(t, restarted, "", time.Now()); code != 503 {
		t.Fatal("restart evidence applied to replacement")
	}
}

func TestCachedAgeAndSampleExpire(t *testing.T) {
	cluster := liveCluster(t)
	now := time.Now().UTC()
	cluster.Status.PGBackRest.ScheduledBackups[0].CompletionTime = now.Add(-28799 * time.Second).Format(time.RFC3339Nano)
	_, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{cluster}}}, store, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	if code, e := lookup(t, r, "db/db/repo2", time.Now()); code != 200 || e.Values["ageSeconds"].(float64) < 3600 {
		t.Fatal(code, e)
	}
	later := time.Now().Add(2 * time.Second)
	code, e := lookup(t, r, "db/db/repo1", later)
	if code != 503 || !strings.HasPrefix(e.Results["freshness"], "backup_stale(") {
		t.Fatal("cached success stayed green", code, e)
	}
	if code, e := lookup(t, r, "", time.Now().Add(181*time.Second)); code != 503 || !strings.HasPrefix(e.Results["collection"], "stale(") {
		t.Fatal("sample stayed green", code, e)
	}
	if code, _ := r.Lookup(Route+"/db/db/repo4", time.Now()); code != 404 {
		t.Fatal(code)
	}
}

func TestPersistenceFailureDoesNotPublishSuccess(t *testing.T) {
	backend, store := memoryStore(emptyLedger)
	backend.BeforeWrite = func(int) error { return &kube.Error{Code: kube.CodeHTTP, Status: 500, Path: "x"} }
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, store, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	code, e := lookup(t, r, "db/db/repo1", time.Now())
	if code != 503 || e.Results["state"] != "unpersisted" || e.Results["freshness"] != result.Pending || backend.Data() != emptyLedger {
		t.Fatal("failed write was published", code, e)
	}
}

func TestConcurrentRefreshAndReports(t *testing.T) {
	api := &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}
	_, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), api, store, false, discard))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				r.Lookup(Route, time.Now())
				r.Lookup(Route+"/db/db/repo1", time.Now())
				r.Index(time.Now())
			}
		})
	}
	wg.Go(func() {
		for range 10 {
			r.Refresh(t.Context(), "pgbackrest")
		}
	})
	wg.Wait()
}

func TestCachedCompletionFailsClosedDuringClockRollback(t *testing.T) {
	cluster := liveCluster(t)
	completed := time.Now().UTC().Add(-5 * time.Second)
	for i := range cluster.Status.PGBackRest.ScheduledBackups {
		cluster.Status.PGBackRest.ScheduledBackups[i].CompletionTime = completed.Format(time.RFC3339Nano)
	}
	_, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{cluster}}}, store, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	// A wall clock rollback makes the cached completion future while the sample is fresh.
	rollback := completed.Add(-time.Second)
	if code, e := lookup(t, r, "db/db/repo1", rollback); code != 503 || e.Results["evidence"] != "invalid_timestamp" {
		t.Errorf("future cached completion reported healthy: %d %+v", code, e)
	}
	if code, e := lookup(t, r, "", rollback); code != 503 || e.Results["evidence"] != "2 failing: db/db/repo1, db/db/repo2" {
		t.Errorf("aggregate accepted future cached completion: %d %+v", code, e)
	}
	if code, e := lookup(t, r, "", time.Now()); code != 200 {
		t.Fatalf("clock catch-up did not restore health: %d %+v", code, e)
	}
}

// failingStore wraps a Memory backend whose writes can be switched off.
type failingStore struct {
	backend *state.Memory
	fail    bool
}

func newFailingStore() *failingStore {
	f := &failingStore{backend: state.NewMemory(emptyLedger, true)}
	f.backend.BeforeWrite = func(int) error {
		if f.fail {
			return &kube.Error{Code: kube.CodeHTTP, Status: 507, Path: "x"}
		}
		return nil
	}
	return f
}

func (f *failingStore) entries(t *testing.T) state.Ledger {
	t.Helper()
	ledger, err := state.Decode([]byte(f.backend.Data()))
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func requireRepositoryStatus(t *testing.T, r *runner.Runner, want int) {
	t.Helper()
	if code, e := lookup(t, r, "db/db/repo1", time.Now()); code != want {
		t.Fatalf("status %d, want %d: %+v", code, want, e)
	}
}

func TestInvalidationsSurviveFailedCheckpoints(t *testing.T) {
	for _, kind := range []string{"destination", "repository", "cluster"} {
		t.Run(kind, func(t *testing.T) {
			api := &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}
			disk := newFailingStore()
			check := New(testConfig(), api, state.New(disk.backend), false, discard)
			r := harness(check)
			r.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, r, 200)
			original := api.sample.Clusters[0]
			persisted := maps.Clone(disk.entries(t))
			disk.fail = true
			switch kind {
			case "destination":
				api.sample.Clusters[0].Spec.Backups.PGBackRest.Global = map[string]string{"repo1-path": "/changed"}
			case "repository":
				api.sample.Clusters[0].Spec.Backups.PGBackRest.Repos = api.sample.Clusters[0].Spec.Backups.PGBackRest.Repos[1:]
			case "cluster":
				api.sample.Clusters = nil
			}
			for range 3 {
				r.Refresh(t.Context(), "pgbackrest")
				requireRepositoryStatus(t, r, 503)
				if !maps.Equal(disk.entries(t), persisted) {
					t.Fatal("failed write changed durable evidence")
				}
			}
			// Collection failure must not discard the pending invalidation either.
			api.err = errors.New("temporary API outage")
			r.Refresh(t.Context(), "pgbackrest")
			api.err = nil
			// The original destination and retained old evidence reappear before recovery.
			api.sample.Clusters = []kube.Cluster{original}
			oldJob := testJob(t)
			oldJob.Metadata.CreationTimestamp = time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano)
			oldJob.Status.CompletionTime = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
			api.sample.Jobs = []kube.Job{oldJob}
			r.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, r, 503)
			disk.fail = false
			r.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, r, 503)
			entry := disk.entries(t)["db/db/repo1"]
			if !entry.LastSuccess.IsZero() || entry.NotBefore.IsZero() {
				t.Fatal("old completion survived invalidation", entry)
			}
			// Recovery writes a valid barrier ledger, which also survives restart.
			restarted := harness(New(testConfig(), api, state.New(state.NewMemory(disk.backend.Data(), true)), false, discard))
			restarted.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, restarted, 503)
			// A backup created after the recovered barrier can make this destination healthy.
			job := testJob(t)
			birth := entry.NotBefore.Add(time.Nanosecond)
			job.Metadata.CreationTimestamp = birth.Format(time.RFC3339Nano)
			job.Status.CompletionTime = birth.Add(time.Nanosecond).Format(time.RFC3339Nano)
			api.sample.Jobs = []kube.Job{job}
			disk.fail = true
			r.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, r, 503)
			if !check.ledger["db/db/repo1"].LastSuccess.IsZero() {
				t.Fatal("retained unpersisted success")
			}
			api.sample.Jobs = nil
			disk.fail = false
			r.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, r, 503)
			api.sample.Jobs = []kube.Job{job}
			r.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, r, 200)
			api.sample.Jobs = nil
			r.Refresh(t.Context(), "pgbackrest")
			requireRepositoryStatus(t, r, 200)
		})
	}
}

func TestFailedCheckpointPreservesOnlyCommittedSuccess(t *testing.T) {
	api := &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}
	disk := newFailingStore()
	check := New(testConfig(), api, state.New(disk.backend), false, discard)
	r := harness(check)
	r.Refresh(t.Context(), "pgbackrest")
	old := disk.entries(t)["db/db/repo1"]
	disk.fail = true
	api.sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].CompletionTime = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	r.Refresh(t.Context(), "pgbackrest")
	requireRepositoryStatus(t, r, 503)
	if check.ledger["db/db/repo1"] != old {
		t.Fatal("uncommitted success replaced ledger evidence")
	}
	if _, e := lookup(t, r, "db/db/repo1", time.Now()); e.Values["lastSuccess"] != old.LastSuccess.Format(time.RFC3339Nano) {
		t.Fatal("uncommitted success was published", e.Values)
	}
	api.sample.Clusters[0].Status.PGBackRest.ScheduledBackups = nil
	disk.fail = false
	r.Refresh(t.Context(), "pgbackrest")
	requireRepositoryStatus(t, r, 200)
	if disk.entries(t)["db/db/repo1"] != old {
		t.Fatal("uncommitted success persisted on recovery")
	}
}

func TestExistingCheckpointSourcesRemainUsableAfterRestart(t *testing.T) {
	for _, source := range []string{"Completed Job", "CronJob lastSuccessfulTime"} {
		t.Run(source, func(t *testing.T) {
			now := time.Now().UTC()
			cluster := withStanzas(testCluster(t))
			cluster.Metadata.CreationTimestamp = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
			cluster.Status.PGBackRest.ScheduledBackups = nil
			const key = "db/db/repo1"
			entry := state.Entry{UID: cluster.Metadata.UID, Created: now.Add(-24 * time.Hour), RepositoryID: repositoryIdentity(cluster, "repo1"), NotBefore: now.Add(-2 * time.Hour), LastSuccess: now.Add(-time.Hour), Source: source}
			data, err := state.Encode(state.Ledger{key: entry})
			if err != nil {
				t.Fatal(err)
			}
			backend, store := memoryStore(string(data))
			r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{cluster}}}, store, false, discard))
			if code, _ := lookup(t, r, key, now); code != 503 {
				t.Fatal("ledger bypassed fresh API sample")
			}
			r.Refresh(t.Context(), "pgbackrest")
			if code, e := lookup(t, r, key, time.Now()); code != 200 {
				t.Fatal("existing completion source was invalidated", code, e)
			}
			recovered, err := state.Decode([]byte(backend.Data()))
			if err != nil {
				t.Fatal(err)
			}
			if recovered[key] != entry {
				t.Fatal("existing completion or safety boundary changed", recovered[key])
			}
		})
	}
}

// Conditional writes and read-back convergence against a fake API server.

func TestConflictAdoptsStoredLedgerAndRewrites(t *testing.T) {
	backend, store := memoryStore(emptyLedger)
	tombstone := state.Entry{UID: "other-uid", Created: time.Now().Add(-48 * time.Hour).UTC(), RepositoryID: "missing", NotBefore: time.Now().Add(-time.Hour).UTC()}
	backend.BeforeWrite = func(attempt int) error {
		if attempt == 0 {
			// Another writer (or Argo re-applying a compared state) lands first.
			data, _ := state.Encode(state.Ledger{"old/old/repo1": tombstone})
			backend.Set(string(data))
		}
		return nil
	}
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, store, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	stored, err := state.Decode([]byte(backend.Data()))
	if err != nil || stored["old/old/repo1"].UID != tombstone.UID || stored["db/db/repo1"].LastSuccess.IsZero() || backend.Writes() != 1 {
		t.Fatal("conflict did not converge on the adopted ledger", stored, err, backend.Writes())
	}
	if code, e := lookup(t, r, "db/db/repo1", time.Now()); code != 200 || e.Results["state"] != result.OK {
		t.Fatal(code, e)
	}
}

func TestTimeoutAfterCommitConvergesWithoutRewrite(t *testing.T) {
	backend, store := memoryStore(emptyLedger)
	backend.AfterWrite = func(attempt int) error {
		if attempt == 0 {
			return &kube.Error{Code: kube.CodeTimeout, Path: "x"}
		}
		return nil
	}
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, store, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	if backend.Writes() != 1 {
		t.Fatal("read-back did not recognise the committed write", backend.Writes())
	}
	if code, e := lookup(t, r, "db/db/repo1", time.Now()); code != 200 {
		t.Fatal(code, e)
	}
}

// Completions that land while the sample is being read are evaluated after
// the read, so they are never "in the future".
func TestEvaluatesAfterCollecting(t *testing.T) {
	cluster := liveCluster(t)
	api := &slowAPI{cluster: cluster}
	_, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), api, store, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	if code, e := lookup(t, r, "db/db/repo1", time.Now()); code != 200 {
		t.Fatal(code, e.Results)
	}
}

type slowAPI struct{ cluster kube.Cluster }

func (s *slowAPI) Collect(context.Context) (Sample, error) {
	time.Sleep(20 * time.Millisecond)
	cluster := s.cluster
	cluster.Status.PGBackRest.ScheduledBackups = append([]kube.Backup(nil), cluster.Status.PGBackRest.ScheduledBackups...)
	cluster.Status.PGBackRest.ScheduledBackups[0].CompletionTime = time.Now().UTC().Format(time.RFC3339Nano)
	return Sample{Clusters: []kube.Cluster{cluster}}, nil
}

func TestCorruptUnknownMissingAndOversizedStateFailClosed(t *testing.T) {
	for name, data := range map[string]string{
		"corrupt":         `{"version":2,"entries":`,
		"unknown version": `{"version":3,"entries":{}}`,
		"oversized":       `{"version":2,"entries":{}}` + strings.Repeat(" ", state.MaxBytes),
		"missing key":     "",
	} {
		t.Run(name, func(t *testing.T) {
			backend := state.NewMemory(data, name != "missing key")
			api := &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}
			r := harness(New(testConfig(), api, state.New(backend), false, discard))
			for range 2 {
				r.Refresh(t.Context(), "pgbackrest")
				code, e := lookup(t, r, "db/db/repo1", time.Now())
				if code != 503 || e.Results["state"] != "invalid" || e.Results["collection"] != result.OK {
					t.Fatal(code, e)
				}
			}
			if backend.Data() != data || backend.Writes() != 0 {
				t.Fatal("invalid state was overwritten")
			}
			// An operator repair is picked up on the next sample.
			backend.Set(emptyLedger)
			r.Refresh(t.Context(), "pgbackrest")
			if code, e := lookup(t, r, "db/db/repo1", time.Now()); code != 200 {
				t.Fatal("repaired state not adopted", code, e)
			}
		})
	}
	// A document corrupted underneath during convergence is never overwritten.
	backend := state.NewMemory(emptyLedger, true)
	backend.BeforeWrite = func(int) error {
		backend.Set(`{"version":9}`)
		return nil
	}
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, state.New(backend), false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	if _, e := lookup(t, r, "db/db/repo1", time.Now()); e.Results["state"] != "invalid" || backend.Data() != `{"version":9}` {
		t.Fatal("corruption during convergence was overwritten", e)
	}
	missing := state.NewMemory(emptyLedger, true).Missing()
	r = harness(New(testConfig(), &fakeAPI{}, state.New(missing), false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	if _, e := lookup(t, r, "db/db/repo1", time.Now()); e.Results["state"] != "missing" {
		t.Fatal(e)
	}
}

func TestLedgerOverSizeLimitIsNotWritten(t *testing.T) {
	_, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, oversized{store}, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	if code, e := lookup(t, r, "db/db/repo1", time.Now()); code != 503 || e.Results["state"] != "too_large" {
		t.Fatal(code, e)
	}
}

type oversized struct{ *state.Store }

func (oversized) Save(context.Context, state.Ledger) error { return state.ErrTooLarge }

func TestReadOnlyNeverWrites(t *testing.T) {
	backend, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), &fakeAPI{sample: Sample{Clusters: []kube.Cluster{liveCluster(t)}}}, store, true, discard))
	r.Refresh(t.Context(), "pgbackrest")
	if code, e := lookup(t, r, "db/db/repo1", time.Now()); code != 200 || backend.Writes() != 0 || e.Values["readOnly"] != true {
		t.Fatal(code, e, backend.Writes())
	}
}

func TestDimensionsAndInventory(t *testing.T) {
	cluster := liveCluster(t)
	cluster.Status.PGBackRest.Repos = nil
	extra := cluster.Spec.Backups.PGBackRest.Repos[0]
	extra.Name = "repo3"
	cluster.Spec.Backups.PGBackRest.Repos = append(cluster.Spec.Backups.PGBackRest.Repos, extra)
	other := liveCluster(t)
	other.Metadata.Namespace, other.Metadata.Name, other.Metadata.UID = "other", "other", "other-uid"
	other.Spec.Backups.PGBackRest.Repos = nil
	api := &fakeAPI{sample: Sample{Clusters: []kube.Cluster{cluster, other}}}
	_, store := memoryStore(emptyLedger)
	r := harness(New(testConfig(), api, store, false, discard))
	r.Refresh(t.Context(), "pgbackrest")
	code, e := lookup(t, r, "", time.Now())
	if code != 503 || e.Results["inventory"] != "unexpected(2): db/db/repo3, other/other" || e.Results["stanza"] != "2 failing: db/db/repo1, db/db/repo2" ||
		e.Results["freshness"] != result.OK {
		t.Fatal(e.Results)
	}
	api.set(func(s *Sample) {
		s.Clusters = s.Clusters[:1]
		s.Clusters[0].Spec.Backups.PGBackRest.Repos = s.Clusters[0].Spec.Backups.PGBackRest.Repos[:1]
		s.Clusters[0] = withStanzas(s.Clusters[0])
	})
	r.Refresh(t.Context(), "pgbackrest")
	_, e = lookup(t, r, "db/db/repo2", time.Now())
	if e.Results["destination"] != "repo_missing" || e.Results["freshness"] != "unknown" {
		t.Fatal(e.Results)
	}
	if _, e := lookup(t, r, "", time.Now()); e.Results["inventory"] != "missing(1): db/db/repo2" {
		t.Fatal(e.Results)
	}
	if _, e := lookup(t, r, "db/db/repo1", time.Now()); e.Results["stanza"] != result.OK {
		t.Fatal(e.Results)
	}
	api.set(func(s *Sample) {
		s.Clusters[0].Spec.Backups.PGBackRest.Repos = append(s.Clusters[0].Spec.Backups.PGBackRest.Repos, cluster.Spec.Backups.PGBackRest.Repos[1])
	})
	r.Refresh(t.Context(), "pgbackrest")
	if _, e := lookup(t, r, "db/db/repo2", time.Now()); !strings.HasPrefix(e.Results["destination"], "changed(since ") || e.Results["freshness"] != "no_success" {
		t.Fatal("re-added repository did not wait for a backup after its barrier", e.Results)
	}
}

// The import keeps every entry of a ledger exported by backrestwatch itself:
// re-evaluating the same sample adds no barrier and drops no success.
// testdata/backrestwatch-ledger.json was written by backrestwatch's own
// Evaluate and evidence store at revision 46085b6 (see
// testdata/generate_ledger_test.go.txt) from the sample beside it.
func TestRealBackrestwatchLedgerSurvivesImport(t *testing.T) {
	ledger, err := state.ValidateFile("testdata/backrestwatch-ledger.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/backrestwatch-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		EvaluatedAt time.Time        `json:"evaluatedAt"`
		Clusters    []kube.Cluster   `json:"clusters"`
		Jobs        []kube.Job       `json:"jobs"`
		CronJobs    []kube.CronJob   `json:"cronJobs"`
		Inventory   []config.Cluster `json:"inventory"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	cfg := config.PGBackRest{MaxAgeSeconds: 28800, Clusters: fixture.Inventory}
	sample := Sample{Clusters: fixture.Clusters, Jobs: fixture.Jobs, CronJobs: fixture.CronJobs}
	for _, later := range []time.Duration{time.Minute, 10 * time.Minute, 6 * time.Hour} {
		_, next, err := Evaluate(cfg, sample, ledger, fixture.EvaluatedAt.Add(later))
		if err != nil {
			t.Fatal(err)
		}
		if len(next) != len(ledger) {
			t.Fatal("entries lost or added on import", len(next), len(ledger))
		}
		for key, entry := range ledger {
			if next[key] != entry {
				t.Fatalf("%s changed on import:\n got %+v\nwant %+v", key, next[key], entry)
			}
		}
	}
	// Through the check itself: loading the imported ledger needs no write.
	data, err := os.ReadFile("testdata/backrestwatch-ledger.json")
	if err != nil {
		t.Fatal(err)
	}
	backend, store := memoryStore(string(data))
	check := New(cfg, &fakeAPI{sample: sample}, store, false, discard)
	if _, err := check.Collect(t.Context(), func() time.Time { return fixture.EvaluatedAt.Add(10 * time.Minute) }); err != nil {
		t.Fatal(err)
	}
	if backend.Writes() != 0 || backend.Data() != string(data) {
		t.Fatal("import rewrote the ledger")
	}
}
