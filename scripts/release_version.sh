#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(cat VERSION)
if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo 'VERSION must be a stable semantic version' >&2
  exit 1
fi
test "$(sed -n 's/^version: //p' charts/argus/Chart.yaml)" = "$version"
test "$(sed -n 's/^appVersion: "\(.*\)"/\1/p' charts/argus/Chart.yaml)" = "$version"
case "${GITHUB_REF:-}" in
  refs/tags/v*) test "${GITHUB_REF#refs/tags/v}" = "$version" ;;
esac
printf '%s\n' "$version"
