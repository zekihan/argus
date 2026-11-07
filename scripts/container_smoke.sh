#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(./scripts/release_version.sh)
image="argus-smoke:$version"
docker build --build-arg "VERSION=$version" -t "$image" .
test "$(docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges "$image" -version)" = "$version"
test "$(docker image inspect --format '{{.Config.User}}' "$image")" = '25120:25120'
# Validate the real entrypoint and mounted configuration without API side effects.
docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -v "$PWD/examples/config.json:/config/config.json:ro" "$image" serve -config /config/config.json -check-config
docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -v "$PWD/internal/state/testdata/backrestwatch-v2.json:/import/evidence.json:ro" "$image" state validate -file /import/evidence.json
