package checks

import (
	"github.com/zekihan/argus/internal/checks/certificates"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("certificates", func(cfg config.Config, deps Deps) (runner.Check, error) {
		return certificates.NewRenewals(*cfg.Checks.Certificates, certificates.API{Client: deps.API}), nil
	})
	register("pki", func(cfg config.Config, deps Deps) (runner.Check, error) {
		c := *cfg.Checks.PKI
		return certificates.NewPKI(c, certificates.PKIAPI{Client: deps.API, RootNamespace: c.RootNamespace, Bundles: len(c.ExpectedBundles) > 0}), nil
	})
}
