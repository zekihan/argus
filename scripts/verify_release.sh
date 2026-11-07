#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=${1:?release version required}
test "$version" = "$(./scripts/release_version.sh)"
mkdir -p dist/verified
# Run with fresh credentials stores to verify public access, even on an authenticated workstation.
verify_dir=$(mktemp -d)
trap 'rm -rf "$verify_dir"' EXIT HUP INT TERM
# Preserve only plugin discovery paths; do not copy registry credentials.
python3 - "$verify_dir/docker" <<'PYTHON'
import json, os, pathlib, sys
original = pathlib.Path(os.environ.get('DOCKER_CONFIG', pathlib.Path.home() / '.docker'))
try:
    config = json.loads((original / 'config.json').read_text())
except FileNotFoundError:
    config = {}
destination = pathlib.Path(sys.argv[1])
destination.mkdir()
plugins = [str(original / 'cli-plugins'), *config.get('cliPluginsExtraDirs', [])]
(destination / 'config.json').write_text(json.dumps({'cliPluginsExtraDirs': plugins}))
PYTHON
for image in docker.io/zekihan/argus ghcr.io/zekihan/argus; do
  DOCKER_CONFIG="$verify_dir/docker" docker buildx imagetools inspect "$image:$version" --raw > "$verify_dir/manifest.json"
  python3 - "$verify_dir/manifest.json" <<'PY'
import json, sys
manifest = json.load(open(sys.argv[1]))
platforms = {(m.get('platform', {}).get('os'), m.get('platform', {}).get('architecture')) for m in manifest['manifests']}
assert {('linux', 'amd64'), ('linux', 'arm64')} <= platforms, platforms
PY
  DOCKER_CONFIG="$verify_dir/docker" docker buildx imagetools inspect "$image:$version"
done
HELM_REGISTRY_CONFIG="$verify_dir/registry.json" helm pull oci://ghcr.io/zekihan/charts/argus --version "$version" --destination dist/verified
helm show chart "dist/verified/argus-$version.tgz" > "$verify_dir/chart.yaml"
mkdir -p "$verify_dir/chart"
tar -xzf "dist/verified/argus-$version.tgz" -C "$verify_dir/chart"
test "$(sed -n 's/^version: //p' "$verify_dir/chart.yaml")" = "$version"
# Helm normalizes appVersion quoting when displaying metadata.
app_version=$(sed -n 's/^appVersion: //p' "$verify_dir/chart.yaml" | tr -d '"')
test "$app_version" = "$version"
helm template verify "dist/verified/argus-$version.tgz" -n argus -f "$verify_dir/chart/argus/ci/default-values.yaml" > "$verify_dir/rendered.yaml"
test -s "$verify_dir/rendered.yaml"
printf 'Verified both image platforms and OCI chart %s\n' "$version"
