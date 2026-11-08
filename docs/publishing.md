# Publishing

Ordinary pull requests run native checks and builds. Container smoke checks and
release cross-compilation run only before publication, on the captured source commit.

The Publish workflow runs nightly or manually on `main` to update multi-architecture
`edge` in GHCR and Docker Hub. Pushes to `main` do not publish. Unchanged successful
inputs are skipped. After a failure, fix the source or explicitly rerun the failed
jobs; the same failed revision is not retried automatically. Manual force rebuilds
unchanged edge inputs and never replaces a stable version.

Stable releases start from an existing signed `vX.Y.Z` tag whose commit belongs to
main and whose version matches `VERSION`. Prepare the version locally, update any
version adapters, run checks, commit and sign the tag. Push only when ready:

```sh
git push --atomic origin main vX.Y.Z
```

Keep `charts/argus/Chart.yaml` version and appVersion equal to `VERSION`.
Charts publish to OCI only with stable releases and default to that release image.
A chart change needs a new release; homelab may override the image with `edge@sha256`.

A tag builds fresh artifacts, publishes immutable `X.Y.Z` images to both registries,
verifies them, moves `latest` forward and creates the GitHub release last. An identical
existing version is a no-op; different content fails. Fix a bad release with a new
patch version. Old published tags remain available.

Publishing uses full-SHA pins to `zekihan/release-workflows`. Review pin updates;
the shared workflow embeds its helpers rather than fetching executable scripts from
a moving branch. Repo-specific build commands run without publication credentials.

Both registries must be configured before first publication. Image packages and
release charts must permit anonymous pulls; publication verifies that access.

Registry updates are not atomic. If publication stops partway, use **Re-run failed
jobs** within seven days to reuse the prepared artifacts. Re-running every job can
produce different bytes and correctly fail the immutability check. Never retag or
overwrite an existing release to recover. After artifact expiry, use a new version
if the original content cannot be recovered.

The first release is expected to fail its anonymous-pull verification, because a
GHCR package exists only after its first push. Make the `argus` image and chart
packages public, then use **Re-run failed jobs** (not all jobs). After the release completes, run
`scripts/verify_release.sh X.Y.Z` from a clean checkout: it checks anonymously that both registries
serve the amd64 and arm64 images and that the published chart renders.

Edge images run against the latest released chart. The PR checks render each
released chart's `ci/*-values.yaml` and validate the configuration with the new
binary (`make chart-released-check`); before the first stable tag this is a no-op.
