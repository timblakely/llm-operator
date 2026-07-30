# LLM Operator

A Kubernetes Operator for managing LLM model serving backends (vLLM, SGLang, and llama.cpp). It is currently in an **observation-mode migration** from the legacy ConfigMap + `vllm-proxy` control plane.

The target architecture replaces the monolithic proxy controller with CRDs, a leader-elected controller manager, a standalone cache-manager, and a read-only API proxy. The migration converter and proxy dual-read path are implemented; ConfigMaps remain the rollback source of truth until migration comparison is complete.

## Status

| Status | Scope | Notes |
|---|---|---|
| ✅ Complete | Milestone 0 — render and API validation | Generated CRDs/RBAC, schema validation, reproducible rendering, and `make check` are in place. |
| ✅ Complete | Milestone 1 — transition test gate | Fake-client and envtest coverage covers transition safety, cancellation, singleton ownership, and failure paths. |
| ✅ Complete | Milestone 2 — backend drivers | vLLM, SGLang, and llama.cpp drivers own runtime arguments, health, discovery, metadata, and cache requests. |
| ✅ Accepted | Milestone 3 — Flux observation-mode validation | The non-production cluster rollout passed its initial passive checks. The sustained window was explicitly waived; transitions remain disabled. |
| ✅ Accepted | Milestone 4 — migration and proxy dual read | The non-production comparison passed for four valid models and the Gemma overlay. CRs are authoritative; ConfigMaps remain rollback until the deferred Fable/vanilla cleanup. |
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

Do not treat this as an unconditional quick start. Cogito deployment is a Flux-managed Helm release, not a direct `kubectl apply`. The operator infrastructure is already deployed there in observation mode. Before adding any workload CR, verify its reference against the live inventory and capture an observation baseline.

```bash
make observation-preflight
```

Cogito reconciles chart version `0.1.2` from `oci://ghcr.io/timblakely/charts/llm-operator` through `OCIRepository` and `HelmRelease`, with `transitions.enabled=false`. The live release is pinned to chart digest `sha256:028936…85d4` and manager image digest `sha256:56863a…a596`; it has two ready, leader-elected replicas. The dependent resource Kustomization is also Ready with two `LLMBackend`s, four `LLMModel`s, and one `LLMModelOverlay`. Follow the complete [observation procedure and rollback steps](plans/observation_validation.md). `make samples-apply` is intentionally not part of the default procedure because the samples must be reviewed for the target cluster first.

### Docker

```bash
make docker-build
make docker-push
```

For a releasable, digest-pinned manager image and matching OCI chart, follow
[Releasing the Manager Image and Helm Chart](docs/releasing.md).

### ConfigMap migration

Render the legacy model and overlay ConfigMaps from GitOps, then convert the
reviewed input into declarative CR YAML:

```bash
go run ./hack/migration/configmap-to-crds.go --input rendered-configmaps.yaml --output migrated-resources.yaml
```

The converter accepts only labeled `v1/ConfigMap` documents, preserves a
`llm.cogito.dev/migrated-from-configmap` annotation, and never writes to a
cluster. The Cogito proxy reads `LLMModel` and `LLMModelOverlay` resources
first, then falls back to valid ConfigMaps. CRs win on duplicate logical model
or overlay names; skipped legacy items are exposed through proxy diagnostics.

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

The [Helm chart](charts/llm-operator/README.md) packages generated CRDs, RBAC, the manager Deployment, metrics Service, and PodDisruptionBudget. It renders two manager replicas by default with leader election, health probes, hardened security contexts, and transitions disabled. The manager image is required by immutable digest. In Cogito, chart/app `0.1.2` is Ready with two manager replicas; four models are `Ready`/`Configured`, and the Gemma overlay is valid. During M4, the upgraded legacy proxy intentionally activated Gemma; vLLM is 1/1 ready with the reviewed Gemma revision, while Laguna remains stopped. The operator remains passive and does not own that activation.

Model transitions are disabled by default so the operator can safely observe an
existing proxy-managed installation. Enable them only after migration by passing
`--enable-transitions=true` to the manager.

## TODO and Migration Plan

### In progress — observation-mode validation

- [x] Render and test all operator manifests locally.
- [x] Lock the rendered manager in observation mode with `make observation-preflight`.
- [x] Create and test an OCI Helm chart for operator infrastructure only: CRDs, RBAC, manager, metrics Service, and PDB.
- [x] Create Cogito `OCIRepository` and `HelmRelease` resources with `transitions.enabled=false` and CRD `CreateReplace` policy.
- [x] Publish reviewed immutable image and chart artifacts, pin their digests in Cogito, and reconcile through Flux.
- [x] Verify the live manager is healthy: Flux sources Ready, two replicas available, leader election active, and transitions disabled.
- [x] Reconcile reviewed Cogito resources: two `LLMBackend`s, four `LLMModel`s, and one Gemma overlay; verify their initial status without workload mutation.
- [x] Explicitly waive the sustained observation window for the non-production cluster. Keep the known Fable proxy/catalog drift excluded until it is corrected.

### Accepted — ConfigMap migration and proxy dual read

- [x] Implement `hack/migration/configmap-to-crds.go` with deterministic typed conversion and unit coverage.
- [x] Add CRD-first reading, ConfigMap fallback, source precedence, duplicate diagnostics, and read-only CR RBAC to `vllm-proxy`.
- [x] Verify the CR-first catalog, Gemma overlay, proxy readiness, and cache hot path after accepting the legacy proxy's intentional Gemma activation.
- [x] Compare the valid ConfigMap and CR-first catalogs: four models and the Gemma overlay match exactly in identity, ordered args, and defaults. Explicitly exclude invalid Fable/vanilla entries and retain ConfigMaps as rollback.

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
