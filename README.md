# argus

Read-only, detailed health checks for the homelab Kubernetes cluster and its hosts, served over a
versioned HTTP/JSON API.

Each check reports one result per dimension (`ok`, `n/a` or a fixed reason code) plus the raw values
behind it, per item and aggregated per collection, so a consumer can alert on a single field and see
exactly what failed. Gatus is the first consumer; the API is not tied to it.

Argus replaces the homelab's separately deployed monitors with one static Go binary:

- pgBackRest backup completion freshness (absorbs [backrestwatch](https://github.com/zekihan/backrestwatch))
- PostgreSQL cluster health (Crunchy `PostgresCluster`)
- PVC filesystem fill from verified kubelet statistics (80% default), including pgdata and Redis
- Longhorn volumes, replicas, backup targets and system backups
- cert-manager certificates and the shared PostgreSQL PKI
- External Secrets sync and store readiness
- CronJob freshness and run history
- Kubernetes nodes and Argo CD Applications
- Host health over SSH through a fixed, forced-command probe

It performs no writes to the cluster other than its own state ConfigMap, never reads Secret contents,
and never echoes raw condition messages, API bodies or spec fields.

## API

Every route except `/healthz` requires `Authorization: Bearer <token>`. Responses are 200 when every
result is healthy (`ok` or `n/a`) and 503 otherwise.

| Route | Body |
| --- | --- |
| `/v1/<collection>/<item>` | one item: `results` per dimension plus `collection`, raw `values` |
| `/v1/<collection>` | per dimension `ok` or the failing items, plus `collection` and `inventory`; `items` gives each item's verdict |
| `/v1/checks` | every route and its verdict; `results.apiserver` and `results.sample_fresh` describe the process |
| `/healthz` | process liveness only, no authentication |

"Persistent for" windows (a node cordoned or version-skewed for more than 2h, an Argo CD app
OutOfSync for more than 15m or 7 days) are tracked in memory and restart with the pod: a window
never fails before it has been observed in full, and `observedSinceSeconds` shows how long the
current pod has watched. With nightly `edge` updates, a 7-day window rarely elapses.

Collections: `pvcs`, `hosts`, `nodes`, `postgres`, `pgbackrest`, `longhorn/volumes`, `longhorn/system`,
`cronjobs`, `certificates`, `pki`, `externalsecrets`, `clusterexternalsecrets`, `secretstores`,
`clustersecretstores`, `argocd`.

```json
{"check":"longhorn/volume","target":"immich/library","healthy":false,
 "observedAt":"2026-11-07T14:11:28Z","summary":"backup: backup_stale(last 1d7h, max 1d2h)",
 "results":{"collection":"ok","volume":"ok","state":"ok","robustness":"ok","replicas":"ok",
            "backup":"backup_stale(last 1d7h, max 1d2h)","scheduled":"ok","snapshots":"ok"},
 "values":{"sizeBytes":53687091200,"replicasHealthy":2,"replicasDesired":2,"attached":true}}
```

## Command line

```sh
argus serve -config /config/config.json            # the service
argus serve -config config.json -check-config      # validate configuration only
argus once -config config.json -check cronjobs     # one collection; exit 0 healthy, 1 unhealthy, 2 usage or config error
kubectl --context <ctx> proxy --port 8001 &
argus once -api-url http://127.0.0.1:8001 -check pki-lifecycle
argus once -check pki-lifecycle -snapshot certificates.json
argus state validate -file evidence.json           # check an exported backrestwatch ledger before import
```

Plain HTTP is accepted only for a loopback origin such as `kubectl proxy`. `once` never writes. A
failed collection (API error, 403) is reported in the envelope and exits 1, like any unhealthy result.

## Development

`make lint test test/race test/integration` (or `make audit`), `make chart-test`,
`make container-test`, `make release-check`. See [docs/design.md](docs/design.md) for the design,
[docs/parity.md](docs/parity.md) for the mapping from each replaced monitor's tests,
[charts/argus/README.md](charts/argus/README.md) for deployment and
[docs/publishing.md](docs/publishing.md) for releases.

## License

GNU Affero General Public License v3.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
