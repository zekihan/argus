package checks

import (
	"github.com/zekihan/argus/internal/checks/argocd"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("argocd", func(cfg config.Config, deps Deps) (runner.Check, error) {
		c := *cfg.Checks.ArgoCD
		return argocd.New(c, argocd.API{Client: deps.API, Namespace: c.Namespace}), nil
	})
}
