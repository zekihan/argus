# Parity

Stage 3 of [design.md](design.md): every assertion of the monitors Argus
replaces maps to an Argus parity field and a Go fixture with the same expected
verdict. Cutover is blocked while any row is missing.

A **parity condition** is what Gatus conditions a replaced aggregate on at
cutover: its parity dimensions plus `collection` and `inventory`, each `== ok`.
New detail dimensions are excluded until promoted. Reason strings differ from
the Python ones; only verdicts are compared. Paths without a repository prefix
are in zekihan/homelab at the revision recorded in [NOTICE](../NOTICE).

| Replaced endpoint | Argus route(s) | Parity condition |
| --- | --- | --- |
| backrestwatch `/backups` | `/v1/pgbackrest` | `freshness`, `evidence`, `destination`, `state`, `collection`, `inventory` |
| `certificate_monitor.py` `/certificates` | `/v1/pki` | every result (`ready`, `readiness_generation`, `expiry`, `ca_key_pinned`, `issuer_lifetime`, `ca_renewal`, `issuer_present`, `inventory`, `root_pin`, `bundles`, `collection`) |
| `certificate_monitor.py` `/renewals` | `/v1/certificates` | `ready`, `renewal`, `expiry`, `collection`, `inventory` |
| `check_postgres_certificates.py` | `argus once -check pki-lifecycle` | exit status |
| `sync_monitor.py` `/externalsecrets` | `/v1/externalsecrets`, `/v1/clusterexternalsecrets`, `/v1/secretstores`, `/v1/clustersecretstores` | `ready`, `synced`, `refresh` (ExternalSecrets); `ready` (others); `collection`, `inventory` |
| `backup_monitor.py` `/backups` | `/v1/longhorn/volumes`, `/v1/longhorn/system` | `volume`, `backup`, `robustness`; `backup_targets`, `system_backup`; `collection`, `inventory` |
| dmarc `freshness.py` `/freshness`, mailvault `sync-freshness.py` `/sync` | `/v1/cronjobs` | `schedule`, `last_success`, `collection`, `inventory` |
| Gatus SSH `check-host.sh` | `/v1/hosts` | `ssh`, `script` and every probe field |

Configuration needed for parity: `checks.certificates.excludeIssuers:
[selfsigned-ca-issuer]` (the chart default), each CronJob's `maxAgeSeconds`
(dmarc 10800, mailvault 25200), Longhorn `maxBackupAgeSeconds`,
`systemBackupMaxAgeSeconds` and `systemBackupGraceSeconds` as in
`platform/longhorn` values, and the pki `rootSHA256`, `rootSPKI`,
`expectedCertificates` and `expectedBundles` from `platform/cert-manager` values.

## backrestwatch

backrestwatch's Go tests moved with the code. `Evaluate` and
`repositoryIdentity` are unchanged; the ported tests keep their assertions.

| backrestwatch test | Argus test (`internal/checks/pgbackrest`) |
| --- | --- |
| `evaluate_test.go`, `repository_test.go` (all) | same files, ported mechanically: sources, retained success, freshness overrides, inventory, removal barriers, ledger bound, owners, timestamps, destination identity, clock rollback |
| `monitor_test.go` restart recovery and failed collection | `TestRestartRecoveryAndFailedCollection` |
| `monitor_test.go` cached age and sample expiry | `TestCachedAgeAndSampleExpire` (response-time aging through `Refine`) |
| `monitor_test.go` persistence failure | `TestPersistenceFailureDoesNotPublishSuccess` |
| `monitor_test.go` concurrent refresh and reports | `TestConcurrentRefreshAndReports` (run under `-race`) |
| `monitor_test.go` clock rollback of cached completion | `TestCachedCompletionFailsClosedDuringClockRollback` |
| `monitor_test.go` report snapshot and timestamp ownership | envelopes are rebuilt per response from immutable values (`internal/result` `TestRefineRunsAtResponseTime`) |
| `persistence_test.go` invalidations survive failed checkpoints | `TestInvalidationsSurviveFailedCheckpoints` |
| `persistence_test.go` only committed success preserved | `TestFailedCheckpointPreservesOnlyCommittedSuccess` |
| `compatibility_test.go` existing sources after restart | `TestExistingCheckpointSourcesRemainUsableAfterRestart` |
| `store_test.go` corrupt and legacy checkpoints | `internal/state` `TestCorruptMissingAndOversizedStateFailClosed`, `TestLegacyAndIdentityOnlyCheckpoints` |
| `client_test.go` pagination, retries, budgets, redaction | `internal/kube` `client_test.go` |
| `auth_test.go`, `server_test.go` | `internal/server` `TestAuthorizationOnEveryRouteButHealthz`, `TestServeBoundsWaitForBlockedPoller` |
| `tests/integration/process_test.go` | `tests/integration` `TestServeEndToEnd` |
| `maintenance_test.go`, `tests/integration/maintenance_test.go` | not ported: they exercise the flocked PVC checkpoint and its offline retirement runbook, which Argus does not have |

New for the ConfigMap ledger: `TestConflictAdoptsStoredLedgerAndRewrites`
(409), `TestTimeoutAfterCommitConvergesWithoutRewrite` (timeout then
read-back), `TestRolledBackLedgerIsRederivedOnNextWrite` (ledger changed
underneath), `TestCorruptUnknownMissingAndOversizedStateFailClosed` (corrupt,
unknown version, oversized, missing), `TestLedgerOverSizeLimitIsNotWritten`, the same cases through the real
ConfigMap backend against a fake API server (`TestConfigMapBackendConvergence`,
`TestConfigMapBackendRejectsInvalidDocuments`), and
`TestRealBackrestwatchLedgerSurvivesImport`: a ledger written by
backrestwatch's own `Evaluate` and evidence store re-evaluates with every entry
unchanged (no new `NotBefore`, no lost success). Re-run that test with the real
`evidence.json` exported at stage 4 before enabling the check.

## certificate_monitor.py

`platform/cert-manager/tests/test_certificate_monitor.py` →
`internal/checks/certificates/certificates_test.go`.

| Python test | Expected | Argus test / field |
| --- | --- | --- |
| `test_expected_root_and_current_public_request_pass` | 200 | `TestExpectedRootAndCurrentPublicRequestPass` |
| `test_changed_root_is_not_silently_trusted` | 503 | `TestChangedRootIsNotSilentlyTrusted`, `root_pin` |
| `test_same_key_renewal_is_accepted_only_with_the_pinned_public_key` | 503 / 200 / 503 | `TestSameKeyRenewalIsAcceptedOnlyWithThePinnedPublicKey`, `root_pin` |
| `test_expected_bundles_must_be_synced_at_their_generation` | missing / not synced / stale generation → 503 | `TestExpectedBundlesMustBeSyncedAtTheirGeneration`, `bundles` |
| `test_refresh_lists_bundles_only_when_expected` | bundles listed only when expected | `TestBundlesAreListedOnlyWhenExpected`; RBAC for bundles only when expected (`tests/helm`) |
| `test_wrong_owner_revision_missing_or_ambiguous_request_fails` | 503 | `TestWrongOwnerRevisionMissingOrAmbiguousRequestFails`, `root_pin` |
| `test_inventory_and_lifetime_and_key_pin_fail_closed` | 503 | `TestInventoryAndLifetimeAndKeyPinFailClosed`: `issuer_present`, `inventory`, `issuer_lifetime`, `ca_key_pinned`, `readiness_generation` |
| `test_api_error_does_not_expose_response_or_keep_cached_green` | 503, no body, stale → 503 | `TestAPIErrorDoesNotExposeResponseOrKeepCachedGreen`, `collection`; staleness in `internal/runner` |
| `test_renewal_endpoint` | not ready > 1h, renewal overdue > 24h, expiry < 14d → 503; within grace or CA issuer → 200 | `TestRenewalEndpoint`: `ready`, `renewal`, `expiry` |
| `test_invalid_renewal_data_does_not_affect_postgres_report` | renewals 503, certificates 200 | `TestInvalidRenewalDataFailsOnlyThatCertificate` |
| `test_postgres_evaluation_failure_preserves_renewal_report` | independent reports | separate checks and routes (`certificates`, `pki`) |
| `test_monitor_is_opt_in_with_no_secret_permissions` | opt-in, get/list only, no Secrets | `tests/helm` `test_default_has_no_checks_and_no_permissions`, `test_rbac_matches_the_design` |

## check_postgres_certificates.py

`tests/gitops/test_gitops_postgres_certificates.py` →
`TestLifecycleProfile` (and `tests/integration` `TestPKILifecycleSnapshot` for
the CLI exit codes).

| Python test | Expected | Argus field |
| --- | --- | --- |
| `test_unpinned_root_fails_before_automatic_key_rotation` | problem | `ca_key_pinned: not_pinned` |
| `test_ready_must_be_true_at_current_generation` | problem | `readiness_generation: stale(generation 2)` |
| `test_another_ca_cannot_hide_missing_root` | unhealthy | `issuer_present: issuer_certificate_missing` |
| `test_timestamp_without_timezone_is_rejected` | error (exit 2) | `expiry: invalid_timestamp`, unhealthy (exit 1) |
| `test_ready_ca_with_headroom_passes` | healthy | healthy |
| `test_short_ca_blocks_long_leaf_issuance` | problem | `issuer_lifetime: insufficient(…)` |
| `test_renewal_gate_precedes_expiry` | problem | `ca_renewal: due(…)` |
| `test_missing_status_and_ca_fail_closed` | unhealthy | empty snapshot → `issuer_present`; empty status → `expiry: unknown` |
| `test_unrelated_acme_certificate_is_not_a_database_failure` | healthy | healthy |
| `main()` margins | CLI 30 days, service 90 days | `pki-lifecycle` uses 30, `pki` uses `marginDays` (90); both in `TestLifecycleProfile` |

## sync_monitor.py

`platform/external-secrets/tests/test_sync_monitor.py` →
`internal/checks/externalsecrets/externalsecrets_test.go`.

| Python test | Expected | Argus test / field |
| --- | --- | --- |
| `test_healthy` | 200 | `TestHealthy` |
| `test_stores_not_ready` | 503 for each store kind and ClusterExternalSecrets | `TestStoresNotReady`, `ready` |
| `test_secret_sync_error_never_exposes_provider_message_or_spec` | 503, fixed reason | `TestSecretSyncErrorNeverExposesProviderMessageOrSpec`, `ready: not_ready` |
| `test_old_generation`, `test_old_condition_generation`, `test_missing_generation_evidence` | 503 | `TestGenerationEvidence`, `synced` |
| `test_refresh_boundary` | 7800 s → 200, 7801 s → 503 | `TestRefreshBoundary`, `refresh` |
| `test_zero_interval_does_not_require_refresh_time` | 200 | `TestZeroIntervalDoesNotRequireRefreshTime`, `refresh: n/a` |
| `test_onchange_nonzero_interval_uses_explicit_freshness_contract` | 503 / 200 / 503 | `TestOnChangeNonzeroIntervalUsesExplicitFreshnessContract` |
| `test_missing_or_invalid_refresh` | 503 | `TestMissingOrInvalidRefresh`, `refresh: invalid_refresh_time` |
| `test_durations` | parse / reject | `TestDurations` |
| `test_stale_and_uninitialized_samples` | 503 | `internal/runner` `TestPendingThenPublishedThenStale` |
| `test_failed_sample_cannot_reuse_healthy_state` | 503, no leak | `TestFailedSampleCannotReuseHealthyState` |
| `test_http_endpoints` | healthz 200, report 503 before a sample | `internal/server`, `internal/runner` |
| `PaginationTests` | continuation, token re-read, size and page budgets | `internal/kube` `TestPaginationAndTokenRotation`, `TestPageBudgetAndChangedSnapshot`, `TestOversizedPageIsNotRetried` |

## backup_monitor.py

`platform/longhorn/tests/test_backup_monitor.py` →
`internal/checks/longhorn/longhorn_test.go`.

| Python test | Expected | Argus test / field |
| --- | --- | --- |
| `test_healthy` | 200, summary ok | `TestHealthy` |
| `test_bad_backup_dates` | 503 | `TestBadBackupDates`, `backup` |
| `test_backup_problems_explain_cause` | no backup / invalid / stale / future | `TestBackupProblemsExplainCause`: `no_backup(created 1h30m)`, `backup_timestamp_invalid`, `backup_stale(last 2d0h, max 1d6h)`, `backup_in_future(1h0m)` |
| `test_missing_inventory_volume` | 503 | `TestMissingHistoricalAmbiguousAndUnexpectedVolumes`, `volume: missing`, `inventory` |
| `test_historical_or_ambiguous_volume` | 503 | same, `volume: ambiguous(…)`, `inventory` |
| `test_unexpected_backup_group_volume` | 503 | same, `inventory: unexpected(…)` |
| `test_unavailable_target` | false / null / "true" / none → 503 | `TestUnavailableTarget`, `backup_targets` |
| `test_old_failed_or_missing_system_backup` | 503 | `TestOldFailedOrMissingSystemBackup`, `system_backup` |
| `test_newest_system_backup_cannot_hide_behind_ready_older_one` | 503 | `TestNewestSystemBackupCannotHideBehindReadyOlderOne` |
| `test_degraded_and_faulted_volume` | 503 | `TestDegradedAndFaultedVolume`, `robustness` (attached or detached) |
| `test_stale_and_initial_sample` | 503 | `internal/runner` `TestPendingThenPublishedThenStale` |
| `test_failed_refresh_never_exposes_credentials`, `test_failed_refresh_reports_http_status_only` | 503, no leak | `TestFailedRefreshNeverExposesCredentials`, `collection: forbidden` |
| `test_refresh_reads_only_namespaced_resources` | namespaced paths | `TestRefreshReadsOnlyNamespacedResources` |
| `ListTests` | pagination, token re-read, budgets | `internal/kube` |

## freshness.py and sync-freshness.py

`apps/dmarc/tests/test_freshness.py` and
`apps/mailvault/tests/test_sync_freshness.py` →
`internal/checks/cronjobs/cronjobs_test.go` (`TestDmarcFreshnessParity`,
`TestMailvaultSyncParity`).

| Python case | Expected | Argus field |
| --- | --- | --- |
| age 0, age = max | 200 | `last_success: ok` |
| age = max + 1 | 503 | `last_success: stale(…)` |
| age −1 (future) | 503 | `last_success: future_timestamp(…)` |
| no status | 503 | `last_success: no_success` |
| only `lastScheduleTime` | 503 | `last_success: no_success` |
| `lastSuccessfulTime: invalid` | 503 | `last_success: invalid_timestamp` |
| naive timestamp | 503 | `last_success: invalid_timestamp` |
| `suspend: true` | 503 | `schedule: suspended` |
| API failure, no error text | 503 | `collection`, no leak |
| render tests (named CronJob `get` RBAC) | narrow RBAC | namespace Role `list` on `jobs` and `cronjobs` (`tests/helm`) |

Gatus test from the design: for two CronJobs where only one is stale the
aggregate's parity condition fails; for two where only one fails `last_run`
it holds (`TestRunHistoryDetailDoesNotAffectParity`).

## check-host.sh

The probe stays in homelab as the `argus_probe` Ansible role; Argus reads its
fixed JSON (`apps/gatus/tests/test_ssh.py`'s eleven fields).
`internal/checks/hosts/hosts_test.go`:

| Contract / case | Argus |
| --- | --- |
| Gatus `[CONNECTED] == true` | `ssh`: `ok`, `unreachable`, `timeout`, `auth_failed`, `handshake_failed`, `session_failed` |
| Gatus `[STATUS] == 0` | the probe exits 1 when a field is unhealthy, so exit 0 and 1 both report `script: ok` and the fields decide; any other exit is `script: exit_N` |
| `[BODY].<field> == ok` for the eleven fields | the same field names as dimensions; values pass through capped at 200 characters |
| `test_healthy` | all `ok` (`TestKnownUnknownAndChangedHostKeys`) |
| `test_failures_include_values_and_thresholds` | failing text passes through (`TestProbeExitOutputAndTimeout`) |
| `test_required_commands_unavailable` | `unavailable` fails |
| optional tools (today a silent `ok`) | the updated probe reports `n/a` for a missing tool and `unavailable` for a failing one; `n/a` fails for a dimension the host `expects` (`TestExpectedDimensionsTurnMissingToolsUnavailable`) |
| new: pinned host key | `host_key_unknown`, `host_key_mismatch` (`TestKnownUnknownAndChangedHostKeys`) |
| new: no script is sent | the fixture asserts a single `shell` request with no payload |
| new: stderr and output hygiene | `TestCollectionThroughRunnerLeaksNothing` |

## Response and log hygiene

Each check's tests plant credential-shaped strings in condition messages,
error bodies and spec fields and assert, through `checktest.NoLeak`, that none
appears in any route or log line. `tests/integration` `TestServeEndToEnd` does
the same for the real binary, including pod environment values in Job specs
and 403/503 response bodies.
