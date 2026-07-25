# Remaining Work Plan

## Objective

Safely migrate model-serving control from the legacy `vllm-proxy` to the
operator while retaining portable support for vLLM, SGLang, and llama.cpp.

The manager must remain in observation mode until the transition test and
cluster-validation gates below have passed.

## Current Baseline

- CRDs, controllers, backend drivers, parser fields, cache client, metrics,
  RBAC, and manager deployment manifests exist.
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

**Goal:** deploy safely beside the proxy and compare operator observations to
the live system without changing it.

1. Deploy CRDs, RBAC, and the manager with `--enable-transitions=false`.
2. Create `LLMBackend` and `LLMModel` CRs representing the existing vLLM and
   llama.cpp deployments.
3. Verify model, backend, and overlay conditions; expose the metrics Service to
   Prometheus.
4. Compare operator-derived runtime metadata and active-model status with the
   proxy's current state.
5. Record rollback steps: scale manager to zero or keep transitions disabled;
   existing workloads remain proxy-controlled.

**Exit criteria:** a sustained observation period reports expected status with
no Deployment, cache, or proxy mutations.

## Milestone 4 — Migration Tool and Proxy Dual Read

**Goal:** make CRDs the validated representation while ConfigMaps remain the
rollback source of truth.

1. Implement `hack/migration/configmap-to-crds.go` to convert model and overlay
   ConfigMaps into deterministic CR YAML.
2. Add fixture tests from existing vLLM and Laguna definitions.
3. Update `vllm-proxy` to read CRDs with ConfigMap fallback, including overlay
   resolution and `/v1/models` behavior.
4. Add an explicit source-precedence policy and duplicate-model diagnostics.
5. Run a staged migration: generate → review → apply CRs → compare proxy output.

**Exit criteria:** proxy output is equivalent for a representative model catalog
when backed by CRDs, with ConfigMap fallback still available.

## Milestone 5 — Controlled Transition Cutover

**Goal:** move transition ownership from the proxy to the operator.

1. Extract cache-manager to its standalone Deployment and Service with the
   required hot/cold cache volumes.
2. Add kind or equivalent integration tests with a mock runtime and
   cache-manager for end-to-end transitions.
3. Disable proxy controller behavior while retaining reverse proxy, overlays,
   Hermes endpoints, and CLI utilities.
4. Enable operator transitions for one canary backend/model pair.
5. Exercise rollback: disable transitions, restore proxy control, and verify
   active model recovery.
6. Expand the cutover only after canary metrics and transition logs are clean.

**Exit criteria:** operator completes successful and failed transitions with a
tested rollback path; proxy no longer mutates backend Deployments.

## Milestone 6 — Production Hardening and ConfigMap Retirement

**Goal:** make the operator the production control plane.

1. Add validating admission webhooks for duplicate canonical model names,
   backend availability, parser capabilities, and unsafe arguments.
2. Configure Flux ownership/drift exceptions so Helm owns static workload
   template fields and the operator owns replicas, activation annotations, and
   runtime container arguments.
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

M0–M2 are local development gates. M3 is safe to perform against the existing
cluster. M4–M6 require coordination with the proxy and deployment repositories.
