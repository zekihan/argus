package checks

import (
	"github.com/zekihan/argus/internal/checks/pvc"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("pvcs", func(cfg config.Config, deps Deps) (runner.Check, error) {
		return pvc.New(*cfg.Checks.PVCs, pvc.API{Client: deps.API, ExpectedNodes: cfg.Checks.PVCs.ExpectedNodes, CheckAttachments: len(cfg.Checks.PVCs.AllowUnmounted) > 0}), nil
	})
}
