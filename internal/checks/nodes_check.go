package checks

import (
	"github.com/zekihan/argus/internal/checks/nodes"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("nodes", func(cfg config.Config, deps Deps) (runner.Check, error) {
		c := *cfg.Checks.Nodes
		return nodes.New(c, nodes.API{Client: deps.API, Metrics: c.MetricsEnabled(), LonghornNamespace: c.LonghornNamespace}), nil
	})
}
