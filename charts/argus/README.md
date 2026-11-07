# argus chart

Installs `oci://ghcr.io/zekihan/charts/argus`: one Argus pod (`Recreate`, a
single replica), its Service, ServiceAccount, RBAC rendered from the enabled
checks, an optional NetworkPolicy and cert-manager Certificate, the config
ConfigMap and the `argus-state` ledger ConfigMap.

Every check is disabled by default and receives permissions only when enabled.
`ci/default-values.yaml` shows the required values; `ci/all-checks-values.yaml`
enables every check with an illustrative inventory.

```yaml
accessControl:
  existingSecret: argus-monitor   # key "token": the API bearer token
tls:
  certificate:
    create: true
    issuerRef: {name: selfsigned-ca-issuer}
checks:
  pgbackrest:
    enabled: true
    clusters:
      - {namespace: database, name: database, repos: {repo1: {}, repo2: {maxAgeSeconds: 36000}}}
networkPolicy:
  enabled: true
  apiPeers: [10.43.0.1/32]
  ingressFrom:
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: gatus}}
      podSelector: {matchLabels: {app.kubernetes.io/name: gatus}}
```

| values | default / behaviour |
| --- | --- |
| `image.repository`, `tag`, `digest` | GHCR; tag defaults to appVersion; digest takes precedence |
| `accessControl.existingSecret`, `secretKey` | required; the bearer token required on every route except `/healthz`, re-read on every request |
| `tls.enabled`, `existingSecret` | native TLS on 8443 from a Secret with `tls.crt`/`tls.key`, re-read when it changes |
| `tls.certificate.create`, `issuerRef`, `dnsNames` | optional cert-manager Certificate `<fullname>-tls`; DNS names default to the Service names |
| `ssh.existingSecret`, `secretKey`, `knownHosts` | the hosts check's private key and reviewed known_hosts; required only with `checks.hosts` |
| `state.configMapName` | `argus-state`, the pgBackRest ledger |
| `config.*` | global poll interval, sample lifetime, request and collection timeouts |
| `checks.<name>.enabled` | false; every other key is the check's configuration section |
| `serviceAccount.*`, `rbac.create` | dedicated account; RBAC from enabled checks |
| `networkPolicy.*` | ingress TCP 8443 from `ingressFrom`; egress to `apiPeers`, DNS, and TCP to each host (IPv4 addresses are derived, others need `sshPeers`) |
| `podSecurityContext`, `securityContext` | non-root UID/GID 25120, RuntimeDefault seccomp, read-only root, no capabilities |

## RBAC

| Check | Permission |
| --- | --- |
| nodes | ClusterRole get/list `nodes`, metrics.k8s.io `nodes`; Role list `nodes.longhorn.io` in `longhornNamespace` |
| postgres, pgbackrest | ClusterRole get/list `postgresclusters` (inventory drift) |
| pgbackrest | Role list `jobs`, `cronjobs` in each cluster namespace; get/update `configmaps/argus-state` only |
| cronjobs | Role list `jobs`, `cronjobs` in each inventory namespace |
| longhorn | Role list `volumes`, `replicas`, `backuptargets`, `systembackups` in the Longhorn namespace |
| certificates | ClusterRole get/list `certificates`, `certificaterequests`, `issuers`, `clusterissuers` |
| pki | ClusterRole get/list `certificates`, `certificaterequests`, and `bundles` when bundles are expected |
| externalsecrets | ClusterRole get/list ExternalSecrets, ClusterExternalSecrets, SecretStores, ClusterSecretStores |
| argocd | Role list `applications` in the Argo CD namespace |
| hosts | none |

No rule grants Secrets, pods or `nodes/proxy`. The service-account token is
projected only into the Argus container. Every Role namespace must be a
destination of the Argo CD project that deploys the chart.

## The state ConfigMap under Argo CD

`argus-state` is created once with an empty ledger and carries
`helm.sh/resource-policy: keep` and `argocd.argoproj.io/sync-options:
Prune=false,Delete=false`. Argus then owns its data, so the Application must
ignore it by name, and apply server-side so a 200 KiB ledger fits:

```yaml
ignoreDifferences:
  - kind: ConfigMap
    jqPathExpressions: ['select(.metadata.name == "argus-state") | .data']
syncPolicy:
  syncOptions: [ServerSideApply=true, RespectIgnoreDifferences=true]
```

A bare `.data` rule would also freeze the config ConfigMap.

## Compatibility

An `edge` image runs against the latest released chart. Every configuration
field added after a release is optional, and Argus CI renders each released
chart's `ci/*-values.yaml` and validates the result with the new binary. A check
whose permissions the released chart does not grant reports
`collection: forbidden` on its own routes; the process keeps serving.
