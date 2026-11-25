package longhorn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

func TestAPICollectStopsAfterTheFirstFailure(t *testing.T) {
	const secret = "secret-api-body"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/replicas") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, secret)
			return
		}
		_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
	}))
	defer server.Close()
	client, err := kube.New(server.URL, "", "", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (API{Client: client, Namespace: "longhorn"}).Collect(t.Context())
	if err == nil || kube.CodeOf(err) != kube.CodeForbidden ||
		strings.Contains(err.Error(), secret) ||
		len(paths) != 2 {
		t.Fatalf("error = %v, paths = %v", err, paths)
	}
}

func TestVolumeAndSystemBackupEdgeCasesFailClosed(t *testing.T) {
	s := healthy(t)
	s.Volumes[0].Metadata.CreationTimestamp = "not-a-time"
	s.Volumes[0].Status.LastBackupAt = nil
	s.Volumes[0].Spec.Size = "not-a-size"
	s.Volumes[0].Status.Conditions = []kube.Condition{
		{Type: "Scheduled", Status: "Unknown", Reason: "secret-condition"},
	}
	extra := s.Volumes[0]
	extra.Metadata.Name = ""
	extra.Metadata.Labels = map[string]string{"recurring-job-group.longhorn.io/backup": "enabled"}
	extra.Status.KubernetesStatus.Namespace = "other"
	extra.Status.KubernetesStatus.PVCName = "spare"
	s.Volumes = append(s.Volumes, extra)
	backup := s.SystemBackups[0]
	backup.Metadata.Name = "broken"
	backup.Status.CreatedAt = nil
	future := now.Add(time.Hour).Format(time.RFC3339)
	s.SystemBackups = append(s.SystemBackups, backup)
	s.SystemBackups[0].Status.CreatedAt = &future
	h := checktest.New(New(cfg, &fakeAPI{sample: s}), now).Refresh(t)
	_, volume := h.Get(t, VolumesRoute+"/bazarr/bazarr-state")
	if volume.Results["backup"] != "no_backup" || volume.Results["scheduled"] != "unknown" {
		t.Fatal(volume.Results)
	}
	if _, ok := volume.Values["sizeBytes"]; ok {
		t.Fatal(volume.Values)
	}
	_, system := h.Get(t, SystemRoute)
	if system.Results["inventory"] != "unexpected(1): unknown" ||
		system.Results["system_backup"] != "stale_or_invalid(weekly, age 1h0m, max 8d1h)" {
		t.Fatal(system.Results)
	}
	h.NoLeak(t, "secret-condition")
	onlyBroken := healthy(t)
	onlyBroken.SystemBackups = []SystemBackup{backup}
	if _, e := checktest.New(New(cfg, &fakeAPI{sample: onlyBroken}), now).
		Refresh(t).
		Get(t, SystemRoute); e.Results["system_backup"] != "invalid_created_at(broken)" {
		t.Fatal(e.Results)
	}
	code, body := h.Runner.Lookup(SystemRoute, time.Now().Add(400*24*time.Hour))
	envelope, ok := body.(result.Envelope)
	if !ok || code != 503 || !strings.HasPrefix(envelope.Results["collection"], "stale(") {
		t.Fatalf("stale sample = %d %+v", code, body)
	}
}
