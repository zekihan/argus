package externalsecrets

import (
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

func TestAPICollectStopsOnTheFailingResource(t *testing.T) {
	const secret = "secret-api-body"
	const base = "/apis/external-secrets.io/v1/"
	for _, failAt := range []string{base + "externalsecrets", base + "clustersecretstores", base + "secretstores", base + "clusterexternalsecrets"} {
		t.Run(failAt, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == failAt {
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
			_, err = (API{Client: client}).Collect(t.Context())
			if err == nil || kube.CodeOf(err) != kube.CodeForbidden || strings.Contains(err.Error(), secret) {
				t.Fatal(err)
			}
		})
	}
}

func TestReadyNamespacedStoreAndStaleSample(t *testing.T) {
	s := healthy(t)
	s.ExternalSecrets[0].Spec.SecretStoreRef = &StoreRef{Name: "example", Kind: "SecretStore"}
	c := cfg()
	c.ExpectedSecretStores = []config.NamespacedName{{Namespace: "apps", Name: "example"}}
	h := checktest.New(New(c, &fakeAPI{sample: s}), now).Refresh(t)
	if _, e := h.Get(t, ExternalSecretsRoute+"/apps/example"); e.Results["store"] != result.OK {
		t.Fatal(e.Results)
	}
	if _, e := h.Get(t, SecretStoresRoute); e.Results["inventory"] != result.OK {
		t.Fatal(e.Results)
	}
	code, body := h.Runner.Lookup(ExternalSecretsRoute, time.Now().Add(400*24*time.Hour))
	envelope, ok := body.(result.Envelope)
	if !ok || code != 503 || !strings.HasPrefix(envelope.Results["collection"], "stale(") {
		t.Fatalf("stale sample = %d %+v", code, body)
	}
}
