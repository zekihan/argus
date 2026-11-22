package cronjobs

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

func TestAPICollectsEachNamespaceAndStopsOnForbidden(t *testing.T) {
	cfg := config.CronJobs{Items: []config.CronJob{
		{Namespace: "mail", Name: "b", MaxAgeSeconds: 60},
		{Namespace: "app", Name: "a", MaxAgeSeconds: 60},
		{Namespace: "mail", Name: "c", MaxAgeSeconds: 60},
	}}
	if got := strings.Join(Namespaces(cfg), ","); got != "app,mail" {
		t.Fatal(got)
	}
	const secret = "secret-api-body"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/jobs") && strings.Contains(r.URL.Path, "/app/") {
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
	_, err = (API{Client: client, Namespaces: Namespaces(cfg)}).Collect(t.Context())
	if err == nil || kube.CodeOf(err) != kube.CodeForbidden || strings.Contains(err.Error(), secret) {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/apis/batch/v1/namespaces/app/cronjobs,/apis/batch/v1/namespaces/app/jobs" {
		t.Fatal(paths)
	}
	h := checktest.New(New(inventory("a", "b", 10800), &fakeAPI{err: errors.New(secret)}), now).Refresh(t)
	if code, e := h.Get(t, Route); code != 503 || e.Results["collection"] != "collection_error" {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, secret)
	fresh := checktest.New(New(inventory("a", "b", 10800), &fakeAPI{}), now).Refresh(t)
	requireStale(t, fresh, Route)
}

func TestUnparseableJobTimeDoesNotBecomeASuccess(t *testing.T) {
	c := decode[CronJob](t, `{"metadata":{"namespace":"a","name":"b","uid":"cron-uid"},"status":{"lastSuccessfulTime":"2026-11-07T12:00:00Z","lastScheduleTime":"not-a-time"}}`)
	job := decode[Job](t, `{"metadata":{"name":"b-1","namespace":"a","creationTimestamp":"not-a-time","ownerReferences":[{"kind":"CronJob","uid":"cron-uid","controller":true}]},
		"status":{"conditions":[{"type":"Failed","status":"True","message":"secret-job-log"}]}}`)
	h := checktest.New(New(inventory("a", "b", 10800), &fakeAPI{sample: Sample{CronJobs: []CronJob{c}, Jobs: []Job{job}}}), now).Refresh(t)
	_, e := h.Get(t, Route+"/a/b")
	if e.Results["last_run"] != "failed" || e.Results["schedule"] != result.OK {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, "secret-job-log")
}

func requireStale(t *testing.T, h *checktest.Harness, path string) {
	t.Helper()
	code, body := h.Runner.Lookup(path, time.Now().Add(400*24*time.Hour))
	envelope, ok := body.(result.Envelope)
	if !ok || code != 503 || !strings.HasPrefix(envelope.Results["collection"], "stale(") {
		t.Fatalf("%s after the sample lifetime = %d %+v", path, code, body)
	}
}
