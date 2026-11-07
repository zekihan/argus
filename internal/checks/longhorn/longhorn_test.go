package longhorn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
)

// Fixtures from platform/longhorn/tests/test_backup_monitor.py.
var now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

const stamp = "2026-11-05T12:00:00+00:00"

var cfg = config.Longhorn{Namespace: "longhorn", ExpectedPVCs: []string{"bazarr/bazarr-state"}, BackupGroup: "backup",
	MaxBackupAgeSeconds: 108000, SystemBackupMaxAgeSeconds: 691200, SystemBackupGraceSeconds: 3600}

const volumeJSON = `{"metadata":{"name":"pvc-123","creationTimestamp":"2026-11-05T10:30:00Z","labels":{"recurring-job-group.longhorn.io/backup":"enabled"}},
"status":{"kubernetesStatus":{"namespace":"bazarr","pvcName":"bazarr-state","lastPVCRefAt":""},"lastBackupAt":"` + stamp + `","robustness":"healthy"}}`

func decode[T any](t *testing.T, value string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type fakeAPI struct {
	sample Sample
	err    error
}

func (f *fakeAPI) Collect(context.Context) (Sample, error) { return f.sample, f.err }

func healthy(t *testing.T) Sample {
	return Sample{Volumes: []Volume{decode[Volume](t, volumeJSON)},
		BackupTargets: []BackupTarget{decode[BackupTarget](t, `{"metadata":{"name":"rustfs"},"status":{"available":true}}`)},
		SystemBackups: []SystemBackup{decode[SystemBackup](t, `{"metadata":{"name":"weekly"},"status":{"createdAt":"`+stamp+`","state":"Ready"}}`)}}
}

// verdict is the cutover parity condition: Python's single /backups summary
// maps to both Longhorn routes' parity dimensions.
func verdict(t *testing.T, sample Sample) (bool, *checktest.Harness) {
	t.Helper()
	h := checktest.New(New(cfg, &fakeAPI{sample: sample}), now).Refresh(t)
	_, volumes := h.Get(t, VolumesRoute)
	_, system := h.Get(t, SystemRoute)
	return checktest.Parity(volumes, "volume", "backup", "robustness", "inventory") &&
		checktest.Parity(system, "backup_targets", "system_backup", "inventory"), h
}

func TestHealthy(t *testing.T) {
	ok, h := verdict(t, healthy(t))
	if !ok {
		t.Fatal("healthy fixture failed")
	}
	if code, e := h.Get(t, VolumesRoute+"/bazarr/bazarr-state"); code != 200 || e.Values["lastBackupAgeSeconds"] != 0.0 {
		t.Fatal(code, e)
	}
	if code, _ := h.Get(t, SystemRoute); code != 200 {
		t.Fatal(code)
	}
}

func TestBadBackupDates(t *testing.T) {
	for _, date := range []string{"null", `""`, `"bad"`, `"2026-11-05T12:00:00"`, `"2026-11-03T12:00:00Z"`, `"2026-11-06T12:00:00Z"`} {
		s := healthy(t)
		s.Volumes[0] = decode[Volume](t, strings.Replace(volumeJSON, `"`+stamp+`"`, date, 1))
		if ok, _ := verdict(t, s); ok {
			t.Fatal("bad backup date accepted", date)
		}
	}
}

func TestBackupProblemsExplainCause(t *testing.T) {
	for date, want := range map[string]string{
		"":                     "no_backup(created 1h30m)",
		"bad":                  "backup_timestamp_invalid",
		"2026-11-03T12:00:00Z": "backup_stale(last 2d0h, max 1d6h)",
		"2026-11-05T13:00:00Z": "backup_in_future(1h0m)",
	} {
		s := healthy(t)
		s.Volumes[0] = decode[Volume](t, strings.Replace(volumeJSON, stamp, date, 1))
		h := checktest.New(New(cfg, &fakeAPI{sample: s}), now).Refresh(t)
		code, e := h.Get(t, VolumesRoute+"/bazarr/bazarr-state")
		if code != 503 || e.Results["backup"] != want || e.Summary != "backup: "+want {
			t.Fatal(date, e)
		}
		if _, aggregate := h.Get(t, VolumesRoute); aggregate.Results["backup"] != "1 failing: bazarr/bazarr-state" || aggregate.Items["bazarr/bazarr-state"] != "backup: "+want {
			t.Fatal(aggregate)
		}
	}
}

func TestMissingHistoricalAmbiguousAndUnexpectedVolumes(t *testing.T) {
	s := healthy(t)
	s.Volumes = nil
	if ok, h := verdict(t, s); ok {
		t.Fatal("missing inventory volume accepted")
	} else if _, e := h.Get(t, VolumesRoute+"/bazarr/bazarr-state"); e.Results["volume"] != "missing" {
		t.Fatal(e)
	}
	s = healthy(t)
	s.Volumes[0].Status.KubernetesStatus.LastPVCRefAt = stamp
	if ok, _ := verdict(t, s); ok {
		t.Fatal("historical PVC reference accepted")
	}
	s = healthy(t)
	s.Volumes = append(s.Volumes, s.Volumes[0])
	if ok, h := verdict(t, s); ok {
		t.Fatal("ambiguous volume accepted")
	} else if _, e := h.Get(t, VolumesRoute+"/bazarr/bazarr-state"); e.Results["volume"] != "ambiguous(volumes 2)" {
		t.Fatal(e)
	}
	s = healthy(t)
	extra := decode[Volume](t, volumeJSON)
	extra.Status.KubernetesStatus.PVCName = "untracked"
	s.Volumes = append(s.Volumes, extra)
	if ok, h := verdict(t, s); ok {
		t.Fatal("unexpected backup volume accepted")
	} else if _, e := h.Get(t, SystemRoute); e.Results["inventory"] != "unexpected(1): pvc-123" {
		t.Fatal(e.Results)
	}
}

func TestUnavailableTarget(t *testing.T) {
	for _, available := range []string{"false", "null", `"true"`} {
		s := healthy(t)
		s.BackupTargets[0] = decode[BackupTarget](t, `{"metadata":{"name":"rustfs"},"status":{"available":`+available+`}}`)
		if ok, h := verdict(t, s); ok {
			t.Fatal(available)
		} else if _, e := h.Get(t, SystemRoute); e.Results["backup_targets"] != "unavailable(rustfs)" {
			t.Fatal(e.Results)
		}
	}
	s := healthy(t)
	s.BackupTargets = nil
	if ok, _ := verdict(t, s); ok {
		t.Fatal("missing targets accepted")
	}
}

func TestOldFailedOrMissingSystemBackup(t *testing.T) {
	for _, tc := range []struct{ state, date string }{{"Ready", "2026-10-27T00:00:00Z"}, {"Error", stamp}, {"Uploading", stamp}, {"Ready", ""}} {
		s := healthy(t)
		s.SystemBackups[0] = decode[SystemBackup](t, `{"metadata":{"name":"weekly"},"status":{"createdAt":"`+tc.date+`","state":"`+tc.state+`"}}`)
		if ok, _ := verdict(t, s); ok {
			t.Fatal(tc)
		}
	}
	s := healthy(t)
	s.SystemBackups = nil
	if ok, _ := verdict(t, s); ok {
		t.Fatal("missing system backup accepted")
	}
}

func TestNewestSystemBackupCannotHideBehindReadyOlderOne(t *testing.T) {
	s := healthy(t)
	s.SystemBackups[0].Status.State = "Error"
	s.SystemBackups = append(s.SystemBackups, decode[SystemBackup](t, `{"metadata":{"name":"older"},"status":{"createdAt":"2026-11-04T12:00:00Z","state":"Ready"}}`))
	if ok, h := verdict(t, s); ok {
		t.Fatal("older ready backup hid the failed newest one")
	} else if _, e := h.Get(t, SystemRoute); e.Results["system_backup"] != "not_ready(weekly)" {
		t.Fatal(e.Results)
	}
}

func TestDegradedAndFaultedVolume(t *testing.T) {
	for _, robustness := range []string{"degraded", "faulted"} {
		for _, state := range []string{"attached", "detached"} {
			s := healthy(t)
			s.Volumes[0].Status.Robustness, s.Volumes[0].Status.State = robustness, state
			if ok, _ := verdict(t, s); ok {
				t.Fatal(robustness, state)
			}
		}
	}
	// Only unknown while detached is n/a (mailvault-state between runs).
	s := healthy(t)
	s.Volumes[0].Status.Robustness, s.Volumes[0].Status.State = "unknown", "detached"
	if ok, _ := verdict(t, s); !ok {
		t.Fatal("detached unknown volume failed")
	}
	s.Volumes[0].Status.State = "attached"
	if ok, _ := verdict(t, s); ok {
		t.Fatal("attached unknown volume passed")
	}
}

func TestDetailDimensions(t *testing.T) {
	s := healthy(t)
	v := &s.Volumes[0]
	v.Status.State, v.Spec.NumberOfReplicas, v.Status.CurrentNodeID = "attached", 2, "node-a"
	v.Status.Conditions = []kube.Condition{{Type: "Scheduled", Status: "False"}, {Type: "TooManySnapshots", Status: "True"}}
	s.Replicas = []Replica{
		decode[Replica](t, `{"spec":{"volumeName":"pvc-123","healthyAt":"x"},"status":{"currentState":"running"}}`),
		decode[Replica](t, `{"spec":{"volumeName":"pvc-123","healthyAt":"x","failedAt":"y"},"status":{"currentState":"stopped"}}`),
		decode[Replica](t, `{"spec":{"volumeName":"other","healthyAt":"x"},"status":{"currentState":"running"}}`),
	}
	ok, h := verdict(t, s)
	_, e := h.Get(t, VolumesRoute+"/bazarr/bazarr-state")
	if !ok || e.Results["replicas"] != "degraded(healthy 1, desired 2)" || e.Results["scheduled"] != "not_scheduled" || e.Results["snapshots"] != "too_many_snapshots" ||
		e.Values["node"] != "node-a" || e.Values["replicasHealthy"] != 1 {
		t.Fatal("detail dimensions wrong or affected parity", ok, e.Results, e.Values)
	}
	// Detached: replica health does not apply.
	v.Status.State = "detached"
	_, h = verdict(t, s)
	if _, e := h.Get(t, VolumesRoute+"/bazarr/bazarr-state"); e.Results["replicas"] != "n/a" {
		t.Fatal(e.Results)
	}
	// A volume stuck between steady states fails only after it persists.
	v.Status.State = "attaching"
	check := New(cfg, &fakeAPI{sample: s})
	h = checktest.New(check, now).Refresh(t)
	if _, e := h.Get(t, VolumesRoute+"/bazarr/bazarr-state"); e.Results["state"] != "ok" {
		t.Fatal(e.Results)
	}
	collections := check.Evaluate(s, now.Add(16*time.Minute))
	if got := collections[0].Items[0].Results["state"]; got != "stuck(attaching, for 16m)" {
		t.Fatal(got)
	}
}

func TestFailedRefreshNeverExposesCredentials(t *testing.T) {
	for _, err := range []error{errors.New("secret-token"), &kube.Error{Code: kube.CodeForbidden, Status: 403, Path: "/apis/longhorn.io"}} {
		h := checktest.New(New(cfg, &fakeAPI{err: err}), now).Refresh(t)
		code, e := h.Get(t, SystemRoute)
		if code != 503 || e.Results["collection"] == "ok" {
			t.Fatal(e)
		}
		h.NoLeak(t, "secret-token")
	}
}

func TestRefreshReadsOnlyNamespacedResources(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`))
	}))
	defer server.Close()
	client, err := kube.New(server.URL, "", "", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (API{Client: client, Namespace: "longhorn"}).Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"volumes", "replicas", "backuptargets", "systembackups"}
	for i, resource := range want {
		if paths[i] != "/apis/longhorn.io/v1beta2/namespaces/longhorn/"+resource {
			t.Fatal(paths)
		}
	}
}
