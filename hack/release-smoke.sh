#!/usr/bin/env bash
set -euo pipefail

tag=${RELEASE_VERSION:?Set RELEASE_VERSION to the published version tag}
digest=$(jq -er '."containerimage.digest"' release/build.json)
[[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]
image="ghcr.io/${GITHUB_REPOSITORY,,}"

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
export DOCKER_CONFIG="$temporary"

test "$(crane digest "$image:$tag")" = "$digest"
for arch in amd64 arm64; do
  docker pull --platform "linux/$arch" "$image@$digest"
done
