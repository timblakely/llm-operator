# Shared Model Cache

## Status

**Implementation and GitOps rollout in progress.** SMC.0–SMC.2 are complete;
SMC.3 is reconciled but awaits HelmRelease recovery verification. Do not
activate DeepSeek V4 Flash until SMC.3 and SMC.4 pass.

## Objective

Replace Cogito's runtime/deployment-specific hot caches with one node-local,
capacity-managed hot cache. The cache is keyed by immutable artifact identity,
not by vLLM, llama.cpp, a Deployment, or a particular model family. It can
materialize Hugging Face repository snapshots and GGUF file sets, evict complete
artifacts by LRU policy, and restore them from the existing cold NAS archive.

The design supports transitions such as vLLM → llama.cpp, llama.cpp → vLLM,
and Laguna → DeepSeek (two distinct llama.cpp backends) without treating each
backend as a separate cache.

## Target Contract

One `ReadWriteOnce` PVC is mounted read-write by the cache-manager and by
whichever backend is scheduled on `iggy`:

```text
hot cache PVC
├── hub/                         # Hugging Face cache layout for vLLM/SGLang
├── gguf/<artifact-key>/          # Direct GGUF payloads for llama.cpp
└── .llm-cache/<artifact-key>/    # completion marker, immutable spec, LRU time

cold NAS
└── artifacts/<artifact-key>/     # verified archive + manifest
```

`artifact-key` is derived from the artifact kind, repository, and immutable
revision. It is stable for a given artifact and changes when its revision
changes. The cache-manager owns all writes below the hot root; a runtime only
reads its materialized artifact.

For a GGUF artifact, the cache request must carry a validated relative target
directory, for example `gguf/huggingface-files-unsloth--DeepSeek-V4-Flash-GGUF-<revision>`.
The model's `source` is the corresponding path from the llama.cpp mount. This
removes the cache-manager's current hard-coded `laguna/` destination. Hub
artifacts retain the standard Hugging Face `hub/models--…/snapshots/…` layout.

## Ownership and Coordination

| Area | Owner | Required change |
|---|---|---|
| Cache API and drivers | `llm-operator` | Add a versioned/validated artifact materialization target to the cache request for file artifacts; keep existing hub requests compatible. |
| Transition orchestration | `llm-operator` | Compare resolved `LLMBackend` identity/deployment, not only backend type, before deciding whether to scale down the current backend. This is required for Laguna → DeepSeek. |
| Cache implementation and image | Cogito `vllm/vllm-proxy` | Replace `hotVLLM`/`hotLaguna` routing with one hot root; materialize, verify, archive, sweep, capacity-account, and evict by immutable artifact key. Publish a new immutable cache-manager image. |
| Storage and mounts | Cogito GitOps | Mount one shared hot PVC at the cache-manager root, at the Hugging Face cache subpath for vLLM, and at the GGUF subpath for llama.cpp deployments. Retain old PVCs during migration; do not delete them as part of this work. |
| Model definitions | Cogito GitOps | Update GGUF `model.source` values to their deterministic shared-cache paths and define the materialization target. Keep repository, revision, and exact file list pinned. |
| Activation and proxy validation | Cogito operations | Reconcile in stages, execute canary transitions, and validate the public proxy's plain chat, reasoning, tool-call, and streaming behavior. |

The cache-manager must accept the operator's request contract before a Cogito
model uses it. A cache-manager image must be published before its HelmRelease
is updated to a new image digest. These are release dependencies, not optional
documentation work.

## Stages

### SMC.0 — Contract and migration design

1. Define the cache-request fields for a file artifact's materialization target
   and validation rules: relative, clean, non-empty, and confined to `gguf/`.
2. Specify source-to-mount mapping for each backend and ensure it is independent
   of the serving Deployment name.
3. Define compatibility behavior: the new cache-manager accepts the old request
   form only during migration, while new GGUF models require the new target.
4. Record capacity policy: a single global high/low watermark and whole-artifact
   LRU eviction. In-flight and active artifacts are never evicted.
5. Record rollback: old PVCs remain intact and a Git revert restores their
   mounts/image without deleting data.

**Exit:** reviewed API/layout/rollback record; no live mounts, images, or
active model selection changed.

**Status:** complete.

### SMC.1 — Operator contract and transition safety

1. Extend the cache request emitted by the llama.cpp driver with the validated
   materialization target.
2. Add validation tests for missing, absolute, traversal, duplicate, and valid
   targets, plus cache-request contract tests for hub and GGUF artifacts.
3. Fix transition selection so two `LLMBackend`s of the same runtime type are
   treated as distinct workloads. Resolve the previous model's `backendRef` and
   scale down its actual Deployment before the target Deployment is activated.
4. Add transition tests for Laguna → DeepSeek and DeepSeek → Laguna, asserting
   scale-down, cache ensure, target patch, scale-up, and health ordering.
5. Release a new immutable operator image and chart only after `make check`
   passes, including envtest and generated-manifest drift checks.

**Exit:** a model's cache request identifies its exact shared-cache location,
and same-runtime cross-backend transitions are covered by automated tests.

**Status:** complete. `make check` passed, the operator image was published as
`sha256:c430d8b5791960a88c70d1c82a4d94ec2b96e33006b4151022777639f713e682`,
and chart `0.1.10` was published at
`sha256:971633c4d9428bcb4f67d31e1dc3e7cb06e5c384a23b32c04d51876c08c1a8ac`.

### SMC.2 — Cache-manager implementation and image

1. Replace backend-specific hot paths with one `CACHE_HOT_ROOT` mount.
2. Make download, materialization, archive, existence checks, and eviction use
   artifact-key paths. GGUF payloads must land under their requested `gguf/`
   target; hub artifacts must preserve the Hugging Face layout.
3. Discover one PVC capacity and export one hot-cache filesystem metric. Keep
   per-artifact counters/labels only where they do not reintroduce cache
   partitioning.
4. Add tests proving a hub artifact and two independent GGUF artifacts coexist,
   that LRU eviction removes only the selected complete artifact, and that a
   cold restore verifies payload hashes before publishing `.complete`.
5. Build and publish a new immutable cache-manager image; record its digest in
   Cogito before changing the HelmRelease.

**Exit:** cache-manager tests pass and the published image can safely manage
multiple artifact kinds from one hot root without a backend-specific path.

**Status:** complete. The image was published as
`ghcr.io/timblakely/llm-cache-manager:shared-cache-20260802.1@sha256:2990176abc04777860f61c70ebacf5d74c4bf6aad943d1db0fe5daa5b13884e9`.

### SMC.3 — Cogito storage and GitOps migration

1. Use the existing 300Gi `llm-huggingface-cache` PVC as the initial shared
   candidate after confirming real allocatable capacity. Do not assume 300Gi is
   sufficient: account for the active vLLM artifact, DeepSeek's ~110Gi binary
   footprint, staging overhead, and the configured eviction watermark.
2. Mount that claim at `/cache/hot` in cache-manager, `hub` into vLLM's
   Hugging Face cache path, and `gguf` into each llama.cpp `/models` path.
3. Keep Laguna and DeepSeek's old dedicated PVCs retained but unused until the
   shared cache has survived the validation window and rollback is no longer
   required.
4. Replace `CACHE_HOT_VLLM`, `CACHE_HOT_LAGUNA`, and
   `CACHE_HOT_DEEPSEEK_V4_FLASH` with `CACHE_HOT_ROOT`.
5. Update the Laguna and DeepSeek `LLMModel.source` fields and materialization
   targets as one reviewed change. Pin the cache-manager and llama.cpp images
   by digest.
6. Render the complete Flux tree with `flux-local test`; review the targeted
   HelmRelease diff before reconcile.

**Exit:** all three runtime Deployments and cache-manager render with one PVC
and correct subpaths; no model is activated during the storage/mount migration.

**Status:** in progress. Flux applied Cogito revision `1e3d2001`; operator and
cache-manager upgraded successfully. Laguna and DeepSeek HelmReleases require
post-fix Ready verification after correcting the `existingClaim` chart schema.

### SMC.4 — Canary activation and evidence

1. Verify that the cache-manager reports one shared capacity and that existing
   hot/cold markers are internally consistent.
2. Activate a known small/previously validated artifact, then Laguna, then
   DeepSeek. Confirm each cache ensure produces the intended shared-cache path
   and no backend writes outside its assigned read path.
3. Exercise eviction by ensuring enough reviewed artifacts to cross the high
   watermark; confirm an inactive complete artifact is archived/evicted and an
   active one is preserved.
4. Validate Laguna → DeepSeek and DeepSeek → Laguna transitions: the source
   Deployment reaches zero before the target consumes GPUs.
5. Through the public proxy, validate DeepSeek plain chat, reasoning,
   one-tool completion plus tool-result continuation, and streaming deltas.
6. Capture Deployment args, mounted paths, `LLMModel`/`LLMBackend` status,
   cache-manager metrics, and logs as the acceptance record.

**Exit:** all acceptance checks below pass in Cogito without manual file copies
or cache-manager path overrides.

**Status:** not started. The next action is to confirm the two HelmReleases are
Ready and inspect their rendered mounts before any model activation.

## Acceptance Conditions

The shared-cache update is successful only when all conditions hold:

- Exactly one hot model-cache PVC is used by vLLM, every llama.cpp backend, and
  cache-manager; no production serving path depends on a per-backend hot PVC.
- The cache manager can host and independently evict both a Hugging Face hub
  artifact and multiple GGUF artifacts without deleting an active artifact.
- Every materialized artifact is selected by immutable repo/revision/file
  identity, verified before `.complete` is written, and recoverable from cold
  NAS after hot eviction.
- A GGUF model source resolves to the same path the cache-manager materializes;
  DeepSeek no longer relies on the hard-coded `laguna/` directory.
- The operator scales down the actual previous `LLMBackend` when switching
  between different backends of the same runtime type.
- All images used by the operator, cache-manager, and DeepSeek llama.cpp server
  are immutable digest references.
- `make check` in `llm-operator` and the targeted Cogito Flux render both pass.
- Public proxy tests pass for the DeepSeek request modes listed in SMC.4.
- A Git revert restores the prior image/mount configuration without deleting
  the retained legacy PVCs or the cold archive.

## Non-goals

- A shared filesystem across nodes. The initial cache remains RWO and pinned to
  `iggy`; multi-node storage is a separate design.
- Concurrently serving multiple GPU backends. The operator continues to make
  activation a controlled, exclusive transition.
- Deleting existing cache PVCs during this migration.
- Treating the mutable upstream model repository or image tags as cache keys or
  release pins.
