// Package runner schedules each check's collections, keeps the last published
// sample, and renders routes so that every check fails closed on a stale
// sample, an API error, a 403 or a missing expected item.
package runner

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

// Check collects one or more collections per sample.
type Check interface {
	Name() string
	// Describe returns the static route families with their configured
	// inventory targets, served before the first sample completes.
	Describe() []result.Collection
	Collect(ctx context.Context, now time.Time) ([]result.Collection, error)
}

// Failure is a collection failure with a fixed reason code.
type Failure struct{ Code string }

func (f *Failure) Error() string { return "collection failed: " + f.Code }

// Fail returns a Failure for a fixed reason code.
func Fail(code string) error { return &Failure{Code: result.Reason(code)} }

// Code maps a collection error to a fixed reason code, never its text.
func Code(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	var kerr *kube.Error
	if errors.As(err, &kerr) {
		return kerr.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return kube.CodeTimeout
	}
	return "collection_error"
}

// HealthSource reports the API client's last success and last failure code.
type HealthSource interface {
	Health() (time.Time, string)
}

// Timing is one check's schedule.
type Timing struct {
	Interval, Timeout, MaxAge time.Duration
}

type entry struct {
	check  Check
	timing Timing

	refresh     sync.Mutex
	mu          sync.RWMutex
	described   []result.Collection
	collections []result.Collection
	sampledAt   time.Time
	attemptedAt time.Time
	failure     string
	summaries   map[string]string
}

type Runner struct {
	logger    *slog.Logger
	version   string
	api       HealthSource
	apiMaxAge time.Duration
	entries   []*entry
}

// New returns a runner. api may be nil when no check talks to Kubernetes.
func New(logger *slog.Logger, version string, api HealthSource, apiMaxAge time.Duration) *Runner {
	return &Runner{logger: logger, version: version, api: api, apiMaxAge: apiMaxAge}
}

// Add registers a check. Call before Run.
func (r *Runner) Add(check Check, timing Timing) {
	r.entries = append(r.entries, &entry{check: check, timing: timing, described: check.Describe(), summaries: map[string]string{}})
}

// Run polls every check on its own interval until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, e := range r.entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				r.refreshEntry(ctx, e)
				timer := time.NewTimer(e.timing.Interval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	wg.Wait()
}

// Refresh collects one sample of the named check.
func (r *Runner) Refresh(ctx context.Context, name string) bool {
	for _, e := range r.entries {
		if e.check.Name() == name {
			r.refreshEntry(ctx, e)
			return true
		}
	}
	return false
}

func (r *Runner) refreshEntry(ctx context.Context, e *entry) {
	e.refresh.Lock()
	defer e.refresh.Unlock()
	ctx, cancel := context.WithTimeout(ctx, e.timing.Timeout)
	defer cancel()
	now := time.Now()
	collections, err := e.check.Collect(ctx, now)
	e.mu.Lock()
	e.attemptedAt = time.Now()
	if err != nil {
		e.failure = Code(err)
		e.mu.Unlock()
		// Only the fixed code is logged: errors may wrap response details.
		r.logger.Error("Collection failed", "check", e.check.Name(), "code", e.failure)
		return
	}
	e.collections, e.sampledAt, e.failure = collections, now, ""
	e.mu.Unlock()
	items, unhealthy := 0, 0
	for _, c := range collections {
		aggregate := c.Aggregate(result.OK, now, now)
		r.transition(e, "/v1/"+c.Route, aggregate.Summary)
		for target, summary := range aggregate.Items {
			items++
			if summary != result.OK {
				unhealthy++
			}
			r.transition(e, "/v1/"+c.Route+"/"+target, summary)
		}
	}
	r.logger.Info("Sample published", "check", e.check.Name(), "items", items, "unhealthy", unhealthy)
}

// transition logs a route whose verdict changed. Summaries hold only reason codes.
func (r *Runner) transition(e *entry, route, summary string) {
	if previous, ok := e.summaries[route]; ok && previous == summary {
		return
	}
	e.summaries[route] = summary
	if summary == result.OK {
		r.logger.Info("Route healthy", "route", route)
	} else {
		r.logger.Warn("Route unhealthy", "route", route, "summary", summary)
	}
}

// status is the collection result for an entry at now.
func (e *entry) status(now time.Time) (string, time.Time) {
	switch {
	case e.failure != "":
		return e.failure, e.sampledAt
	case e.sampledAt.IsZero():
		return result.Pending, e.sampledAt
	}
	age := now.Sub(e.sampledAt)
	if age < -time.Minute || age > e.timing.MaxAge {
		return result.Reason("stale", result.Labeled("age", result.Dur(age.Seconds())), result.Labeled("max", result.Dur(e.timing.MaxAge.Seconds()))), e.sampledAt
	}
	return result.OK, e.sampledAt
}

func (e *entry) current() []result.Collection {
	if e.collections != nil {
		return e.collections
	}
	return e.described
}

func findCollection(collections []result.Collection, route string) (result.Collection, bool) {
	for _, c := range collections {
		if c.Route == route {
			return c, true
		}
	}
	return result.Collection{}, false
}

// Lookup renders /v1/<path>, where path is a collection route or a route
// followed by an item target.
func (r *Runner) Lookup(path string, now time.Time) (int, any) {
	for _, e := range r.entries {
		e.mu.RLock()
		code, body, found := r.lookupEntry(e, path, now)
		e.mu.RUnlock()
		if found {
			return code, body
		}
	}
	return http.StatusNotFound, map[string]string{"reason": "unknown route"}
}

func (r *Runner) lookupEntry(e *entry, path string, now time.Time) (int, any, bool) {
	status, sampled := e.status(now)
	// Longest route first, so a target never shadows a deeper route.
	described := append([]result.Collection(nil), e.described...)
	sort.Slice(described, func(i, j int) bool { return len(described[i].Route) > len(described[j].Route) })
	for _, d := range described {
		c, ok := findCollection(e.current(), d.Route)
		if !ok {
			c = d
		}
		if path == d.Route {
			envelope := c.Aggregate(status, sampled, now)
			return httpStatus(envelope.Healthy), envelope, true
		}
		target, ok := strings.CutPrefix(path, d.Route+"/")
		if !ok || d.Singleton || target == "" {
			continue
		}
		item, found := c.Find(target)
		if !found {
			if item, found = d.Find(target); !found {
				return http.StatusNotFound, map[string]string{"reason": "unknown item"}, true
			}
		}
		envelope := c.ItemEnvelope(item, status, sampled, now)
		return httpStatus(envelope.Healthy), envelope, true
	}
	return 0, nil, false
}

func httpStatus(healthy bool) int {
	if healthy {
		return http.StatusOK
	}
	return http.StatusServiceUnavailable
}

// Index is the /v1/checks body: every route and its verdict, plus the
// process-level apiserver and sample_fresh results.
type Index struct {
	result.Envelope
	Routes map[string]string `json:"routes"`
}

// Index lists every route with its verdict. Its own health reflects only the
// process: API reachability and whether every check is still sampling.
func (r *Runner) Index(now time.Time) (int, Index) {
	routes := map[string]string{}
	unhealthy := 0
	var stalled []string
	names := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		names = append(names, e.check.Name())
		e.mu.RLock()
		status, sampled := e.status(now)
		if e.attemptedAt.IsZero() || now.Sub(e.attemptedAt) > e.timing.MaxAge {
			stalled = append(stalled, e.check.Name())
		}
		for _, d := range e.described {
			c, ok := findCollection(e.current(), d.Route)
			if !ok {
				c = d
			}
			aggregate := c.Aggregate(status, sampled, now)
			routes["/v1/"+c.Route] = aggregate.Summary
			if !aggregate.Healthy {
				unhealthy++
			}
			for target, summary := range aggregate.Items {
				routes["/v1/"+c.Route+"/"+target] = summary
			}
		}
		e.mu.RUnlock()
	}
	results := map[string]string{"apiserver": r.apiserver(now), "sample_fresh": result.OK}
	if len(stalled) > 0 {
		sort.Strings(stalled)
		results["sample_fresh"] = result.Reason("stale") + ": " + strings.Join(stalled, ", ")
	}
	healthy := result.Healthy(results["apiserver"]) && result.Healthy(results["sample_fresh"])
	at := now.UTC()
	envelope := result.Envelope{Check: "argus", Healthy: healthy, ObservedAt: &at, Summary: result.Summarize(results, []string{"apiserver", "sample_fresh"}),
		Results: results, Values: map[string]any{"version": r.version, "checks": names, "routes": len(routes), "unhealthyCollections": unhealthy}}
	return httpStatus(healthy), Index{Envelope: envelope, Routes: routes}
}

func (r *Runner) apiserver(now time.Time) string {
	if r.api == nil {
		return result.NA
	}
	success, failure := r.api.Health()
	if !success.IsZero() && now.Sub(success) <= r.apiMaxAge {
		return result.OK
	}
	if failure != "" {
		return failure
	}
	if success.IsZero() {
		return result.Pending
	}
	return result.Reason("stale")
}

// Envelopes renders every aggregate of the named check.
func (r *Runner) Envelopes(name string, now time.Time) []result.Envelope {
	var out []result.Envelope
	for _, e := range r.entries {
		if e.check.Name() != name {
			continue
		}
		e.mu.RLock()
		status, sampled := e.status(now)
		for _, d := range e.described {
			c, ok := findCollection(e.current(), d.Route)
			if !ok {
				c = d
			}
			out = append(out, c.Aggregate(status, sampled, now))
		}
		e.mu.RUnlock()
	}
	return out
}
