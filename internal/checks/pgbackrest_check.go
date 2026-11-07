package checks

import (
	"github.com/zekihan/argus/internal/checks/pgbackrest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
	"github.com/zekihan/argus/internal/state"
)

func init() {
	register("pgbackrest", func(cfg config.Config, deps Deps) (runner.Check, error) {
		c := *cfg.Checks.PGBackRest
		store := state.New(&state.ConfigMap{Client: deps.API, Namespace: cfg.State.Namespace, Name: cfg.State.Name})
		api := pgbackrest.API{Client: deps.API, Namespaces: pgbackrest.Namespaces(c)}
		return pgbackrest.New(c, api, store, deps.ReadOnly, deps.Logger), nil
	})
}
