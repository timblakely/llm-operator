# Observation-Mode Cluster Validation

## Status

> Historical record: the passive-observation rollout passed on 2026-07-29.
> M5 and M6 have since completed in Cogito, followed by M7 and M8. This is a
> validation record, not a current runbook; use
> [template_management.md](template_management.md) for active work and
> [remaining_work.md](remaining_work.md) for the historical migration record.

The sustained window was explicitly waived for this non-production cluster on
2026-07-29. That accepted M3 for planning purposes; later M5 enabled the
reviewed non-production handoff and M6 retired the ConfigMap control plane.

Cogito context `main` (API `https://k8s.internal:6443`) now has a healthy,
Flux-managed operator release in namespace `llm`:

- `OCIRepository/llm-operator` is Ready at chart digest
  `sha256:02893675f50e7c41a6ec0254c1e47fc699f2981d7371bdf8485e542405a985d4`.
- `HelmRelease/llm-operator` is Ready/Released at chart/app `0.1.2` (release
  v4).
- `Deployment/llm-operator` is 2/2 ready and available; both Pods are Running
  with zero restarts and use manager image digest
  `sha256:56863a45d3c63d11eadb5d08330deb3ea7dc4e74b8ca399a38d0f9e858e6a596`.
- The live manager arguments include exactly `--enable-transitions=false` and
  leader election is active. All four CRDs are Established and controller logs
  have no current warnings or errors.

Both Flux Kustomizations are Ready and reconcile two `LLMBackend`s, four
`LLMModel`s, and the Gemma overlay. Both backends correctly report their
existing zero-replica Deployments; all models report `ModelConfigured=True` and
`Ready`; the overlay reports `OverlayValid=True`. There is no
`LLMActiveModel`. Workload replicas, container arguments, active-model
annotations, proxy Pods, and backend Pods were unchanged by the rollout.

The Fable Fusion model and its overlays are intentionally excluded: its
pre-existing proxy catalog configuration uses an unsupported backend, a
controller-injected model argument, and a non-existent deployment. The live
vLLM Gemma annotation versus Fable argument mismatch remains a recorded proxy
drift, not an operator-owned change.

During the subsequent M4 CR-first proxy rollout, the legacy proxy's existing
startup reconciliation intentionally activated Gemma. vLLM is now 1/1 ready on
the reviewed Gemma revision; the proxy catalog and cache-manager hot path are
healthy, and Laguna remains stopped. This is a legacy-proxy action, not an
operator transition. The proxy continues to warn about the separate missing
`llm-llama-cpp` backend, which must remain visible in the migration comparison.

There is also a validation-scope gap in the current controller behavior:
observation mode records backend health and activation annotations, but runtime
metadata collection occurs only after an operator-owned transition. Likewise,
an `LLMActiveModel` with transitions disabled reports `TransitionsDisabled`; it
does not infer and write the proxy's current active model into its status. Until
passive metadata/status observation is designed, comparison must use
`LLMBackend.status`, Deployment annotations, and the proxy state rather than
`LLMModel.status.runtimeMetadata` or a synthesized active-model status.

## Local safety evidence

- The rendered manager argument is `--enable-transitions=false`.
- `make observation-preflight` fails if the rendered manifest omits that exact
  argument, contains it more than once, or enables transitions.
- The rendered Deployment, RBAC, CRDs, metrics Service, and PDB are internally
  consistent through `make check`.
- `make chart-check` proves the Helm chart CRDs match the generated manifests,
  the manager image is digest-pinned, and transitions render disabled by
  default. The chart is version `0.1.2` at the intended OCI artifact URL
  `oci://ghcr.io/timblakely/charts/llm-operator`.
- Transition mutation remains opt-in in manager code and manifests.

## Remaining work for the observation window

1. Confirm the already-deployed release remains healthy before each workload
   change:

   ```bash
   kubectl -n llm get ocirepository/llm-operator helmrelease/llm-operator
   kubectl -n llm rollout status deployment/llm-operator
   ```

2. Record a baseline and periodically inspect the reconciled resources:

   ```bash
   kubectl -n llm get llmbackend,llmmodel,llmmodeloverlay -o wide
   kubectl -n llm logs deployment/llm-operator --since=1h
   kubectl -n llm get deployment,service,pod -o wide
   ```

3. Compare the recorded baseline at the end of the agreed window and document
   any status, proxy-catalog, or workload-field mismatch before M4.

## Observation procedure

The operator chart and reviewed workload CRs are already reconciled. Capture a
fresh baseline at the start of the sustained observation window:

```bash
kubectl -n llm get deployment -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{.spec.template.metadata.annotations.llm\.cogito\.dev/active-model}{"\t"}{range .spec.template.spec.containers[*]}{.name}{"="}{.args}{";"}{end}{"\n"}{end}' \
  > /tmp/llm-deployment-fields.before.txt
kubectl -n llm get pod -o wide > /tmp/llm-pods.before.txt
make observation-preflight
flux reconcile source oci llm-operator -n llm
flux reconcile helmrelease llm-operator -n llm --with-source
kubectl -n llm rollout status deployment/llm-operator
```

Immediately verify the live Deployment retained the safety flag:

```bash
kubectl -n llm get deployment llm-operator \
  -o jsonpath='{.spec.template.spec.containers[0].args}'
```

After reviewing references, commit only the backend and model CRs that describe
existing workloads to Cogito GitOps and reconcile their owning Kustomization.
Do not use these repository samples as direct imperative applies. Apply
`LLMActiveModel` only after confirming transitions are disabled in the live
manager:

```bash
# Replace <llm-models> with the reviewed Cogito Flux Kustomization name.
flux reconcile kustomization <llm-models> -n flux-system --with-source
```

Validate observed conditions and metadata:

```bash
kubectl -n llm get llmbackend,llmmodel,llmmodeloverlay,llmactivemodel -o wide
kubectl -n llm get llmbackend,llmmodel,llmmodeloverlay,llmactivemodel -o yaml
kubectl -n llm logs deployment/llm-operator --since=1h
```

Expected observation-mode behavior:

- backends report Deployment existence, replica counts, annotations, and health;
- models report configuration and backend resolution;
- overlays report base-model resolution;
- an active-model request reports `TransitionsDisabled` and does not call the
  cache-manager or patch any Deployment;
- runtime metadata is only written after an operator-owned transition, so it is
  not expected to appear during passive observation.

Check operator metrics locally:

```bash
kubectl -n llm port-forward service/llm-operator-metrics 18081:8081
curl --fail http://127.0.0.1:18081/metrics
```

Compare backend annotations and status with the proxy's current catalog/state
using the proxy's existing operational endpoint or logs. Record any canonical
model-name, backend type, replica, health, or active-model mismatch before
cutover work.

After the agreed observation window, prove the workloads did not change:

```bash
kubectl -n llm get deployment -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{.spec.template.metadata.annotations.llm\.cogito\.dev/active-model}{"\t"}{range .spec.template.spec.containers[*]}{.name}{"="}{.args}{";"}{end}{"\n"}{end}' \
  > /tmp/llm-deployment-fields.after.txt
diff -u /tmp/llm-deployment-fields.before.txt /tmp/llm-deployment-fields.after.txt
kubectl -n llm get pod -o wide
```

Expected differences are limited to unrelated workload reconciliation and the
new operator Deployment. Existing backend container arguments, replicas, and
operator-owned activation annotations must not change.

## Rollback

The safest rollback leaves CRDs and observations intact and suspends or reverts
the HelmRelease in Cogito GitOps:

```bash
flux suspend helmrelease llm-operator -n llm
```

Commit the equivalent suspension or removal in Cogito GitOps so Flux does not
restore the release. Alternatively, leave the manager running with
`--enable-transitions=false`. The proxy remains the serving controller in both
cases. Do not delete CRDs as an incident response step: that removes observation
data and may interact with model finalizers. Do not change backend,
cache-manager, or proxy workloads as part of operator rollback.
