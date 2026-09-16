#!/usr/bin/env bash
set -euo pipefail

image=${RELEASE_IMAGE:?Set RELEASE_IMAGE to the release image digest reference}
[[ "$image" =~ ^[a-z0-9.:/_-]+@sha256:[a-f0-9]{64}$ ]]

mkdir -p release/manifests
printf 'resources:\n  - ../../config\nimages:\n  - name: petri-apiserver\n    newName: %s\n    digest: %s\n' \
  "${image%@*}" "${image#*@}" > release/manifests/kustomization.yaml
kubectl kustomize release/manifests > release/install.yaml
