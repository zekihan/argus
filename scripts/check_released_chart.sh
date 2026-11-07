#!/bin/sh
# Render the latest released chart and validate its configuration with this build.
# Edge images run against the released chart, so every config it renders must load.
set -eu
cd "$(dirname "$0")/.."
binary=$(cd "$(dirname "${1:?argus binary required}")" && pwd)/$(basename "$1")
HELM=${HELM:-helm}
PYTHON=${PYTHON:-python3}
version=$(git tag --list 'v*' | sed -n 's/^v\(\(0\|[1-9][0-9]*\)\.\(0\|[1-9][0-9]*\)\.\(0\|[1-9][0-9]*\)\)$/\1/p' | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)
if [ -z "$version" ]; then
  echo 'No stable release tag yet; no released chart to check'
  exit 0
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
# Anonymous pull: the released chart must be publicly readable anyway.
HELM_REGISTRY_CONFIG="$work/registry.json" "$HELM" pull oci://ghcr.io/zekihan/charts/argus --version "$version" --destination "$work"
tar -xzf "$work/argus-$version.tgz" -C "$work"
checked=0
for values in none "$work"/argus/ci/*-values.yaml; do
  set -- template argus "$work/argus" -n argus --set accessControl.existingSecret=released-chart-check
  if [ "$values" != none ]; then
    [ -f "$values" ] || continue
    set -- "$@" -f "$values"
  fi
  "$HELM" "$@" > "$work/rendered.yaml"
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
printf 'Released chart %s: %d rendered configurations accepted\n' "$version" "$checked"
