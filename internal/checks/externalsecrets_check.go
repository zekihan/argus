package checks

import (
	"github.com/zekihan/argus/internal/checks/externalsecrets"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("externalsecrets", func(cfg config.Config, deps Deps) (runner.Check, error) {
		return externalsecrets.New(*cfg.Checks.ExternalSecrets, externalsecrets.API{Client: deps.API}), nil
	})
}
