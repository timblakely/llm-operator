# Implementation Plan

## Goal

Build a lightweight Kubernetes Operator (controller-runtime + Kubebuilder, Go) that replaces the current ConfigMap + vllm-proxy monolith with proper CRDs, a controller manager with leader election, a decoupled API proxy, and a clean migration path from the existing system.

---

## Current System Summary (from code review)

| Component | Location | Role | Problems |
|-----------|----------|------|----------|
| Model ConfigMaps | `~/git/cogito/kubernetes/apps/llm/{vllm,laguna}/models/` | Define models via `model.yaml` in ConfigMap data, label `llm.cogito.dev/model-config=true` | No schema validation, no status, no finalizers |
| Overlay ConfigMaps | `~/git/cogito/kubernetes/apps/llm/vllm/models/gemma-4/gemma4-agentic.yaml` | Virtual models with `request_defaults.json`, label `llm.cogito.dev/model-overlay=true` | Same ConfigMap problems |
| vllm-proxy (Go) | `~/git/cogito/vllm/vllm-proxy/cmd/vllm-proxy/main.go` (2102 lines) | 4-in-1: reverse proxy, model switcher, cache coordinator, Hermes config generator | Not a proper controller, no leader election, no reconcile loop, ephemeral state via pod annotations |
| cache-manager (Go) | `~/git/cogito/vllm/vllm-proxy/cmd/vllm-proxy/cache_manager.go` | Hot/cold cache lifecycle via HTTP sidecar | Tightly coupled to proxy, no independent lifecycle |
| HelmRelease (vllm) | `~/git/cogito/kubernetes/apps/llm/vllm/app/helmrelease.yaml` | app-template chart with 3 containers (vllm, proxy, cache-manager) | Drift detection suppressed on replicas/annotations/args |
| HelmRelease (laguna) | `~/git/cogito/kubernetes/apps/llm/laguna/app/helmrelease.yaml` | app-template chart for llama-cpp backend | Same drift suppression pattern |

**Model YAML schema** (from `model.yaml` in ConfigMaps):
```yaml
version: 1
model:
  name: google/gemma-4-31B-it-qat-w4a16-ct
  source: google/gemma-4-31B-it-qat-w4a16-ct   # HF repo or local path
  revision: 52f3f65bc7a02d555763bc923bd1d9094898219d
  artifactRepository: poolside/Laguna-S-2.1-GGUF  # optional, for GGUF
artifact:
  expectedSize: 60Gi
  files: [laguna-s-2.1-Q4_K_M.gguf, ...]         # for GGUF backends
serving:
  backend: vllm | llama-cpp
  displayName: "Gemma 4 31B IT QAT W4A16"
  maxModelLen: 229376
  args: [--tensor-parallel-size, "2", ...]
metadata:
  createdAt: "2026-07-16T00:00:00Z"
```

**Overlay schema** (from ConfigMap data keys):
```yaml
model_name: gemma4-agentic
display_name: "Gemma 4 Agentic"
base_model: google/gemma-4-31B-it-qat-w4a16-ct
created_at: "2026-07-20T00:00:00Z"
request_defaults.json: |
  {"chat_template_kwargs": {"enable_thinking": true, "preserve_thinking": true}}
```

---

## 1. CRD Design

### 1.1 `LLMModel` (singular: `llmmodel`)

The primary resource representing a model definition. Replaces ConfigMaps with `llm.cogito.dev/model-config=true`.

**Group:** `llm.cogito.dev`
**Version:** `v1alpha1`
**Kind:** `LLMModel`
**Scope:** `Namespaced`
**Short names:** `llmm`

**Spec:**
```go
type LLMModelSpec struct {
    // Model identity
    Model LLMModelRef `json:"model"`

    // Artifact download/cache configuration (optional, for models needing materialization)
    Artifact *ArtifactSpec `json:"artifact,omitempty"`

    // Serving configuration
    Serving ServingSpec `json:"serving"`

    // Which backend deployment to use. If omitted, defaults to the backend
    // matching the serving.backend value from the cluster's LLMBackend registry.
    BackendRef *LocalObjectReference `json:"backendRef,omitempty"`
}

type LLMModelRef struct {
    // Name is the canonical model identifier (e.g. "google/gemma-4-31B-it-qat-w4a16-ct")
    Name string `json:"name"`
    // Source is the artifact location: HF repo ID for vllm, local path for llama-cpp
    Source string `json:"source"`
    // Revision is an immutable commit SHA or tag
    Revision string `json:"revision,omitempty"`
    // ArtifactRepository is the upstream repo for GGUF files (optional, for llama-cpp)
    ArtifactRepository string `json:"artifactRepository,omitempty"`
}

type ArtifactSpec struct {
    // ExpectedSize is a human-readable size estimate (e.g. "60Gi")
    ExpectedSize string `json:"expectedSize,omitempty"`
    // Files lists individual files for GGUF/file-based artifacts
    Files []string `json:"files,omitempty"`
}

type ServingSpec struct {
    // Backend is the serving runtime: "vllm" or "llama-cpp"
    Backend BackendType `json:"backend"`
    // DisplayName is a human-readable label
    DisplayName string `json:"displayName"`
    // MaxModelLen is the maximum context length in tokens
    MaxModelLen int `json:"maxModelLen"`
    // Args are backend-specific CLI arguments (model name, revision, and served-model-name
    // are injected by the controller and must NOT be included here)
    Args []string `json:"args"`
}

type BackendType string
const (
    BackendVLLM    BackendType = "vllm"
    BackendLlamaCpp BackendType = "llama-cpp"
)
```

**Status:**
```go
type LLMModelStatus struct {
    // Phase is the high-level lifecycle state
    Phase LLMModelPhase `json:"phase"`

    // Active indicates this model is currently served by a backend
    Active bool `json:"active"`

    // CacheState describes the artifact availability
    CacheState *CacheState `json:"cacheState,omitempty"`

    // RuntimeMetadata captures observed runtime properties after successful startup
    RuntimeMetadata *RuntimeMetadata `json:"runtimeMetadata,omitempty"`

    // Conditions track subresource health
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // ObservedGeneration is the last reconciled generation
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

type LLMModelPhase string
const (
    ModelPhasePending    LLMModelPhase = "Pending"     // Initial state
    ModelPhaseReady      LLMModelPhase = "Ready"       // Configured and available for activation
    ModelPhaseActive     LLMModelPhase = "Active"      // Currently served by a backend
    ModelPhaseTransitioning LLMModelPhase = "Transitioning" // Model switch in progress
    ModelPhaseFailed     LLMModelPhase = "Failed"      // Reconciliation failed
)

type CacheState struct {
    // Location: "hot", "cold", "external", or "unknown"
    Location string `json:"location"`
    // LastHydrated is when the artifact was last promoted to hot cache
    LastHydrated *metav1.Time `json:"lastHydrated,omitempty"`
}

type RuntimeMetadata struct {
    ObservedAt          metav1.Time            `json:"observedAt"`
    ServedModelIDs      []string               `json:"servedModelIDs,omitempty"`
    ContextLength       int                    `json:"contextLength"`
    MaxConcurrentReqs   int                    `json:"maxConcurrentRequests,omitempty"`
    LaunchArguments     map[string]string      `json:"launchArguments,omitempty"`
    KVCache             map[string]string      `json:"kvCache,omitempty"`
}
```

**Conditions:**
- `ModelConfigured` - model spec is valid and backend exists
- `ArtifactCached` - model artifact is available in hot or cold cache
- `BackendHealthy` - the serving backend is responding to health checks

**Finalizer:** `llm.cogito.dev/model-protection` — prevents deletion of the active model. The controller clears the finalizer after deactivating the model.

---

### 1.2 `LLMModelOverlay` (singular: `llmmodeloverlay`)

Virtual model that composes request defaults on top of a base `LLMModel`. Replaces ConfigMaps with `llm.cogito.dev/model-overlay=true`.

**Group:** `llm.cogito.dev`
**Version:** `v1alpha1`
**Kind:** `LLMModelOverlay`
**Scope:** `Namespaced`
**Short names:** `llmo`

**Spec:**
```go
type LLMModelOverlaySpec struct {
    // DisplayName is a human-readable label for the overlay
    DisplayName string `json:"displayName"`

    // BaseModel references the underlying LLMModel by name
    BaseModel string `json:"baseModel"`

    // RequestDefaults merges into client requests before forwarding to the backend.
    // Keys not present in the client request are filled in. Client values always win.
    // Must NOT contain a "model" key (the controller sets this to the base model).
    RequestDefaults json.RawMessage `json:"requestDefaults"`
}
```

**Status:**
```go
type LLMModelOverlayStatus struct {
    // ResolvedBaseModel is the effective base model name after resolution
    ResolvedBaseModel string `json:"resolvedBaseModel,omitempty"`

    // Conditions track validation
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
```

**Condition:** `OverlayValid` — base model exists and request defaults are valid JSON without a "model" key.

---

### 1.3 `LLMBackend` (singular: `llmbackend`)

Declares a serving backend deployment. The operator manages this resource; it is NOT user-facing. It provides a registry of known backends so models can reference them by name without hardcoding Deployment names.

**Group:** `llm.cogito.dev`
**Version:** `v1alpha1`
**Kind:** `LLMBackend`
**Scope:** `Namespaced`
**Short names:** `llmb`

**Spec:**
```go
type LLMBackendSpec struct {
    // Type is the backend runtime: "vllm" or "llama-cpp"
    Type BackendType `json:"type"`

    // DeploymentRef points to the Kubernetes Deployment that runs this backend
    DeploymentRef LocalObjectReference `json:"deploymentRef"`

    // ContainerName is the container within the deployment that serves requests
    ContainerName string `json:"containerName"`

    // ServiceRef points to the Service that exposes this backend
    ServiceRef LocalObjectReference `json:"serviceRef"`

    // Port is the serving port on the backend container
    Port int `json:"port"`

    // RuntimeClassName for GPU nodes (e.g. "nvidia")
    RuntimeClassName string `json:"runtimeClassName,omitempty"`

    // GPU resources required by this backend
    GPUResources *GPUResourceRequirements `json:"gpuResources,omitempty"`
}

type GPUResourceRequirements struct {
    Requests ResourceList `json:"requests,omitempty"`
    Limits   ResourceList `json:"limits,omitempty"`
}
```

**Status:**
```go
type LLMBackendStatus struct {
    // Phase indicates backend health
    Phase LLMBackendPhase `json:"phase"`

    // ActiveModel is the model currently served by this backend (if any)
    ActiveModel string `json:"activeModel,omitempty"`

    // ActiveModelSince is when the current model became active
    ActiveModelSince *metav1.Time `json:"activeModelSince,omitempty"`

    // Replicas is the current replica count
    Replicas int32 `json:"replicas"`

    // AvailableReplicas
    AvailableReplicas int32 `json:"availableReplicas"`

    // Conditions
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

type LLMBackendPhase string
const (
    BackendPhaseStopped   LLMBackendPhase = "Stopped"     // 0 replicas, no model served
    BackendPhaseStarting  LLMBackendPhase = "Starting"    // scaling up, waiting for readiness
    BackendPhaseServing   LLMBackendPhase = "Serving"     // healthy, serving a model
    BackendPhaseFailed    LLMBackendPhase = "Failed"      // error during transition
)
```

**Conditions:**
- `DeploymentExists` — the referenced Deployment exists
- `BackendHealthy` — the backend responds to health checks
- `ModelLoaded` — a model is loaded and serving

---

### 1.4 `LLMActiveModel` (singular: `llmactivemodel`)

A singleton resource that tracks the currently active model. Replaces the ephemeral pod annotation `llm.cogito.dev/active-model`.

**Group:** `llm.cogito.dev`
**Version:** `v1alpha1`
**Kind:** `LLMActiveModel`
**Scope:** `Namespaced`
**Short names:** `llma`

**Spec:**
```go
type LLMActiveModelSpec struct {
    // ModelName is the LLMModel to activate
    ModelName string `json:"modelName"`
}
```

**Status:**
```go
type LLMActiveModelStatus struct {
    // ModelName is the currently active model (may differ from spec during transitions)
    ModelName string `json:"modelName"`

    // BackendType is the backend currently serving the active model
    BackendType BackendType `json:"backendType"`

    // Phase describes the transition state
    Phase LLMActiveModelPhase `json:"phase"`

    // TransitionFrom is the previous model (if switching)
    TransitionFrom string `json:"transitionFrom,omitempty"`

    // TransitionStarted is when the current transition began
    TransitionStarted *metav1.Time `json:"transitionStarted,omitempty"`

    // TransitionDuration is how long the last transition took
    TransitionDuration *metav1.Duration `json:"transitionDuration,omitempty"`

    // Conditions
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

type LLMActiveModelPhase string
const (
    ActiveModelPhaseStable      LLMActiveModelPhase = "Stable"        // model is serving
    ActiveModelPhaseTransitioning LLMActiveModelPhase = "Transitioning" // switch in progress
    ActiveModelPhaseFailed      LLMActiveModelPhase = "Failed"        // transition failed
)
```

**Conditions:**
- `ModelActive` — the requested model is currently serving
- `TransitionComplete` — the last model switch completed successfully

**Important:** This is a singleton — only one `LLMActiveModel` should exist per namespace. The controller enforces this. Users set `spec.modelName` to request a model switch.

---

## 2. Controller Design

Three controllers in a single manager, with leader election:

### 2.1 `LLMModelController`

**Watches:** `LLMModel`, `LLMBackend`
**Reconciles:** Model configuration validity, cache state tracking

**Responsibilities:**
1. Validate model spec (backend exists, args don't contain injected flags like `--model`, `--revision`, `--served-model-name`)
2. Watch for model `spec.serving.backend` changes
3. Update `status.phase` to `Ready` when model is configured and backend exists
4. Set `status.cacheState` based on cache-manager feedback (when cache integration is wired)
5. Enforce finalizer: reject deletion of active models
6. Write runtime metadata to status after successful backend startup (collected from backend `/v1/models` and `/metrics` endpoints)

**Reconcile flow:**
```
1. Get LLMModel
2. If deleting: check if active → if so, reject with status message; else remove finalizer
3. Validate backendRef resolves to an existing LLMBackend
4. Validate args don't contain controller-injected flags
5. If backend healthy: collect runtime metadata → write to status
6. Update status.phase = Ready (or Failed with reason)
7. Update status.observedGeneration
```

---

### 2.2 `LLMActiveModelController`

**Watches:** `LLMActiveModel`, `LLMModel`, `LLMBackend`
**Reconciles:** Model transitions (scale down → cache → patch → scale up → health check)

**This is the core controller that replaces the proxy-as-controller logic.**

**Responsibilities:**
1. Read `spec.modelName` from the singleton `LLMActiveModel`
2. Look up the target `LLMModel` and its `LLMBackend`
3. If a different model is currently active, execute the transition:
   a. Scale down the current backend Deployment to 0 replicas
   b. Wait for current backend to stop (availableReplicas == 0)
   c. Call cache-manager to ensure artifact is in hot cache
   d. Patch target backend Deployment: set replicas=1, inject args, set annotations
   e. Wait for rollout (observedGeneration updated, availableReplicas == 1)
   f. Probe backend health endpoint
   g. Collect runtime metadata, write to LLMModel status
   h. Update LLMActiveModel status.phase = Stable
4. If same model is already active, no-op
5. Handle transition failures with proper status and retry

**Transition flow (from current code in `proxy.transition()`):**
```
1. Verify target backend Deployment exists
2. If current backend is different:
   a. Scale current backend to 0
   b. Poll until current backend availableReplicas == 0
3. Call cache-manager POST /v1/ensure with model cache spec
4. Patch target backend Deployment:
   - spec.replicas = 1
   - spec.template.metadata.annotations[llm.cogito.dev/active-model] = modelName
   - spec.template.metadata.annotations[llm.cogito.dev/switched-at] = timestamp
   - spec.template.spec.containers[N].args = effectiveArgs(model)
5. Poll until target backend availableReplicas == 1 AND health check passes
6. Update LLMActiveModel status → Stable
7. Update target LLMModel status → Active, with runtime metadata
8. Update previous LLMModel status → Ready (no longer active)
```

**Leader election:** The controller manager uses controller-runtime's built-in leader election with a Lease. Only one controller manager replica executes transitions at a time.

**Re-entrancy:** If `spec.modelName` changes during an active transition, the controller cancels the current transition and starts a new one to the new target. The current code handles this via `transitionCancel` context cancellation.

---

### 2.3 `LLMModelOverlayController`

**Watches:** `LLMModelOverlay`, `LLMModel`
**Reconciles:** Overlay validation

**Responsibilities:**
1. Validate `spec.baseModel` references an existing `LLMModel`
2. Validate `spec.requestDefaults` is valid JSON and doesn't contain a "model" key
3. Set `status.resolvedBaseModel` and condition `OverlayValid`

This controller is simple — it validates overlays but doesn't do anything with them at the K8s level. The proxy uses overlay data at request time.

---

### 2.4 `LLMBackendController` (optional, Phase 3)

**Watches:** `LLMBackend`, `Deployment` (referenced)
**Reconciles:** Backend health monitoring

**Responsibilities:**
1. Monitor the referenced Deployment's replica count and health
2. Update `LLMBackend` status with current state
3. Report backend health to `LLMModel` status

This is optional for Phase 1-2 but useful for observability.

---

## 3. API Proxy Design

The proxy is **decoupled from the controller**. It runs as a standalone Deployment managed by the operator (or independently via Helm).

### 3.1 Architecture

```
Client → Ingress → LLM Proxy Deployment → Backend Deployment (vllm or llama-cpp)
                          ↑
                     Reads LLMModel, LLMModelOverlay, LLMActiveModel CRDs
                     via controller-runtime cache (read-only)
```

### 3.2 Proxy responsibilities (reduced from 4 to 2):

1. **Reverse proxy** with OpenAI-compatible API normalization
2. **Overlay composition** — merge `request_defaults.json` into requests for overlay models

The proxy reads CRDs via a controller-runtime cache (read-only informers) but does NOT write to K8s APIs, does NOT manage Deployments, and does NOT coordinate cache operations.

### 3.3 Proxy reads:
- `LLMModel` list — for model catalog (`/v1/models`)
- `LLMModelOverlay` list — for overlay resolution
- `LLMActiveModel` — for current active model and Hermes config generation

### 3.4 Proxy does NOT:
- Watch or mutate Deployments
- Call cache-manager
- Manage model transitions
- Generate Hermes config as a side effect of reconciliation (it serves it as an API endpoint)

### 3.5 Deployment model:
- Single container, no sidecars
- Managed by the operator's HelmRelease or as a separate Deployment resource
- RBAC: read-only access to `llm.cogito.dev` CRDs
- Can be scaled independently of the controller

### 3.6 Hermes config:
- The proxy serves `/vllm-proxy/config/{target}` as before
- External tools (like the Hermes agent) call this endpoint to get current config
- The `sync-hermes` and `bootstrap-hermes` subcommands remain as CLI tools

---

## 4. Cache Management

### 4.1 Current state

The cache-manager runs as a sidecar in the proxy pod, communicating via HTTP on localhost. It manages:
- **Hot cache**: PVC-mounted directories (`/cache/vllm`, `/cache/laguna`)
- **Cold cache**: NFS share (`/cold/llm-cache`)
- Operations: `ensure` (materialize artifact), `sweep` (evict old artifacts)

### 4.2 New architecture

The cache-manager remains as a **standalone Deployment** (not a sidecar), with its own Service. The `LLMActiveModelController` calls it during transitions.

**Changes:**
1. Extract cache-manager from the proxy pod
2. Deploy as a standalone Deployment in the `llm` namespace
3. Expose via a ClusterIP Service
4. The controller calls the cache-manager Service during model transitions
5. Cache-manager discovers PVC limits via the K8s API (as it does today)

**Benefits:**
- Cache-manager can be scaled/restarted independently
- Controller can call it from any pod (not just the proxy pod)
- Clear ownership: controller orchestrates, cache-manager executes

### 4.3 Cache status in LLMModel

The `LLMModel.status.cacheState` field is populated by the controller after calling cache-manager during a transition. The controller reads the `X-LLM-Cache-Result` header (`hot`, `cold`, `external`) and writes it to status.

---

## 5. Migration Path

### Phase 1: CRD + Proxy reads CRDs (Weeks 1-3)

**Goal:** Define CRDs, allow proxy to read from both ConfigMaps and CRDs simultaneously.

**Tasks:**
1. Scaffold project with Kubebuilder (`kubebuilder init --domain cogito.dev`)
2. Define `LLMModel`, `LLMModelOverlay`, `LLMActiveModel`, `LLMBackend` CRDs
3. Write CRD validation (CEL or webhook) for basic constraints
4. Generate CRD manifests, RBAC
5. Deploy CRDs to cluster (no controllers yet)
6. Modify vllm-proxy to read from CRDs (with ConfigMap fallback)
7. Write migration tool: convert existing ConfigMaps to CRDs
8. Test: create LLMModel CRDs, verify proxy reads them

**Deliverables:**
- `llm-operator/` Go module with CRD types and manifests
- Modified vllm-proxy with CRD reader (dual-mode)
- Migration script: `configmap-to-crds.sh`

**Risk:** Proxy must handle both sources gracefully. ConfigMaps remain source of truth until Phase 2.

---

### Phase 2: Controller replaces proxy controller logic (Weeks 4-7)

**Goal:** Controller manager handles model transitions. Proxy becomes read-only.

**Tasks:**
1. Implement `LLMActiveModelController` with full transition logic (ported from proxy)
2. Implement `LLMModelController` for validation and status
3. Implement `LLMModelOverlayController` for validation
4. Add leader election to the manager
5. Extract cache-manager from proxy pod to standalone Deployment
6. Modify proxy to remove controller logic (watchConfigs, watchDeployments, reconcileActiveDeployment, transition, ensureCached, sweepCache, persistRuntimeMetadata)
7. Proxy now reads CRDs only, serves API, does overlay composition
8. Controller calls cache-manager Service during transitions
9. Update HelmRelease to deploy controller manager + proxy + cache-manager as separate workloads
10. Update drift detection: remove ignored fields since controller now owns them via CRD

**Deliverables:**
- Controller manager binary with 3 controllers
- Slimmed proxy binary (reverse proxy + overlay only)
- Standalone cache-manager Deployment
- Updated HelmRelease manifests

**Risk:** Transition logic must be identical to current proxy behavior. Port carefully with tests.

---

### Phase 3: Full operator + polish (Weeks 8-10)

**Goal:** Production-ready operator with admission webhooks, metrics, and Flux integration.

**Tasks:**
1. Add validating admission webhook for `LLMModel` (reject invalid args, duplicate names, missing backends)
2. Add `LLMBackendController` for health monitoring
3. Add Prometheus metrics to controller manager (transition duration, error rates, model switch count)
4. Add controller-runtime metrics to the manager
5. Write envtest-based unit tests for all controllers
6. Write integration tests with kind cluster
7. Update Flux HelmRelease to deploy the operator
8. Remove ConfigMap support from proxy
9. Documentation: CRD reference, migration guide, troubleshooting

**Deliverables:**
- Full operator with admission webhooks
- Test suite (unit + integration)
- Documentation
- Production-ready HelmRelease

---

## 6. Project Structure

```
llm-operator/
├── cmd/
│   └── manager/
│       └── main.go                 # controller-runtime manager entrypoint
├── api/
│   └── cogito.dev/
│       └── v1alpha1/
│           ├── groupversion_info.go
│           ├── llmmodel_types.go           # LLMModel CRD
│           ├── llmmodel_overlay_types.go   # LLMModelOverlay CRD
│           ├── llmmodel_active_types.go    # LLMActiveModel CRD
│           ├── llmbackend_types.go         # LLMBackend CRD
│           ├── llmmodel_webhook.go         # validating webhook (Phase 3)
│           ├── webhook_suite_test.go
│           └── zz_generated.deepcopy.go
├── config/
│   ├── crd/                          # generated CRD manifests
│   │   ├── llm.cogito.dev_llmmodels.yaml
│   │   ├── llm.cogito.dev_llmmodeloverlays.yaml
│   │   ├── llm.cogito.dev_llmactivemodels.yaml
│   │   └── llm.cogito.dev_llmbackends.yaml
│   ├── rbac/                         # generated RBAC
│   ├── default/
│   ├── manager/
│   │   └── manager.yaml              # controller manager Deployment
│   └── samples/
│       ├── llm_v1alpha1_llmmodel.yaml
│       ├── llm_v1alpha1_llmmodeloverlay.yaml
│       ├── llm_v1alpha1_llmactivemodel.yaml
│       └── llm_v1alpha1_llmbackend.yaml
├── internal/
│   ├── controller/
│   │   ├── llmmodel_controller.go
│   │   ├── llmmodel_controller_test.go
│   │   ├── llmmodeloverlay_controller.go
│   │   ├── llmmodeloverlay_controller_test.go
│   │   ├── llmactivemodel_controller.go
│   │   ├── llmactivemodel_controller_test.go
│   │   ├── llmbackend_controller.go      # Phase 3
│   │   ├── llmbackend_controller_test.go # Phase 3
│   │   └── suite_test.go                 # envtest setup
│   ├── cache/
│   │   ├── client.go                   # HTTP client for cache-manager
│   │   └── client_test.go
│   └── proxy/
│       ├── proxy.go                    # slim proxy (Phase 2+)
│       └── overlay.go
├── hack/
│   ├── boilerplate.go.txt
│   └── migration/
│       └── configmap-to-crds.go        # Phase 1 migration tool
├── Dockerfile
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

### Makefile targets (Kubebuilder-generated + custom):

```makefile
# Generated by Kubebuilder
.envtest        # run envtest-based tests
.test           # run all tests
.build          # build manager binary
.docker-build   # build container image
.manifests      # generate CRD manifests
.generate       # generate deepcopy code
.fmt            # format code
.vet            # vet code

# Custom
.migrate        # run configmap-to-crds migration tool
.deploy         # kubectl apply config/
```

---

## 7. Testing Strategy

### 7.1 Unit tests (envtest)

Every controller gets an envtest-based test file:

- **`llmmodel_controller_test.go`**: Test validation (invalid backend, duplicate names, args with injected flags), finalizer behavior, status updates
- **`llmmodeloverlay_controller_test.go`**: Test base model resolution, invalid request defaults, missing base model
- **`llmactivemodel_controller_test.go`**: Test transition flow (mock cache-manager, mock Deployment), transition cancellation, error recovery
- **`llmbackend_controller_test.go`**: Test health monitoring, status updates

### 7.2 Integration tests (kind)

- Deploy operator to kind cluster
- Create LLMModel, LLMBackend, LLMActiveModel CRDs
- Verify transition flow end-to-end (with real vllm/laguna containers or mocks)
- Test model switch from vllm → llama-cpp and back
- Test overlay resolution via proxy API

### 7.3 Proxy tests

- Unit tests for overlay composition (existing `main_test.go` patterns)
- Integration test: proxy reads CRDs, serves correct model catalog, applies overlays

### 7.4 Migration tests

- Test configmap-to-crds tool with all existing ConfigMaps
- Verify generated CRDs match expected schema

---

## 8. Phased Rollout Summary

| Phase | Timeline | Scope | Effort | Risk |
|-------|----------|-------|--------|------|
| **Phase 1** | Weeks 1-3 | CRDs, dual-mode proxy, migration tool | 2-3 person-weeks | Low — no behavior changes, additive only |
| **Phase 2** | Weeks 4-7 | Controller replaces proxy logic, extract cache-manager | 3-4 person-weeks | Medium — transition logic must be identical |
| **Phase 3** | Weeks 8-10 | Webhooks, metrics, tests, docs, remove ConfigMap support | 2-3 person-weeks | Low — polish and hardening |

**Total estimated effort:** 7-10 person-weeks

---

## Files to Modify (existing)

| File | Changes |
|------|---------|
| `~/git/cogito/vllm/vllm-proxy/cmd/vllm-proxy/main.go` | Phase 1: add CRD reader alongside ConfigMap reader. Phase 2: remove controller logic (watchConfigs, watchDeployments, reconcileActiveDeployment, transition, ensureCached, sweepCache, persistRuntimeMetadata). Keep: reverse proxy, overlay composition, Hermes config endpoints, CLI subcommands. |
| `~/git/cogito/vllm/vllm-proxy/cmd/vllm-proxy/cache_manager.go` | Extract to standalone binary. Add ClusterIP Service. No logic changes needed. |
| `~/git/cogito/vllm/vllm-proxy/Dockerfile` | Split into two images: proxy (distroless) and cache-manager (python). Currently multi-stage with both. |
| `~/git/cogito/kubernetes/apps/llm/vllm/app/helmrelease.yaml` | Phase 2: remove proxy and cache-manager containers from vllm controller. Add separate HelmRelease or Deployment for controller manager. Update drift detection config. |
| `~/git/cogito/kubernetes/apps/llm/laguna/app/helmrelease.yaml` | Phase 2: update drift detection (controller now owns the fields). |

## New Files

| File | Purpose |
|------|---------|
| `llm-operator/cmd/manager/main.go` | Controller-runtime manager entrypoint |
| `llm-operator/api/cogito.dev/v1alpha1/llmmodel_types.go` | LLMModel CRD Go types |
| `llm-operator/api/cogito.dev/v1alpha1/llmmodel_overlay_types.go` | LLMModelOverlay CRD Go types |
| `llm-operator/api/cogito.dev/v1alpha1/llmmodel_active_types.go` | LLMActiveModel CRD Go types |
| `llm-operator/api/cogito.dev/v1alpha1/llmbackend_types.go` | LLMBackend CRD Go types |
| `llm-operator/api/cogito.dev/v1alpha1/llmmodel_webhook.go` | Validating admission webhook (Phase 3) |
| `llm-operator/internal/controller/llmmodel_controller.go` | LLMModel reconciliation |
| `llm-operator/internal/controller/llmmodeloverlay_controller.go` | LLMModelOverlay validation |
| `llm-operator/internal/controller/llmactivemodel_controller.go` | Model transition orchestration |
| `llm-operator/internal/controller/llmbackend_controller.go` | Backend health monitoring (Phase 3) |
| `llm-operator/internal/cache/client.go` | HTTP client for cache-manager Service |
| `llm-operator/internal/proxy/proxy.go` | Slim proxy (Phase 2) |
| `llm-operator/internal/proxy/overlay.go` | Overlay composition logic |
| `llm-operator/config/crd/*.yaml` | Generated CRD manifests |
| `llm-operator/config/rbac/*.yaml` | Generated RBAC |
| `llm-operator/config/manager/manager.yaml` | Controller manager Deployment |
| `llm-operator/config/samples/*.yaml` | Example CRD instances |
| `llm-operator/hack/migration/configmap-to-crds.go` | ConfigMap → CRD migration tool |
| `llm-operator/Dockerfile` | Multi-stage build for controller manager |
| `llm-operator/Makefile` | Kubebuilder Makefile |
| `llm-operator/go.mod` | Go module |

---

## Dependencies

| Task | Depends On |
|------|------------|
| Phase 1: CRD scaffolding | Nothing |
| Phase 1: CRD validation | CRD scaffolding |
| Phase 1: Dual-mode proxy | CRDs deployed to cluster |
| Phase 1: Migration tool | CRD types defined |
| Phase 2: ActiveModel controller | Phase 1 CRDs, cache-manager extraction |
| Phase 2: Model controller | Phase 1 CRDs |
| Phase 2: Overlay controller | Phase 1 CRDs |
| Phase 2: Slim proxy | ActiveModel controller working |
| Phase 2: HelmRelease update | All Phase 2 controllers working |
| Phase 3: Admission webhook | Phase 2 complete |
| Phase 3: Backend controller | Phase 2 complete |
| Phase 3: Metrics | Phase 2 complete |
| Phase 3: Tests | All controllers implemented |
| Phase 3: Remove ConfigMap support | All tests passing, migration complete |

---

## Risks

1. **Transition logic parity**: The current proxy's `transition()` function (main.go:1560-1620) is complex with precise timing, health checks, and cache coordination. Porting this to the controller must be exact. **Mitigation:** Port the function line-by-line, then write envtest tests that verify the exact same sequence of K8s API calls.

2. **Cache-manager extraction**: The cache-manager currently runs as a sidecar sharing the proxy's PVC mounts. Extracting it to a standalone Deployment requires ensuring the same PVC mounts are available. **Mitigation:** The cache-manager Deployment will mount the same PVCs (huggingface-cache, laguna-models, cold-model-cache via NFS). This is straightforward but must be verified.

3. **Flux drift detection**: Currently Flux ignores `/spec/replicas`, annotations, and `/spec/containers/0/args` on the vllm and laguna Deployments. After Phase 2, the controller owns these fields via CRD reconciliation. Flux drift detection can be re-enabled for these fields, OR the Deployments can be managed entirely by the operator (not Helm). **Recommendation:** Keep Helm for the base Deployment template (resources, images, volumes) but let the controller patch replicas/args/annotations. Keep drift detection ignoring those fields — this is correct because the controller, not Helm, owns them.

4. **Multi-backend transitions**: Switching from vllm to llama-cpp (or vice versa) requires scaling down one backend and scaling up another. The current code handles this. The controller must replicate this exactly. **Mitigation:** Test both directions in integration tests.

5. **Active model singleton**: `LLMActiveModel` is a singleton per namespace. If multiple users try to switch models simultaneously, the controller must handle this gracefully. **Mitigation:** The controller uses the standard reconcile loop — if `spec.modelName` changes during a transition, the current transition is cancelled and a new one starts. This is the same behavior as the current proxy's `transitionCancel` pattern.

6. **GGUF vs HF artifact handling**: The cache-manager handles two artifact types differently (HF hub cache vs GGUF files). The controller must pass the correct cache spec. **Mitigation:** The `LLMModel.spec.artifact` field captures this, and the controller passes it to the cache-manager as-is.

7. **Go version**: The existing proxy uses Go 1.26.0. The operator should use the same Go version for consistency. Kubebuilder may scaffold with a different default. **Mitigation:** Set `go 1.26.0` in go.mod explicitly.

8. **Namespace scope**: All CRDs are namespaced (in the `llm` namespace). This is correct for the current single-namespace deployment but limits multi-tenant use. **Decision:** Keep namespaced for now; upgrade to cluster-scoped if needed later.

9. **Backward compatibility during migration**: During Phase 1, the proxy reads from both ConfigMaps and CRDs. Priority must be defined: CRDs win over ConfigMaps for the same model name. **Mitigation:** Document this clearly and test.

10. **Hermes config generation**: The proxy generates Hermes config dynamically. After Phase 2, the proxy reads CRDs for this. The `sync-hermes` and `bootstrap-hermes` CLI subcommands remain unchanged. **Mitigation:** No changes needed to these subcommands.

---

## Detailed Task Breakdown

### Phase 1 Tasks

1. **Task 1.1**: Initialize Kubebuilder project
   - Run `kubebuilder init --domain cogito.dev --repo github.com/timblakely/llm-operator --license apache2 --component=controller`
   - Set Go version to 1.26.0 in go.mod
   - File: `llm-operator/` (new)

2. **Task 1.2**: Create LLMModel CRD types
   - Define spec/status Go types matching the schema above
   - Run `make generate manifests`
   - Add CEL validation: `backend` must be "vllm" or "llama-cpp", `maxModelLen` > 0, `args` must not contain `--model`, `--revision`, `--served-model-name` (for vllm) or `-m`, `--model`, `--alias` (for llama-cpp)
   - File: `llm-operator/api/cogito.dev/v1alpha1/llmmodel_types.go`

3. **Task 1.3**: Create LLMModelOverlay CRD types
   - Define spec/status Go types
   - Add CEL validation: `baseModel` must match `^[a-zA-Z0-9._-]+$`, `requestDefaults` must be valid JSON
   - File: `llm-operator/api/cogito.dev/v1alpha1/llmmodel_overlay_types.go`

4. **Task 1.4**: Create LLMActiveModel CRD types
   - Define spec/status Go types
   - Add CEL validation: `modelName` must match `^[a-zA-Z0-9._-]+$`
   - File: `llm-operator/api/cogito.dev/v1alpha1/llmmodel_active_types.go`

5. **Task 1.5**: Create LLMBackend CRD types
   - Define spec/status Go types
   - Add CEL validation: `type` must be "vllm" or "llama-cpp", `port` > 0
   - File: `llm-operator/api/cogito.dev/v1alpha1/llmbackend_types.go`

6. **Task 1.6**: Generate and review CRD manifests
   - Run `make manifests`
   - Review generated YAML for correctness
   - Add OpenAPI v3 validation where CEL is insufficient
   - File: `llm-operator/config/crd/*.yaml`

7. **Task 1.7**: Create sample CRD instances
   - Convert existing ConfigMaps to CRD YAML format
   - `gemma-4-31b-it-qat-w4a16-ct` → LLMModel
   - `laguna-s-2-1-q4` → LLMModel
   - `gemma4-agentic` → LLMModelOverlay
   - `qwen-3-6-27b-autoround` → LLMModel
   - LLMBackend for vllm and llama-cpp
   - LLMActiveModel with default model
   - File: `llm-operator/config/samples/*.yaml`

8. **Task 1.8**: Write configmap-to-crds migration tool
   - Read ConfigMaps with `llm.cogito.dev/model-config=true` and `llm.cogito.dev/model-overlay=true`
   - Parse `model.yaml` or flat data keys
   - Output LLMModel and LLMModelOverlay YAML
   - File: `llm-operator/hack/migration/configmap-to-crds.go`

9. **Task 1.9**: Modify vllm-proxy for dual-mode CRD reading
   - Add controller-runtime cache (read-only) for LLMModel and LLMModelOverlay
   - Keep ConfigMap reading as fallback
   - Priority: CRD > ConfigMap for same model name
   - File: `~/git/cogito/vllm/vllm-proxy/cmd/vllm-proxy/main.go`

10. **Task 1.10**: Deploy CRDs and test
    - `kubectl apply -f config/crd/`
    - Apply sample CRDs
    - Verify proxy reads them correctly
    - Verify `/v1/models` endpoint returns correct data

### Phase 2 Tasks

11. **Task 2.1**: Implement LLMModelController
    - Validate model spec against backend registry
    - Set status.phase = Ready/Failed
    - Enforce finalizer on active models
    - Collect runtime metadata from healthy backends
    - File: `llm-operator/internal/controller/llmmodel_controller.go`

12. **Task 2.2**: Implement LLMActiveModelController
    - Port transition logic from proxy.main.go `transition()` function
    - Scale down current backend → cache → patch → scale up → health check
    - Handle transition cancellation when spec.modelName changes
    - Call cache-manager Service for artifact materialization
    - Update LLMModel and LLMBackend status
    - File: `llm-operator/internal/controller/llmactivemodel_controller.go`

13. **Task 2.3**: Implement LLMModelOverlayController
    - Validate base model reference
    - Validate request defaults
    - File: `llm-operator/internal/controller/llmmodeloverlay_controller.go`

14. **Task 2.4**: Extract cache-manager to standalone Deployment
    - Create cache-manager Deployment manifest
    - Mount same PVCs as before (huggingface-cache, laguna-models, NFS cold cache)
    - Create ClusterIP Service
    - Update proxy HelmRelease to remove cache-manager container
    - File: `llm-operator/config/manager/cache-manager.yaml`

15. **Task 2.5**: Write cache-manager HTTP client
    - Wrapper for cache-manager Service endpoints (`/v1/ensure`, `/v1/sweep`)
    - File: `llm-operator/internal/cache/client.go`

16. **Task 2.6**: Slim the proxy
    - Remove: watchConfigs, watchDeployments, reconcileActiveDeployment, transition, ensureCached, sweepCache, persistRuntimeMetadata, syncActiveDeployment, backendFor, deploymentNeedsActivation, effectiveArgs, effectiveVLLMArgs
    - Keep: reverse proxy, overlay composition, Hermes config, CLI subcommands, /v1/models, /healthz, /readyz, /metrics
    - Add: CRD reader (controller-runtime cache)
    - File: `~/git/cogito/vllm/vllm-proxy/cmd/vllm-proxy/main.go`

17. **Task 2.7**: Update Dockerfile
    - Split into two images: proxy (distroless) and cache-manager (python)
    - File: `~/git/cogito/vllm/vllm-proxy/Dockerfile`

18. **Task 2.8**: Update HelmRelease manifests
    - vllm HelmRelease: remove proxy and cache-manager containers, keep vllm container
    - Add controller manager Deployment (via HelmRelease or raw)
    - Add proxy Deployment
    - Add cache-manager Deployment
    - Update drift detection config
    - File: `~/git/cogito/kubernetes/apps/llm/vllm/app/helmrelease.yaml`

19. **Task 2.9**: Write envtest-based unit tests
    - Test all 3 controllers
    - Mock cache-manager with test HTTP server
    - File: `llm-operator/internal/controller/*_test.go`

### Phase 3 Tasks

20. **Task 3.1**: Add validating admission webhook for LLMModel
    - Reject: invalid backend types, args with injected flags, duplicate model names, missing backends
    - File: `llm-operator/api/cogito.dev/v1alpha1/llmmodel_webhook.go`

21. **Task 3.2**: Implement LLMBackendController
    - Monitor referenced Deployment health
    - Update LLMBackend status
    - File: `llm-operator/internal/controller/llmbackend_controller.go`

22. **Task 3.3**: Add Prometheus metrics
    - Transition duration histogram
    - Transition success/failure counter
    - Model switch counter
    - Controller-runtime metrics (already available)
    - File: `llm-operator/internal/controller/` (add metrics package)

23. **Task 3.4**: Write integration tests with kind
    - Full end-to-end test: create CRDs → trigger transition → verify backend serves correct model
    - Test vllm → llama-cpp → vllm round-trip
    - File: `llm-operator/test/e2e/`

24. **Task 3.5**: Remove ConfigMap support from proxy
    - Remove ConfigMap reading code
    - Remove migration fallback
    - File: `~/git/cogito/vllm/vllm-proxy/cmd/vllm-proxy/main.go`

25. **Task 3.6**: Write documentation
    - CRD reference
    - Migration guide
    - Troubleshooting
    - Architecture overview
    - File: `llm-operator/docs/`

---

## Acceptance Criteria Per Phase

### Phase 1 Acceptance
- [ ] All 4 CRDs apply cleanly to the cluster
- [ ] CRD validation rejects invalid specs (wrong backend, missing required fields)
- [ ] Proxy reads models from CRDs and serves them via `/v1/models`
- [ ] Proxy falls back to ConfigMaps when CRDs are absent
- [ ] Migration tool converts all existing ConfigMaps to CRD YAML

### Phase 2 Acceptance
- [ ] Controller manager starts with leader election
- [ ] Setting `LLMActiveModel.spec.modelName` triggers a model transition
- [ ] Transition follows the exact sequence: scale down → cache → patch → scale up → health check
- [ ] Proxy serves API without controller logic
- [ ] Cache-manager runs as standalone Deployment
- [ ] Flux drift detection no longer needs to ignore controller-owned fields (or ignores them correctly)
- [ ] Unit tests pass for all controllers

### Phase 3 Acceptance
- [ ] Admission webhook rejects invalid LLMModel specs
- [ ] Backend controller reports accurate health status
- [ ] Prometheus metrics are scrapeable
- [ ] Integration tests pass on kind
- [ ] ConfigMap support is removed from proxy
- [ ] Documentation is complete