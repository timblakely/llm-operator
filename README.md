# LLM Operator

A Kubernetes Operator for managing LLM model serving backends (vLLM, SGLang, and llama.cpp). It is currently in an **observation-mode migration** from the legacy ConfigMap + `vllm-proxy` control plane.

The target architecture replaces the monolithic proxy controller with CRDs, a leader-elected controller manager, a standalone cache-manager, and a read-only API proxy. The proxy CRD reader and migration tooling are not implemented yet.

## Status

| Status | Scope | Notes |
|---|---|---|
| ✅ Complete | Milestone 0 — render and API validation | Generated CRDs/RBAC, schema validation, reproducible rendering, and `make check` are in place. |
| ✅ Complete | Milestone 1 — transition test gate | Fake-client and envtest coverage covers transition safety, cancellation, singleton ownership, and failure paths. |
| ✅ Complete | Milestone 2 — backend drivers | vLLM, SGLang, and llama.cpp drivers own runtime arguments, health, discovery, metadata, and cache requests. |
| 🟡 In progress / blocked | Milestone 3 — Flux observation-mode validation | The digest-pinned Helm chart and Cogito Flux resources are implemented. Publishing immutable OCI artifacts and target-cluster validation remain. No cluster resource has been changed. |
| ⬜ TODO | Milestone 4 — migration and proxy dual read | Build ConfigMap-to-CRD conversion and add CRD reading with ConfigMap fallback to `vllm-proxy`. |
| ⬜ TODO | Milestone 5 — controlled cutover | Extract cache-manager, add end-to-end tests, canary operator-owned transitions, and rollback validation. |
| ⬜ TODO | Milestone 6 — production hardening | Admission webhooks, Flux ownership, dashboards/alerts, runbooks, and ConfigMap retirement. |

Transitions are disabled by default. Do not enable them or apply sample CRs to a live cluster until the observation-mode preconditions and workload-reference review in [observation validation](plans/observation_validation.md) are complete.

## CRDs

| Kind | Description | Short Name |
|------|-------------|------------|
| `LLMModel` | Model definition with serving config | `llmm` |
| `LLMModelOverlay` | Virtual model with request defaults | `llmo` |
| `LLMActiveModel` | Singleton tracking the active model | `llma` |
| `LLMBackend` | Serving backend deployment registry | `llmb` |

## Quick Start

### Build

```bash
make build
make check
```

`make check` runs formatting, vet, unit tests, envtest, manifest rendering, observation-mode preflight, and generated-file drift checks. Envtest requires permission to bind local test sockets.

### Run locally

```bash
make run
```

The manager defaults to `--enable-transitions=false`, including when run locally.

### Observation-mode deployment

Do not treat this as an unconditional quick start. Cogito deployment is a Flux-managed Helm release, not a direct `kubectl apply`. Before deployment, restore cluster access, publish reviewed immutable image and chart digests, and verify every workload CR reference against the live inventory.

```bash
make observation-preflight
```

Cogito reconciles chart version `0.1.0` from `oci://ghcr.io/timblakely/charts/llm-operator` through `OCIRepository` and `HelmRelease`, with `transitions.enabled=false`. The manager image must be supplied by immutable digest. Follow the complete [observation procedure and rollback steps](plans/observation_validation.md). `make samples-apply` is intentionally not part of the default procedure because the samples must be reviewed for the target cluster first.

### Docker

```bash
make docker-build
make docker-push
```

For a releasable, digest-pinned manager image and matching OCI chart, follow
[Releasing the Manager Image and Helm Chart](docs/releasing.md).

## Architecture

### Current state

```
Client → Ingress → Legacy vllm-proxy → Backend Deployment
                         ↑
                  ConfigMaps remain authoritative

LLM Operator (observation mode) → reads CRDs and referenced Deployments
```

### Target state

```
Client → Ingress → LLM Proxy → Backend Deployment (vLLM, SGLang, or llama.cpp)
                    ↑
             Reads LLMModel, LLMModelOverlay, LLMActiveModel CRDs
```

### Controllers

- **LLMModelController**: Validates model specs, manages finalizers, updates status
- **LLMActiveModelController**: Implements tested transition orchestration (scale down → cache → patch → scale up → health check), but is disabled by default until cutover
- **LLMModelOverlayController**: Validates overlays against base models
- **LLMBackendController**: Monitors backend health and deployment status

### Parser configuration

Use structured parser selections for portable vLLM/SGLang model definitions:

```yaml
spec:
  serving:
    toolCallParser: hermes
    reasoningParser: deepseek-r1
```

The backend driver translates these into runtime flags. llama.cpp currently
rejects structured parser selections. Existing parser flags in `serving.args`
remain supported, but must not be combined with the matching structured field.

### Backend drivers

Runtime-specific launch arguments, health checks, model discovery, runtime
metadata, and cache-manager requests are isolated behind compiled-in backend
drivers. See [Adding a Compiled-in Backend](docs/adding-backend.md) for the
capability matrix, extension contract, and required tests.

### Metrics

The operator exposes Prometheus metrics on `:8081`:

- `llm_operator_transition_duration_seconds` — histogram of transition durations
- `llm_operator_transition_failures_total` — counter of failed transitions by reason
- `llm_operator_model_switches_total` — counter of model switches

### Deployment

The [Helm chart](charts/llm-operator/README.md) packages generated CRDs, RBAC, the manager Deployment, metrics Service, and PodDisruptionBudget. It renders two manager replicas by default with leader election, health probes, hardened security contexts, and transitions disabled. The manager image is required by immutable digest. Cogito consumes the versioned OCI chart through Flux; it has not yet been validated against the target cluster.

Model transitions are disabled by default so the operator can safely observe an
existing proxy-managed installation. Enable them only after migration by passing
`--enable-transitions=true` to the manager.

## TODO and Migration Plan

### In progress — observation-mode validation

- [x] Render and test all operator manifests locally.
- [x] Lock the rendered manager in observation mode with `make observation-preflight`.
- [x] Create and test an OCI Helm chart for operator infrastructure only: CRDs, RBAC, manager, metrics Service, and PDB.
- [x] Create Cogito `OCIRepository` and `HelmRelease` resources with `transitions.enabled=false` and CRD `CreateReplace` policy.
- [ ] Publish reviewed immutable image and chart artifacts.
- [ ] Restore Kubernetes API connectivity, reconcile through Flux, and compare `LLMBackend` status/annotations to the proxy state over an observation window.

### TODO — ConfigMap migration and proxy dual read

- [ ] Implement `hack/migration/configmap-to-crds.go` with fixtures from vLLM and Laguna ConfigMaps.
- [ ] Add CRD reading plus ConfigMap fallback, source precedence, and duplicate diagnostics to `vllm-proxy`.
- [ ] Compare proxy model catalog and overlay behavior between ConfigMap and CRD sources.

### TODO — controlled cutover

- [ ] Deploy cache-manager as a standalone workload with the required cache volumes.
- [ ] Add runtime/container integration tests for successful and failed transitions.
- [ ] Disable proxy controller behavior, then enable operator transitions for a canary backend/model pair.
- [ ] Exercise rollback before expanding the cutover.

### TODO — production hardening

- [ ] Add validating admission webhooks and Flux ownership/drift rules.
- [ ] Add dashboards, alerts, and operational runbooks.
- [ ] Retire ConfigMap fallback after the agreed migration-retention period.

The authoritative execution plan is [remaining_work.md](plans/remaining_work.md). [observation_validation.md](plans/observation_validation.md) records the current blocker, Flux deployment procedure, and rollback. [expand_operator.md](plans/expand_operator.md) is the earlier Phase 1.5 gap analysis; use it as historical context, not current status. [OPERATOR_PLAN.md](plans/OPERATOR_PLAN.md) contains the original long-range architecture and migration design.

## Project Structure

```
llm-operator/
├── cmd/manager/main.go              # Controller manager entrypoint
├── api/cogito.dev/v1alpha1/         # CRD types + generated deepcopy
├── internal/controller/              # 4 controllers
├── internal/cache/                   # Cache-manager HTTP client
├── internal/backend/                 # Runtime driver contracts and tests
├── internal/metrics/                 # Prometheus metrics
├── config/crd/                      # Generated CRD manifests
├── config/rbac/                     # ServiceAccount, ClusterRole, RoleBinding
├── config/manager/                  # Deployment, metrics Service, PDB
├── config/samples/                  # Reviewable sample CR instances
├── charts/llm-operator/             # Digest-pinned OCI Helm chart
├── docs/adding-backend.md            # Compiled-in backend extension guide
├── docs/releasing.md                 # Manager image and chart promotion guide
├── plans/                            # Current plan, validation record, historical plan
├── hack/                             # Generation and observation preflight scripts
├── Dockerfile
├── Makefile
└── go.mod
```

## License

Apache 2.0
