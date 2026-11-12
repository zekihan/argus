package checks

import (
	"github.com/zekihan/argus/internal/checks/doh"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("doh", func(cfg config.Config, _ Deps) (runner.Check, error) { return doh.New(*cfg.Checks.DoH), nil })
}
