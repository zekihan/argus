package pgbackrest

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
)

func TestCollectListsBackupObjectsOnlyInInventoryNamespaces(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+"?"+r.URL.Query().Get("labelSelector"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`))
	}))
	defer server.Close()
	client, err := kube.New(server.URL, "", "", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.PGBackRest{Clusters: []config.Cluster{{Namespace: "b", Name: "x"}, {Namespace: "a", Name: "y"}, {Namespace: "b", Name: "z"}}}
	if _, err := (API{Client: client, Namespaces: Namespaces(cfg)}).Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	want := []string{
		"/apis/batch/v1/namespaces/a/cronjobs?postgres-operator.crunchydata.com/pgbackrest-backup=scheduled",
		"/apis/batch/v1/namespaces/a/jobs?postgres-operator.crunchydata.com/pgbackrest-backup",
		"/apis/batch/v1/namespaces/b/cronjobs?postgres-operator.crunchydata.com/pgbackrest-backup=scheduled",
		"/apis/batch/v1/namespaces/b/jobs?postgres-operator.crunchydata.com/pgbackrest-backup",
		"/apis/postgres-operator.crunchydata.com/v1beta1/postgresclusters?",
	}
	if len(paths) != len(want) {
		t.Fatal(paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatal(paths)
		}
	}
}
