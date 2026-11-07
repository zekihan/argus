package checks

import (
	"github.com/zekihan/argus/internal/checks/longhorn"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("longhorn", func(cfg config.Config, deps Deps) (runner.Check, error) {
		c := *cfg.Checks.Longhorn
		return longhorn.New(c, longhorn.API{Client: deps.API, Namespace: c.Namespace}), nil
	})
}
