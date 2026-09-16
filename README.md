# petri-apiserver

HTTP API for managing Petri ephemeral environments on Kubernetes. The server creates and updates resources, while [petri-operator](https://github.com/petri-dev/petri-operator) reconciles them into workloads and handles cleanup.

## Quickstart

Prerequisites: Go 1.26.6 or newer, Docker, Kind, kubectl, and Helm. Run from the repository root. This creates an isolated development cluster and installs operator v0.1.10, matching `go.mod`:

```bash
export KUBECONFIG="$(mktemp)"
kind create cluster --name petri-dev --image kindest/node:v1.36.1 --kubeconfig "$KUBECONFIG"
helm upgrade --install petri oci://ghcr.io/petri-dev/charts/petri --version 0.1.10 \
  --kube-context kind-petri-dev --namespace petri-system --create-namespace --wait

PETRI_APISERVER_AUTH_MODE=token \
PETRI_APISERVER_ALLOW_INSECURE_TOKEN=true \
PETRI_APISERVER_TOKEN=dev-only-change-me \
PETRI_APISERVER_ADDR=127.0.0.1:8080 \
PETRI_APISERVER_METRICS_ADDR=127.0.0.1:9090 \
make run
```

Use a shell without existing `PETRI_APISERVER_OIDC_*` or allowlist settings. This binds the API to loopback; use the static token only for development.

In another terminal:

```bash
curl --fail-with-body http://localhost:8080/readyz
curl --fail-with-body -H 'Authorization: Bearer dev-only-change-me' \
  http://localhost:8080/v1/templates
```

The template list is initially empty. Install an `EnvironmentTemplate` through the operator before creating environments. Stop the server with Ctrl-C, then remove the cluster with `kind delete cluster --name petri-dev`.

## API

| Method | Path | Result |
| --- | --- | --- |
| POST | `/v1/environments` | Create (`201`) or update (`200`) an environment |
| GET | `/v1/environments` | Paginated environments |
| GET | `/v1/environments/{name}` | Environment details and status |
| DELETE | `/v1/environments/{name}` | Accept deletion (`204`) |
| GET | `/v1/templates` | Paginated template and component names |

Create or update with a JSON body such as:

```json
{"name":"pr-42","template":"service-template","source":{"branch":"feature/x"},"ttl":"24h"}
```

API success acknowledges the Kubernetes operation. Check the environment's `phase` for reconciliation progress; deletion and workload cleanup are asynchronous. See the API reference (**TODO** refer to documentation) for update semantics, pagination, limits, and errors.

## Authentication and deployment

OIDC is the default. Configure an issuer, audience, and organization or repository
allowlist. All admitted identities share access to every environment in the configured
management namespace. There is no per-user or per-repository ownership isolation.
`/healthz` and `/readyz` are public.

- [Configuration](**TODO** link to configuration guide): environment variables, OIDC, and development auth.
- [Operations](**TODO** link to general operations documentation): installation, permissions, monitoring, and releases.

`config/` is a minimal Kubernetes deployment example. Set your image and supply the `petri-apiserver-config` Secret with OIDC settings. Integrate it into your own Helm, Kustomize, or GitOps setup; networking and scaling are installation-specific.

## Metrics

Scrape each pod's `/metrics` on port 9090 (`PETRI_APISERVER_METRICS_ADDR`).
Keep it internal, the endpoint has no authentication or TLS (TBD if it should be configurable).

- `petri_http_requests_total`: completed requests.
- `petri_http_request_duration_seconds`: request duration histogram.

Both use `route`, `method`, and `status` labels. Configure alerts and notification
routing in your monitoring system, no alert rules are installed.

## Releases

Version tags (`vX.Y.Z`) build Linux/macOS binaries for amd64/arm64, checksums, a digest-pinned `install.yaml`, and scanned, signed multi-platform GHCR images. Manual workflow dispatch validates without publishing. Supply the OIDC configuration Secret before applying `install.yaml`.

## Development

```bash
make build                # Build bin/server
make test                 # Race tests and vet
make lint                 # Strict lint checks
make validate-manifests   # Render all Kustomize entry points
make test-e2e             # Real operator lifecycle in a disposable Kind cluster
```

E2E requires Kind v0.32.0 and Docker API 1.48+ in addition to the quickstart tools. It pulls the released operator, deployer, and Helm chart selected by `go.mod`, no sibling checkout is needed. Normal completion or failure deletes the cluster.
After a forced interruption, delete the disposable cluster name printed in the test log.

See [testing](**TODO** refer to the testing documntation page) for optional load tests and their limits.
Run `make help` for all targets.
