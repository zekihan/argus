<!--
Design document for Argus. Paths without a repository prefix refer to the homelab GitOps
repository (zekihan/homelab). backrestwatch paths such as internal/evidence/store.go refer to
zekihan/backrestwatch. "Stage N" refers to the rollout section below.
-->
# Argus design

## Context

Custom monitoring is spread over seven separately deployed pieces:

| Today | Where | Consumer |
|---|---|---|
| backrestwatch (Go, own repo, PVC, bearer token + nginx TLS proxy) | `platform/backrestwatch` | Gatus, 21 endpoints |
| `certificate_monitor.py` + `check_postgres_certificates.py` | `platform/cert-manager/files/` | Gatus `/certificates`, `/renewals` |
| `sync_monitor.py` | `platform/external-secrets/files/` | Gatus `/externalsecrets` |
| `backup_monitor.py` | `platform/longhorn/files/` | Gatus `/backups` |
| `freshness.py` | `apps/dmarc/scripts/` | Gatus `/freshness` |
| `sync-freshness.py` (near-copy of the above) | `apps/mailvault/scripts/` | Gatus `/sync` |
| `check-host.sh` | `apps/gatus/scripts/`, sent by Gatus over SSH | Gatus SSH checks, every host in `hosts.yaml` |

Each Python monitor brings its own Deployment, ServiceAccount, RBAC, Service, NetworkPolicy,
`python:alpine` image pin and a matching Gatus egress policy (six templates in
`apps/gatus/templates/*-networkpolicy.yaml`). The goal is one compiled Go service, **Argus**, in a new
repo `zekihan/argus`. It absorbs backrestwatch and every script above, and exposes a stable
HTTP/JSON API that is not tied to Gatus. Gatus is only the first consumer; Argus should be able to
outlive it (an aggregate index and, later, alerting or a UI).

## Decisions

- **Name and repo:** `zekihan/argus`. Public, AGPL-3.0 to match backrestwatch. Images
  `ghcr.io/zekihan/argus` and `docker.io/zekihan/argus` (linux amd64/arm64, scratch). Helm chart
  `oci://ghcr.io/zekihan/charts/argus`. The repo, its DOCKER secrets and its ruleset are created in
  `terraform/git/repository.tf`, and its mirror in git-mirrorer's values, not by hand.
- **Standalone Deployment, not a Gatus sidecar.** Argus is meant to outlive Gatus. A sidecar would
  put cluster-wide read RBAC on the Gatus pod and restart Gatus on every Argus release. Lives in
  `platform/argus`, namespace `argus`, replacing `platform/backrestwatch`.
- **Keep backrestwatch's security posture without the nginx sidecar.** Argus serves TLS natively from
  the cert-manager Secret and re-reads the certificate when its files change, so neither Reloader
  nor a pod restart is needed. It requires the bearer token (backrestwatch's `auth.go`) on every
  route except `/healthz`. Ingress is limited to the Gatus pod.
- **Same release pattern as every other owned app.**
  - Argus uses the shared `zekihan/release-workflows` edge and release flow unchanged: a nightly `edge`
    image, a stable chart published on `vX.Y.Z` tags, and homelab pinning the released chart with an
    `edge@sha256` image override, as backrestwatch does today.
  - Updates arrive through the common owned-edge Renovate rule and auto-merge like the rest. There is
    no Argus-specific Renovate exception.
  - Two rules make edge images safe to run against a released chart:
    - **Config stays backward compatible.** Every config field added after a chart release is optional
      with a safe default, so an edge image always accepts the config the latest released chart
      renders. Argus CI renders the latest released chart and starts the edge binary with
      `-check-config` against it; a failure blocks the edge publish.
    - **A missing permission fails one check, not the process.** A check whose RBAC the released chart
      doesn't grant yet reports `collection: forbidden` on its own routes. New checks ship disabled by
      default until a chart release grants their RBAC.
  - The SSH key in an auto-updated pod is acceptable because of the forced command (*Secrets and
    SSH*): even a bad image can only run the fixed probe.
- **State in a ConfigMap, not a PVC.** The only state that must survive a restart is the pgBackRest
  ledger. Keeping it off a PVC removes the Longhorn dependency: a Longhorn monitor should not depend
  on a Longhorn volume. Rules below.
- **Stdlib only**, plus `golang.org/x/crypto/ssh` for host checks. Reuse backrestwatch's hand-written
  Kubernetes client rather than client-go.
- **Config is one strict JSON file** (unknown fields rejected), rendered by the chart with `toJson`.
  Each check is optional.

## State and single writer

backrestwatch's guarantees are kept, with a ConfigMap in place of the flocked file
(`internal/evidence/store.go`, `internal/monitor/monitor.go:78-95`). No Lease: Argo CD here excludes
`coordination.k8s.io/Lease` (`platform/argocd/values.cluster-1.yaml:23-26`).

- **One writer by construction.** One replica with `strategy: Recreate`, so a new pod starts only
  after the old one has terminated. An owner claim on top of that was dropped in round 4: it guarded
  against a second writer that this setup already excludes.
- **Every write is conditional, and conflicts converge.** Each PUT carries the resourceVersion of the
  last GET. After a 409, timeout or reset, Argus re-reads the ConfigMap:
  1. a corrupt or unknown-version document → `state: invalid`, failing closed and never overwritten;
  2. otherwise it adopts the stored ledger and re-evaluates the current sample against it, then
     writes again if needed.

  The ledger is derived from observations. Re-evaluation therefore re-creates any destination-change
  boundary (`NotBefore`) and any success whose Job or CronJob still exists, whichever ledger it
  started from. That covers a second writer and Argo's compare-then-apply race (*Out of Argo's reach*
  below). The one thing it can't recover is a success whose Job has already been deleted. Completed
  pgBackRest Jobs are retained well beyond one poll, so that window is negligible.
- **Success is published only after it's persisted**, as in backrestwatch: until a write succeeds,
  newly observed invalidations are kept (`retainInvalidations`), new successes are dropped, and the
  check reports `state: unpersisted`. The ledger is written only when it changes.
- **Strict load and import.**
  - Key `pgbackrest.json` holds `{version, entries}`, with backrestwatch's validation for both ledger
    versions 1 and 2 (`store.go:55-89`).
  - The size cap drops from 4 MiB to 200 KiB. That is under both the 1 MiB ConfigMap limit and the
    256 KiB client-side-apply annotation limit, and far above the roughly 20 repos today. The Argus
    Application also sets `serverSideApply: true`, as Gatus does.
  - `repository.go` (the destination-identity SHA-256) is ported verbatim. A test loads a real
    exported backrestwatch ledger and asserts every entry survives re-evaluation with no new
    `NotBefore` barrier. Otherwise the import would turn all repos red until their next backup.
  - The import happens before the pgbackrest check is enabled, so it can't race an empty ledger:
    1. Deploy with `checks.pgbackrest` disabled.
    2. Copy `evidence.json` off the RWO `backrestwatch-state` PVC using a disposable read-only pod on
       the same node, as `docs/backrestwatch-migration.md` did. The backrestwatch image is scratch,
       so `kubectl cp` won't work.
    3. Run `argus state validate --file evidence.json`.
    4. Put the file into `argus-state`.
    5. Enable the check.
- **Out of Argo's reach.**
  - The chart creates `argus-state` once with an empty `pgbackrest.json`, and the object carries
    `Prune=false,Delete=false`.
  - The Application's `ignoreDifferences` uses
    `jqPathExpressions: ['select(.metadata.name == "argus-state") | .data']`, with
    `respectIgnoreDifferences: true`. It is scoped by name because the schema has no name field, and a
    bare `.data` would also freeze the config ConfigMap.
  - With `RespectIgnoreDifferences`, a sync re-applies the live `.data` it saw at compare time without
    a precondition. An Argus write between compare and apply can be rolled back one step; the
    convergence rule above re-derives it.
  - RBAC allows `get`/`update` on that one name only. A homelab render test asserts the rule matches
    only `argus-state`.
- **Tests:**
  - A fake API server covering 409, timeout then read-back, a stored ledger changed underneath, a
    corrupt document, an unknown version and an oversized ledger.
  - A case in `tests/integration/test_gitops_integration.py` (disposable K3s with Argo CD): write a
    200 KiB ledger, sync the Argus Application again, and assert that the sync succeeds and the ledger
    is unchanged.

## Response and log hygiene

Detailed results must not leak what Argus can read:

- Results are fixed reason codes with allowlisted parameters: counts, durations, timestamps, object
  names and mount paths. Examples: `backup_stale(last 31h, max 26h)`, `not_ready`.
- Raw condition `message` fields, API response bodies, decoded spec fields and Go error strings never
  appear in responses or logs. ESO and Crunchy provider conditions can carry secret material, which
  is why `sync_monitor.py` already emits fixed reasons only.
- Host results come only from the probe's fixed JSON fields, each capped in length. SSH and
  stderr diagnostics map to fixed codes (`ssh: host_key_mismatch`, `ssh: unreachable`,
  `script: exit_3`).
- Tests feed fixtures containing credential-shaped strings in condition messages, error bodies, pod
  env values and script stderr, and assert none appear in any response or log line.

## API (v1): detailed checks, like the SSH host check

Every check works like `check-host.sh`: each item has its own route, and its body has one named
**result** per dimension plus raw **values**. A consumer can alert on a single field and show exactly
what failed. A result is exactly `ok`, `n/a` (the dimension doesn't apply, e.g. a missing condition
or a non-Longhorn node), or a reason code. `ok` and `n/a` are healthy. HTTP 200 when every result is
healthy, otherwise 503. Each check fails closed on a stale sample, an API error, a 403 or a missing
expected item.

```json
{"check":"longhorn/volume","target":"immich/library","healthy":false,
 "observedAt":"2026-11-07T14:11:28Z","summary":"backup: backup_stale(last 31h, max 26h)",
 "results":{"volume":"ok","state":"ok","robustness":"ok","replicas":"ok",
            "backup":"backup_stale(last 31h, max 26h)","scheduled":"ok","snapshots":"ok"},
 "values":{"sizeBytes":53687091200,"actualSizeBytes":31457280000,"replicasHealthy":2,"replicasDesired":2,
           "lastBackupAt":"...","lastBackupAgeSeconds":111600,"attached":true,"node":"cluster-1-agent-ahy"}}
```

Each collection also has an aggregate route, such as `/v1/longhorn/volumes`. Its `results` are
**per dimension**, not per item: each dimension (`backup`, `robustness`, ...) is `ok` or names the
failing items, capped in length. Two extra dimensions cover the collection itself: `collection`
(sample fresh, no API error) and `inventory` (missing or unexpected items). A separate `items` map
gives each item's overall verdict. This lets a consumer alert on one dimension across all items
(e.g. `[BODY].results.last_success == ok`) without being triggered by another. `GET /v1/checks`
lists every route and its verdict. `GET /healthz` (no auth) reports process health only.

Items come from an expected inventory where a disappearance must alert (hosts, pgBackRest repos,
Longhorn PVCs, CronJobs, nodes, Postgres clusters). Otherwise they are discovered from the API, with an
optional `expected` list (certificates, External Secrets, Argo CD apps, namespaces). Inventory checks
also flag the reverse: a PostgresCluster or backup-labelled Longhorn volume that exists but isn't
in the inventory. backrestwatch does this today (`evaluate.go:111-120`).

"Persistent for" thresholds use an in-memory first-seen time and don't fail before that long has
been observed. After a restart, the value `observedSinceSeconds` says how long has been seen.

| Route (per item; aggregate without the last segment) | Results | Values | Replaces / new |
|---|---|---|---|
| `/v1/hosts/{name}` | `ssh` (connect and pinned host key), `disk`, `inodes`, `memory`, `swap`, `load`, `cpu_pressure`, `uptime`, `failed_units`, `zfs`, `time_sync`, `ups`. A tool that isn't installed is `n/a`; a tool that is present but fails (permission, error) is `unavailable`, never `ok`. Today a failed `zpool status` silently stays `ok` (`check-host.sh:214`). The host inventory declares the dimensions a host must have (`expects: [zfs, ups]`), and for those a missing tool is `unavailable`. | SSH and probe latency | `check-host.sh` via Gatus |
| `/v1/nodes/{name}` | `ready` (must be True; missing or Unknown fails), `memory_pressure`, `disk_pressure`, `pid_pressure` (must be present and False; all eight nodes report them today), `network` (only NetworkUnavailable may be absent, which is `n/a` on k3s/flannel); `schedulable` and `kubelet_version` (cordoned or skewed from the control plane **for more than 2h**, so system-upgrade's rolling drain doesn't page); `cpu`, `memory` (metrics-server usage against allocatable); `longhorn_disk` (disks ready and storage left; `n/a` where no Longhorn node exists, i.e. the control plane) | CPU and memory usage and %, Longhorn storage % | **new** |
| `/v1/doh` | `answer`: a POSTed A query for the configured name over the configured HTTPS URL must return HTTP 200, `application/dns-message`, the same ID and question, QR set, TC clear, NOERROR and an A record of the configured name with the expected IPv4 address. Redirects, other content types, SERVFAIL, NXDOMAIN, empty answers and malformed messages are `invalid_response`. An optional `dnsResolver` resolves the HTTPS host through a public resolver so the probe follows the public route. | none | Gatus DoH status check (**OPS-462**) |
| `/v1/pvcs/{ns}/{pvc}` | `bound`, `usage` (filesystem used bytes / capacity above 80%); aggregate `/v1/pvcs` also checks node and optional PVC inventory. Discover every PVC in configured storage classes, including pgdata and Redis. Missing, invalid, future or stale statistics fail closed. Explicit `allowUnmounted` claims may be idle when no VolumeAttachment exists or during a bounded first-sample grace period for a new attachment. Raw-block claims have no filesystem usage dimension. | usedBytes, capacityBytes, usedPercent, measuredAt | **OPS-420** |
| `/v1/postgres/{ns}/{cluster}` | `instances` (ready/updated against desired), `proxy` (PgBouncer ready), `conditions` (any present condition that is False; missing ones such as watchledger's `PostgresDataInitialized` are not failures), `scheduled_backups` (latest scheduled backup failed) | replica counts | **new** |
| `/v1/pgbackrest/{ns}/{cluster}/{repo}` | `freshness`, `evidence` (source eligible), `destination` (identity unchanged), `stanza` (created), `state` (ledger valid and persisted) | last success, age, max age, source | backrestwatch |
| `/v1/pgbackrest` and `/v1/postgres` aggregates | plus `inventory`: clusters/repos present but not configured, and configured but missing | counts | backrestwatch |
| `/v1/longhorn/volumes/{ns}/{pvc}` | `volume` (exists, unambiguous), `backup`, `scheduled`, `snapshots` (TooManySnapshots), `robustness` (`degraded` or `faulted` fail whether or not the volume is attached, as `backup_monitor.py:139` does; only `unknown` while detached is `n/a`), `state`, `replicas` (healthy against desired, from `replicas.longhorn.io`; `n/a` while detached, as for `mailvault-state` between runs) | sizes, replicas, last backup and age, attached, node | `backup_monitor.py`, expanded |
| `/v1/longhorn/system` | `backup_targets`, `system_backup` (latest Ready and fresh), `inventory` (unexpected or missing backup volumes) | target count, system backup age | `backup_monitor.py` |
| `/v1/cronjobs/{ns}/{name}` | parity: `schedule` (suspended), `last_success` (within max age). Detail: `last_run` (latest Job failed or overran), `active` (stuck runs), `failures` (consecutive failed Jobs) | last success/schedule, age, active, failed count | dmarc/mailvault freshness, generalised |
| `/v1/certificates/{ns}/{name}` | parity: `ready` (not ready > 1h), `renewal` (overdue > 24h), `expiry` (< 14 days). Detail: `issuer` (issuer Ready), `request` (latest CertificateRequest failed or denied) | notAfter, days left, renewal time, revision | `certificate_monitor.py` `/renewals` |
| `/v1/pki` (the service profile, `certificate_monitor.py` `/certificates`) | per certificate under `selfsigned-ca-issuer`: `ready`, `readiness_generation`, `expiry`, `ca_key_pinned`, `issuer_lifetime` and `ca_renewal` with the service's 90-day margin (180 days of root headroom); plus `issuer_present`, `inventory`, `root_pin` (fingerprint or reviewed SPKI, from CertificateRequests) and `bundles` (trust-manager Synced at current generation) | days left, renewal time, root fingerprint | `certificate_monitor.py` `/certificates` |
| `pki-lifecycle` (CLI only, `argus once`) | exactly `check_postgres_certificates.py`: the same per-certificate assertions with its 30-day margin (120 days of headroom); no pin, bundles or inventory, so it works from an offline snapshot | as above | the CLI |
| `/v1/externalsecrets/{ns}/{name}` | `ready`, `synced` (generation), `refresh` (overdue), `store` (referenced store Ready) | refresh age and interval | `sync_monitor.py` |
| `/v1/clusterexternalsecrets/{name}` | `ready` | n/a | `sync_monitor.py` |
| `/v1/secretstores/{ns}/{name}`, `/v1/clustersecretstores/{name}` | `ready`, for every store whether or not anything references it | n/a | `sync_monitor.py` |
| `/v1/argocd/{app}` | `sync`: OutOfSync for more than 15m on an app with automated sync, or more than 7 days on a manual app, since `autoSync: false` is the readiness gate and manual apps sit OutOfSync after merges by design. `health`: app-level `status.health` Degraded or Missing, or Progressing for more than 15m (from its `lastTransitionTime`). `operation`: last sync Failed or Error | out-of-sync resource count, manual apps currently OutOfSync, last sync. Argo CD v3.6 stores no per-resource health, so there are no degraded counts | **new** |
| `/v1/workloads/{ns}`, **deferred to v2** with an explicit short namespace list; parity ships first | `deployments`, `statefulsets`, `daemonsets` (ready against desired), `pods` (CrashLoopBackOff, ImagePullBackOff, Pending > 10m), `restarts` (more than N observed in the window), `oom` (container `lastState.terminated.reason == OOMKilled` with `finishedAt` in the last 24h), `jobs` (failed Jobs that don't belong to a CronJob) | counts per kind, restarts in the window, `restartWindowObservedSeconds` | **new** |

`oom` reads each container's last termination from pod status: it needs no history but sees only the
most recent one. `restarts` compares `restartCount` across samples in memory, keyed by pod UID and
container name. After a restart it judges only the part of the window it has observed, still reports
plain `ok`, and puts the coverage in `restartWindowObservedSeconds`.

Dropped on purpose:
- **Longhorn allocation and node pod capacity:** `actualSize` includes snapshots (the redis volumes are
  at 107–124% today), and node pod counts would need a cluster-wide pod list.

### The `once` CLI

`argus once -config f.json -check <name>` runs a single collection, prints the envelope and exits
non-zero when unhealthy. For `pki-lifecycle` it has both of `check_postgres_certificates.py`'s modes:
- explicit context: `kubectl --context <ctx> proxy --port 8001 &` then
  `argus once -api-url http://127.0.0.1:8001 -check pki-lifecycle` (plain HTTP is accepted only for a
  loopback origin);
- offline: `argus once -check pki-lifecycle -snapshot certificates.json`.

`docs/postgres-access-hardening-*.md` move to these commands in the same commit that deletes the
Python file.

## Gatus wiring

- **Alerts: an Argus fault pages exactly once.**
  - Per-item endpoints (hosts, nodes, Postgres, pgBackRest repos, Longhorn volumes, CronJobs,
    certificates) are visible but set `alerts: []`.
  - About 14 aggregate endpoints page, one per collection, and each names the failing items.
  - Every paging aggregate has `dependsOn: [argus]`, the repo's existing gate mechanism (rendered as
    a suite, `apps/gatus/README.md` §Gates).
  - The `argus` gate calls the **authenticated** `/v1/checks` with the token. `/v1/checks` also
    reports process-level results: `apiserver` (the last API request succeeded) and `sample_fresh`.
    The gate is conditioned on `[STATUS] == any(200, 503)` and `[BODY].results.apiserver == ok`.
    So TLS, auth (401), reachability, API server, service-account token and RBAC-wide failures all
    trip the gate, and only the gate pages. `/healthz` alone would stay green while every collection
    gets 401.
  - This adds about 14 suites. The suites API is already slow (about 31 s, TwiN/gatus#1909), so stage
    6 measures it before and after, and the gate can drop to a smaller set of aggregates if it
    regresses badly.
- **Conditions: parity first, per dimension.** At cutover each replaced paging aggregate is
  conditioned on its parity dimensions plus `collection` and `inventory`. For example, CronJobs use
  `[BODY].results.schedule == ok`, `[BODY].results.last_success == ok`, `[BODY].results.collection
  == ok` and `[BODY].results.inventory == ok`. It is not conditioned on `[STATUS] == 200`, which also
  includes new detail dimensions. Detail dimensions are promoted into conditions one collection at a
  time after cutover.
- **Timing is part of parity.** Each replaced group keeps its current interval and alert thresholds,
  so time-to-page doesn't change at cutover:
  - pgBackRest and Longhorn: every 30m, failure 2 / success 1;
  - dmarc and mailvault: failure 2 / success 1;
  - certificates and External Secrets: failure 5 / success 3.

  Longhorn's `[BODY].summary == ok` maps to the aggregate's per-dimension results.
- **Test:** for two CronJobs where only one is stale, the CronJob aggregate pages at cutover. For two
  where only one fails a new detail dimension (`last_run`), it doesn't.
- **Gatus tests.** `apps/gatus/tests/test_checks.py:78-82` (`test_every_alert_is_explicit`) gets an
  explicit exemption for the per-item groups with `alerts: []`.
- **Inventory tests.**
  - A homelab test renders both charts and fails when Gatus's endpoints and Argus's inventory differ.
  - A second test fails when a rendered PostgresCluster (from any chart under `apps/` or `platform/`)
    is missing from Argus's pgBackRest/Postgres inventory. This replaces the cross-check in
    `apps/paperless-ngx/tests/test_render.py:353-361`.
  - The existing drift guards move to Argus's config in the stage 8 commit:
    `tests/gitops/test_gitops_certificate_inventory.py` (shared-CA certificates against
    `expectedCertificates`) is re-pointed at Argus's `pki` list, and
    `platform/longhorn/tests/test_backup_inventory.py` (labelled PVCs against `expectedPVCs`) at
    Argus's Longhorn inventory.

## Repo layout (`zekihan/argus`, scaffolded from backrestwatch)

```
cmd/argus/main.go            serve | once | state validate
internal/config              strict JSON, per-check sections  (from backrestwatch/internal/config)
internal/kube                paginated list[T], get, ConfigMap get/update, loopback proxy origin (from internal/kube/client.go)
internal/state               ledger store, conditional writes, read-back convergence; file backend for tests (from internal/evidence/store.go)
internal/runner              per-check interval, collection timeout, max sample age, first-seen tracking, fail-closed (from internal/monitor/monitor.go)
internal/server              routes, envelope, bearer auth, TLS file reload (from internal/service/{server,auth}.go)
internal/result              envelope, ok/n/a/reason codes, allowlisted parameters, aggregate builder
internal/checks/pgbackrest   evaluate.go, repository.go, unchanged semantics and tests (from internal/monitor/)
internal/checks/postgres     PostgresCluster instances, PgBouncer, conditions
internal/checks/cronjob      generic CronJob freshness and run history (from freshness.py)
internal/checks/certificates per-certificate renewals; pki and pki-lifecycle profiles (from certificate_monitor.py + check_postgres_certificates.py)
internal/checks/externalsecrets  ExternalSecrets, ClusterExternalSecrets, stores (from sync_monitor.py)
internal/checks/longhorn     volumes, replicas, nodes, targets, system backups (from backup_monitor.py)
internal/checks/hosts        x/crypto/ssh with pinned known_hosts; reads the fixed probe's JSON (no script is sent)
internal/checks/nodes        node conditions, version skew, metrics-server usage, Longhorn node disks
internal/checks/argocd       Application sync, health, operation
internal/checks/workloads    (v2) per-namespace controllers, pod failure states, restart and OOM history
charts/argus                 Deployment (Recreate), Service, SA, RBAC from enabled checks, NetworkPolicy, ConfigMaps, Certificate
```

The tooling is copied from backrestwatch and renamed: `Makefile` (build/lint/test/race/integration/
container-test/chart-test), `Dockerfile` (scratch, a new dedicated UID), `.goreleaser.yaml`,
`.golangci.yaml`, `renovate.json`, `VERSION`, and `.github/workflows/{pr,release}.yaml`, which call
`zekihan/release-workflows` decide/image/release at a pinned SHA. Stable releases come from `vX.Y.Z`
tags and publish the chart. Signed tags on `v*` are required because the shared workflow checks only the version and the
branch (`scripts/decide.py`). The `terraform/git` module has only a branch ruleset today
(`repository/github/repository_ruleset.tf`), so this is a module change: a new
`github_repository_ruleset` with `target = "tag"`, `refs/tags/v*` and `required_signatures`, plus a
variable in `vars.tf`. Licensing:
- carry over backrestwatch's `NOTICE` and `LICENSES/Apache-2.0.txt`;
- extend `NOTICE` to credit the Apache-2.0 homelab Python monitors being ported. `check-host.sh` stays
  in the homelab repo as the Ansible probe.

Tests:
- backrestwatch's Go tests move with the code;
- the Python tests become Go fixtures with the same expected verdicts (`test_certificate_monitor.py`,
  `test_sync_monitor.py`, `test_backup_monitor.py`, `test_freshness.py`, `test_sync_freshness.py`);
- the hosts check gets an SSH-server fixture covering a known key, an unknown key and a changed key;
- the state and hygiene tests described above.

## RBAC and network (chart defaults for cluster-1)

The chart renders RBAC from the enabled checks; a disabled check gets no permissions.
- **ClusterRoles** (get/list), for cluster-wide discovery and for objects that carry no credentials:
  - `nodes`, metrics.k8s.io `nodes`, `persistentvolumeclaims`;
  - PVC `allowUnmounted` additionally reads storage.k8s.io `volumeattachments` to confirm detachment;
  - PVC usage adds **get only** on `nodes/stats`, restricted to `expectedNodes`. It reads
    `https://<InternalIP>:10250/stats/summary` directly using the projected token and cluster CA.
    TLS validates the node IP, redirects are refused, and kubelet failures affect only this check.
    NetworkPolicy must allow TCP 10250 to the expected node IPs. No API-server proxy is used.
  - `postgresclusters`, for inventory drift;
  - cert-manager `certificates`, `certificaterequests` (CSRs and public certificates only), `issuers`
    and `clusterissuers`; trust-manager `bundles`;
  - ESO `externalsecrets`, `clusterexternalsecrets`, `secretstores` and `clustersecretstores`.
- **Namespace Roles** (list), only where pod or controller specs would otherwise be readable
  cluster-wide:
  - `jobs` and `cronjobs` in the pgBackRest and CronJob inventory namespaces;
  - (v2) `pods`, `deployments`, `statefulsets`, `daemonsets` and `jobs` in the workload namespaces;
  - Longhorn (`volumes`, `replicas`, `nodes`, `backuptargets`, `systembackups`) and Argo CD
    (`applications`);
  - get/update on `configmaps/argus-state` in `argus`.
- Every Role namespace must be a destination in `platform/argus/argocd/project.yaml`, as reloader's
  are, and project changes need a manual root sync. A homelab test checks the two lists match. Adding
  a namespace to an inventory is therefore four steps: values, project, root sync, argus sync.
- No access to Secrets or `nodes/proxy` anywhere. The token is projected and mounted only into the
  Argus container.
- NetworkPolicy ingress: TCP 8443 from the Gatus pod only. Egress: the Kubernetes API peers
  (`10.43.0.1/32` and `192.168.2.30-32/32:6443`, as in the current `apiPeers`), DNS, and TCP 22 to
  the host list.

## Secrets and SSH

- **Two SOPS keys, so Gatus can't read the SSH key.** Gatus reads monitor tokens with
  `dataFrom.extract` of a whole key.
  - `selfhosted/argus-monitor/cluster-1` holds only `token`, read by both namespaces. Gatus uses
    `remoteRef.property: token`.
  - `selfhosted/argus/cluster-1` holds `ssh_private_key`, read only by `argus`, and is created at
    stage 7, not before.
  - `argus` is added to the SOPS store's `clusterSecretStore.namespaces` in the same commit as the
    first `argus` ExternalSecret (stage 4). `check_secret_store_namespaces.py` fails on a listed
    namespace with no consumer as well as on a missing one.
- **A new key that can run only a fixed probe.** Today the Gatus key is in `common_authorized_keys`
  for `root` and `zekihan` on every host, and Gatus logs in as root on the bastion and the k8s nodes.
  `restrict` alone would not limit what commands the key runs, and a forced `sh -s` would still
  accept any script. So:
  - **The probe lives on the hosts.** `check-host.sh` moves to an `argus_probe` Ansible role.
    - It is installed as root-owned `/usr/local/libexec/argus-probe`, mode 0755, with a `#!/bin/sh`
      shebang added (today it has none, because Gatus sends it as a body).
    - Argus sends no script; it opens a session and reads the JSON.
  - **Privileges are scoped to that one file.**
    - The role creates an `argus` user with `/bin/sh` as its shell. OpenSSH runs `command=` through
      the login shell, so `nologin` would break it.
    - It does not use `new_user`, which grants passwordless sudo.
    - It installs `sudo` and `/etc/sudoers.d` itself, because `install_basics` doesn't run on PVE/PBS.
    - A single sudoers rule, checked with `visudo -cf` as `new_user` does, allows exactly `sudo -n
      /usr/local/libexec/argus-probe` with no arguments.
    - Root is needed to `df` kubelet CSI mounts under the 0750 `/var/lib/kubelet/pods`. Under sudo's
      `secure_path`, `/usr/sbin/zpool` also becomes visible on PVE/PBS for the first time, so expect
      new ZFS results during acceptance.
  - **The key is pinned to that command.** `authorized_keys` has `restrict,command="sudo -n
    /usr/local/libexec/argus-probe"`, through a new Ansible variable, not the common key set. LAN
    hosts also get `from="<node IPs>"`.
    - The OCI hosts get no `from=`. Their SSH is open to `0.0.0.0/0`, but the home WAN IP is dynamic
      (it's why `cloudflare-ddns` exists).
    - `from=` hostname patterns depend on reverse DNS, so they can't help.
    - The forced command and `restrict` still apply.
  - **Semaphore:** the role runs from a new playbook with its own `semaphoreui_project_template` in
    `terraform/semaphore/project_template.tf`. Each template is bound to one playbook, so this needs an
    operator Terraform apply before the first run.
  - **The residual risk is stated, not hidden.** A stolen Argus key can run the probe as root on
    every host and nothing else.
  - **Host classes in `hosts.yaml`:** k8s nodes (root `df` over CSI mounts), PVE and PBS (ZFS, no
    sudo by default), the unprivileged LXC `victoriametrics` (`cpu_pressure` and `time_sync` skipped),
    the NUT host `nut` / DNS 02 (real `ups`), OCI, and the bastion. TrueNAS and Home Assistant are
    banner-only and stay that way. The disabled `Pi` is dropped from the inventory.
  - **Acceptance per host class** under the real `argus` account, before the old checks are retired:
    - CSI mounts appear in `disk`;
    - `zfs`, `failed_units` and `ups` report real values, or `unavailable`, never a silent `ok`;
    - results match the current Gatus SSH results for the same host.
- **Retiring the old Gatus key**, in this order, after cutover:
  1. A Gatus commit removes the `gatus-ssh-secret` ExternalSecret and its `envFrom`
     (`apps/gatus/templates/secrets.yaml:36-41`, `values.yaml:38-41`). Deleting the SOPS value first
     would make that ExternalSecret's template fail, and Argus's own `externalsecrets` check would
     page.
  2. Delete `GATUS_SSH_PRIVATE_KEY` from `selfhosted/gatus`. It also reaches Gatus through `envFrom`
     of the whole `gatus-secret`.
  3. Remove its public key from `common_authorized_keys` if nothing else uses it.
- **Host key pinning.** Argus verifies against a reviewed `known_hosts`, seeded from
  `apps/semaphore/files/target_known_hosts`.

## Rollout (one merge is not atomic, so stages are separate pushes)

0. **Gatus trust prerequisite** (`apps/gatus`, its own commit): move `SSL_CERT_DIR: /etc/postgres-ca`
   from `values.backrestwatch-tls.yaml` into `values.postgres-access.yaml`, next to the mount, and add
   a render test. Otherwise retiring the backrestwatch profiles breaks Gatus's TLS trust in Argus.
1. **Secrets:** create `selfhosted/argus-monitor/cluster-1` (`token`, new random value). The store
   namespace is added in stage 4.
2. **Repo:**
   - create `zekihan/argus` through `terraform/git` (repo, secrets, ruleset) and add the git-mirrorer
     mirror;
   - build and test, push, and tag `v0.1.0`;
   - the first release run is expected to fail its anonymous-pull check, because a GHCR package exists
     only after its first push. Make the image and chart packages public, then **re-run the failed
     jobs** (not all jobs), as backrestwatch's `docs/publishing.md` describes.
3. **Parity before deployment:**
   - every case from the Python tests and backrestwatch's tests becomes a Go fixture, including cases a
     healthy shadow run never hits (missing, invalid and future timestamps, missing inventory, stale
     samples, API failures);
   - `docs/parity.md` maps each old assertion to its Argus parity field;
   - cutover is blocked while any row is missing.
4. **Add the app** (`platform/argus`, `autoSync: false`):
   - membership, AppProject with every Role namespace, then a manual root sync;
   - also in this stage:
     - the `docs/bootstrap-plan.json` entry;
     - network-baseline values and project (sync by hand once the namespace exists);
     - the `tests/gitops` lists;
     - `argus` in the SOPS store's namespaces, with its first ExternalSecret;
     - Argus's `selfsigned-ca-issuer` Certificate added to cert-manager's `expectedCertificates`
       (`tests/gitops/test_gitops_certificate_inventory.py` requires exact set equality);
     - `scripts/ci/generate_files_for_apps.sh`;
   - deploy with `pgbackrest` disabled, import `evidence.json` as described under *State*, then
     enable it.
5. **Shadow run:**
   - add the Argus endpoints to Gatus with `alerts: []` to watch verdicts over time. Gatus stores only
     pass/fail and Argus is a scratch image, so the field-by-field diff uses a disposable client pod
     with a narrow NetworkPolicy allowed to both old monitors and Argus, as
     `docs/backrestwatch-migration.md`'s "disposable Gatus-policy client" did;
   - compare field by field for at least 24h, or 1.5× the longest configured max age if that's longer,
     so every pgBackRest repo records a new backup;
   - **new fields:** each is `ok`/`n/a` or every red is explained, with at least one deliberate safe
     failure per check type (cordon a node briefly, suspend a CronJob);
   - **access from the real consumer:** TLS chain and SAN verify in the Gatus pod, the token is
     accepted, a wrong token gets 401, and 8443 is denied from other pods.
6. **Cutover** (`apps/gatus`, one commit): point the six replaced configs at Argus using the parity
   conditions and the alert layout above. Then:
   - drop the six monitor egress templates and remove `values.backrestwatch-{secret,network,tls}`,
     `values.external-secrets-{network,monitor}`, `values.mailvault-sync`, `values.mailvault-sync-health`,
     `values.dmarc-freshness` and `values.longhorn-backup-monitor` from Gatus's `valueFiles`.

   Rollback is reverting this commit, since the old monitors are still running.
7. **Hosts** (separate pushes; Ansible takes effect on the next Semaphore task, not at merge):
   1. Add the `argus_probe` Ansible role (probe with the `unavailable` fix, `argus` user, sudoers rule,
      forced-command key) and run it through Semaphore.
   2. Create `selfhosted/argus/cluster-1`.
   3. Enable the hosts check in Argus and run the per-host-class acceptance next to the existing Gatus
      SSH checks.
   4. Switch `hosts.yaml` to the Argus per-host and aggregate endpoints.
   5. Retire the old Gatus key in the order described under *Secrets and SSH*, and delete
      `apps/gatus/scripts/check-host.sh`.
8. **Removal**, one commit per scope, after Gatus has been green on Argus for at least 24h:
   - Drop the monitor files, templates, values and tests from dmarc, mailvault, cert-manager,
     external-secrets and longhorn.
   - Also: the Python entry in `tests/run.sh:42`, and re-point the certificate and Longhorn inventory
     tests at Argus (see *Gatus wiring*).
   - Update `docs/{bootstrap-dependencies.md,bootstrap-plan.json,network-dependency-graph.md,backrestwatch-migration.md}`.
   - Applications sync with `Prune=false` (`gitops/foundation.yaml:60`), so merging does not delete
     anything. List each monitor's live objects (Deployment, Service, ServiceAccount, Role,
     ClusterRole and ClusterRoleBinding such as `cert-manager-certificate-monitor` and
     `external-secrets-sync-monitor`, NetworkPolicy, ConfigMap) and delete them explicitly in a
     separately approved step, as `docs/backrestwatch-migration.md:38-40` did.
9. **Retire backrestwatch** (`docs/applicationset-migration.md` §Retirement; there is no `handoff`
   field any more):
   1. Confirm the live Application has no `syncPolicy.automated` and no running operation.
   2. Remove the membership and wait for the ApplicationSet to reconcile; the Application is retained.
   3. Delete it non-cascading: `argocd --core app delete backrestwatch-cluster-1 --cascade=false`.
      Generated Applications carry `resources-finalizer.argocd.argoproj.io`, so a plain `kubectl
      delete` would cascade.
   4. In a separately approved step, delete its Deployment, Service, NetworkPolicy, ServiceAccount,
      both ConfigMaps, and ClusterRole and ClusterRoleBinding `backrestwatch-backrestwatch-*`. Keep
      the `backrestwatch-state` PVC. Until this step, rollback is re-adding the membership.
   5. In the same commit as the membership removal, drop every other reference CI checks:
      - `platform/external-secrets-sops/values.cluster-1.yaml:23` (store namespace);
      - `platform/network-baseline/values.cluster-1.yaml:6` and its project;
      - `tests/gitops/test_gitops.py:108,148` and `docs/bootstrap-plan.json`;
      - `apps/git-mirrorer/values.yaml:30-39`, reloader's values and test, and
        `apps/paperless-ngx/tests/test_render.py:353` (both reference backrestwatch, not the Python
        monitors);
      - `backrestwatch-monitor-tls` in cert-manager's `expectedCertificates`, since its Certificate
        stops rendering here.

      Then run `scripts/ci/generate_files_for_apps.sh` to drop `backrestwatch_pr.yaml` and its Renovate
      groups.
   6. After a further approval, delete the PVC and the retained Secrets and Certificate, and archive
      `zekihan/backrestwatch` through `terraform/git`.

## Verification

- **Repo:** `make audit` (gofmt, golangci-lint, `go test`, `-race`, integration),
  `make container-test`, `make chart-test`, `goreleaser check`. Also run `argus once` against
  `kubectl proxy` for every check.
- **Homelab:** for argus, gatus and every touched chart, run `scripts/ci/helm_lint_matrix.sh`,
  `helm_template_matrix.sh` and the chart's `tests/run.sh`. Then run `PYTHON=... sh tests/run.sh`,
  and the RBAC/project, inventory and ignore-rule tests above.
- **Live, per stage:** run the acceptance in stage 5 and step 7.3. Also:
  - renewing the certificate (`cmctl renew`) is picked up without a restart;
  - stopping Argus pages once (the `Argus` endpoint), not per item.
- **Nothing outward-facing without a separate go-ahead:** no push to either repo, no GitHub repo, no
  secrets and no deletions.

## Review log

Each review round lists what it raised and how the plan changed, so later rounds don't repeat it.

**Round 1 (GPT-6.1 Sol)**, all accepted:
1. ConfigMap state lost the single writer and safe recovery → *State and single writer*.
2. backrestwatch retirement skipped steps → stage 9.
3. ClusterExternalSecrets and unreferenced stores dropped → routes and RBAC added.
4. The strict PKI gate was weakened → `pki` and `pki-lifecycle` profiles.
5. No sanitisation contract → *Response and log hygiene*.
6. Cluster-wide pod and controller reads → RBAC rendered per enabled check, namespace Roles.
7. 8h shadow can't prove parity → stage 3 fixtures, stage 5 acceptance.
8. Restart/OOM history needed state → OOM from last termination; restarts best effort.

**Round 2 (Claude Opus 5.5)**, all accepted:
1. Leases are excluded from Argo CD, so the round 1 Lease would never exist → claim fencing inside the
   ConfigMap, no periodic writes. *(Superseded in round 4.)*
2. Namespace-scoped RBAC lost backrestwatch's unexpected-cluster detection → ClusterRole on
   `postgresclusters`, `inventory` results, PostgresCluster inventory test.
3. The root-capable SSH key would sit in a nightly-updated pod, and the old key stays in Gatus via
   `envFrom` → new restricted non-root key, provisioned at stage 7; old key deleted from
   `selfhosted/gatus`; no edge automerge. *(Automerge part superseded by the owner's decision after round 4.)*
4. One SOPS key would expose the SSH key to Gatus → separate `argus-monitor` (token) and `argus` (SSH)
   keys.
5. `SSL_CERT_DIR` lives only in the backrestwatch TLS profile → stage 0.
6. `handoff` no longer exists; `kubectl delete` cascades through the finalizer; RBAC leftovers →
   stage 9 rewritten.
7. `Prune=false` means template removal doesn't delete the old monitors → explicit deletion in
   stage 8.
8. A bare `.data` ignore rule would freeze the config ConfigMap; frequent writes refresh the app →
   name-scoped jq rule and test; write only on change.
9. Several new fields aren't computable or would be red today → `n/a` result, persistence windows,
   attached-only Longhorn fields, allocation and pod capacity dropped.
10. Manual apps are OutOfSync by design; there's no per-resource health → automated vs manual windows,
    app-level health only.
11. Discovered certificates in Role-less namespaces would 403; Roles need AppProject destinations →
    ClusterRole for certificaterequests, project/Role test, four-step inventory note.
12. One Argus fault would page about 80 times → per-item endpoints silent, aggregates and `Argus`
    endpoint page.
13. Verdict parity is impossible with the new fields, and there's no comparison setup → parity-field
    conditions, shadow endpoints in Gatus with `alerts: []`.
14. Missing steps for adding and removing apps and for GitHub repos → stages 2, 4, 6 and 8.
15. Import raced the empty ledger; `conflict` never cleared; the 1 MiB ConfigMap limit → import before
    enabling, adopt-on-read-back, 900 KiB cap.
16. CLI and service PKI margins differ, and the snapshot can't pin the root → two profiles.
17. `ok(observed …)` broke the `== ok` contract → plain `ok`, coverage in values.
18. Edge image vs released chart drift → released versions only. *(Superseded by the owner's decision after
    round 4: common edge pattern, with backward-compatible config and per-check RBAC failure.)*
19. Signed tags not enforced; licence notices → tag ruleset; NOTICE and attribution.

**Round 3 (GPT-6 Astra)**, all accepted:
1. Fencing only on failed writes let a stale pod keep serving `ok`; read-back compared ledger before
   owner → owner checked on every collection, owner-first read-back, two-client test. *(Superseded in
   round 4.)*
2. Paging aggregates had one verdict per item, so parity conditions couldn't be expressed →
   aggregates report per dimension plus `collection` and `inventory`; exact Gatus conditions and test.
3. The generic `docker-image-digests` rule would still auto-merge release digests → final Argus
   package rule with `automerge: false` for every update type, plus a policy test. *(Superseded by your
   decision after round 4.)*
4. Collection aggregates would all page during an Argus outage, and `/healthz` misses 401 → paging
   aggregates `dependsOn` an authenticated `argus` gate; suite cost measured.
5. Attached-only robustness hid detached faulted volumes → `degraded`/`faulted` always fail; only
   `unknown` while detached is `n/a`.
6. Non-root `df` can't see CSI mounts; a failed `zpool` stays `ok` → privileged fixed probe,
   `unavailable` result, per-host-class acceptance, no `new_user` role.
7. `restrict` doesn't limit commands; `sh -s` allows any script → probe installed on hosts, key
   forced to `sudo -n` that one file, residual risk stated.
8. Client-side apply's 256 KiB annotation limit → 200 KiB ledger cap, `serverSideApply: true`, a K3s
   integration test.
9. `n/a` for missing conditions would pass a node without `Ready` → condition-specific rules.

**Round 4 (Claude Fable 5.1)**, all accepted:
1. Stage order broke CI inventory tests: store namespaces need a consumer; the shared-CA certificate
   set must match exactly; the drift guards would be lost → namespace and certificate moved to stage
   4, `backrestwatch-monitor-tls` removed at stage 9, both inventory tests re-pointed at Argus in
   stage 8.
2. `unavailable` for a missing tool would fail most hosts → missing tool is `n/a`, failing tool is
   `unavailable`, per-host `expects`.
3. An API server, token or RBAC failure still paged every aggregate → `/v1/checks` reports `apiserver`
   and the gate conditions on it.
4. Deleting the SOPS value first breaks `gatus-ssh-secret` → ordered retirement of the old key.
5. `from=` with the dynamic WAN IP would break the OCI checks → no `from=` for OCI hosts; reverse-DNS
   patterns ruled out.
6. Probe role details: login shell, sudo installation, `visudo`, shebang and mode, Semaphore template
   → *Secrets and SSH*.
7. The import was underspecified → disposable read-only pod, v1 and v2 ledgers, verbatim
   repository identity with a real-ledger test.
8. Over-engineering → workloads deferred to v2; owner fencing replaced by conditional writes that
   converge by re-evaluation (supersedes round 2.1 and round 3.1).
9. Stage cross-references were wrong (reloader and paperless belong to stage 9; missing stage 9 CI
   references; verification stage numbers; Gatus alert test) → fixed.
10. No tag ruleset exists in the `terraform/git` module → module change described.
11. Argo's compare-then-apply can roll the ledger back one step → covered by convergence; stated.
12. Cutover changed intervals and thresholds → timing is part of parity.
13. GHCR packages can't be made public before their first push → expected first failure, then re-run
    the failed jobs.
14. Wrong host classes (no TrueNAS/FreeBSD target; Pi disabled) → real class list.
15. Renovate rule is not last in the generated file → wording and a test on the generated file.
    *(Moot: no Argus-specific Renovate rule any more.)*
16. Gatus can't diff old against new field by field → disposable client pod for the comparison.

**After round 4 (the owner's decision):** releases use the common owned-app pattern: `edge@sha256` images on
a released chart, auto-merged by the shared owned-edge Renovate rule. The Argus-only Renovate override
and its test are dropped. In their place, config stays backward compatible (an edge-against-released-chart
check in Argus CI), a missing permission fails only that check, and the SSH risk rests on the forced
command.

**OPS-420 (owner decision, 2026-11-12):** add PVC fill monitoring at 80% for pgdata, Redis and all Longhorn PVCs. This supersedes the earlier omission: direct kubelet stats uses [nodes/stats](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-authn-authz/), preserving the prohibition on nodes/proxy. The check is optional and disabled by default. An unlisted detached PVC without fresh statistics is unavailable. Explicit `allowUnmounted` claims report `n/a` only when a successful VolumeAttachment inventory shows no attachment to their bound PV. A newly created attachment gets a first-sample grace period bounded by `maxStatsAgeSeconds` and its API creation timestamp, including at response time. Older attachments require statistics; malformed or future timestamps fail. Short scheduled jobs may finish before kubelet measures them, so `n/a` is unmeasured, not proof of low usage. Fresh high, invalid and future readings never get the grace exception. Multiple mounts use the highest fresh usage. Missing measurements reuse the last valid measurement for the same PVC UID only until its original timestamp expires (300 seconds by default); high retained readings still fail. Raw-block usage is `n/a` because no filesystem exists. API errors and missing expected nodes fail closed; an empty PVC inventory cannot pass.

**OPS-462 (2026-11-12):** Gatus judged DNS over HTTPS by HTTP status alone, so an HTTP 200 carrying SERVFAIL stayed green. Gatus cannot decode a binary DNS body, so Argus owns the verdict and Gatus reads `results.answer`. The check uses no Kubernetes API and needs only egress to the resolver and the HTTPS destination.
