#!/usr/bin/env bash
set -euo pipefail

repository=${GITHUB_REPOSITORY:?}
prerelease=$(gh release view "${GITHUB_REF_NAME:?}" --repo "$repository" --json isPrerelease --jq .isPrerelease)
if [[ "$prerelease" == true ]]; then
  exit 0
fi
[[ "$prerelease" == false ]]

tag=$(gh api "repos/$repository/releases/latest" --jq .tag_name)
image="ghcr.io/${repository,,}"
ref=$(gh release download "$tag" --repo "$repository" --pattern image-ref.txt --output -)
digest=${ref#"$image@"}
[[ "$ref" == "$image@$digest" && "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
export DOCKER_CONFIG="$temporary"
printf '%s' "${GH_TOKEN:?}" | crane auth login ghcr.io -u "${GITHUB_ACTOR:?}" --password-stdin
test "$(crane digest "$image:$tag")" = "$digest"
crane tag "$ref" latest

test "$(DOCKER_CONFIG="$temporary/anonymous" crane digest "$image:latest")" = "$digest"
