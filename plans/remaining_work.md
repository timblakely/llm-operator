# Remaining Work Plan

## Objective

Safely migrate model-serving control from the legacy `vllm-proxy` to the
operator while retaining portable support for vLLM, SGLang, and llama.cpp.

The manager must remain in observation mode until the transition test and
cluster-validation gates below have passed.

## Deployment Packaging Principle

The operator is delivered to Cogito as a versioned OCI Helm chart reconciled by
Flux. The chart owns only operator infrastructure: CRDs, ServiceAccount/RBAC,
manager Deployment, metrics Service, and PDB. Cogito's GitOps repository owns
the `OCIRepository`, `HelmRelease`, environment-specific values, and the
`LLMBackend`/`LLMModel`/overlay CRs that describe real workloads.

Do not package sample or live model CRs in the operator chart. They must be
reviewed independently against the target cluster. Keep transitions disabled in
the chart's default values and enable them only through a reviewed Git change
after the controlled-cutover gate.

## Current Baseline

- CRDs, controllers, backend drivers, parser fields, cache client, metrics,
  RBAC, and manager deployment manifests exist.
- The `charts/llm-operator` OCI chart packages only operator infrastructure,
  requires a digest-pinned manager image, and defaults transitions to disabled.
- Cogito has reconciled chart/app `0.1.2` at digest
  `sha256:02893675f50e7c41a6ec0254c1e47fc699f2981d7371bdf8485e542405a985d4`
  and manager image digest
  `sha256:56863a45d3c63d11eadb5d08330deb3ea7dc4e74b8ca399a38d0f9e858e6a596`.
  Both Flux Kustomizations, the `OCIRepository`, and `HelmRelease` are Ready;
  the manager Deployment is 2/2 available with transitions disabled.
- vLLM, SGLang, and llama.cpp have runtime-specific launch-argument drivers.
- `--enable-transitions=false` is the deployment default.
- Unit tests cover driver behavior, disabled-transition safety, and
  sidecar-preserving deployment activation.

## Milestone 0 — Render and API Validation

**Goal:** prove that the distributable manifests and CRDs are internally
consistent before contacting a cluster.

1. Add a reproducible manifest-render target using Kustomize or `kubectl
   kustomize`.
2. Regenerate CRDs/RBAC with the pinned controller-gen version and fail CI on
   generated-file drift.
3. Validate CRD schemas, including SGLang and structured parser fields.
4. Add a `make check` target: formatting, vet, unit tests, manifest render,
   and generated-file drift.

**Exit criteria:** `make check` succeeds from a clean checkout.

## Milestone 1 — Transition Controller Test Gate

**Goal:** establish deterministic behavior before enabling any Deployment
mutation in a cluster.

1. Extend fake-client unit tests for:
   - target container missing;
   - cache-manager unavailable and cache-manager success;
   - rollout timeout and failed health checks;
   - previous-model status deactivation;
   - vLLM → llama.cpp and llama.cpp → vLLM scale-down ordering;
   - changed `spec.modelName` during a transition.
2. Add `envtest` coverage for status subresources, finalizers, watches, and
   singleton `LLMActiveModel` behavior.
3. Add a bounded cancellation design: persist a transition token/generation in
   status and check it between each blocking transition step.
4. Test that exactly one active-model reconcile can mutate state at a time.

**Exit criteria:** all transition states and failure paths are covered by tests;
no live transition is enabled before this gate passes.

## Milestone 2 — Complete the Backend Driver Boundary

**Goal:** ensure new runtimes do not add `switch backend` logic throughout
controllers.

1. Move health semantics, model discovery, runtime-metadata parsing, and cache
   request construction into the `backend.Driver` interface.
2. Specify each driver's supported capabilities: OpenAI model discovery,
   tool-call parser, reasoning parser, metrics, cache format, and health path.
3. Keep portable API fields in `LLMModel.spec.serving`; reject unsupported
   feature/runtime combinations with conditions.
4. Add SGLang sample `LLMBackend` and `LLMModel` instances, plus contract tests
   for every driver.
5. Document the extension procedure for an additional compiled-in backend.

**Exit criteria:** controllers depend on driver interfaces for all
runtime-specific behavior; every driver has contract tests.

## Milestone 3 — Observation-Mode Cluster Validation

**Goal:** deploy safely beside the proxy through Flux and compare operator
observations to the live system without changing it.

1. **Complete:** create `charts/llm-operator/` with generated CRDs in `crds/`
   and templates for the operator infrastructure only.
2. **Complete locally:** add chart lint/template/package tests and Cogito
   `OCIRepository` and `HelmRelease` resources. The release uses
   `CreateReplace` CRD policy for install and upgrade and sets
   `transitions.enabled=false`.
3. **Complete:** publish the manager image and chart to GHCR with reviewed
   immutable digests and commit the Cogito GitOps resources. Current chart
   digest: `sha256:02893675f50e7c41a6ec0254c1e47fc699f2981d7371bdf8485e542405a985d4`;
   image digest:
   `sha256:56863a45d3c63d11eadb5d08330deb3ea7dc4e74b8ca399a38d0f9e858e6a596`.
4. **Complete:** reconcile Flux. The manager is 2/2 available and its exact
   runtime argument includes `--enable-transitions=false`.
5. **Complete:** reconcile separately reviewed `LLMBackend` and `LLMModel` CRs
   for vLLM and llama.cpp, plus the valid Gemma overlay. Both backends report
   their existing stopped Deployments; all models are `Ready`/`Configured` and
   the overlay is valid.
6. **In progress:** observe conditions, logs, metrics, and workload fields over
   a sustained window. Verify Prometheus scraping separately.
7. **In progress:** compare observations with the proxy. Runtime metadata and
   inferred active-model status are intentionally unavailable in passive mode.
   Exclude the known pre-existing Fable/Gemma proxy catalog drift until it is
   corrected rather than masking it with CRs.
8. **TODO:** record a GitOps rollback: suspend or revert the HelmRelease while keeping
   transitions disabled; existing workloads remain proxy-controlled.

**Exit criteria:** a sustained observation period reports expected status with
no backend Deployment, cache, or proxy mutations. The chart and image are
reconciled by Flux from immutable artifacts.

## Milestone 4 — Migration Tool and Proxy Dual Read

**Goal:** make CRDs the validated representation while ConfigMaps remain the
rollback source of truth.

1. **Complete:** implement `hack/migration/configmap-to-crds.go` to convert
   labeled model and overlay ConfigMaps into deterministic, reviewable CR YAML.
   It is file-based and never writes to a cluster.
2. **Complete:** add converter coverage for valid models/overlays, malformed
   labeled input, non-ConfigMap input, canonical slash-containing base-model
   names, and JSON request defaults.
3. **Complete:** update `vllm-proxy` to read `LLMModel` and
   `LLMModelOverlay` CRDs with ConfigMap fallback, including overlay resolution
   and `/v1/models` source metadata.
4. **Complete:** make CRs authoritative on duplicate logical names; retain
   bounded diagnostics and a metric for skipped legacy ConfigMaps. The proxy
   has read-only CR RBAC and never writes CR status.
5. **Complete for the non-production scope:** render → convert → review →
   compare the CR-first and ConfigMap-only catalogs/overlays. The four valid
   models and Gemma overlay match in identity, source/revision, backend,
   display name, context length, ordered arguments, artifact configuration, and
   request defaults. CR catalog entries expose `config_source=crd/...` and
   diagnostics prove duplicate ConfigMaps are skipped.

**Current non-production state:** the M4 proxy rollout intentionally activated
`google/gemma-4-31B-it-qat-w4a16-ct` through the legacy proxy. vLLM is 1/1
ready, the CR-first catalog and Gemma overlay are healthy, and cache-manager
reports the artifact hot. This is not operator-controlled: transitions remain
disabled in the operator and Laguna remains stopped. The invalid Fable model
and its overlays are explicitly excluded because they use the pre-existing
missing `llm-llama-cpp` backend and a controller-injected model argument. Keep
ConfigMaps as the rollback source; resolve those legacy entries before
ConfigMap retirement.

**Exit criteria:** proxy output is equivalent for a representative model catalog
when backed by CRDs, with ConfigMap fallback still available.

## Milestone 5 — Controlled Transition Cutover

**Goal:** move transition ownership from the proxy to the operator.

1. **Complete foundation:** add an operator transition allowlist and a proxy
   mutation gate. Both preserve current behavior by default; the proxy can be
   made catalog/overlay-only before any operator canary is enabled.
2. **Complete foundation:** define a standalone shadow cache-manager Deployment
   and Service in Cogito. It is `iggy`-pinned and reuses the existing RWO hot
   cache claims and NFS cold cache, but the live proxy remains on its localhost
   sidecar until the service behavior is compared.
3. **Complete for the non-production scope:** compare the shadow cache-manager
   to the sidecar, repoint the proxy, remove its sidecar, and disable proxy
   deployment mutations while retaining reverse proxy, overlays, Hermes
   endpoints, and CLI utilities.
4. **Complete for the non-production scope:** enable operator transitions and
   complete a stable Gemma activation through the standalone cache service.
5. **Eventual TODO:** add kind or equivalent runtime/container integration
   tests with a mock runtime and cache-manager for successful and failed
   transitions.
6. **Eventual TODO:** exercise a repeatable canary/rollback path before any
   production cutover.

**Accepted non-production exit:** the operator completed a stable Gemma
transition through the standalone cache service and the proxy no longer mutates
backend Deployments. Successful/failed runtime integration coverage and a
tested rollback path remain eventual TODOs before production use.

## Milestone 6 — Production Hardening and ConfigMap Retirement

**Goal:** make the operator the production control plane.

1. Add validating admission webhooks for duplicate canonical model names,
   backend availability, parser capabilities, and unsafe arguments.
2. Configure Flux ownership/drift exceptions so Helm owns static operator and
   backend-template fields while the operator owns replicas, activation
   annotations, and runtime container arguments.
3. Add dashboards and alerts for transition duration/failures, backend health,
   cache failures, and leader-election availability.
4. Write operational runbooks: add backend, add model, activate model, rollback,
   cache recovery, and incident diagnostics.
5. Remove ConfigMap fallback and archive the migration utility after an agreed
   retention period.

**Exit criteria:** ConfigMaps and proxy controller logic are retired, with
documented operations and monitored production SLOs.

## Recommended Execution Order

`M0 → M1 → M2 → M3 → M4 → M5 → M6`

M0–M2 are local development gates. M3 requires the operator chart and Cogito
Flux resources before safe cluster observation. M4–M6 require coordination with
the proxy and deployment repositories.
