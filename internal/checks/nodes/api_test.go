package nodes

import (
	"errors"
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

// failClosedAPI serves the fixtures, forbidding one path.
func failClosedAPI(t *testing.T, failAt string) *httptest.Server {
	t.Helper()
	const secret = "secret-api-body"
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == failAt {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, secret)
				return
			}
			if r.URL.Path == "/version" {
				_, _ = io.WriteString(w, `{"gitVersion":"v1.31.0"}`)
				return
			}
			_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
		}),
	)
	t.Cleanup(server.Close)
	return server
}

func TestAPICollectFailsClosed(t *testing.T) {
	const secret = "secret-api-body"
	for _, tc := range []struct {
		name   string
		failAt string
	}{
		{"nodes", "/api/v1/nodes"},
		{"version", "/version"},
		{"metrics", "/apis/metrics.k8s.io/v1beta1/nodes"},
		{"longhorn", "/apis/longhorn.io/v1beta2/namespaces/longhorn/nodes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := failClosedAPI(t, tc.failAt)
			client, err := kube.New(server.URL, "", "", "test", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = (API{Client: client, Metrics: true, LonghornNamespace: "longhorn"}).Collect(
				t.Context(),
			)
			if err == nil || kube.CodeOf(err) != kube.CodeForbidden ||
				strings.Contains(err.Error(), secret) {
				t.Fatal(err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = io.WriteString(w, `{"gitVersion":"v1.31.0"}`)
			return
		}
		_, _ = io.WriteString(
			w,
			`{"metadata":{"resourceVersion":"1"},"items":[{"metadata":{"name":"worker"}}]}`,
		)
	}))
	defer server.Close()
	client, err := kube.New(server.URL, "", "", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := (API{Client: client}).Collect(t.Context())
	if err != nil || sample.ServerVersion != "v1.31.0" || len(sample.Nodes) != 1 ||
		sample.Metrics != nil ||
		sample.Longhorn != nil {
		t.Fatal(sample, err)
	}
}

func TestNodeDecisionsFailClosed(t *testing.T) {
	check := New(cfg(), nil)
	s := healthy(t)
	s.Metrics[0].Usage["cpu"] = "nope"
	if got := evaluate(t, check, s, now, "agent")["cpu"]; got != "metrics_invalid" {
		t.Fatal(got)
	}
	if got := usage("1", "2", false, 90, "cpu", map[string]any{}); got != "metrics_missing" {
		t.Fatal(got)
	}
	assertDiskDecisions(t, check)
}

// assertDiskDecisions verifies longhorn disk sub-decisions.
func assertDiskDecisions(t *testing.T, check *Check) {
	t.Helper()
	s := healthy(t)
	s.Longhorn[0].Status.Conditions = nil
	if got := evaluate(t, check, s, now, "agent")["longhorn_disk"]; got != "node_not_ready" {
		t.Fatal(got)
	}
	s = healthy(t)
	s.Longhorn[0].Spec.Disks = nil
	if got := evaluate(t, check, s, now, "agent")["longhorn_disk"]; got != "no_disks" {
		t.Fatal(got)
	}
	s = healthy(t)
	disk := s.Longhorn[0].Status.DiskStatus["permanent"]
	disk.Conditions[1].Status = "False"
	s.Longhorn[0].Status.DiskStatus["permanent"] = disk
	if got := evaluate(t, check, s, now, "agent")["longhorn_disk"]; got != "disk_not_schedulable(permanent)" {
		t.Fatal(got)
	}
	s = healthy(t)
	disk = s.Longhorn[0].Status.DiskStatus["permanent"]
	disk.StorageMaximum = 0
	s.Longhorn[0].Status.DiskStatus["permanent"] = disk
	if got := evaluate(t, check, s, now, "agent")["longhorn_disk"]; got != "disk_size_unknown(permanent)" {
		t.Fatal(got)
	}
}

func TestNodeCollectionFailures(t *testing.T) {
	const secret = "secret-api-body"
	h := checktest.New(New(cfg(), &fakeAPI{err: errors.New(secret)}), now).Refresh(t)
	if code, e := h.Get(t, Route); code != 503 || e.Results["collection"] != "collection_error" {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, secret)
	forbidden := checktest.New(New(cfg(), &fakeAPI{err: &kube.Error{Code: kube.CodeForbidden, Status: 403, Path: "/api/v1/nodes"}}), now).
		Refresh(t)
	if code, e := forbidden.Get(
		t,
		Route+"/agent",
	); code != 503 || e.Results["collection"] != "forbidden" ||
		e.Results["ready"] != result.Pending {
		t.Fatal(e.Results)
	}
	fresh := checktest.New(New(cfg(), &fakeAPI{sample: healthy(t)}), now).Refresh(t)
	code, body := fresh.Runner.Lookup(Route, time.Now().Add(400*24*time.Hour))
	envelope, ok := body.(result.Envelope)
	if !ok || code != 503 || !strings.HasPrefix(envelope.Results["collection"], "stale(") {
		t.Fatalf("stale sample = %d %+v", code, body)
	}
}
