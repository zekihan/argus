package runner

import (
	"github.com/zekihan/go-commons/metrics"

	"github.com/zekihan/argus/internal/result"
)

// collectionHealthy is the per-collection health gauge served on /metrics.
// The label holds the collection route (for example "cronjobs" or
// "longhorn/volumes"): a bounded, configuration-driven name, never an item
// target or raw path.
const collectionHealthy = "argus_collection_healthy"

// SetMetrics attaches the registry that collects per-collection health.
// Call before Run. A nil registry disables collection metrics.
func (r *Runner) SetMetrics(reg *metrics.Registry) {
	r.reg = reg
}

// observeCollection publishes 1 when the collection aggregate is healthy
// and 0 otherwise.
func (r *Runner) observeCollection(route string, healthy bool) {
	if r.reg == nil {
		return
	}
	gauge := r.reg.Gauge(collectionHealthy, metrics.Labels{"collection": route})
	if healthy {
		gauge.Set(1)
	} else {
		gauge.Set(0)
	}
}

// observeFailure marks every described collection of the check unhealthy:
// a failed collection publishes nothing, so its last gauge would go stale.
func (r *Runner) observeFailure(described []result.Collection) {
	if r.reg == nil {
		return
	}
	for _, d := range described {
		r.observeCollection(d.Route, false)
	}
}
