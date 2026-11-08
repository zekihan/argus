// Package checktest runs checks through the real runner for fixture tests.
package checktest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
)

// fixed collects with a fixed evaluation time, so fixtures can use the
// timestamps of the Python tests they port.
type fixed struct {
	runner.Check
	now time.Time
}

func (f fixed) Collect(ctx context.Context, _ func() time.Time) ([]result.Collection, error) {
	return f.Check.Collect(ctx, func() time.Time { return f.now })
}

// Harness is a runner with one check, plus everything it logged.
type Harness struct {
	Runner *runner.Runner
	Logs   *bytes.Buffer
	name   string
}

// New registers check with a runner whose sample is evaluated at now.
// Responses are rendered at time.Now() unless At is used.
func New(check runner.Check, now time.Time) *Harness {
	logs := &bytes.Buffer{}
	r := runner.New(slog.New(slog.NewJSONHandler(logs, nil)), "test", nil, time.Hour)
	if !now.IsZero() {
		check = fixed{Check: check, now: now}
	}
	r.Add(check, runner.Timing{Interval: time.Minute, Timeout: 10 * time.Second, MaxAge: 24 * 365 * time.Hour})
	return &Harness{Runner: r, Logs: logs, name: check.Name()}
}

// Refresh collects one sample.
func (h *Harness) Refresh(t *testing.T) *Harness {
	t.Helper()
	h.Runner.Refresh(t.Context(), h.name)
	return h
}

// Get renders /v1/<path>.
func (h *Harness) Get(t *testing.T, path string) (int, result.Envelope) {
	t.Helper()
	code, body := h.Runner.Lookup(path, time.Now())
	envelope, ok := body.(result.Envelope)
	if !ok {
		t.Fatalf("%s: %d %+v", path, code, body)
	}
	return code, envelope
}

// Parity reports whether every named result of an envelope is healthy: the
// verdict a consumer conditioned on those dimensions sees.
func Parity(e result.Envelope, dimensions ...string) bool {
	for _, d := range append([]string{"collection"}, dimensions...) {
		value, ok := e.Results[d]
		if !ok || !result.Healthy(value) {
			return false
		}
	}
	return true
}

// NoLeak fails if any secret appears in any rendered route or log line.
func (h *Harness) NoLeak(t *testing.T, secrets ...string) {
	t.Helper()
	var all strings.Builder
	_, index := h.Runner.Index(time.Now())
	data, _ := json.Marshal(index)
	all.Write(data)
	for route := range index.Routes {
		_, body := h.Runner.Lookup(strings.TrimPrefix(route, "/v1/"), time.Now())
		data, _ := json.Marshal(body)
		all.Write(data)
	}
	all.WriteString(h.Logs.String())
	for _, secret := range secrets {
		if strings.Contains(all.String(), secret) {
			t.Fatalf("%q leaked into a response or log line", secret)
		}
	}
}
