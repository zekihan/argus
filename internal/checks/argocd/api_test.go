package argocd

import (
	"context"
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
	for _, tc := range []struct {
		name string
		code int
		body string
		want string
	}{
		{"forbidden", http.StatusForbidden, secret, kube.CodeForbidden},
		{"api error", http.StatusInternalServerError, secret, kube.CodeHTTP},
		{"malformed", http.StatusOK, `{"items":`, kube.CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client, err := kube.New(server.URL, "", "", "test", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = (API{Client: client, Namespace: "argocd"}).Collect(t.Context())
			if err == nil || kube.CodeOf(err) != tc.want || strings.Contains(err.Error(), secret) {
				t.Fatalf("error = %v, want %s without the response body", err, tc.want)
			}
			h := checktest.New(New(cfg(), errAPI{err: err}), now).Refresh(t)
			code, envelope := h.Get(t, Route)
			if code != 503 || envelope.Results["collection"] != tc.want {
				t.Fatalf("HTTP %d, results %v", code, envelope.Results)
			}
			h.NoLeak(t, secret)
		})
	}
	h := checktest.New(New(cfg(), &fakeAPI{apps: []Application{app(t, "auto", true, "Synced", "Healthy", "", "Succeeded"), app(t, "manual", false, "Synced", "Healthy", "", "Succeeded")}}), now).Refresh(t)
	requireStale(t, h, Route)
}

func TestAPICollectReportsMissingExpectedApplication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[{"metadata":{"name":"auto","uid":"u"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}]}`)
	}))
	defer server.Close()
	client, err := kube.New(server.URL, "", "", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	collections, err := New(cfg(), API{Client: client, Namespace: "argocd"}).Collect(t.Context(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	item, ok := collections[0].Find("manual")
	if !ok || item.Results["sync"] != "missing" || item.Results["health"] != result.NA || collections[0].Results["inventory"] != "missing(1): manual" {
		t.Fatal(collections[0].Results, item)
	}
	if _, leaked := collections[0].Values["message"]; leaked {
		t.Fatal(collections[0])
	}
}

type errAPI struct{ err error }

func (e errAPI) Collect(context.Context) ([]Application, error) { return nil, e.err }

func requireStale(t *testing.T, h *checktest.Harness, path string) {
	t.Helper()
	code, body := h.Runner.Lookup(path, time.Now().Add(400*24*time.Hour))
	envelope, ok := body.(result.Envelope)
	if !ok || code != 503 || !strings.HasPrefix(envelope.Results["collection"], "stale(") {
		t.Fatalf("%s after the sample lifetime = %d %+v", path, code, body)
	}
}
