# LLM Operator

A Kubernetes Operator for managing LLM model serving backends (vLLM, SGLang,
and llama.cpp). The Cogito non-production migration is complete: desired model
state is CRD-backed, with a read-only CR-only API proxy.

The architecture replaces the monolithic proxy controller with CRDs, a
leader-elected controller manager, a standalone cache-manager, and a read-only
API proxy. The migration converter is retained as an offline import tool;
legacy model and overlay ConfigMaps have been retired.

## Status

| Status | Scope | Notes |
|---|---|---|
| ✅ Complete | Milestone 0 — render and API validation | Generated CRDs/RBAC, schema validation, reproducible rendering, and `make check` are in place. |
| ✅ Complete | Milestone 1 — transition test gate | Fake-client and envtest coverage covers transition safety, cancellation, singleton ownership, and failure paths. |
| ✅ Complete | Milestone 2 — backend drivers | vLLM, SGLang, and llama.cpp drivers own runtime arguments, health, discovery, metadata, and cache requests. |
| ✅ Accepted | Milestone 3 — Flux observation-mode validation | The non-production cluster rollout passed its initial passive checks. The sustained window was explicitly waived; transitions remain disabled. |
| ✅ Complete | Milestone 4 — migration and proxy dual read | The non-production comparison passed for four valid models and the Gemma overlay; its temporary dual-read migration path is retired. |
| ✅ Complete | Milestone 5 — controlled cutover | Accepted for the non-production cluster: the standalone cache-manager is in use, the proxy is read-only, and the operator completed a stable Gemma activation. |
| ✅ Complete | Milestone 6 — hardening and ConfigMap retirement | CR-only catalog, legacy ConfigMap cleanup, CR-safe runtime observations, admission handlers, ownership rules, monitoring assets, and runbooks are deployed and verified. |
| ✅ Complete | Milestone 7 — llama.cpp backend validation | Cogito completed an operator-owned transition from vLLM Gemma to cache-hot Laguna; the CR-only proxy served a successful llama.cpp completion and backend health converged to `Serving`. |
| ✅ Complete | Milestone 8 — proxy-to-operator model handoff | A request for a non-active model now updates `LLMActiveModel/default`, waits for the operator transition, and serves the original request without regaining Deployment mutation permission. |
| 📋 Planned | Milestone 9 — managed serving templates | Introduce a reviewed, digest-pinned server-side template reference on `LLMModel`; do not implement or deploy it until the M9 plan is approved. |

Transitions remain opt-in. Do not apply sample CRs directly to a live cluster;
use reviewed Flux resources and the operational guidance in
[observation validation](plans/observation_validation.md).

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
cluster. The migration-era proxy read CRs before valid ConfigMaps; that
fallback is no longer deployed. Cogito now reads only `LLMModel` and
`LLMModelOverlay` CRs.

## Architecture

### Current state

```
Client → Ingress → LLM Proxy → Backend Deployment
                    ↑
          LLMModel / LLMModelOverlay CRs

LLM Operator → reconciles model, backend, overlay, and active-model state
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

The [Helm chart](charts/llm-operator/README.md) packages generated CRDs, RBAC,
the manager Deployment, metrics Service, and PodDisruptionBudget. It renders
two manager replicas by default with leader election, health probes, hardened
security contexts, and transitions disabled. The manager image is required by
immutable digest. In Cogito, the operator owns the stable Gemma activation;
vLLM is serving, Laguna is stopped, and the proxy is read-only.

Model transitions are disabled by default so the operator can safely observe an
existing proxy-managed installation. Enable them only after migration by passing
`--enable-transitions=true` to the manager.

## Status, TODO, and Active Plan

M0–M8 are complete for Cogito's non-production cluster. The active next work
is [M9 — managed serving templates](plans/template_management.md). The earlier
migration documents in `plans/` are retained as historical decision and
validation records; they are not execution checklists.

### Complete — observation-mode validation

- [x] Render and test all operator manifests locally.
- [x] Lock the rendered manager in observation mode with `make observation-preflight`.
- [x] Create and test an OCI Helm chart for operator infrastructure only: CRDs, RBAC, manager, metrics Service, and PDB.
- [x] Create Cogito `OCIRepository` and `HelmRelease` resources with `transitions.enabled=false` and CRD `CreateReplace` policy.
- [x] Publish reviewed immutable image and chart artifacts, pin their digests in Cogito, and reconcile through Flux.
- [x] Verify the live manager is healthy: Flux sources Ready, two replicas available, leader election active, and transitions disabled.
- [x] Reconcile reviewed Cogito resources: two `LLMBackend`s, four `LLMModel`s, and one Gemma overlay; verify their initial status without workload mutation.
- [x] Explicitly waive the sustained observation window for the non-production cluster.

### Complete — ConfigMap migration and proxy dual read

- [x] Implement `hack/migration/configmap-to-crds.go` with deterministic typed conversion and unit coverage.
- [x] Add CRD-first reading, ConfigMap fallback, source precedence, duplicate diagnostics, and read-only CR RBAC to `vllm-proxy`.
- [x] Verify the CR-first catalog, Gemma overlay, proxy readiness, and cache hot path after accepting the legacy proxy's intentional Gemma activation.
- [x] Compare the valid ConfigMap and CR-first catalogs: four models and the Gemma overlay match exactly in identity, ordered args, and defaults.

### Complete — controlled cutover

- [x] Add a proxy mutation gate and an operator transition canary allowlist; both default to the current safe behavior.
- [x] Define and validate the `iggy`-pinned standalone cache-manager against the existing hot-cache path, then repoint the proxy and remove its sidecar.
- [x] Disable proxy deployment mutations and complete a stable, operator-owned Gemma activation through the cache service.
- [ ] Eventual TODO: add runtime/container integration tests for successful and failed transitions.
- [ ] Eventual TODO: add a repeatable canary/rollback exercise before any production cutover.

### Complete — hardening and ConfigMap retirement

- [x] Add validating admission handlers and Flux ownership/drift rules.
- [x] Add dashboards, alerts, and operational runbooks.
- [x] Retire model/overlay ConfigMap fallback; retain only `llm-model-status` runtime metadata.
- [x] Delete the nine ConfigMaps orphaned by the historical non-pruning Flux parent, using a narrowly scoped, idempotent cleanup Job.
- [x] Persist runtime and model-card metadata under ConfigMap-safe CR-source keys (`crd__<resource>.*`), while retaining original model identity in the payload.

### Potential TODO — production readiness and backend expansion

- [ ] Add runtime/container integration coverage for successful and failed
  transitions, including cache-manager and backend-health behavior.
- [ ] Exercise and document a repeatable rollback path before production use.
- [ ] Wire the opt-in admission webhook into a TLS/certificate-managed cluster
  deployment and validate an admission rejection end-to-end.
- [x] Add and validate a live CR-backed llama.cpp serving path in Cogito:
  Laguna's artifacts were hot, the operator transition completed, the proxy
  served a completion, and the backend reported `Serving`.
- [ ] Add and validate additional production backend instances, such as SGLang,
  through the same CRD and GitOps workflow.

### Planned — managed serving templates (M9)

- [ ] Add a portable, model-level `serving.chatTemplate` reference with a
  same-namespace ConfigMap key and pinned content digest.
- [ ] Teach the controller and backend drivers to validate, mount, inject, and
  remove the selected server-side template without making request overlays own
  runtime behavior.
- [ ] Vendor and review the Qwen fixed template in Cogito, bind it to Qwen,
  and validate the captured Pi tool-call request before treating it as a
  general solution.
- [ ] Consider a reusable `LLMServingProfile` resource only if several models
  demonstrably need the same template/parser/reasoning bundle.

The active plan is [template_management.md](plans/template_management.md).
[remaining_work.md](plans/remaining_work.md),
[observation_validation.md](plans/observation_validation.md),
[expand_operator.md](plans/expand_operator.md), and
[OPERATOR_PLAN.md](plans/OPERATOR_PLAN.md) are retained as historical context.

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
