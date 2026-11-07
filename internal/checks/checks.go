// Package checks builds the enabled checks from configuration.
package checks

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/runner"
)

// Deps are the shared dependencies of every check.
type Deps struct {
	API    *kube.Client
	Logger *slog.Logger
	// ReadOnly checks never write: argus once evaluates against stored state.
	ReadOnly bool
}

// Built is one constructed check with its schedule.
type Built struct {
	Check  runner.Check
	Timing runner.Timing
}

type constructor func(cfg config.Config, deps Deps) (runner.Check, error)

var registry = map[string]constructor{}

func register(name string, build constructor) { registry[name] = build }

// Build constructs every enabled check, in configuration order.
func Build(cfg config.Config, deps Deps) ([]Built, error) {
	var out []Built
	for _, enabled := range cfg.Enabled() {
		build, ok := registry[enabled.Name]
		if !ok {
			return nil, fmt.Errorf("check %s is not available in this build", enabled.Name)
		}
		check, err := build(cfg, deps)
		if err != nil {
			return nil, fmt.Errorf("check %s: %w", enabled.Name, err)
		}
		interval, timeout, maxAge := cfg.Timing(enabled.Schedule)
		out = append(out, Built{Check: check, Timing: runner.Timing{Interval: seconds(interval), Timeout: seconds(timeout), MaxAge: seconds(maxAge)}})
	}
	return out, nil
}

// Kubernetes reports whether any enabled check talks to the Kubernetes API.
func Kubernetes(cfg config.Config) bool {
	for _, enabled := range cfg.Enabled() {
		if enabled.Name != "hosts" {
			return true
		}
	}
	return false
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// Checks register themselves from this package as they are added.
var _ = register
