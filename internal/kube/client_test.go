package kube

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tlsClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := New(server.URL, filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt"), "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.HTTP.CloseIdleConnections)
	return client, server
}

type named struct {
	Metadata Metadata `json:"metadata"`
}

// collect lists three resources the way the pgBackRest check does.
func collect(ctx context.Context, c *Client) ([]named, error) {
	var all []named
	for _, path := range []string{"/apis/" + ClusterAPI + "/postgresclusters", "/apis/batch/v1/namespaces/db/jobs", "/apis/batch/v1/namespaces/db/cronjobs"} {
		items, err := List[named](ctx, c, path, Prefix+"pgbackrest-backup")
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	return all, nil
}

func TestPaginationAndTokenRotation(t *testing.T) {
	var client *Client
	var calls atomic.Int32
	client, _ = tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		want := "Bearer rotated"
		if call == 1 {
			want = "Bearer first"
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("token was not reread at request %d", call)
		}
		if r.URL.Query().Get("limit") != "200" || r.URL.Query().Get("labelSelector") != Prefix+"pgbackrest-backup" {
			t.Error("missing page bound or selector")
		}
		metadata := map[string]string{"resourceVersion": "1"}
		if r.URL.Query().Get("continue") == "" {
			metadata["continue"] = "next"
		}
		if call == 1 {
			if err := os.WriteFile(client.TokenFile+".new", []byte("rotated"), 0600); err != nil {
				t.Error(err)
			}
			if err := os.Rename(client.TokenFile+".new", client.TokenFile); err != nil {
				t.Error(err)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": metadata, "items": []any{map[string]any{"metadata": map[string]string{"name": "page-object"}}}})
	})
	items, err := collect(t.Context(), client)
	if err != nil || len(items) != 6 || calls.Load() != 6 {
		t.Fatal(items, calls.Load(), err)
	}
	if client.Health(time.Time{}).LastSuccess.IsZero() {
		t.Fatal("success not recorded")
	}
}

func TestPaginationFailureDiscardsPartialSample(t *testing.T) {
	for name, reply := range map[string]string{
		"loop":             `{"metadata":{"continue":"same","resourceVersion":"1"},"items":[]}`,
		"missing metadata": `{"items":[]}`,
		"null items":       `{"metadata":{},"items":null}`,
		"truncated":        `{"metadata":{},"items":[`,
		"wrong shape":      `{"metadata":{},"items":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(reply)) })
			if items, err := collect(t.Context(), client); err == nil || len(items) != 0 || CodeOf(err) != CodeInvalid {
				t.Fatal("partial sample escaped", items, err)
			}
		})
	}
	client, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/postgresclusters") {
			_, _ = w.Write([]byte(`{"metadata":{},"items":[{}]}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("sensitive-secret-body"))
	})
	items, err := collect(t.Context(), client)
	if err == nil || len(items) != 0 || strings.Contains(err.Error(), "sensitive") || CodeOf(err) != CodeForbidden {
		t.Fatal(items, err)
	}
	if h := client.Health(time.Time{}); h.ForbiddenPaths != 1 || h.OKPaths != 1 || !h.LastFailure.IsZero() {
		t.Fatal("refusals not recorded as missing permissions", h)
	}
}

func TestHealthTracksTheLatestOutcome(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{}`))
	})
	var out map[string]any
	_ = client.Get(t.Context(), "/version", &out)
	ok := client.Health(time.Time{})
	if ok.LastSuccess.IsZero() || !ok.LastFailure.IsZero() {
		t.Fatal(ok)
	}
	status.Store(401)
	_ = client.Get(t.Context(), "/version", &out)
	failed := client.Health(time.Time{})
	if failed.FailureCode != CodeUnauthorized || !failed.LastFailure.After(failed.LastSuccess) {
		t.Fatal("failure after success not recorded", failed)
	}
	status.Store(200)
	_ = client.Get(t.Context(), "/version", &out)
	if recovered := client.Health(time.Time{}); !recovered.LastSuccess.After(recovered.LastFailure) {
		t.Fatal("recovery not recorded", recovered)
	}
}

// A success from before a revocation must not hide later refusals: a check
// stops at its first refused read, so its other paths are never re-read.
func TestRevokedPermissionsOutliveEarlierSuccesses(t *testing.T) {
	var revoked atomic.Bool
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if revoked.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`))
	})
	if _, err := collect(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	revoked.Store(true)
	before := time.Now()
	if _, err := collect(t.Context(), client); CodeOf(err) != CodeForbidden {
		t.Fatal(err)
	}
	if h := client.Health(time.Time{}); h.OKPaths != 2 || h.ForbiddenPaths != 1 {
		t.Fatal("expected two stale successes and one refusal over all time", h)
	}
	if h := client.Health(before); h.OKPaths != 0 || h.ForbiddenPaths != 1 {
		t.Fatal("successes before the revocation were counted", h)
	}
}

// One aggregated API or conversion webhook failing is that check's problem,
// not the API server's: it must not trip the apiserver gate.
func TestAggregatedAPIFailureIsNotAnAPIFailure(t *testing.T) {
	client, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/apis/metrics.k8s.io/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`))
	})
	if err := client.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := List[named](t.Context(), client, "/apis/metrics.k8s.io/v1beta1/nodes", ""); CodeOf(err) != CodeHTTP {
		t.Fatal(err)
	}
	if h := client.Health(time.Time{}); !h.LastFailure.IsZero() || h.LastSuccess.IsZero() {
		t.Fatal("aggregated API failure recorded as API-wide", h)
	}
	core, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	_, _ = List[named](t.Context(), core, "/api/v1/nodes", "")
	if h := core.Health(time.Time{}); h.FailureCode != CodeHTTP {
		t.Fatal("core API failure not recorded", h)
	}
}

func TestStatusCodesMapToFixedCodes(t *testing.T) {
	for status, code := range map[int]string{401: CodeUnauthorized, 403: CodeForbidden, 404: CodeNotFound, 409: CodeConflict, 500: CodeHTTP, 429: CodeHTTP} {
		client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("token=secret-body"))
		})
		var out map[string]any
		err := client.Get(t.Context(), "/version", &out)
		if CodeOf(err) != code || strings.Contains(err.Error(), "secret") {
			t.Fatal(status, err)
		}
	}
}

func TestRetryCancellationAndRedirect(t *testing.T) {
	var calls atomic.Int32
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{},"items":[]}`))
	})
	if _, err := collect(t.Context(), client); err != nil || calls.Load() != 5 {
		t.Fatal(calls.Load(), err)
	}
	blocked, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := collect(ctx, blocked); err == nil || time.Since(start) > time.Second || CodeOf(err) != CodeTimeout {
		t.Fatal("cancellation not bounded", err)
	}
	redirect, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/credentials", http.StatusFound)
	})
	if _, err := collect(t.Context(), redirect); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestPageBudgetAndChangedSnapshot(t *testing.T) {
	var page atomic.Int32
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := page.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"continue": n, "resourceVersion": "1"}, "items": []any{}})
	})
	// Continuation fields must be strings, even on an otherwise empty response.
	if _, err := collect(t.Context(), client); err == nil {
		t.Fatal("malformed metadata accepted")
	}
	client, _ = tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := page.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"continue": time.Unix(int64(n), 0).String(), "resourceVersion": "1"}, "items": []any{}})
	})
	page.Store(0)
	if _, err := collect(t.Context(), client); err == nil || page.Load() != 100 {
		t.Fatal("page budget not enforced", page.Load(), err)
	}
	page.Store(0)
	client, _ = tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		version := "1"
		if page.Add(1) > 1 {
			version = "2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"continue": "next", "resourceVersion": version}, "items": []any{}})
	})
	if _, err := collect(t.Context(), client); err == nil {
		t.Fatal("changed snapshot accepted")
	}
}

func TestInvalidClientConfiguration(t *testing.T) {
	for _, origin := range []string{"http://localhost", "http://example.com:8001", "http://10.0.0.1:8001", "https://user:password@localhost",
		"https://localhost/path", "https://localhost?token=secret", "http://127.0.0.1:8001/path"} {
		if _, err := New(origin, "token", "ca", "test", time.Second); err == nil {
			t.Fatal(origin)
		}
	}
}

func TestLoopbackProxyOriginSendsNoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential sent to a plain-HTTP origin")
		}
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[{"metadata":{"name":"a"}}]}`))
	}))
	defer server.Close()
	for _, origin := range []string{server.URL, strings.Replace(server.URL, "127.0.0.1", "localhost", 1)} {
		if !Loopback(origin) {
			t.Fatal(origin)
		}
		client, err := New(origin, "/nonexistent/token", "/nonexistent/ca", "test", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		items, err := List[named](t.Context(), client, "/api/v1/nodes", "")
		if err != nil || len(items) != 1 {
			t.Fatal(items, err)
		}
	}
}

func TestCollectionRetriesBodyReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  int32
		wantCalls int32
		wantError bool
	}{
		{name: "recovers", failures: 1, wantCalls: 4},
		{name: "exhausts retry budget", failures: 3, wantCalls: 3, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) <= tc.failures {
					w.Header().Set("Content-Length", "1024")
					_, _ = w.Write([]byte(`{"metadata":{"sensitive-secret-body":`))
					return
				}
				_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`))
			})
			items, err := collect(t.Context(), client)
			if (err != nil) != tc.wantError || calls.Load() != tc.wantCalls {
				t.Fatalf("calls=%d, items=%+v, error=%v", calls.Load(), items, err)
			}
			if err != nil && (len(items) != 0 || strings.Contains(err.Error(), "sensitive")) {
				t.Fatal("failed response leaked into sample or diagnostics", items, err)
			}
		})
	}
}

// sync_monitor.py's test_total_size_and_page_count_budgets: three valid 12 MiB
// pages exceed the 32 MiB list budget, as do 101 small pages.
func TestTotalSizeAndPageCountBudgets(t *testing.T) {
	var calls atomic.Int32
	padding := strings.Repeat(" ", 12*1024*1024)
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1","continue":"` + strconv.Itoa(int(n)) + `"},"items":[]}` + padding))
	})
	if _, err := List[named](t.Context(), client, "/x", ""); CodeOf(err) != CodeBudget || calls.Load() != 3 {
		t.Fatal("cumulative size budget not enforced", calls.Load(), err)
	}
	calls.Store(0)
	client, _ = tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1","continue":"` + strconv.Itoa(int(n)) + `"},"items":[]}`))
	})
	if _, err := List[named](t.Context(), client, "/x", ""); CodeOf(err) != CodeBudget || calls.Load() != 100 {
		t.Fatal("page budget not enforced", calls.Load(), err)
	}
}

func TestOversizedPageIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	body := strings.Repeat(" ", 16*1024*1024+1)
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(body))
	})
	if _, err := collect(t.Context(), client); err == nil || calls.Load() != 1 || CodeOf(err) != CodeBudget {
		t.Fatal("oversized response was accepted or retried", calls.Load(), err)
	}
}

func TestCanceledBodyReadIsNotRetried(t *testing.T) {
	flushed := make(chan struct{})
	client, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"metadata":`))
		w.(http.Flusher).Flush()
		close(flushed)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { <-flushed; cancel() }()
	data, retry, err := client.request(ctx, http.MethodGet, "/apis/"+ClusterAPI+"/postgresclusters", url.Values{}, nil)
	if err == nil || retry || data != nil {
		t.Fatal("canceled response was accepted or marked retryable", data, retry, err)
	}
}

func TestTruncatedChunkedPage(t *testing.T) {
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"metadata":`))
		w.(http.Flusher).Flush()
	})
	items, err := collect(t.Context(), client)
	if err == nil || len(items) != 0 {
		t.Fatal("truncated chunked page accepted", items, err)
	}
}

func TestConfigMapConditionalUpdatePreservesMetadata(t *testing.T) {
	var puts atomic.Int32
	client, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/argus/configmaps/argus-state" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"argus-state","namespace":"argus","resourceVersion":"7","labels":{"keep":"me"}},"data":{"pgbackrest.json":"old"}}`))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var object struct {
				Metadata struct {
					ResourceVersion string            `json:"resourceVersion"`
					Labels          map[string]string `json:"labels"`
				} `json:"metadata"`
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(body, &object); err != nil || object.Metadata.Labels["keep"] != "me" || object.Data["pgbackrest.json"] != "new" {
				t.Errorf("update lost metadata or data: %s", body)
			}
			if puts.Add(1) == 1 && object.Metadata.ResourceVersion == "7" {
				_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"8"},"data":{"pgbackrest.json":"new"}}`))
				return
			}
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"secret conflict body"}`))
		}
	})
	current, err := client.GetConfigMap(t.Context(), "argus", "argus-state")
	if err != nil || current.ResourceVersion != "7" || current.Data["pgbackrest.json"] != "old" {
		t.Fatal(current, err)
	}
	updated, err := client.UpdateConfigMap(t.Context(), "argus", "argus-state", current, map[string]string{"pgbackrest.json": "new"})
	if err != nil || updated.ResourceVersion != "8" {
		t.Fatal(updated, err)
	}
	_, err = client.UpdateConfigMap(t.Context(), "argus", "argus-state", current, map[string]string{"pgbackrest.json": "new"})
	if !IsConflict(err) || strings.Contains(err.Error(), "secret") || puts.Load() != 2 {
		t.Fatal("conflict not reported or retried", err, puts.Load())
	}
	if _, err := client.UpdateConfigMap(t.Context(), "argus", "argus-state", ConfigMap{}, nil); err == nil {
		t.Fatal("update without a prior read accepted")
	}
}
