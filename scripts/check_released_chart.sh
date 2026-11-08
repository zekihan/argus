#!/bin/sh
# Render the latest released chart and validate its configuration with this build.
# Edge images run against the released chart, so every config it renders must load.
set -eu
cd "$(dirname "$0")/.."
binary=$(cd "$(dirname "${1:?argus binary required}")" && pwd)/$(basename "$1")
HELM=${HELM:-helm}
PYTHON=${PYTHON:-python3}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
if [ -n "${RELEASED_CHART_ARCHIVE:-}" ]; then
  # Local verification of this script against a packaged chart.
  version=local
  tar -xzf "$RELEASED_CHART_ARCHIVE" -C "$work"
else
  # A tag run checks the release before its chart is published, so the tag
  # being released is never the released chart. TAGS overrides git for tests.
  current=''
  case "${GITHUB_REF:-}" in
    refs/tags/v*) current=${GITHUB_REF#refs/tags/v} ;;
  esac
  tags=${TAGS-$(git tag --list 'v*')}
  version=$(printf '%s\n' "$tags" | sed -n 's/^v\(\(0\|[1-9][0-9]*\)\.\(0\|[1-9][0-9]*\)\.\(0\|[1-9][0-9]*\)\)$/\1/p' |
    grep -vxF "${current:-no-current-release}" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1 || true)
  if [ -n "${SELECT_ONLY:-}" ]; then
    printf '%s\n' "$version"
    exit 0
  fi
  if [ -z "$version" ]; then
    echo 'No published stable release yet; no released chart to check'
    exit 0
  fi
  # Anonymous pull: the released chart must be publicly readable anyway.
  HELM_REGISTRY_CONFIG="$work/registry.json" "$HELM" pull oci://ghcr.io/zekihan/charts/argus --version "$version" --destination "$work"
  tar -xzf "$work/argus-$version.tgz" -C "$work"
fi
checked=0
# The chart's own ci/*-values.yaml files supply every value it requires.
for values in "$work"/argus/ci/*-values.yaml; do
  [ -f "$values" ] || continue
  "$HELM" template argus "$work/argus" -n argus -f "$values" > "$work/rendered.yaml"
  "$PYTHON" - "$work/rendered.yaml" "$work/config.json" <<'PYTHON'
import sys
import yaml
documents = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
configs = [d["data"]["config.json"] for d in documents if d.get("kind") == "ConfigMap" and "config.json" in (d.get("data") or {})]
if len(configs) != 1:
    sys.exit("expected exactly one rendered config.json")
open(sys.argv[2], "w").write(configs[0])
PYTHON
  "$binary" serve -config "$work/config.json" -check-config
  checked=$((checked + 1))
done
if [ "$checked" -eq 0 ]; then
  echo "Released chart $version has no ci/*-values.yaml to render" >&2
  exit 1
fi
printf 'Released chart %s: %d rendered configurations accepted\n' "$version" "$checked"
