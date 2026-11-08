package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

type fakeCheck struct {
	mu          sync.Mutex
	collections []result.Collection
	err         error
}

func (f *fakeCheck) Name() string { return "fake" }
func (f *fakeCheck) Describe() []result.Collection {
	return []result.Collection{
		{Route: "things", Check: "thing", Dimensions: []string{"ready"}, Items: []result.Item{{Target: "ns/expected"}}},
		{Route: "things/system", Check: "things/system", Singleton: true},
	}
}
func (f *fakeCheck) Collect(context.Context, func() time.Time) ([]result.Collection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.collections, f.err
}

type fakeHealth struct {
	h     kube.Health
	since time.Time
}

func (f *fakeHealth) Health(since time.Time) kube.Health { f.since = since; return f.h }

// Permission outcomes are judged within the longest sample lifetime.
func TestPermissionWindowIsTheLongestSampleLifetime(t *testing.T) {
	api := &fakeHealth{}
	r := New(slog.New(slog.DiscardHandler), "test", api, 3*time.Minute)
	r.Add(&fakeCheck{}, Timing{Interval: time.Minute, Timeout: time.Second, MaxAge: 25 * time.Minute})
	now := time.Now()
	r.Index(now)
	if !api.since.Equal(now.Add(-25 * time.Minute)) {
		t.Fatal(api.since)
	}
}

func healthyCollections() []result.Collection {
	return []result.Collection{
		{Route: "things", Check: "thing", Dimensions: []string{"ready"}, Items: []result.Item{
			{Target: "ns/expected", Results: map[string]string{"ready": result.OK}, Values: map[string]any{}},
			{Target: "ns/other", Results: map[string]string{"ready": result.OK}, Values: map[string]any{}}}},
		{Route: "things/system", Check: "things/system", Singleton: true, Results: map[string]string{"targets": result.OK}},
	}
}

func newRunner(check Check, api HealthSource) (*Runner, *bytes.Buffer) {
	var logs bytes.Buffer
	r := New(slog.New(slog.NewJSONHandler(&logs, nil)), "test", api, 3*time.Minute)
	r.Add(check, Timing{Interval: time.Minute, Timeout: time.Second, MaxAge: 3 * time.Minute})
	return r, &logs
}

func TestPendingThenPublishedThenStale(t *testing.T) {
	check := &fakeCheck{collections: healthyCollections()}
	r, _ := newRunner(check, nil)
	now := time.Now()
	code, body := r.Lookup("things/ns/expected", now)
	envelope := body.(result.Envelope)
	if code != 503 || envelope.Results["collection"] != result.Pending {
		t.Fatal("configured item served before the first sample", code, envelope)
	}
	if code, _ := r.Lookup("things/ns/other", now); code != 404 {
		t.Fatal("undiscovered item served", code)
	}
	r.Refresh(t.Context(), "fake")
	for _, path := range []string{"things", "things/ns/expected", "things/ns/other", "things/system"} {
		if code, body := r.Lookup(path, time.Now()); code != 200 {
			t.Fatal(path, code, body)
		}
	}
	_, body = r.Lookup("things/system", time.Now())
	if body.(result.Envelope).Check != "things/system" {
		t.Fatal("singleton route shadowed by an item", body)
	}
	code, body = r.Lookup("things", time.Now().Add(4*time.Minute))
	if code != 503 || !strings.HasPrefix(body.(result.Envelope).Results["collection"], "stale(") {
		t.Fatal("stale sample still healthy", code, body)
	}
	if code, _ := r.Lookup("unknown", time.Now()); code != 404 {
		t.Fatal(code)
	}
}

func TestExpectedItemMissingFromAPublishedSampleFailsClosed(t *testing.T) {
	collections := healthyCollections()
	collections[0].Items = collections[0].Items[1:]
	r, _ := newRunner(&fakeCheck{collections: collections}, nil)
	r.Refresh(t.Context(), "fake")
	code, body := r.Lookup("things/ns/expected", time.Now())
	if envelope := body.(result.Envelope); code != 503 || envelope.Results["ready"] != "missing" || envelope.Healthy {
		t.Fatal("expected item absent from the sample served healthy", code, envelope)
	}
}

// Before any sample, and for a route a sample omitted, no dimension reads ok:
// a consumer conditioned on one dimension must not see green.
func TestUnsampledAndOmittedCollectionsNeverReadOK(t *testing.T) {
	check := &fakeCheck{err: &kube.Error{Code: kube.CodeForbidden, Status: 403}}
	r, _ := newRunner(check, nil)
	r.Refresh(t.Context(), "fake")
	_, body := r.Lookup("things", time.Now())
	if e := body.(result.Envelope); e.Results["collection"] != "forbidden" || e.Results["ready"] != result.Pending || e.Results["inventory"] != result.Pending {
		t.Fatal("dimensions read ok before any sample", e.Results)
	}
	_, body = r.Lookup("things/ns/expected", time.Now())
	if e := body.(result.Envelope); e.Healthy || e.Results["ready"] != result.Pending {
		t.Fatal(e.Results)
	}
	check.err, check.collections = nil, healthyCollections()[:1]
	r.Refresh(t.Context(), "fake")
	code, body := r.Lookup("things/system", time.Now())
	if e := body.(result.Envelope); code != 503 || e.Results["collection"] != "missing" {
		t.Fatal("omitted collection served healthy", code, e.Results)
	}
	check.collections = []result.Collection{healthyCollections()[1]}
	r.Refresh(t.Context(), "fake")
	code, body = r.Lookup("things", time.Now())
	if e := body.(result.Envelope); code != 503 || e.Results["collection"] != "missing" || e.Results["ready"] != "missing" {
		t.Fatal("omitted collection served healthy", code, e.Results)
	}
	if code, _ := r.Lookup("things/ns/expected", time.Now()); code != 503 {
		t.Fatal("item of an omitted collection served healthy", code)
	}
}

func TestFailedCollectionFailsClosedWithoutLeakingErrors(t *testing.T) {
	check := &fakeCheck{collections: healthyCollections()}
	r, logs := newRunner(check, nil)
	r.Refresh(t.Context(), "fake")
	check.err = errors.Join(errors.New("password=hunter2 response body"), &kube.Error{Code: kube.CodeForbidden, Status: 403, Path: "/x"})
	r.Refresh(t.Context(), "fake")
	for _, path := range []string{"things", "things/ns/expected", "things/system"} {
		code, body := r.Lookup(path, time.Now())
		envelope := body.(result.Envelope)
		if code != 503 || envelope.Results["collection"] != kube.CodeForbidden {
			t.Fatal(path, code, envelope)
		}
	}
	check.err = errors.New("token=secret raw failure")
	r.Refresh(t.Context(), "fake")
	_, body := r.Lookup("things", time.Now())
	data, _ := json.Marshal(body)
	if body.(result.Envelope).Results["collection"] != "collection_error" || strings.Contains(string(data), "secret") || strings.Contains(logs.String(), "secret") ||
		strings.Contains(logs.String(), "hunter2") {
		t.Fatal(string(data), logs.String())
	}
	check.err = Fail("rejected")
	r.Refresh(t.Context(), "fake")
	if _, body := r.Lookup("things", time.Now()); body.(result.Envelope).Results["collection"] != "rejected" {
		t.Fatal(body)
	}
	check.err = nil
	r.Refresh(t.Context(), "fake")
	if code, _ := r.Lookup("things", time.Now()); code != 200 {
		t.Fatal("recovery not published", code)
	}
}

func TestIndexGateReportsProcessHealthOnly(t *testing.T) {
	check := &fakeCheck{collections: healthyCollections()}
	now := time.Now()
	api := &fakeHealth{}
	r, _ := newRunner(check, api)
	code, index := r.Index(now)
	if code != 503 || index.Results["apiserver"] != result.Pending || !strings.HasPrefix(index.Results["sample_fresh"], "stale") {
		t.Fatal(code, index.Results)
	}
	// One check's missing permission does not trip the gate while others succeed.
	api.h = kube.Health{ForbiddenPaths: 1, OKPaths: 3, LastSuccess: now}
	r.Refresh(t.Context(), "fake")
	check.collections[0].Items[0].Results["ready"] = "not_ready"
	r.Refresh(t.Context(), "fake")
	code, index = r.Index(time.Now())
	if code != 200 || index.Results["apiserver"] != result.OK || index.Routes["/v1/things/ns/expected"] != "ready: not_ready" || index.Routes["/v1/things/system"] != result.OK {
		t.Fatal("unhealthy item or one forbidden check tripped the process gate", code, index)
	}
	// success -> failure: a failure newer than the last success trips the gate at once.
	api.h = kube.Health{LastSuccess: now, LastFailure: now.Add(time.Second), FailureCode: kube.CodeUnauthorized}
	if code, index := r.Index(now.Add(2 * time.Second)); code != 503 || index.Results["apiserver"] != kube.CodeUnauthorized {
		t.Fatal("recent success masked a newer failure", code, index.Results)
	}
	// failure -> recovery.
	api.h.LastSuccess = now.Add(3 * time.Second)
	if code, index := r.Index(now.Add(4 * time.Second)); code != 200 || index.Results["apiserver"] != result.OK {
		t.Fatal("recovery not reflected", code, index.Results)
	}
	// RBAC-wide refusal: nothing succeeds.
	api.h = kube.Health{LastSuccess: now, ForbiddenPaths: 4}
	if code, index := r.Index(now); code != 503 || index.Results["apiserver"] != kube.CodeForbidden {
		t.Fatal("RBAC-wide failure did not trip the gate", code, index.Results)
	}
	api.h = kube.Health{LastSuccess: now.Add(-time.Hour)}
	if _, index := r.Index(now); index.Results["apiserver"] != "stale" {
		t.Fatal(index.Results)
	}
	r, _ = newRunner(check, nil)
	r.Refresh(t.Context(), "fake")
	if code, index := r.Index(time.Now()); code != 200 || index.Results["apiserver"] != result.NA {
		t.Fatal(code, index.Results)
	}
}

type countingProber struct{ calls atomic.Int32 }

func (p *countingProber) Probe(context.Context) error { p.calls.Add(1); return nil }

func TestProbeRunsOnItsOwnInterval(t *testing.T) {
	r, _ := newRunner(&fakeCheck{collections: healthyCollections()}, nil)
	prober := &countingProber{}
	r.ProbeEvery(prober, 10*time.Millisecond, time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	r.Run(ctx)
	if prober.calls.Load() < 3 {
		t.Fatal("probe did not repeat", prober.calls.Load())
	}
}

func TestFirstSeen(t *testing.T) {
	f := NewFirstSeen()
	now := time.Now()
	f.Begin(now)
	if f.Active("a", true, now) != 0 {
		t.Fatal("new condition has history")
	}
	f.End()
	f.Begin(now.Add(time.Hour))
	if f.Active("a", true, now.Add(time.Hour)) != time.Hour || f.Observed(now.Add(time.Hour)) != time.Hour {
		t.Fatal("duration not tracked")
	}
	f.End()
	f.Begin(now.Add(2 * time.Hour))
	f.End()
	f.Begin(now.Add(3 * time.Hour))
	if f.Active("a", true, now.Add(3*time.Hour)) != 0 {
		t.Fatal("unobserved condition kept its first-seen time")
	}
	if f.Active("a", false, now.Add(3*time.Hour)) != 0 || f.Active("a", true, now.Add(4*time.Hour)) != 0 {
		t.Fatal("cleared condition kept its first-seen time")
	}
}
