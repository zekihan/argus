package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zekihan/go-commons/metrics"
)

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	reg.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics scrape = %d, want 200", recorder.Code)
	}
	body, err := io.ReadAll(recorder.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func get(t *testing.T, handler http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestHandlerRecordsRequestsByRouteFamily(t *testing.T) {
	reg := metrics.New()
	handler := Handler(fakeRoutes{}, writeToken(t, testToken), reg)

	if recorder := get(t, handler, "/healthz", ""); recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", recorder.Code)
	}
	if recorder := get(t, handler, "/v1/things/ns/a", testToken); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("collection = %d, want 503", recorder.Code)
	}
	if recorder := get(t, handler, "/nope", testToken); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d, want 404", recorder.Code)
	}

	body := scrape(t, reg)
	for _, want := range []string{
		`http_requests_total{method="GET",route="/healthz",status="200"} 1`,
		`http_requests_total{method="GET",route="/v1/collection",status="503"} 1`,
		`http_requests_total{method="GET",route="unknown",status="404"} 1`,
		`http_request_duration_seconds_bucket{method="GET",route="/healthz",status="200"`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestHandlerWithoutRegistryServesUnchanged(t *testing.T) {
	handler := Handler(fakeRoutes{}, writeToken(t, testToken), nil)
	if recorder := get(t, handler, "/healthz", ""); recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", recorder.Code)
	}
}
