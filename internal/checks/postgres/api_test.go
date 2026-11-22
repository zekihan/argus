package postgres

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

func TestAPICollectFailsClosed(t *testing.T) {
	const secret = "secret-api-body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/postgres-operator.crunchydata.com/v1beta1/postgresclusters" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, secret)
	}))
	defer server.Close()
	client, err := kube.New(server.URL, "", "", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (API{Client: client}).Collect(t.Context())
	if err == nil || kube.CodeOf(err) != kube.CodeForbidden || strings.Contains(err.Error(), secret) {
		t.Fatal(err)
	}
	h := checktest.New(New(cfg, &fakeAPI{err: err}), now).Refresh(t)
	if code, e := h.Get(t, Route+"/db/db"); code != 503 || e.Results["collection"] != kube.CodeForbidden || e.Results["instances"] != result.Pending {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, secret)
	fresh, _ := run(t, decode(t, clusterJSON))
	code, body := fresh.Runner.Lookup(Route, time.Now().Add(400*24*time.Hour))
	envelope, ok := body.(result.Envelope)
	if !ok || code != 503 || !strings.HasPrefix(envelope.Results["collection"], "stale(") {
		t.Fatalf("stale sample = %d %+v", code, body)
	}
}

func TestShutdownAndEmptyInstanceSetFailClosed(t *testing.T) {
	shutdown := true
	cluster := decode(t, clusterJSON)
	cluster.Spec.Shutdown = &shutdown
	if _, r := run(t, cluster); r["instances"] != result.NA || r["proxy"] != result.NA || r["scheduled_backups"] != result.OK {
		t.Fatal(r)
	}
	cluster = decode(t, clusterJSON)
	cluster.Spec.Instances = nil
	cluster.Spec.Proxy = nil
	if _, r := run(t, cluster); r["instances"] != "no_instances" || r["proxy"] != result.NA {
		t.Fatal(r)
	}
	cluster = decode(t, clusterJSON)
	cluster.Spec.Instances[0].Replicas = nil
	cluster.Status.Instances[0].ReadyReplicas = 1
	cluster.Status.Instances[0].UpdatedReplicas = 1
	if _, r := run(t, cluster); r["instances"] != result.OK {
		t.Fatal(r)
	}
	h := checktest.New(New(cfg, &fakeAPI{err: errors.New("secret-api-body")}), now).Refresh(t)
	if code, e := h.Get(t, Route); code != 503 || e.Results["collection"] != "collection_error" {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, "secret-api-body")
}
