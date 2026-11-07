// Package hosts reads the forced-command host probe over SSH with a pinned
// known_hosts. Argus sends no script: it opens a session and reads the probe's
// fixed JSON. SSH and probe failures map to fixed codes; stderr is discarded.
package hosts

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
)

const Route = "hosts"

// maxFieldLength caps each probe field passed through to results.
const maxFieldLength = 200

// Dimensions: ssh and script describe the transport; the rest are the probe's
// fixed JSON fields (the check-host.sh contract).
var dimensions = append([]string{"ssh", "script"}, config.HostDimensions...)

// Outcome is one probe run.
type Outcome struct {
	// SSH is "ok" or a fixed code: unreachable, timeout, host_key_unknown,
	// host_key_mismatch, auth_failed, handshake_failed, session_failed,
	// key_unavailable, known_hosts_unavailable.
	SSH string
	// Exit is the probe's exit status; -1 when it did not exit normally.
	Exit int
	// Output is the probe's stdout, at most maxOutput bytes.
	Output []byte
	// Truncated reports output beyond maxOutput.
	Truncated bool
	// Timeout reports a probe that did not finish in time.
	Timeout    bool
	SSHLatency time.Duration
	RunLatency time.Duration
}

// Prober runs the probe on one host.
type Prober interface {
	Probe(ctx context.Context, host config.Host) Outcome
}

type Check struct {
	config config.Hosts
	prober Prober
}

func New(c config.Hosts, prober Prober) *Check { return &Check{config: c, prober: prober} }

func (c *Check) Name() string { return "hosts" }

func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "host", Dimensions: dimensions}
	for _, host := range c.config.Hosts {
		collection.Items = append(collection.Items, result.Item{Target: host.Name})
	}
	return []result.Collection{collection}
}

func (c *Check) Collect(ctx context.Context, _ time.Time) ([]result.Collection, error) {
	items := make([]result.Item, len(c.config.Hosts))
	limit := make(chan struct{}, c.config.Concurrency)
	var wg sync.WaitGroup
	for i, host := range c.config.Hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			hostCtx, cancel := context.WithTimeout(ctx, time.Duration(c.config.TimeoutSeconds)*time.Second)
			defer cancel()
			items[i] = Evaluate(host, c.prober.Probe(hostCtx, host))
		}()
	}
	wg.Wait()
	return []result.Collection{{Route: Route, Check: "host", Dimensions: dimensions, Items: items,
		Results: map[string]string{"inventory": result.OK}}}, nil
}

// Evaluate turns a probe outcome into results. A tool the probe reports as
// n/a (not installed) is healthy unless the host declares that dimension in
// expects; a failing tool is "unavailable", never ok.
func Evaluate(host config.Host, outcome Outcome) result.Item {
	results := map[string]string{"ssh": outcome.SSH, "script": result.OK}
	values := map[string]any{}
	unknown := func() {
		for _, dim := range config.HostDimensions {
			results[dim] = result.Reason("unknown")
		}
	}
	if outcome.SSH != result.OK {
		results["script"] = result.Reason("unknown")
		unknown()
		return result.Item{Target: host.Name, Results: results, Values: values}
	}
	values["sshLatencyMs"] = outcome.SSHLatency.Milliseconds()
	values["probeLatencyMs"] = outcome.RunLatency.Milliseconds()
	var fields map[string]json.RawMessage
	switch {
	case outcome.Timeout:
		results["script"] = result.Reason("timeout")
	case outcome.Truncated:
		results["script"] = result.Reason("output_too_large")
	case outcome.Exit != 0 && outcome.Exit != 1:
		// The probe exits 1 when any field is unhealthy; anything else is a failure.
		if outcome.Exit < 0 {
			results["script"] = result.Reason("killed")
		} else {
			results["script"] = "exit_" + strconv.Itoa(outcome.Exit)
		}
	case json.Unmarshal(outcome.Output, &fields) != nil || fields == nil:
		results["script"] = result.Reason("invalid_output")
	}
	if results["script"] != result.OK {
		unknown()
		return result.Item{Target: host.Name, Results: results, Values: values}
	}
	expects := map[string]bool{}
	for _, dim := range host.Expects {
		expects[dim] = true
	}
	for _, dim := range config.HostDimensions {
		raw, ok := fields[dim]
		var value string
		if !ok || json.Unmarshal(raw, &value) != nil {
			results[dim] = result.Reason("unknown")
			continue
		}
		switch value {
		case result.OK:
			results[dim] = result.OK
		case result.NA:
			results[dim] = result.NA
			if expects[dim] {
				results[dim] = result.Reason("unavailable")
			}
		case "unavailable":
			results[dim] = result.Reason("unavailable")
		default:
			text := result.ProbeText(value, maxFieldLength)
			if text == "" || text == result.OK || text == result.NA {
				text = result.Reason("unknown")
			}
			results[dim] = text
		}
	}
	return result.Item{Target: host.Name, Results: results, Values: values}
}
