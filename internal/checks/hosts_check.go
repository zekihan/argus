package checks

import (
	"github.com/zekihan/argus/internal/checks/hosts"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

func init() {
	register("hosts", func(cfg config.Config, _ Deps) (runner.Check, error) {
		c := *cfg.Checks.Hosts
		return hosts.New(c, hosts.SSH{User: c.User, PrivateKeyFile: c.PrivateKeyFile, KnownHostsFile: c.KnownHostsFile}), nil
	})
}
