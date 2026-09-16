#!/usr/bin/env bash
set -euo pipefail
mkdir -p .release-tools
case "${1:-}" in
  scan)
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
      https://github.com/aquasecurity/trivy/releases/download/v0.74.0/trivy_0.74.0_Linux-64bit.tar.gz -o .release-tools/trivy.tar.gz
    printf '%s\n' '2ae6fe3ee734b7fdf11335663e18c75ea12dccc76062f09f164a3b0f8be4371a  .release-tools/trivy.tar.gz' | sha256sum -c -
    tar -xzf .release-tools/trivy.tar.gz -C .release-tools trivy
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
      https://github.com/anchore/syft/releases/download/v1.51.1/syft_1.51.1_linux_amd64.tar.gz -o .release-tools/syft.tar.gz
    printf '%s\n' '8fcb33017a0dc1058298c923c436d19dfa68ae93968e0b423248542e3afb9fc3  .release-tools/syft.tar.gz' | sha256sum -c -
    tar -xzf .release-tools/syft.tar.gz -C .release-tools syft
    ;;
  sign)
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
      https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64 -o .release-tools/cosign
    printf '%s\n' '4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71  .release-tools/cosign' | sha256sum -c -
    chmod +x .release-tools/cosign
    ;;
  *) printf '%s\n' 'usage: bash hack/release-tools.sh scan|sign' >&2; exit 1 ;;
esac
