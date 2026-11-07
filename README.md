# argus

Read-only, detailed health checks for the homelab Kubernetes cluster and its hosts, served over a
versioned HTTP/JSON API.

Each check reports one result per dimension (`ok`, `n/a` or a fixed reason code) plus the raw values
behind it, per item and aggregated per collection, so a consumer can alert on a single field and see
exactly what failed. Gatus is the first consumer; the API is not tied to it.

Argus replaces the homelab's separately deployed monitors with one static Go binary:

- pgBackRest backup completion freshness (absorbs [backrestwatch](https://github.com/zekihan/backrestwatch))
- PostgreSQL cluster health (Crunchy `PostgresCluster`)
- Longhorn volumes, replicas, backup targets and system backups
- cert-manager certificates and the shared PostgreSQL PKI
- External Secrets sync and store readiness
- CronJob freshness and run history
- Kubernetes nodes and Argo CD Applications
- Host health over SSH through a fixed, forced-command probe

It performs no writes to the cluster other than its own state ConfigMap, never reads Secret contents,
and never echoes raw condition messages, API bodies or spec fields.

## Status

In development. The design, API, security model and rollout plan are in
[docs/design.md](docs/design.md).

## License

GNU Affero General Public License v3.0. See [LICENSE](LICENSE).
