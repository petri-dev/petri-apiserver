#!/usr/bin/env bash
set -euo pipefail
release=${1:?release directory required}
tools=${2:?directory containing trivy and syft required}
layout=$(mktemp -d)

trap 'rm -rf "$layout"' EXIT
tar -xf "$release/image.tar" -C "$layout"
digest=$(jq -er '."containerimage.digest"' "$release/build.json")
[[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]
index="$layout/blobs/sha256/${digest#sha256:}"
status=0

for arch in amd64 arm64; do
  mkdir "$layout/$arch"
  ln -s "$layout/blobs" "$layout/$arch/blobs"
  cp "$layout/oci-layout" "$layout/$arch/oci-layout"

  jq -e --arg arch "$arch" '{schemaVersion: 2, manifests: [.manifests[] |
    select(.platform.os == "linux" and .platform.architecture == $arch)]} |
    select(.manifests | length == 1)' "$index" > "$layout/$arch/index.json"
  platform=$(jq -er '.manifests[0].digest' "$layout/$arch/index.json")
  attestation=$(jq -er --arg digest "$platform" '.manifests[] |
    select(.annotations["vnd.docker.reference.digest"] == $digest) | .digest' "$index")
  layer=$(jq -er '.layers[] | select(.annotations["in-toto.io/predicate-type"] ==
    "https://slsa.dev/provenance/v1") | .digest' "$layout/blobs/sha256/${attestation#sha256:}")

  jq -e --arg digest "${platform#sha256:}" 'select(.predicateType == "https://slsa.dev/provenance/v1" and
    any(.subject[]; .digest.sha256 == $digest)) | .predicate |
    select(.buildDefinition != null and .runDetails != null)' \
    "$layout/blobs/sha256/${layer#sha256:}" > "$release/provenance-$arch.json"
  "$tools/trivy" image --input "$layout/$arch" --scanners vuln \
    --severity HIGH,CRITICAL --exit-code 1 --format json \
    --output "$release/trivy-$arch.json" || status=1

  jq -e --arg arch "$arch" '.Metadata.ImageConfig.architecture == $arch and
    any(.Results[]; .Type == "gobinary") and
    ([.Results[]?.Vulnerabilities[]?] | length == 0)' "$release/trivy-$arch.json" > /dev/null || status=1
  "$tools/syft" "oci-dir:$layout/$arch" -o "spdx-json=$release/sbom-$arch.spdx.json"

done
exit "$status"
