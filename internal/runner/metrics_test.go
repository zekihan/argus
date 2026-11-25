package runner

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zekihan/go-commons/metrics"
)

func scrapeRegistry(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	reg.Handler().
		ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics scrape = %d, want 200", recorder.Code)
	}
	body, err := io.ReadAll(recorder.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCollectionHealthGaugeFollowsSamples(t *testing.T) {
	check := &fakeCheck{collections: healthyCollections()}
	r, _ := newRunner(check, nil)
	reg := metrics.New()
	r.SetMetrics(reg)

	r.Refresh(t.Context(), "fake")
	body := scrapeRegistry(t, reg)
	for _, want := range []string{
		`argus_collection_healthy{collection="things"} 1`,
		`argus_collection_healthy{collection="things/system"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}

	check.mu.Lock()
	check.err = errors.New("collection failed")
	check.mu.Unlock()
	r.Refresh(t.Context(), "fake")
	body = scrapeRegistry(t, reg)
	for _, want := range []string{
		`argus_collection_healthy{collection="things"} 0`,
		`argus_collection_healthy{collection="things/system"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q after failure:\n%s", want, body)
		}
	}
}

func TestCollectionHealthGaugeDisabledWithoutRegistry(t *testing.T) {
	check := &fakeCheck{collections: healthyCollections()}
	r, _ := newRunner(check, nil)
	// No SetMetrics: samples must publish without metrics.
	r.Refresh(t.Context(), "fake")
}
