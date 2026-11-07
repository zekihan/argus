package checks

import (
	"github.com/zekihan/argus/internal/checks/cronjobs"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("cronjobs", func(cfg config.Config, deps Deps) (runner.Check, error) {
		c := *cfg.Checks.CronJobs
		return cronjobs.New(c, cronjobs.API{Client: deps.API, Namespaces: cronjobs.Namespaces(c)}), nil
	})
}
