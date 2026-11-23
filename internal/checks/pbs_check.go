package checks

import (
	"github.com/zekihan/argus/internal/checks/pbs"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("pbs", func(cfg config.Config, deps Deps) (runner.Check, error) {
		c := *cfg.Checks.PBS
		api, err := pbs.NewAPI(c)
		if err != nil {
			return nil, err
		}
		return pbs.New(c, api, deps.Metrics), nil
	})
}
