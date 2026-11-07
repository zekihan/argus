package checks

import (
	"github.com/zekihan/argus/internal/checks/postgres"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("postgres", func(cfg config.Config, deps Deps) (runner.Check, error) {
		return postgres.New(*cfg.Checks.Postgres, postgres.API{Client: deps.API}), nil
	})
}
